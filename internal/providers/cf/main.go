package cf

import (
	"context"
	"errors"
	"regexp"

	cloudflare "github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/dns"
	"github.com/cloudflare/cloudflare-go/v4/option"
	"github.com/cloudflare/cloudflare-go/v4/zones"
	"github.com/rs/zerolog/log"
	v1 "k8s.io/api/core/v1"

	cfg "github.com/math280h/greydns/internal/config"
	"github.com/math280h/greydns/internal/types"
)

// Provider implements the DNS provider interface for Cloudflare
type Provider struct {
	client         *cloudflare.Client
	commentPattern *regexp.Regexp
}

// Name returns the provider name
func (p *Provider) Name() string {
	return "cloudflare"
}

// Connect initializes the Cloudflare client with credentials
func (p *Provider) Connect(credentials map[string]string) error {
	apiToken, exists := credentials["cloudflare"]
	if !exists {
		return errors.New("cloudflare API token not found in credentials")
	}

	p.client = cloudflare.NewClient(
		option.WithAPIToken(apiToken),
	)
	p.commentPattern = regexp.MustCompile(`^\[greydns - Do not manually edit].*$`)

	return nil
}

// GetZones returns all available zones
func (p *Provider) GetZones() (map[string]string, error) {
	zonesToNames := make(map[string]string)
	zonesIter := p.client.Zones.ListAutoPaging(context.Background(), zones.ZoneListParams{})
	for zonesIter.Next() {
		zone := zonesIter.Current()
		zonesToNames[zone.Name] = zone.ID
		log.Debug().Msgf("[CF Provider] Found zone: %s (ID: %s)", zone.Name, zone.ID)
	}
	if err := zonesIter.Err(); err != nil {
		return nil, types.NewProviderError("cloudflare", "Failed to get zones", err)
	}
	log.Info().Msgf("[CF Provider] Found %d zones", len(zonesToNames))

	return zonesToNames, nil
}

// GetZone gets a specific zone by ID
func (p *Provider) GetZone(zoneID string) (*types.Zone, error) {
	zone, err := p.client.Zones.Get(context.Background(), zones.ZoneGetParams{
		ZoneID: cloudflare.F(zoneID),
	})
	if err != nil {
		return nil, types.NewProviderError("cloudflare", "Failed to get zone", err)
	}

	return &types.Zone{
		ID:   zone.ID,
		Name: zone.Name,
	}, nil
}

// CheckZoneExists checks if a zone exists and returns it
func (p *Provider) CheckZoneExists(zoneName string, zones map[string]string) (*types.Zone, error) {
	zoneID, exists := zones[zoneName]
	if !exists {
		return nil, types.NewProviderError("cloudflare", "Zone not found", nil)
	}

	return p.GetZone(zoneID)
}

// CreateRecord creates a new DNS record
func (p *Provider) CreateRecord(params types.CreateRecordParams) (*types.DNSRecord, error) {
	proxied := cfg.GetRequiredConfigValue("proxy-enabled") == "true"
	if params.Proxied != nil {
		proxied = *params.Proxied
	}

	var record dns.RecordUnionParam
	switch params.Type {
	case types.RecordTypeA:
		record = dns.ARecordParam{
			Type:    cloudflare.F(dns.ARecordType("A")),
			Name:    cloudflare.F(params.Name),
			Content: cloudflare.F(params.Content),
			TTL:     cloudflare.F(dns.TTL(params.TTL)),
			Comment: cloudflare.F(params.Comment),
			Proxied: cloudflare.F(proxied),
		}
	case types.RecordTypeCNAME:
		record = dns.CNAMERecordParam{
			Type:    cloudflare.F(dns.CNAMERecordType("CNAME")),
			Name:    cloudflare.F(params.Name),
			Content: cloudflare.F(params.Content),
			TTL:     cloudflare.F(dns.TTL(params.TTL)),
			Comment: cloudflare.F(params.Comment),
			Proxied: cloudflare.F(proxied),
		}
	default:
		return nil, types.NewProviderError("cloudflare", "Invalid record type: "+string(params.Type), nil)
	}

	dnsRecord, err := p.client.DNS.Records.New(
		context.Background(),
		dns.RecordNewParams{
			ZoneID: cloudflare.F(params.ZoneID),
			Record: record,
		},
	)
	if err != nil {
		return nil, types.NewProviderError("cloudflare", "Failed to create record", err)
	}

	log.Info().Msgf("[CF Provider] [%s] Record created", params.Name)

	return p.convertToGenericRecord(dnsRecord), nil
}

// UpdateRecord updates an existing DNS record
func (p *Provider) UpdateRecord(params types.UpdateRecordParams) (*types.DNSRecord, error) {
	proxied := cfg.GetRequiredConfigValue("proxy-enabled") == "true"
	if params.Proxied != nil {
		proxied = *params.Proxied
	}

	var record dns.RecordUnionParam
	switch params.Type {
	case types.RecordTypeA:
		record = dns.ARecordParam{
			Type:    cloudflare.F(dns.ARecordType("A")),
			Name:    cloudflare.F(params.Name),
			Content: cloudflare.F(params.Content),
			TTL:     cloudflare.F(dns.TTL(params.TTL)),
			Comment: cloudflare.F(params.Comment),
			Proxied: cloudflare.F(proxied),
		}
	case types.RecordTypeCNAME:
		record = dns.CNAMERecordParam{
			Type:    cloudflare.F(dns.CNAMERecordType("CNAME")),
			Name:    cloudflare.F(params.Name),
			Content: cloudflare.F(params.Content),
			TTL:     cloudflare.F(dns.TTL(params.TTL)),
			Comment: cloudflare.F(params.Comment),
			Proxied: cloudflare.F(proxied),
		}
	default:
		return nil, types.NewProviderError("cloudflare", "Invalid record type: "+string(params.Type), nil)
	}

	dnsRecord, err := p.client.DNS.Records.Update(
		context.Background(),
		params.RecordID,
		dns.RecordUpdateParams{
			ZoneID: cloudflare.F(params.ZoneID),
			Record: record,
		},
	)
	if err != nil {
		return nil, types.NewProviderError("cloudflare", "Failed to update record", err)
	}

	log.Info().Msgf("[CF Provider] [%s] Record updated", params.Name)

	return p.convertToGenericRecord(dnsRecord), nil
}

// DeleteRecord deletes a DNS record
func (p *Provider) DeleteRecord(recordID, zoneID string) error {
	log.Info().Msgf("[CF Provider] Attempting to delete record %s", recordID)
	_, err := p.client.DNS.Records.Delete(
		context.Background(),
		recordID,
		dns.RecordDeleteParams{
			ZoneID: cloudflare.F(zoneID),
		},
	)
	if err != nil {
		return types.NewProviderError("cloudflare", "Failed to delete record", err)
	}

	return nil
}

// GetRecords gets all records for a zone
func (p *Provider) GetRecords(zoneID string) (map[string]*types.DNSRecord, error) {
	records := make(map[string]*types.DNSRecord)
	recordsIter := p.client.DNS.Records.ListAutoPaging(context.Background(), dns.RecordListParams{
		ZoneID: cloudflare.F(zoneID),
	})
	for recordsIter.Next() {
		record := recordsIter.Current()
		if p.commentPattern.MatchString(record.Comment) {
			genericRecord := p.convertToGenericRecord(&record)
			records[record.Name] = genericRecord
			log.Debug().Msgf("[CF Provider] Found record: %s (ID: %s)", record.Name, record.ID)
		}
	}
	if err := recordsIter.Err(); err != nil {
		return nil, types.NewProviderError("cloudflare", "Failed to get records", err)
	}

	return records, nil
}

// RefreshRecordsCache refreshes the cache of all managed DNS records
func (p *Provider) RefreshRecordsCache(zones map[string]string) (map[string]*types.DNSRecord, error) {
	newExistingRecords := make(map[string]*types.DNSRecord)
	for _, id := range zones {
		zoneRecords, err := p.GetRecords(id)
		if err != nil {
			return nil, err
		}
		for name, record := range zoneRecords {
			newExistingRecords[name] = record
		}
	}
	log.Info().Msgf("[CF Provider] Refresh found %d records", len(newExistingRecords))
	return newExistingRecords, nil
}

// CleanupRecords removes old records for a service
func (p *Provider) CleanupRecords(existingRecords map[string]*types.DNSRecord, namespace, serviceName, zoneID, currentDomain string) error {
	expectedComment := "[greydns - Do not manually edit]" + namespace + "/" + serviceName

	for _, record := range existingRecords {
		if record.Comment == expectedComment && record.Name != currentDomain {
			log.Info().Msgf("[CF Provider] [%s/%s] Found old record, cleaning up", namespace, serviceName)
			err := p.DeleteRecord(record.ID, zoneID)
			if err != nil {
				log.Error().Err(err).Msgf("[CF Provider] [%s/%s] Failed to delete record", namespace, serviceName)
				return err
			}
			delete(existingRecords, record.Name)
		}
	}
	return nil
}

// convertToGenericRecord converts a Cloudflare DNS record to the generic format
func (p *Provider) convertToGenericRecord(cfRecord *dns.RecordResponse) *types.DNSRecord {
	proxied := cfRecord.Proxied
	ttl := int(cfRecord.TTL)

	return &types.DNSRecord{
		ID:      cfRecord.ID,
		Name:    cfRecord.Name,
		Type:    string(cfRecord.Type),
		Content: cfRecord.Content,
		TTL:     ttl,
		Comment: cfRecord.Comment,
		Proxied: &proxied,
		ZoneID:  "", // ZoneID is not included in RecordResponse, would need separate call
	}
}

// Legacy functions for backward compatibility - these will be removed eventually
var (
	cloudflareAPI  *cloudflare.Client //nolint:gochecknoglobals // Required for cloudflare
	commentPattern = regexp.MustCompile(`^\[greydns - Do not manually edit].*$`)
)

func Connect(
	secret *v1.Secret,
) {
	cloudflareAPI = cloudflare.NewClient(
		option.WithAPIToken(string(secret.Data["cloudflare"])),
	)
}

func CleanupRecords(
	existingRecords map[string]dns.RecordResponse,
	service *v1.Service,
	name string,
	zoneID string,
) {
	// Check if namespace/service already has another record using comments, if so, delete it in existingRecords
	for _, record := range existingRecords {
		if record.Comment == "[greydns - Do not manually edit]"+service.Namespace+"/"+service.Name {
			// Ensure its not the current record
			if service.ObjectMeta.Annotations["greydns.io/domain"] == record.Name {
				continue
			}
			log.Info().Msgf("[CF Provider] [%s] Found old record, cleaning up", name)
			err := DeleteRecord(record.ID, zoneID)
			if err != nil {
				log.Error().Err(err).Msgf("[CF Provider] [%s] Failed to delete record", name)
			}
			delete(existingRecords, record.Name)
		}
	}
}

func CreateRecord(
	name string,
	ingressDestination string,
	ttl int,
	zoneID string,
	service *v1.Service,
	existingRecords map[string]dns.RecordResponse,
) (*dns.RecordResponse, error) {
	recordType := cfg.GetRequiredConfigValue("record-type")
	proxied := cfg.GetRequiredConfigValue("proxy-enabled") == "true"

	var record dns.RecordUnionParam
	switch recordType {
	case "A":
		record = dns.ARecordParam{
			Type:    cloudflare.F(dns.ARecordType("A")),
			Name:    cloudflare.F(name),
			Content: cloudflare.F(ingressDestination),
			TTL:     cloudflare.F(dns.TTL(ttl)),
			Comment: cloudflare.F("[greydns - Do not manually edit]" + service.Namespace + "/" + service.Name),
			Proxied: cloudflare.F(proxied),
		}
	case "CNAME":
		record = dns.CNAMERecordParam{
			Type:    cloudflare.F(dns.CNAMERecordType("CNAME")),
			Name:    cloudflare.F(name),
			Content: cloudflare.F(ingressDestination),
			TTL:     cloudflare.F(dns.TTL(ttl)),
			Comment: cloudflare.F("[greydns - Do not manually edit]"),
			Proxied: cloudflare.F(proxied),
		}
	default:
		log.Error().Msgf("[CF Provider] Invalid record type: %s", recordType)
		return nil, errors.New("invalid record type")
	}

	CleanupRecords(existingRecords, service, name, zoneID)

	dnsRecord, err := cloudflareAPI.DNS.Records.New(
		context.Background(),
		dns.RecordNewParams{
			ZoneID: cloudflare.F(zoneID),
			Record: record,
		},
	)
	if err != nil {
		log.Error().Err(err).Msgf("[CF Provider] [%s] Failed to create record", name)
	} else {
		log.Info().Msgf("[CF Provider] [%s] Record created", name)
	}

	return dnsRecord, err
}

func UpdateRecord(
	recordID string,
	name string,
	ingressDestination string,
	ttl int,
	zoneID string,
	service *v1.Service,
) (*dns.RecordResponse, error) {
	recordType := cfg.GetRequiredConfigValue("record-type")
	proxied := cfg.GetRequiredConfigValue("proxy-enabled") == "true"

	var record dns.RecordUnionParam
	switch recordType {
	case "A":
		record = dns.ARecordParam{
			Type:    cloudflare.F(dns.ARecordType("A")),
			Name:    cloudflare.F(name),
			Content: cloudflare.F(ingressDestination),
			TTL:     cloudflare.F(dns.TTL(ttl)),
			Comment: cloudflare.F("[greydns - Do not manually edit]" + service.Namespace + "/" + service.Name),
			Proxied: cloudflare.F(proxied),
		}
	case "CNAME":
		record = dns.CNAMERecordParam{
			Type:    cloudflare.F(dns.CNAMERecordType("CNAME")),
			Name:    cloudflare.F(name),
			Content: cloudflare.F(ingressDestination),
			TTL:     cloudflare.F(dns.TTL(ttl)),
			Comment: cloudflare.F("[greydns - Do not manually edit]"),
			Proxied: cloudflare.F(proxied),
		}
	default:
		log.Error().Msgf("[CF Provider] Invalid record type: %s", recordType)
		return nil, errors.New("invalid record type")
	}
	dnsRecord, err := cloudflareAPI.DNS.Records.Update(
		context.Background(),
		recordID,
		dns.RecordUpdateParams{
			ZoneID: cloudflare.F(zoneID),
			Record: record,
		},
	)
	if err != nil {
		log.Error().Err(err).Msgf("[CF Provider] [%s] Failed to update record", name)
	} else {
		log.Info().Msgf("[CF Provider] [%s] Record updated", name)
	}

	return dnsRecord, err
}

func DeleteRecord(
	recordID string,
	zoneID string,
) error {
	log.Info().Msgf("[CF Provider] Attempting to delete record %s", recordID)
	_, err := cloudflareAPI.DNS.Records.Delete(
		context.Background(),
		recordID,
		dns.RecordDeleteParams{
			ZoneID: cloudflare.F(zoneID),
		},
	)
	if err != nil {
		log.Error().Err(err).Msgf("[CF Provider] Failed to delete record")
	}

	return err
}

func RefreshRecordsCache(zonesToNames map[string]string) map[string]dns.RecordResponse {
	newExistingRecords := make(map[string]dns.RecordResponse)
	for _, id := range zonesToNames {
		recordsIter := cloudflareAPI.DNS.Records.ListAutoPaging(context.Background(), dns.RecordListParams{
			ZoneID: cloudflare.F(id),
		})
		for recordsIter.Next() {
			record := recordsIter.Current()
			if commentPattern.MatchString(record.Comment) {
				newExistingRecords[record.Name] = record
				log.Debug().Msgf("[CF Provider] Refresh Found record: %s (ID: %s)", record.Name, record.ID)
			}
		}
		if err := recordsIter.Err(); err != nil {
			log.Fatal().Err(err).Msg("Failed to get records")
		}
	}
	log.Info().Msgf("[CF Provider] Refresh found %d records", len(newExistingRecords))
	return newExistingRecords
}

func GetZoneNames() map[string]string {
	zonesToNames := make(map[string]string)
	zonesIter := cloudflareAPI.Zones.ListAutoPaging(context.Background(), zones.ZoneListParams{})
	for zonesIter.Next() {
		zone := zonesIter.Current()
		zonesToNames[zone.Name] = zone.ID
		log.Debug().Msgf("[CF Provider] Found zone: %s (ID: %s)", zone.Name, zone.ID)
	}
	if err := zonesIter.Err(); err != nil {
		log.Fatal().Err(err).Msg("Failed to get zones")
	}
	log.Info().Msgf("[CF Provider] Found %d zones", len(zonesToNames))

	return zonesToNames
}

func CheckIfZoneExists(
	zonesToNames map[string]string,
	name string,
) (*zones.Zone, error) {
	zoneID := zonesToNames[name]
	zone, err := cloudflareAPI.Zones.Get(context.Background(), zones.ZoneGetParams{
		ZoneID: cloudflare.F(zoneID),
	})
	if err != nil {
		log.Error().Err(err).Msg("[CF Provider] Failed to get zone")
		return nil, err
	}
	return zone, err
}
