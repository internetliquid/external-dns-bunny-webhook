package bunny

import (
	"sigs.k8s.io/external-dns/endpoint"
)

// recordToEndpoint maps a Bunny record to an endpoint. Bunny names an apex
// record "", so it maps to the zone's own name.
func recordToEndpoint(domain string, record *Record) *endpoint.Endpoint {
	dnsName := domain
	if record.Name != "" {
		dnsName = record.Name + "." + domain
	}

	ep := endpoint.NewEndpointWithTTL(
		dnsName,
		record.Type.String(),
		endpoint.TTL(record.TTLSeconds),
		record.Value,
	)

	ps := providerSpecificOptionsFromRecord(record)
	ps.ApplyToEndpoint(ep)

	return ep
}
