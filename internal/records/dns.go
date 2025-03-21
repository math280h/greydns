package records

import (
	"strconv"

	"github.com/cloudflare/cloudflare-go/v4/dns"
	"github.com/rs/zerolog/log"
	v1 "k8s.io/api/core/v1"

	cfg "github.com/math280h/greydns/internal/config"
	cf "github.com/math280h/greydns/internal/providers/cf"
	"github.com/math280h/greydns/internal/utils"
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
	if !exists { //nolint:nestif // TODO:: Refactor
		log.Info().Msgf("[DNS] [%s] Record does not exist, attempting to create", meta.Name)

		ttl, ttlErr := strconv.Atoi(cfg.GetRequiredConfigValue("record-ttl"))
		if ttlErr != nil {
			log.Fatal().Err(ttlErr).Msg("[DNS] TTL is not a valid integer")
		}

		// Create the record
		// TODO:: Support multiple record types
		dnsRecord, cfErr := cf.CreateRecord(
			meta.Annotations["greydns.io/domain"],
			ingressDestination,
			ttl,
			zone.ID,
			service,
		)
		if cfErr != nil {
			log.Error().Err(cfErr).Msgf("[DNS] [%s] Failed to create record", meta.Name)
		} else {
			log.Info().Msgf("[DNS] [%s] Record created", meta.Name)

			// Add the record to the cache
			existingRecords[meta.Annotations["greydns.io/domain"]] = *dnsRecord
		}
	} else {
		// Ensure this service is the owner of the record
		if existingRecords[meta.Annotations["greydns.io/domain"]].Comment !=
			"[greydns - Do not manually edit]"+
				meta.Namespace+"/"+meta.Name {
			utils.Recorder.Eventf(
				service,
				v1.EventTypeWarning,
				"DuplicateDomain",
				"Duplicate domain entry, this domain is already owned by another service",
			)
			return
		}
		log.Debug().Msgf("[DNS] [%s] Record exists", meta.Name)
	}
}

func HandleDeletions(
	existingRecords map[string]dns.RecordResponse,
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
	log.Debug().Msgf("[DNS] [%s] Checking if zone exists", meta.Name)
	zone, err := cf.CheckIfZoneExists(zonesToNames, meta.Annotations["greydns.io/zone"])
	if err != nil {
		log.Error().Err(err).Msgf("[DNS] [%s] Zone does not exist", meta.Name)
		return
	}

	// Check if the record exists
	log.Debug().Msgf("[DNS] [%s] Checking if record exists", meta.Name)
	record, exists := existingRecords[meta.Annotations["greydns.io/domain"]]
	if exists {
		// Ensure this service is the owner of the record
		if record.Comment != "[greydns - Do not manually edit]"+meta.Namespace+"/"+meta.Name {
			log.Debug().Msgf("[DNS] [%s] Record does not belong to this service", meta.Name)
			return
		}

		log.Info().Msgf("[DNS] [%s] Record exists, attempting to delete", meta.Name)

		cfErr := cf.DeleteRecord(
			record.ID,
			zone.ID,
		)
		if cfErr != nil {
			log.Error().Err(cfErr).Msgf("[DNS] [%s] Failed to delete record", meta.Name)
		} else {
			log.Info().Msgf("[DNS] [%s] Record deleted", meta.Name)

			// Remove the record from the cache
			delete(existingRecords, meta.Annotations["greydns.io/domain"])
		}
	} else {
		log.Debug().Msgf("[DNS] [%s] Record does not exist", meta.Name)
	}
}
