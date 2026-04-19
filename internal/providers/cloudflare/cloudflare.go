// Package cloudflare implements dnsprovider.Provider against the Cloudflare
// DNS API. It persists greydns ownership information in the record Comment
// field using the marker format "[greydns]owner=<namespace>/<name>".
//
// Configuration keys (ConfigMap, active when provider: cloudflare):
//
//	cloudflare.proxy-enabled  optional bool, default false
//
// Generic record settings (record-type, record-ttl, ingress-destination)
// are read by the controller and passed through the dnsprovider.Record
// value on each call.
//
// Secret keys (Secret greydns-secret):
//
//	cloudflare-token          required, API token with Zone:Read, DNS:Edit
package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"strings"

	cf "github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/dns"
	"github.com/cloudflare/cloudflare-go/v4/option"
	"github.com/cloudflare/cloudflare-go/v4/zones"

	"github.com/math280h/greydns/internal/dnsprovider"
	"github.com/math280h/greydns/internal/dnsprovider/registry"
)

const (
	providerName = "cloudflare"

	// commentMarker prefixes every greydns-managed record's comment so
	// List filtering can recognise them. Anything after the "=" is the
	// owner ref in the form "<namespace>/<name>".
	commentMarker = "[greydns]owner="
)

func init() { //nolint:gochecknoinits // required for provider self-registration
	registry.Register(providerName, New)
}

// Provider implements dnsprovider.Provider using the Cloudflare API.
type Provider struct {
	api     *cf.Client
	proxied bool
}

// New is the registry.Factory for the cloudflare provider.
func New(cfg registry.ProviderConfig, secret map[string][]byte) (dnsprovider.Provider, error) {
	token := string(secret["cloudflare-token"])
	if token == "" {
		return nil, errors.New("cloudflare: secret key \"cloudflare-token\" is empty or missing")
	}
	proxied, err := cfg.GetBool("proxy-enabled", false)
	if err != nil {
		return nil, fmt.Errorf("cloudflare: %w", err)
	}
	return &Provider{
		api:     cf.NewClient(option.WithAPIToken(token)),
		proxied: proxied,
	}, nil
}

func (p *Provider) Name() string { return providerName }

func (p *Provider) SupportedRecordTypes() []dnsprovider.RecordType {
	return []dnsprovider.RecordType{dnsprovider.RecordTypeA, dnsprovider.RecordTypeCNAME}
}

func (p *Provider) ListZones(ctx context.Context) ([]dnsprovider.Zone, error) {
	iter := p.api.Zones.ListAutoPaging(ctx, zones.ZoneListParams{})
	out := make([]dnsprovider.Zone, 0)
	for iter.Next() {
		z := iter.Current()
		out = append(out, dnsprovider.Zone{ID: z.ID, Name: z.Name})
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("cloudflare: list zones: %w", err)
	}
	return out, nil
}

func (p *Provider) ListOwnedRecords(ctx context.Context, zoneID string) ([]dnsprovider.Record, error) {
	iter := p.api.DNS.Records.ListAutoPaging(ctx, dns.RecordListParams{
		ZoneID: cf.F(zoneID),
	})
	out := make([]dnsprovider.Record, 0)
	for iter.Next() {
		rec := iter.Current()
		owner, ok := parseOwner(rec.Comment)
		if !ok {
			continue
		}
		out = append(out, dnsprovider.Record{
			ID:       rec.ID,
			ZoneID:   zoneID,
			Name:     rec.Name,
			Type:     dnsprovider.RecordType(rec.Type),
			Content:  rec.Content,
			TTL:      int(rec.TTL),
			OwnerRef: owner,
		})
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("cloudflare: list records for zone %s: %w", zoneID, err)
	}
	return out, nil
}

func (p *Provider) CreateRecord(ctx context.Context, rec dnsprovider.Record) (dnsprovider.Record, error) {
	param, err := p.buildRecordParam(rec)
	if err != nil {
		return dnsprovider.Record{}, err
	}
	resp, err := p.api.DNS.Records.New(ctx, dns.RecordNewParams{
		ZoneID: cf.F(rec.ZoneID),
		Record: param,
	})
	if err != nil {
		return dnsprovider.Record{}, fmt.Errorf("cloudflare: create record %s: %w", rec.Name, err)
	}
	return p.fromResponse(rec.ZoneID, resp), nil
}

func (p *Provider) UpdateRecord(ctx context.Context, rec dnsprovider.Record) (dnsprovider.Record, error) {
	param, err := p.buildRecordParam(rec)
	if err != nil {
		return dnsprovider.Record{}, err
	}
	resp, err := p.api.DNS.Records.Update(ctx, rec.ID, dns.RecordUpdateParams{
		ZoneID: cf.F(rec.ZoneID),
		Record: param,
	})
	if err != nil {
		return dnsprovider.Record{}, fmt.Errorf("cloudflare: update record %s: %w", rec.Name, err)
	}
	return p.fromResponse(rec.ZoneID, resp), nil
}

func (p *Provider) DeleteRecord(ctx context.Context, zoneID, recordID string) error {
	_, err := p.api.DNS.Records.Delete(ctx, recordID, dns.RecordDeleteParams{
		ZoneID: cf.F(zoneID),
	})
	if err != nil {
		return fmt.Errorf("cloudflare: delete record %s: %w", recordID, err)
	}
	return nil
}

// buildRecordParam translates a neutral Record into the Cloudflare SDK's
// record param union.
func (p *Provider) buildRecordParam(rec dnsprovider.Record) (dns.RecordUnionParam, error) {
	comment := commentMarker + rec.OwnerRef

	switch rec.Type {
	case dnsprovider.RecordTypeA:
		return dns.ARecordParam{
			Type:    cf.F(dns.ARecordType("A")),
			Name:    cf.F(rec.Name),
			Content: cf.F(rec.Content),
			TTL:     cf.F(dns.TTL(rec.TTL)),
			Comment: cf.F(comment),
			Proxied: cf.F(p.proxied),
		}, nil
	case dnsprovider.RecordTypeCNAME:
		return dns.CNAMERecordParam{
			Type:    cf.F(dns.CNAMERecordType("CNAME")),
			Name:    cf.F(rec.Name),
			Content: cf.F(rec.Content),
			TTL:     cf.F(dns.TTL(rec.TTL)),
			Comment: cf.F(comment),
			Proxied: cf.F(p.proxied),
		}, nil
	default:
		return nil, fmt.Errorf("cloudflare: %w: %q", dnsprovider.ErrUnsupportedRecordType, rec.Type)
	}
}

func (p *Provider) fromResponse(zoneID string, resp *dns.RecordResponse) dnsprovider.Record {
	owner, _ := parseOwner(resp.Comment)
	return dnsprovider.Record{
		ID:       resp.ID,
		ZoneID:   zoneID,
		Name:     resp.Name,
		Type:     dnsprovider.RecordType(resp.Type),
		Content:  resp.Content,
		TTL:      int(resp.TTL),
		OwnerRef: owner,
	}
}

// parseOwner extracts the greydns owner ref from a Cloudflare record
// comment. Returns ok=false unless the comment starts with the greydns
// marker and the suffix is a well-formed "<namespace>/<name>" pair with
// both parts non-empty; otherwise a degenerate record (e.g. a stray
// "[greydns]owner=") would land in the cache without matching any
// Service and could never be cleaned up.
func parseOwner(comment string) (string, bool) {
	if !strings.HasPrefix(comment, commentMarker) {
		return "", false
	}
	owner := strings.TrimPrefix(comment, commentMarker)
	namespace, name, ok := strings.Cut(owner, "/")
	if !ok || namespace == "" || name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return owner, true
}
