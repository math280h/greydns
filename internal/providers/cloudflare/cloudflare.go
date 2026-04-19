// Package cloudflare implements dnsprovider.Provider against the Cloudflare
// DNS API. Ownership is persisted in the record Comment field with the
// marker format "[greydns]owner=<namespace>/<name>".
//
// Configuration keys (ConfigMap, active when provider: cloudflare):
//
//	cloudflare.proxy-enabled  optional bool, default false
//
// Per-Service annotation overrides:
//
//	greydns.io/cloudflare-proxied  "true"/"false", overrides proxy-enabled
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
	"strconv"
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

	// hintProxied is the ProviderHints key for the per-Service
	// "greydns.io/cloudflare-proxied" override.
	hintProxied = "proxied"
)

func init() { //nolint:gochecknoinits // required for provider self-registration
	registry.Register(providerName, New)
}

type Provider struct {
	api     *cf.Client
	proxied bool
}

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
	return []dnsprovider.RecordType{
		dnsprovider.RecordTypeA,
		dnsprovider.RecordTypeAAAA,
		dnsprovider.RecordTypeCNAME,
	}
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
			ID:            rec.ID,
			ZoneID:        zoneID,
			Name:          rec.Name,
			Type:          dnsprovider.RecordType(rec.Type),
			Content:       rec.Content,
			TTL:           int(rec.TTL),
			OwnerRef:      owner,
			ProviderHints: map[string]string{hintProxied: strconv.FormatBool(rec.Proxied)},
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
// record param union. Proxy status comes from the per-Service hint when
// set, falling back to the provider-wide default from config.
func (p *Provider) buildRecordParam(rec dnsprovider.Record) (dns.RecordUnionParam, error) {
	comment := commentMarker + rec.OwnerRef
	proxied := p.resolveProxied(rec.ProviderHints)

	switch rec.Type {
	case dnsprovider.RecordTypeA:
		return dns.ARecordParam{
			Type:    cf.F(dns.ARecordType("A")),
			Name:    cf.F(rec.Name),
			Content: cf.F(rec.Content),
			TTL:     cf.F(dns.TTL(rec.TTL)),
			Comment: cf.F(comment),
			Proxied: cf.F(proxied),
		}, nil
	case dnsprovider.RecordTypeAAAA:
		return dns.AAAARecordParam{
			Type:    cf.F(dns.AAAARecordType("AAAA")),
			Name:    cf.F(rec.Name),
			Content: cf.F(rec.Content),
			TTL:     cf.F(dns.TTL(rec.TTL)),
			Comment: cf.F(comment),
			Proxied: cf.F(proxied),
		}, nil
	case dnsprovider.RecordTypeCNAME:
		return dns.CNAMERecordParam{
			Type:    cf.F(dns.CNAMERecordType("CNAME")),
			Name:    cf.F(rec.Name),
			Content: cf.F(rec.Content),
			TTL:     cf.F(dns.TTL(rec.TTL)),
			Comment: cf.F(comment),
			Proxied: cf.F(proxied),
		}, nil
	default:
		return nil, fmt.Errorf("cloudflare: %w: %q", dnsprovider.ErrUnsupportedRecordType, rec.Type)
	}
}

func (p *Provider) resolveProxied(hints map[string]string) bool {
	raw, ok := hints[hintProxied]
	if !ok {
		return p.proxied
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return p.proxied
	}
	return parsed
}

func (p *Provider) fromResponse(zoneID string, resp *dns.RecordResponse) dnsprovider.Record {
	owner, _ := parseOwner(resp.Comment)
	return dnsprovider.Record{
		ID:            resp.ID,
		ZoneID:        zoneID,
		Name:          resp.Name,
		Type:          dnsprovider.RecordType(resp.Type),
		Content:       resp.Content,
		TTL:           int(resp.TTL),
		OwnerRef:      owner,
		ProviderHints: map[string]string{hintProxied: strconv.FormatBool(resp.Proxied)},
	}
}

// parseOwner returns ok=false unless the comment starts with the
// greydns marker and its suffix is a non-empty "<namespace>/<name>".
func parseOwner(comment string) (string, bool) {
	suffix, ok := strings.CutPrefix(comment, commentMarker)
	if !ok {
		return "", false
	}
	namespace, name, hasSlash := strings.Cut(suffix, "/")
	if !hasSlash || namespace == "" || name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return suffix, true
}
