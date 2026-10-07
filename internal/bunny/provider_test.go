package bunny

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
)

const testZone = "apps.example.com"

type fakeCreate struct {
	zoneID string
	req    CreateRecordRequest
}

type fakeCall struct {
	zoneID   int64
	recordID int64
	value    string
}

// fakeClient serves zones from memory and records every write.
type fakeClient struct {
	zones   []*Zone
	nextID  int64
	creates []fakeCreate
	updates []fakeCall
	deletes []fakeCall
}

func (c *fakeClient) ListZones(_ context.Context, _ ListZonesRequest) (*ListZonesResponse, error) {
	return &ListZonesResponse{Items: c.zones, TotalItems: len(c.zones)}, nil
}

func (c *fakeClient) CreateRecord(_ context.Context, zoneID string, r CreateRecordRequest) (*Record, error) {
	c.creates = append(c.creates, fakeCreate{zoneID: zoneID, req: r})

	c.nextID++
	record := &Record{
		ID:          c.nextID,
		Type:        r.Type,
		TTLSeconds:  r.TTLSeconds,
		Value:       r.Value,
		Name:        r.Name,
		Weight:      r.Weight,
		MonitorType: r.MonitorType,
		Disabled:    r.Disabled,
	}

	for _, zone := range c.zones {
		if strconv.FormatInt(zone.ID, 10) == zoneID {
			zone.Records = append(zone.Records, record)
		}
	}

	return record, nil
}

func (c *fakeClient) UpdateRecord(_ context.Context, zoneID int64, recordID int64, r UpdateRecordRequest) error {
	c.updates = append(c.updates, fakeCall{zoneID: zoneID, recordID: recordID, value: r.Value})
	return nil
}

func (c *fakeClient) DeleteRecord(_ context.Context, zoneID int64, recordID int64) error {
	c.deletes = append(c.deletes, fakeCall{zoneID: zoneID, recordID: recordID})
	return nil
}

// captureLogs sends the default logger to a buffer for the rest of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return &logs
}

func zonesNamed(domains ...string) []*Zone {
	var zones []*Zone
	for i, domain := range domains {
		zones = append(zones, &Zone{ID: int64(i + 1), Domain: domain})
	}

	return zones
}

func TestZoneFor(t *testing.T) {
	tests := []struct {
		name       string
		zones      []string
		include    []string
		dnsName    string
		wantRecord string
		wantZone   string
		wantErr    string
	}{
		{
			name:       "look-alike zone in the account",
			zones:      []string{"ample.com", testZone},
			dnsName:    "joe." + testZone,
			wantRecord: "joe",
			wantZone:   testZone,
		},
		{
			name:    "look-alike zone alone does not match",
			zones:   []string{"ample.com"},
			dnsName: "joe." + testZone,
			wantErr: `no zone in the Bunny account matches "joe.apps.example.com"`,
		},
		{
			name:       "parent and child zones, child wins",
			zones:      []string{"example.com", testZone},
			include:    []string{"example.com", testZone},
			dnsName:    "joe." + testZone,
			wantRecord: "joe",
			wantZone:   testZone,
		},
		{
			name:       "name equal to a zone",
			zones:      []string{testZone},
			include:    []string{testZone},
			dnsName:    testZone,
			wantRecord: "",
			wantZone:   testZone,
		},
		{
			name:    "name under no included zone",
			zones:   []string{"ample.com", testZone},
			include: []string{testZone},
			dnsName: "joe.ample.com",
			wantErr: `no zone on BUNNY_INCLUDE_DOMAINS matches "joe.ample.com"`,
		},
		{
			name:       "case and trailing dot",
			zones:      []string{testZone},
			include:    []string{"APPS.example.com."},
			dnsName:    "Joe.Apps.Example.COM.",
			wantRecord: "joe",
			wantZone:   testZone,
		},
		{
			name:    "included zone missing from the account",
			zones:   []string{"ample.com"},
			include: []string{testZone},
			dnsName: "joe." + testZone,
			wantErr: `zone "apps.example.com" is on BUNNY_INCLUDE_DOMAINS but missing from Bunny`,
		},
		{
			name:       "blank include entries are dropped",
			zones:      []string{testZone},
			include:    []string{"", ""},
			dnsName:    "joe." + testZone,
			wantRecord: "joe",
			wantZone:   testZone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewProvider(&fakeClient{}, Options{IncludeDomains: tt.include})

			record, zone, err := p.zoneFor(tt.zones, tt.dnsName)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if record != tt.wantRecord || zone != tt.wantZone {
				t.Errorf("got (%q, %q), want (%q, %q)", record, zone, tt.wantRecord, tt.wantZone)
			}
		})
	}
}

func TestZoneCacheReplacedByLaterListing(t *testing.T) {
	client := &fakeClient{zones: zonesNamed(testZone, "ample.com")}
	p := NewProvider(client, Options{IncludeDomains: []string{testZone}})

	client.zones = zonesNamed("ample.com")
	if _, err := p.Records(context.Background()); err != nil {
		t.Fatalf("Records: %v", err)
	}

	err := p.ApplyChanges(context.Background(), &plan.Changes{
		Create: []*endpoint.Endpoint{endpoint.NewEndpoint("joe."+testZone, endpoint.RecordTypeA, "192.0.2.1")},
	})
	if err == nil || !strings.Contains(err.Error(), "missing from Bunny") {
		t.Fatalf("err = %v, want the zone reported missing from Bunny", err)
	}
	if len(client.creates) != 0 {
		t.Errorf("creates = %+v, want none", client.creates)
	}
}

func TestApplyChangesCreatesUnderMatchedZone(t *testing.T) {
	client := &fakeClient{zones: zonesNamed("ample.com", testZone)}
	p := NewProvider(client, Options{IncludeDomains: []string{testZone}})

	err := p.ApplyChanges(context.Background(), &plan.Changes{
		Create: []*endpoint.Endpoint{endpoint.NewEndpoint("joe."+testZone, endpoint.RecordTypeA, "192.0.2.1")},
	})
	if err != nil {
		t.Fatalf("ApplyChanges: %v", err)
	}

	if len(client.creates) != 1 {
		t.Fatalf("creates = %+v, want one", client.creates)
	}
	if got := client.creates[0]; got.zoneID != "2" || got.req.Name != "joe" {
		t.Errorf("create went to zone %s as %q, want zone 2 as %q", got.zoneID, got.req.Name, "joe")
	}
}

func TestApplyChangesRefusesUnmatchedName(t *testing.T) {
	client := &fakeClient{zones: zonesNamed("ample.com", testZone)}
	p := NewProvider(client, Options{IncludeDomains: []string{testZone}})

	err := p.ApplyChanges(context.Background(), &plan.Changes{
		Create: []*endpoint.Endpoint{
			endpoint.NewEndpoint("joe.ample.com", endpoint.RecordTypeA, "192.0.2.1"),
			endpoint.NewEndpoint("joe."+testZone, endpoint.RecordTypeA, "192.0.2.1"),
		},
	})
	if err == nil || !strings.Contains(err.Error(), `no zone on BUNNY_INCLUDE_DOMAINS matches "joe.ample.com"`) {
		t.Fatalf("create err = %v, want the name refused", err)
	}
	if len(client.creates) != 0 {
		t.Errorf("creates = %+v, want none", client.creates)
	}

	err = p.ApplyChanges(context.Background(), &plan.Changes{
		Delete: []*endpoint.Endpoint{endpoint.NewEndpoint("joe.ample.com", endpoint.RecordTypeA, "192.0.2.1")},
	})
	if err == nil || !strings.Contains(err.Error(), `no zone on BUNNY_INCLUDE_DOMAINS matches "joe.ample.com"`) {
		t.Fatalf("delete err = %v, want the name refused", err)
	}
	if len(client.deletes) != 0 {
		t.Errorf("deletes = %+v, want none", client.deletes)
	}
}

func TestRefusedNameWritesNothing(t *testing.T) {
	a := func(dnsName string) *endpoint.Endpoint {
		return endpoint.NewEndpoint(dnsName, endpoint.RecordTypeA, "192.0.2.1")
	}

	tests := []struct {
		name    string
		changes plan.Changes
		wantErr string
	}{
		{
			name:    "valid create then a name under no included zone",
			changes: plan.Changes{Create: []*endpoint.Endpoint{a("new." + testZone), a("joe.ample.com")}},
			wantErr: `no zone on BUNNY_INCLUDE_DOMAINS matches "joe.ample.com"`,
		},
		{
			name:    "valid create plus a delete under no included zone",
			changes: plan.Changes{Create: []*endpoint.Endpoint{a("new." + testZone)}, Delete: []*endpoint.Endpoint{a("joe.ample.com")}},
			wantErr: `no zone on BUNNY_INCLUDE_DOMAINS matches "joe.ample.com"`,
		},
		{
			name:    "valid create plus a delete of a record Bunny lacks",
			changes: plan.Changes{Create: []*endpoint.Endpoint{a("new." + testZone)}, Delete: []*endpoint.Endpoint{a("gone." + testZone)}},
			wantErr: `failed to get record identifiers for "gone.apps.example.com"`,
		},
		{
			name: "valid delete plus an update of a record Bunny lacks",
			changes: plan.Changes{
				Delete:    []*endpoint.Endpoint{a("joe." + testZone)},
				UpdateOld: []*endpoint.Endpoint{a("gone." + testZone)},
				UpdateNew: []*endpoint.Endpoint{a("gone." + testZone)},
			},
			wantErr: `failed to get record identifiers for "gone.apps.example.com"`,
		},
	}

	for _, tt := range tests {
		for _, dryRun := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, dry run %v", tt.name, dryRun), func(t *testing.T) {
				client := &fakeClient{zones: []*Zone{
					{ID: 1, Domain: "ample.com"},
					{ID: 2, Domain: testZone, Records: []*Record{{ID: 11, Type: RecordTypeA, Name: "joe", Value: "192.0.2.1"}}},
				}}
				p := NewProvider(client, Options{IncludeDomains: []string{testZone}, DryRun: dryRun})
				logs := captureLogs(t)

				changes := tt.changes
				err := p.ApplyChanges(context.Background(), &changes)
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				if len(client.creates)+len(client.deletes)+len(client.updates) != 0 {
					t.Errorf("creates = %+v, deletes = %+v, updates = %+v, want none", client.creates, client.deletes, client.updates)
				}
				if strings.Contains(logs.String(), "DRY RUN:") {
					t.Errorf("log = %q, want no dry-run change logged", logs.String())
				}
			})
		}
	}
}

func TestApplyChangesUsesEachRecordsOwnID(t *testing.T) {
	newClient := func() *fakeClient {
		return &fakeClient{zones: []*Zone{{
			ID:     7,
			Domain: testZone,
			Records: []*Record{
				{ID: 11, Type: RecordTypeA, Name: "joe", Value: "192.0.2.1"},
				{ID: 12, Type: RecordTypeAAAA, Name: "joe", Value: "2001:db8::1"},
				{ID: 13, Type: RecordTypeTXT, Name: "a-joe", Value: "heritage=external-dns"},
			},
		}}}
	}

	t.Run("delete", func(t *testing.T) {
		client := newClient()
		p := NewProvider(client, Options{IncludeDomains: []string{testZone}})

		err := p.ApplyChanges(context.Background(), &plan.Changes{
			Delete: []*endpoint.Endpoint{
				endpoint.NewEndpoint("joe."+testZone, endpoint.RecordTypeA, "192.0.2.1"),
				endpoint.NewEndpoint("joe."+testZone, endpoint.RecordTypeAAAA, "2001:db8::1"),
			},
		})
		if err != nil {
			t.Fatalf("ApplyChanges: %v", err)
		}

		want := []fakeCall{{zoneID: 7, recordID: 11}, {zoneID: 7, recordID: 12}}
		if !reflect.DeepEqual(client.deletes, want) {
			t.Errorf("deletes = %+v, want %+v", client.deletes, want)
		}
	})

	t.Run("update", func(t *testing.T) {
		client := newClient()
		p := NewProvider(client, Options{IncludeDomains: []string{testZone}})

		err := p.ApplyChanges(context.Background(), &plan.Changes{
			UpdateOld: []*endpoint.Endpoint{
				endpoint.NewEndpoint("joe."+testZone, endpoint.RecordTypeA, "192.0.2.1"),
				endpoint.NewEndpoint("joe."+testZone, endpoint.RecordTypeAAAA, "2001:db8::1"),
			},
			UpdateNew: []*endpoint.Endpoint{
				endpoint.NewEndpoint("joe."+testZone, endpoint.RecordTypeA, "192.0.2.2"),
				endpoint.NewEndpoint("joe."+testZone, endpoint.RecordTypeAAAA, "2001:db8::2"),
			},
		})
		if err != nil {
			t.Fatalf("ApplyChanges: %v", err)
		}

		want := []fakeCall{{zoneID: 7, recordID: 11, value: "192.0.2.2"}, {zoneID: 7, recordID: 12, value: "2001:db8::2"}}
		if !reflect.DeepEqual(client.updates, want) {
			t.Errorf("updates = %+v, want %+v", client.updates, want)
		}
	})
}

func TestApplyChangesFindsRecordStoredWithCapitals(t *testing.T) {
	client := &fakeClient{zones: []*Zone{{
		ID:      7,
		Domain:  testZone,
		Records: []*Record{{ID: 11, Type: RecordTypeA, Name: "Joe", Value: "192.0.2.1"}},
	}}}
	p := NewProvider(client, Options{IncludeDomains: []string{testZone}})

	// external-dns sends the current record, as Records spells it, as the
	// update's old endpoint and as the delete.
	err := p.ApplyChanges(context.Background(), &plan.Changes{
		UpdateOld: []*endpoint.Endpoint{endpoint.NewEndpoint("Joe."+testZone, endpoint.RecordTypeA, "192.0.2.1")},
		UpdateNew: []*endpoint.Endpoint{endpoint.NewEndpoint("joe."+testZone, endpoint.RecordTypeA, "192.0.2.2")},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	err = p.ApplyChanges(context.Background(), &plan.Changes{
		Delete: []*endpoint.Endpoint{endpoint.NewEndpoint("Joe."+testZone, endpoint.RecordTypeA, "192.0.2.2")},
	})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	wantUpdates := []fakeCall{{zoneID: 7, recordID: 11, value: "192.0.2.2"}}
	wantDeletes := []fakeCall{{zoneID: 7, recordID: 11}}
	if !reflect.DeepEqual(client.updates, wantUpdates) || !reflect.DeepEqual(client.deletes, wantDeletes) {
		t.Errorf("updates = %+v, deletes = %+v, want %+v and %+v", client.updates, client.deletes, wantUpdates, wantDeletes)
	}
}

func TestDryRunPairsUpdateStoredWithCapitals(t *testing.T) {
	client := &fakeClient{zones: []*Zone{{
		ID:      7,
		Domain:  testZone,
		Records: []*Record{{ID: 11, Type: RecordTypeA, Name: "Joe", Value: "192.0.2.1"}},
	}}}
	p := NewProvider(client, Options{IncludeDomains: []string{testZone}, DryRun: true})

	// A dry run only logs, so the pairing shows in the log.
	logs := captureLogs(t)

	err := p.ApplyChanges(context.Background(), &plan.Changes{
		UpdateOld: []*endpoint.Endpoint{endpoint.NewEndpoint("Joe."+testZone, endpoint.RecordTypeA, "192.0.2.1")},
		UpdateNew: []*endpoint.Endpoint{endpoint.NewEndpoint("joe."+testZone, endpoint.RecordTypeA, "192.0.2.2")},
	})
	if err != nil {
		t.Fatalf("ApplyChanges: %v", err)
	}
	if len(client.updates) != 0 {
		t.Errorf("updates = %+v, want none in a dry run", client.updates)
	}
	if !strings.Contains(logs.String(), "updated.id=11 updated.value=192.0.2.2") {
		t.Errorf("log = %q, want the update of record 11 to 192.0.2.2", logs.String())
	}
}

func TestDryRunCreateResolvesZone(t *testing.T) {
	create := func(dryRun bool, dnsName string) (*fakeClient, error) {
		client := &fakeClient{zones: zonesNamed("ample.com", testZone)}
		p := NewProvider(client, Options{IncludeDomains: []string{testZone}, DryRun: dryRun})

		return client, p.ApplyChanges(context.Background(), &plan.Changes{
			Create: []*endpoint.Endpoint{endpoint.NewEndpoint(dnsName, endpoint.RecordTypeA, "192.0.2.1")},
		})
	}

	t.Run("name under no included zone", func(t *testing.T) {
		const want = `failed to create record "joe.ample.com": no zone on BUNNY_INCLUDE_DOMAINS matches "joe.ample.com"`

		_, liveErr := create(false, "joe.ample.com")
		client, dryErr := create(true, "joe.ample.com")
		if liveErr == nil || dryErr == nil || !strings.Contains(liveErr.Error(), want) || !strings.Contains(dryErr.Error(), want) {
			t.Fatalf("live err = %v, dry-run err = %v, want both to contain %q", liveErr, dryErr, want)
		}
		if len(client.creates) != 0 {
			t.Errorf("creates = %+v, want none", client.creates)
		}
	})

	t.Run("name under an included zone", func(t *testing.T) {
		logs := captureLogs(t)

		client, err := create(true, "joe."+testZone)
		if err != nil {
			t.Fatalf("ApplyChanges: %v", err)
		}
		if len(client.creates) != 0 {
			t.Errorf("creates = %+v, want none in a dry run", client.creates)
		}
		if !strings.Contains(logs.String(), "DRY RUN: Create record\" zone=apps.example.com zone_id=2") {
			t.Errorf("log = %q, want the create in zone apps.example.com (2)", logs.String())
		}
	})
}

func TestRecordsReturnsOnlyIncludedZones(t *testing.T) {
	newClient := func() *fakeClient {
		return &fakeClient{zones: []*Zone{
			{ID: 1, Domain: "ample.com", Records: []*Record{{ID: 21, Type: RecordTypeA, Name: "www", Value: "192.0.2.9"}}},
			{ID: 2, Domain: testZone, Records: []*Record{{ID: 11, Type: RecordTypeA, Name: "joe", Value: "192.0.2.1"}}},
		}}
	}

	tests := []struct {
		name    string
		include []string
		want    []string
	}{
		{name: "include list set", include: []string{"APPS.example.com."}, want: []string{"joe." + testZone}},
		{name: "no include list", want: []string{"www.ample.com", "joe." + testZone}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewProvider(newClient(), Options{IncludeDomains: tt.include})

			records, err := p.Records(context.Background())
			if err != nil {
				t.Fatalf("Records: %v", err)
			}

			var got []string
			for _, ep := range records {
				got = append(got, ep.DNSName)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("records = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRecordToEndpoint(t *testing.T) {
	tests := []struct {
		name       string
		recordName string
		want       string
	}{
		{name: "apex record", recordName: "", want: testZone},
		{name: "named record", recordName: "joe", want: "joe." + testZone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep := recordToEndpoint(testZone, &Record{Type: RecordTypeA, Name: tt.recordName, Value: "192.0.2.1"})
			if ep.DNSName != tt.want {
				t.Errorf("DNSName = %q, want %q", ep.DNSName, tt.want)
			}
		})
	}
}

func TestApexRecordReadBackUnchanged(t *testing.T) {
	client := &fakeClient{zones: zonesNamed(testZone)}
	p := NewProvider(client, Options{IncludeDomains: []string{testZone}})

	// The provider-specific values a record reads back with by default, so
	// the plan below compares on name, type and target.
	desired := endpoint.NewEndpointWithTTL(testZone, endpoint.RecordTypeA, 300, "192.0.2.1")
	(&providerSpecificOptions{Weight: 100}).ApplyToEndpoint(desired)

	if err := p.ApplyChanges(context.Background(), &plan.Changes{Create: []*endpoint.Endpoint{desired}}); err != nil {
		t.Fatalf("ApplyChanges: %v", err)
	}
	if len(client.creates) != 1 || client.creates[0].req.Name != "" {
		t.Fatalf("creates = %+v, want one apex record named \"\"", client.creates)
	}

	current, err := p.Records(context.Background())
	if err != nil {
		t.Fatalf("Records: %v", err)
	}

	next := (&plan.Plan{
		Current:        current,
		Desired:        []*endpoint.Endpoint{desired},
		ManagedRecords: []string{endpoint.RecordTypeA},
	}).Calculate()
	if next.Changes.HasChanges() {
		t.Errorf("next pass changes = %+v, want none", next.Changes)
	}
}

func TestAdjustEndpointsMatchesRecordAsCreated(t *testing.T) {
	tests := []struct {
		name        string
		created     map[string]string // properties of the endpoint the record was created from
		desired     map[string]string
		wantUpdates int
	}{
		{name: "no properties set"},
		{name: "weight above range", created: map[string]string{providerSpecificWeight: "150"}, desired: map[string]string{providerSpecificWeight: "150"}},
		{name: "weight below range", created: map[string]string{providerSpecificWeight: "0"}, desired: map[string]string{providerSpecificWeight: "0"}},
		{name: "weight not a number", created: map[string]string{providerSpecificWeight: "abc"}, desired: map[string]string{providerSpecificWeight: "abc"}},
		{name: "disabled not a bool", created: map[string]string{providerSpecificDisabled: "yes"}, desired: map[string]string{providerSpecificDisabled: "yes"}},
		{name: "explicit weight that differs", desired: map[string]string{providerSpecificWeight: "50"}, wantUpdates: 1},
	}

	newEndpoint := func(properties map[string]string) *endpoint.Endpoint {
		ep := endpoint.NewEndpoint("joe."+testZone, endpoint.RecordTypeA, "192.0.2.1")
		for name, value := range properties {
			ep.WithProviderSpecific(name, value)
		}

		return ep
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeClient{zones: zonesNamed(testZone)}
			p := NewProvider(client, Options{IncludeDomains: []string{testZone}})

			if err := p.ApplyChanges(context.Background(), &plan.Changes{Create: []*endpoint.Endpoint{newEndpoint(tt.created)}}); err != nil {
				t.Fatalf("create: %v", err)
			}

			adjusted, err := p.AdjustEndpoints([]*endpoint.Endpoint{newEndpoint(tt.desired)})
			if err != nil {
				t.Fatalf("AdjustEndpoints: %v", err)
			}

			current, err := p.Records(context.Background())
			if err != nil {
				t.Fatalf("Records: %v", err)
			}

			changes := (&plan.Plan{
				Current:        current,
				Desired:        adjusted,
				ManagedRecords: []string{endpoint.RecordTypeA},
			}).Calculate().Changes
			if len(changes.Create) != 0 || len(changes.Delete) != 0 || len(changes.UpdateNew) != tt.wantUpdates {
				t.Errorf("changes = %+v, want %d updates and nothing else", changes, tt.wantUpdates)
			}
		})
	}
}
