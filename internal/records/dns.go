package records

import (
	"strconv"

	"github.com/cloudflare/cloudflare-go/v4/dns"
	"github.com/rs/zerolog/log"
	v1 "k8s.io/api/core/v1"

	cfg "github.com/math280h/greydns/internal/config"
	cf "github.com/math280h/greydns/internal/providers/cf"
)

func HandleAnnotations(
	existingRecords map[string]dns.RecordResponse,
	ingressDestination string,
	zonesToNames map[string]string,
	service *v1.Service,
) {
	meta := service.ObjectMeta
	enabled := meta.Annotations["greydns.io/dns"]
	if enabled == "true" {
		log.Info().Msgf("[DNS] Service %s has DNS enabled", meta.Name)
	} else {
		return
	}

	// Check if the zone exists
	// TODO:: Support multiple zones
	zone, err := cf.CheckIfZoneExists(zonesToNames, meta.Annotations["greydns.io/zone"])
	if err != nil {
		log.Error().Err(err).Msgf("[DNS] [%s] Zone does not exist", meta.Name)
		return
	}
	log.Debug().Msgf("[DNS] [%s] Belongs to zone: %s", meta.Name, zone.Name)

	// Check if the record exists
	_, exists := existingRecords[meta.Annotations["greydns.io/domain"]]
	if !exists {
		log.Info().Msgf("[DNS] [%s] Record does not exist, attempting to create", meta.Name)

		ttl, err := strconv.Atoi(cfg.GetRequiredConfigValue("record-ttl"))
		if err != nil {
			log.Fatal().Err(err).Msg("[DNS] TTL is not a valid integer")
		}

		// Create the record
		// TODO:: Support multiple record types
		dnsRecord, err := cf.CreateRecord(
			meta.Annotations["greydns.io/domain"],
			ingressDestination,
			ttl,
			zone.ID,
		)
		if err != nil {
			log.Error().Err(err).Msgf("[DNS] [%s] Failed to create record", meta.Name)
		} else {
			log.Info().Msgf("[DNS] [%s] Record created", meta.Name)

			// Add the record to the cache
			existingRecords[meta.Annotations["greydns.io/domain"]] = *dnsRecord
		}
	} else {
		log.Debug().Msgf("[DNS] [%s] Record exists", meta.Name)
	}
}
