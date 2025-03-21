package providers

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
)

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
