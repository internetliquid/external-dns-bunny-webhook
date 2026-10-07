package bunny

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/puzpuzpuz/xsync/v3"
	"github.com/samber/lo"
	"github.com/samber/oops"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
)

var (
	_ provider.Provider = (*Provider)(nil)
)

const (
	providerValueZoneId   = "BunnyZoneID"
	providerValueRecordId = "BunnyRecordID"
)

type Options struct {
	APIKey               string   `env:"API_KEY, required"`
	DryRun               bool     `env:"DRY_RUN, default=false"`
	ExcludeDomains       []string `env:"EXCLUDE_DOMAINS"`
	ExcludeDomainsRegexp string   `env:"EXCLUDE_DOMAINS_REGEXP"`
	IncludeDomains       []string `env:"INCLUDE_DOMAINS"`
	IncludeDomainsRegexp string   `env:"INCLUDE_DOMAINS_REGEXP"`
}

type Provider struct {
	Options      Options
	client       Client
	filter       endpoint.DomainFilterInterface
	includeZones []string
	zoneMap      *xsync.MapOf[string, int64]
}

func NewProvider(client Client, options Options) *Provider {
	provider := &Provider{
		Options:      options,
		client:       client,
		filter:       getDomainFilter(options),
		includeZones: normalizeZones(options.IncludeDomains),
		zoneMap:      xsync.NewMapOf[string, int64](),
	}

	// On startup, fetch zones so that all available zones are cached. This
	// is necessary to avoid making a call to the API during creates as we
	// need the zone ID to create a record. In addition, this data is used
	// to accurately exctract recordName from the full dnsName. Without it,
	// we could not accurately handle all the expected TLDs without maintaing
	// an internal list.
	_, err := provider.fetchZones(context.Background())
	if err != nil {
		slog.Error("Failed to fetch zones on startup.",
			slog.Any("error", err))
	}

	return provider
}

func (p *Provider) allZones() []string {
	var zones []string

	p.zoneMap.Range(func(key string, value int64) bool {
		zones = append(zones, key)
		return true
	})

	return zones
}

func (p *Provider) cacheZone(zone *Zone) {
	p.zoneMap.Store(zone.Domain, zone.ID)
}

func (p *Provider) Records(ctx context.Context) ([]*endpoint.Endpoint, error) {
	errs := oops.In("Provider").
		Span("Records")

	zones, err := p.fetchZones(ctx)
	if err != nil {
		slog.Error("Failed to fetch zones",
			slog.Any("error", err))

		return nil, errs.Wrapf(err, "failed to fetch zones")
	}

	var endpoints []*endpoint.Endpoint
	for _, zone := range zones {
		// With an include list set, only its zones' records are returned.
		if len(p.includeZones) > 0 && !lo.Contains(p.includeZones, normalizeName(zone.Domain)) {
			continue
		}

		for _, record := range zone.Records {
			// First check if the record type is supported, and if not
			// skip the record altogether.
			if !provider.SupportedRecordType(record.Type.String()) {
				continue
			}

			endpoints = append(endpoints, recordToEndpoint(zone.Domain, record))
		}
	}

	return endpoints, nil
}

func (p *Provider) ApplyChanges(ctx context.Context, changes *plan.Changes) error {
	errs := oops.In("Provider").
		With("creates", len(changes.Create)).
		With("deletes", len(changes.Delete)).
		With("updates", len(changes.UpdateNew)).
		Span("ApplyChanges")

	if changes == nil || !changes.HasChanges() {
		slog.Debug("Skipping request to apply changes because no changes are present")

		return nil
	}

	// If we are in dry-run mode, we can skip the creation of endpoints and
	// only log the changes that would have been made.
	if p.Options.DryRun {
		return p.applyChangesDryRun(ctx, changes)
	}

	err := p.createEndpoints(ctx, changes.Create)
	if err != nil {
		slog.Error("Failed to create endpoints",
			slog.Any("error", err))

		return errs.Wrapf(err, "failed to apply creates")
	}

	// If we have no deletions or updates, we can return early to avoid making a (potentially)
	// expensive call to the Bunny.net API.
	if len(changes.Delete) == 0 && len(changes.UpdateOld) == 0 {
		return nil
	}

	var lookupEndpoints []*endpoint.Endpoint
	lookupEndpoints = append(lookupEndpoints, changes.Delete...)
	lookupEndpoints = append(lookupEndpoints, changes.UpdateOld...)

	tuples, err := p.fetchIdentifiers(ctx, lookupEndpoints)
	if err != nil {
		slog.Error("Failed to fetch identifiers",
			slog.Any("error", err))

		return errs.Wrapf(err, "failed to fetch identifiers")
	}

	err = p.deleteEndpoints(ctx, tuples, changes.Delete)
	if err != nil {
		slog.Error("Failed to delete endpoints",
			slog.Any("error", err))

		return errs.Wrapf(err, "failed to apply deletes")
	}

	err = p.updateEndpoints(ctx, tuples, changes.UpdateNew)
	if err != nil {
		slog.Error("Failed to update endpoints",
			slog.Any("error", err))

		return errs.Wrapf(err, "failed to apply updates")
	}

	return nil
}

func (p *Provider) applyChangesDryRun(ctx context.Context, changes *plan.Changes) error {
	errs := oops.In("Provider").
		With("creates", len(changes.Create)).
		With("deletes", len(changes.Delete)).
		With("updates", len(changes.UpdateNew)).
		Span("applyChangesDryRun")

	if changes == nil || !changes.HasChanges() {
		slog.Debug("DRY RUN: Skipping request to apply changes because no changes are present")
		return nil
	}

	for _, ep := range changes.Create {
		bunnyZoneID, _, domainName, err := p.getZoneID(ep.DNSName)
		if err != nil {
			return errs.Wrapf(err, "failed to create record %q", ep.DNSName)
		}

		slog.InfoContext(ctx, "DRY RUN: Create record",
			slog.String("zone", domainName),
			slog.Int64("zone_id", bunnyZoneID),
			slog.Group("record",
				slog.Any("name", ep.DNSName),
				slog.Any("type", ep.RecordType),
				slog.Any("value", ep.Targets),
				slog.Any("ttl", ep.RecordTTL),
			))
	}

	// If we have no deletions or updates, we can return early to avoid making a (potentially)
	// expensive call to the Bunny.net API.
	if len(changes.Delete) == 0 && len(changes.UpdateOld) == 0 {
		return nil
	}

	var lookupEndpoints []*endpoint.Endpoint
	lookupEndpoints = append(lookupEndpoints, changes.Delete...)
	lookupEndpoints = append(lookupEndpoints, changes.UpdateOld...)

	tuples, err := p.fetchIdentifiers(ctx, lookupEndpoints)
	if err != nil {
		slog.Error("Failed to fetch identifiers",
			slog.Any("error", err))

		return errs.Wrapf(err, "failed to fetch identifiers")
	}

	for _, ep := range changes.Delete {
		tuple, ok := tuples[identifierKey(ep.DNSName, ep.RecordType)]
		if !ok {
			slog.InfoContext(ctx, "DRY RUN: Delete record (would skip, not found in Bunny API)",
				slog.Group("record",
					slog.String("name", ep.DNSName),
					slog.String("type", ep.RecordType),
					slog.String("value", lo.FirstOr(ep.Targets, "")),
					slog.Int("ttl", int(ep.RecordTTL)),
				))

			continue
		}

		slog.InfoContext(ctx, "DRY RUN: Delete record",
			slog.Int64("zone_id", tuple.ZoneID),
			slog.Group("record",
				slog.Int64("id", tuple.RecordID),
				slog.String("name", ep.DNSName),
				slog.String("type", ep.RecordType),
				slog.String("value", lo.FirstOr(ep.Targets, "")),
				slog.Int("ttl", int(ep.RecordTTL)),
			))
	}

	for _, ep := range changes.UpdateOld {
		tuple, ok := tuples[identifierKey(ep.DNSName, ep.RecordType)]
		if !ok {
			slog.InfoContext(ctx, "DRY RUN: Update record (would skip, not found in Bunny API)",
				slog.Group("current",
					slog.String("name", ep.DNSName),
					slog.String("type", ep.RecordType),
					slog.String("value", lo.FirstOr(ep.Targets, "")),
					slog.Int("ttl", int(ep.RecordTTL)),
				))

			continue
		}

		var new *endpoint.Endpoint
		for _, n := range changes.UpdateNew {
			if identifierKey(n.DNSName, n.RecordType) == identifierKey(ep.DNSName, ep.RecordType) {
				new = n
				break
			}
		}

		// The live path updates only records with a new endpoint, so skip as it does.
		if new == nil {
			continue
		}

		slog.InfoContext(ctx, "DRY RUN: Update record",
			slog.Int64("zone_id", tuple.ZoneID),
			slog.Group("current",
				slog.Int64("id", tuple.RecordID),
				slog.Any("name", ep.DNSName),
				slog.Any("type", ep.RecordType),
				slog.Any("value", ep.Targets),
				slog.Any("ttl", ep.RecordTTL),
			),
			slog.Group("updated",
				slog.Int64("id", tuple.RecordID),
				slog.Any("value", new.Targets),
				slog.Any("ttl", new.RecordTTL),
			))
	}

	return nil
}

// AdjustEndpoints canonicalizes a set of candidate endpoints.
// It is called with a set of candidate endpoints obtained from the various sources.
// It returns a set modified as required by the provider. The provider is responsible for
// adding, removing, and modifying the ProviderSpecific properties to match
// the endpoints that the provider returns in `Records` so that the change plan will not have
// unnecessary (potentially failing) changes. It may also modify other fields, add, or remove
// Endpoints. It is permitted to modify the supplied endpoints.
func (p *Provider) AdjustEndpoints(incoming []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) {
	errs := oops.In("Provider").
		Span("AdjustEndpoints")

	fetched, err := p.Records(context.Background())
	if err != nil {
		slog.Error("Failed to fetch records",
			slog.Any("error", err))

		return nil, errs.Wrapf(err, "failed to fetch records")
	}

	// The provider-specific properties a new record is created with. Records
	// read back carry all of them, so an endpoint that lacks one would be
	// updated on every pass.
	defaults := &endpoint.Endpoint{}
	opts, _ := providerSpecificOptionsFromEndpoint(defaults)
	opts.ApplyToEndpoint(defaults)

	for _, editing := range incoming {
		for _, property := range defaults.ProviderSpecific {
			if _, ok := editing.GetProviderSpecificProperty(property.Name); !ok {
				editing.SetProviderSpecificProperty(property.Name, property.Value)
			}
		}

		for _, checked := range fetched {
			if editing.DNSName != checked.DNSName || editing.RecordType != checked.RecordType || editing.SetIdentifier != checked.SetIdentifier {
				continue
			}

			for key, value := range checked.Labels {
				editing.Labels[key] = value
			}
		}
	}

	return incoming, nil
}

// GetDomainFilter returns the domain filter used by this provider.
func (p *Provider) GetDomainFilter() endpoint.DomainFilterInterface {
	return p.filter
}

// getZoneID returns the zone ID, record name and zone for a fully qualified
// DNS name (record + domain. e.g. foo.example.com) using the zone map. If no
// zone can be found, an error is returned.
func (p *Provider) getZoneID(dnsName string) (int64, string, string, error) {
	errs := oops.In("Provider").
		Span("getZoneID").
		With("dnsName", dnsName)

	recordName, domainName, err := p.zoneFor(p.allZones(), dnsName)
	if err != nil {
		return 0, "", "", errs.Wrap(err)
	}

	zoneID, ok := p.zoneMap.Load(domainName)
	if !ok {
		return 0, "", "", errs.Errorf("zone ID for DNS name %q (%s) not found", dnsName, domainName)
	}

	return zoneID, recordName, domainName, nil
}

// createEndpoints creates the given endpoints.
func (p *Provider) createEndpoints(ctx context.Context, creates []*endpoint.Endpoint) error {
	errs := oops.In("Provider").
		Span("createEndpoints").
		With("creates", len(creates))

	for _, create := range creates {
		bunnyZoneID, recordName, domainName, err := p.getZoneID(create.DNSName)
		if err != nil {
			return errs.Wrapf(err, "failed to create record %q", create.DNSName)
		}

		opts, err := providerSpecificOptionsFromEndpoint(create)
		if err != nil {
			return errs.Wrapf(err, "failed to create record %q", create.DNSName)
		}

		record := CreateRecordRequest{
			Name:        recordName,
			Type:        RecordTypeFromString(create.RecordType),
			Value:       create.Targets[0],
			TTLSeconds:  int(create.RecordTTL),
			MonitorType: opts.MonitorType,
			Weight:      opts.Weight,
			Disabled:    opts.Disabled,
		}

		slog.Debug("Creating Record.",
			slog.String("zone", domainName),
			slog.Int64("zone_id", bunnyZoneID),
			slog.Group("record",
				slog.String("name", record.Name),
				slog.String("type", record.Type.String()),
				slog.String("value", record.Value),
				slog.Int("ttl", record.TTLSeconds),
				slog.String("monitor_type", record.MonitorType.String()),
				slog.Int("weight", record.Weight),
				slog.Bool("disabled", record.Disabled),
			),
		)

		created, err := p.client.CreateRecord(ctx, strconv.FormatInt(bunnyZoneID, 10), record)
		if err != nil {
			slog.Error("Failed to create record.",
				slog.Any("error", err),
				slog.Group("record",
					slog.String("name", record.Name),
					slog.String("type", record.Type.String()),
					slog.String("value", record.Value),
					slog.Int("ttl", record.TTLSeconds),
					slog.String("monitor_type", record.MonitorType.String()),
					slog.Int("weight", record.Weight),
					slog.Bool("disabled", record.Disabled),
				))

			return err
		}

		slog.InfoContext(ctx, "Record created successfully.",
			slog.String("zone", domainName),
			slog.Int64("zone_id", bunnyZoneID),
			slog.Group("record",
				slog.Int64("id", created.ID),
				slog.String("name", record.Name),
				slog.String("type", record.Type.String()),
				slog.String("value", record.Value),
				slog.Int("ttl", record.TTLSeconds),
				slog.String("monitor_type", record.MonitorType.String()),
				slog.Int("weight", record.Weight),
				slog.Bool("disabled", record.Disabled),
			))
	}

	return nil
}

// updateEndpoints updates the given endpoints.
func (p *Provider) updateEndpoints(ctx context.Context, identifiers map[string]identifierTuple, updates []*endpoint.Endpoint) error {
	for _, update := range updates {
		tuple, ok := identifiers[identifierKey(update.DNSName, update.RecordType)]
		if !ok {
			return fmt.Errorf("failed to get record identifiers for %q", update.DNSName)
		}

		opts, err := providerSpecificOptionsFromEndpoint(update)
		if err != nil {
			return fmt.Errorf("failed to update record %q", update.DNSName)
		}

		record := UpdateRecordRequest{
			TTLSeconds:  int(update.RecordTTL),
			Value:       update.Targets[0],
			MonitorType: opts.MonitorType,
			Weight:      opts.Weight,
			Disabled:    opts.Disabled,
		}

		err = p.client.UpdateRecord(ctx, tuple.ZoneID, tuple.RecordID, record)
		if err != nil {
			return err
		}

		slog.InfoContext(ctx, "Updated record.",
			slog.Int64("zone_id", tuple.ZoneID),
			slog.Group("record",
				slog.Int64("id", tuple.RecordID),
				slog.String("name", update.DNSName),
				slog.String("value", record.Value),
				slog.Int("ttl", record.TTLSeconds),
				slog.String("monitor_type", record.MonitorType.String()),
				slog.Int("weight", record.Weight),
				slog.Bool("disabled", record.Disabled),
			))
	}

	return nil
}

func (p *Provider) deleteEndpoints(ctx context.Context, identifiers map[string]identifierTuple, deletions []*endpoint.Endpoint) error {
	for _, deletion := range deletions {
		tuple, ok := identifiers[identifierKey(deletion.DNSName, deletion.RecordType)]
		if !ok {
			return fmt.Errorf("failed to get record identifiers for %q", deletion.DNSName)
		}

		opts, err := providerSpecificOptionsFromEndpoint(deletion)
		if err != nil {
			// We can ignore this error as we are deleting the record anyway and we'll always
			// get a usable opts struct (no nil pointers).
		}

		err = p.client.DeleteRecord(ctx, tuple.ZoneID, tuple.RecordID)
		if err != nil {
			return err
		}

		slog.InfoContext(ctx, "Deleted record.",
			slog.Int64("zone_id", tuple.ZoneID),
			slog.Group("record",
				slog.Int64("id", tuple.RecordID),
				slog.String("name", deletion.DNSName),
				slog.String("value", deletion.Targets[0]),
				slog.Int("ttl", int(deletion.RecordTTL)),
				slog.String("monitor_type", opts.MonitorType.String()),
				slog.Int("weight", opts.Weight),
				slog.Bool("disabled", opts.Disabled),
			))

	}

	return nil
}

type identifierTuple struct {
	ZoneID   int64
	RecordID int64
}

// identifierKey ignores the name's case, so an update's old endpoint, spelled
// as Bunny stores the record, pairs with its new endpoint.
func identifierKey(dnsName string, recordType string) string {
	return normalizeName(dnsName) + "|" + recordType
}

// fetchIdentifiers fetches the zone and record identifiers for the given endpoints by listing
// all zones and records and returning a map of DNS names to identifiers. This allows us to get
// all the identifiers in a single call (or paginated calls) and then use them to update or delete
// records.
//
// The function matches on both DNS name and record type to ensure we get the correct record
// when multiple record types exist for the same name (e.g., both A and TXT records).
func (p *Provider) fetchIdentifiers(ctx context.Context, endpoints []*endpoint.Endpoint) (map[string]identifierTuple, error) {
	identifiers := make(map[string]identifierTuple)

	zones, err := p.fetchZones(ctx)
	if err != nil {
		return nil, err
	}

	var domainNames []string
	for _, zone := range zones {
		domainNames = append(domainNames, zone.Domain)
	}

	for _, ep := range endpoints {
		recordName, domainName, err := p.zoneFor(domainNames, ep.DNSName)
		if err != nil {
			return nil, err
		}

		for _, zone := range zones {
			if zone.Domain != domainName {
				continue
			}

			for _, record := range zone.Records {
				if !strings.EqualFold(record.Name, recordName) || record.Type.String() != ep.RecordType {
					continue
				}

				identifiers[identifierKey(ep.DNSName, ep.RecordType)] = identifierTuple{
					ZoneID:   zone.ID,
					RecordID: record.ID,
				}
			}
		}
	}

	return identifiers, nil
}

func (p *Provider) fetchZones(ctx context.Context) ([]*Zone, error) {
	var page = 1
	var zones []*Zone

	for {
		results, err := p.client.ListZones(ctx, ListZonesRequest{
			Page:    page,
			PerPage: 1000,
		})

		if err != nil {
			return nil, err
		}

		zones = append(zones, results.Items...)

		if !results.HasMoreItems {
			break
		}

		page++
	}

	// Cache the zone IDs for lookup during creates. The listing replaces the
	// cache, so a zone deleted from Bunny stops matching.
	listed := make(map[string]bool, len(zones))
	for _, zone := range zones {
		p.cacheZone(zone)
		listed[zone.Domain] = true
	}

	p.zoneMap.Range(func(domain string, _ int64) bool {
		if !listed[domain] {
			p.zoneMap.Delete(domain)
		}
		return true
	})

	return zones, nil
}

// zoneFor splits a DNS name into its record name and zone. Candidates are the
// include-list zones when the list has any, otherwise every zone in the
// account. A candidate matches on whole labels and the longest match wins. The
// record name is "" when the DNS name is the zone itself.
func (p *Provider) zoneFor(zones []string, dnsName string) (string, string, error) {
	name := normalizeName(dnsName)

	account := make(map[string]string, len(zones))
	for _, zone := range zones {
		account[normalizeName(zone)] = zone
	}

	candidates := p.includeZones
	if len(candidates) == 0 {
		candidates = lo.Keys(account)
	}

	best := ""
	for _, candidate := range candidates {
		if (name == candidate || strings.HasSuffix(name, "."+candidate)) && len(candidate) > len(best) {
			best = candidate
		}
	}

	if best == "" {
		if len(p.includeZones) > 0 {
			return "", "", fmt.Errorf("no zone on BUNNY_INCLUDE_DOMAINS matches %q", dnsName)
		}
		return "", "", fmt.Errorf("no zone in the Bunny account matches %q", dnsName)
	}

	zone, ok := account[best]
	if !ok {
		return "", "", fmt.Errorf("zone %q is on BUNNY_INCLUDE_DOMAINS but missing from Bunny", best)
	}

	if name == best {
		return "", zone, nil
	}

	return strings.TrimSuffix(name, "."+best), zone, nil
}

// normalizeName lower-cases a DNS name and drops its trailing dot.
func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

// normalizeZones normalizes each zone and drops blank entries.
func normalizeZones(zones []string) []string {
	var normalized []string
	for _, zone := range zones {
		if zone = normalizeName(zone); zone != "" {
			normalized = append(normalized, zone)
		}
	}

	return normalized
}

func getDomainFilter(options Options) endpoint.DomainFilterInterface {
	if options.ExcludeDomainsRegexp != "" || options.IncludeDomainsRegexp != "" {
		return endpoint.NewRegexDomainFilter(
			regexp.MustCompile(options.IncludeDomainsRegexp),
			regexp.MustCompile(options.ExcludeDomainsRegexp),
		)
	}

	return endpoint.NewDomainFilterWithExclusions(options.IncludeDomains, options.ExcludeDomains)
}
