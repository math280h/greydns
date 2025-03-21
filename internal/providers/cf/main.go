package utils

import (
	"context"
	"errors"
	"strings"

	"github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/dns"
	"github.com/cloudflare/cloudflare-go/v4/option"
	"github.com/cloudflare/cloudflare-go/v4/zones"
	"github.com/rs/zerolog/log"
	v1 "k8s.io/api/core/v1"

	cfg "github.com/math280h/greydns/internal/config"
)

var (
	cloudflareAPI *cloudflare.Client
)

func Connect(
	secret *v1.Secret,
) {
	cloudflareAPI = cloudflare.NewClient(
		option.WithAPIToken(string(secret.Data["cloudflare"])),
	)
}

func CreateRecord(
	name string,
	ingressDestination string,
	ttl int,
	zoneId string,
) (*dns.RecordResponse, error) {
	recordType := cfg.GetRequiredConfigValue("record-type")
	proxied := cfg.GetRequiredConfigValue("proxy-enabled") == "true"

	var record dns.RecordUnionParam
	if recordType != "A" {
		record = dns.ARecordParam{
			Type:    cloudflare.F(dns.ARecordType("A")),
			Name:    cloudflare.F(name),
			Content: cloudflare.F(ingressDestination),
			TTL:     cloudflare.F(dns.TTL(ttl)),
			Comment: cloudflare.F("[greydns - Do not manually edit]"),
			Proxied: cloudflare.F(proxied),
		}
	} else if recordType != "CNAME" {
		record = dns.CNAMERecordParam{
			Type:    cloudflare.F(dns.CNAMERecordType("CNAME")),
			Name:    cloudflare.F(name),
			Content: cloudflare.F(ingressDestination),
			TTL:     cloudflare.F(dns.TTL(ttl)),
			Comment: cloudflare.F("[greydns - Do not manually edit]"),
			Proxied: cloudflare.F(proxied),
		}
	} else {
		log.Error().Msgf("Invalid record type: %s", recordType)
		return nil, errors.New("invalid record type")
	}
	dnsRecord, err := cloudflareAPI.DNS.Records.New(
		context.Background(),
		dns.RecordNewParams{
			ZoneID: cloudflare.F(zoneId),
			Record: record,
		},
	)
	if err != nil {
		log.Error().Err(err).Msgf("[%s] Failed to create record", name)
	} else {
		log.Info().Msgf("[%s] Record created", name)
	}

	return dnsRecord, err
}

func RefreshRecordsCache(zonesToNames map[string]string) map[string]dns.RecordResponse {
	newExistingRecords := make(map[string]dns.RecordResponse)
	for _, id := range zonesToNames {
		recordsIter := cloudflareAPI.DNS.Records.ListAutoPaging(context.Background(), dns.RecordListParams{
			ZoneID: cloudflare.F(id),
		})
		for recordsIter.Next() {
			record := recordsIter.Current()
			if strings.Contains("[greydns - Do not manually edit]", record.Comment) {
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
	zoneId := zonesToNames[name]
	zone, err := cloudflareAPI.Zones.Get(context.Background(), zones.ZoneGetParams{
		ZoneID: cloudflare.F(zoneId),
	})
	if err != nil {
		log.Error().Err(err).Msg("[CF Provider] Failed to get zone")
		return nil, err
	}
	return zone, err
}
