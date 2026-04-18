// Package route53 is a scaffolded AWS Route 53 provider. It registers under
// the name "route53" and implements the dnsprovider.Provider interface, but
// every method returns dnsprovider.ErrNotImplemented. Its purpose today is
// to validate that the Provider interface is shaped to accommodate a
// non-Cloudflare backend (different credential model, no native comment
// field, so ownership will be persisted via a sibling TXT record in the real
// implementation).
//
// Configuration keys (ConfigMap, active when provider: route53):
//
//	route53.hosted-zone-id  required, the hosted zone to manage
//	route53.region          optional, defaults to us-east-1
//
// Secret keys (Secret greydns-secret):
//
//	aws-access-key-id       required
//	aws-secret-access-key   required
package route53

import (
	"context"
	"errors"

	"github.com/math280h/greydns/internal/dnsprovider"
	"github.com/math280h/greydns/internal/dnsprovider/registry"
)

const providerName = "route53"

func init() {
	registry.Register(providerName, New)
}

// Provider is a stub implementation of dnsprovider.Provider for AWS Route 53.
type Provider struct {
	hostedZoneID    string
	region          string
	accessKeyID     string
	secretAccessKey string
}

// New is the registry.Factory for the route53 provider.
func New(cfg registry.ProviderConfig, secret map[string][]byte) (dnsprovider.Provider, error) {
	zoneID, err := cfg.GetRequired("hosted-zone-id")
	if err != nil {
		return nil, err
	}
	akid := string(secret["aws-access-key-id"])
	sak := string(secret["aws-secret-access-key"])
	if akid == "" || sak == "" {
		return nil, errors.New("route53: secret keys \"aws-access-key-id\" and \"aws-secret-access-key\" are required")
	}
	return &Provider{
		hostedZoneID:    zoneID,
		region:          cfg.GetOptional("region", "us-east-1"),
		accessKeyID:     akid,
		secretAccessKey: sak,
	}, nil
}

func (p *Provider) Name() string { return providerName }

func (p *Provider) SupportedRecordTypes() []dnsprovider.RecordType {
	return []dnsprovider.RecordType{dnsprovider.RecordTypeA, dnsprovider.RecordTypeCNAME}
}

func (p *Provider) ListZones(_ context.Context) ([]dnsprovider.Zone, error) {
	return nil, dnsprovider.ErrNotImplemented
}

func (p *Provider) ListOwnedRecords(_ context.Context, _ string) ([]dnsprovider.Record, error) {
	return nil, dnsprovider.ErrNotImplemented
}

func (p *Provider) CreateRecord(_ context.Context, _ dnsprovider.Record) (dnsprovider.Record, error) {
	return dnsprovider.Record{}, dnsprovider.ErrNotImplemented
}

func (p *Provider) UpdateRecord(_ context.Context, _ dnsprovider.Record) (dnsprovider.Record, error) {
	return dnsprovider.Record{}, dnsprovider.ErrNotImplemented
}

func (p *Provider) DeleteRecord(_ context.Context, _ string, _ string) error {
	return dnsprovider.ErrNotImplemented
}
