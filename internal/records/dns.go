package records

import (
	"strconv"

	"github.com/rs/zerolog/log"
	v1 "k8s.io/api/core/v1"

	cfg "github.com/math280h/greydns/internal/config"
	"github.com/math280h/greydns/internal/types"
	"github.com/math280h/greydns/internal/utils"
)

// DNSManager handles DNS operations using any provider
type DNSManager struct {
	provider types.Provider
}

// NewDNSManager creates a new DNS manager with the specified provider
func NewDNSManager(provider types.Provider) *DNSManager {
	return &DNSManager{
		provider: provider,
	}
}

func HandleAnnotations(
	provider types.Provider,
	existingRecords map[string]*types.DNSRecord,
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
	zone, err := provider.CheckZoneExists(meta.Annotations["greydns.io/zone"], zonesToNames)
	if err != nil {
		log.Error().Err(err).Msgf("[DNS] [%s] Zone does not exist", meta.Name)
		return
	}
	log.Debug().Msgf("[DNS] [%s] Belongs to zone: %s", meta.Name, zone.Name)

	// Check if the record exists
	domain := meta.Annotations["greydns.io/domain"]
	_, exists := existingRecords[domain]
	if !exists {
		log.Info().Msgf("[DNS] [%s] Record does not exist, attempting to create", meta.Name)

		ttl, ttlErr := strconv.Atoi(cfg.GetRequiredConfigValue("record-ttl"))
		if ttlErr != nil {
			log.Fatal().Err(ttlErr).Msg("[DNS] TTL is not a valid integer")
		}

		recordType := types.RecordType(cfg.GetRequiredConfigValue("record-type"))

		// Create the record
		params := types.CreateRecordParams{
			Name:    domain,
			Type:    recordType,
			Content: ingressDestination,
			TTL:     ttl,
			Comment: "[greydns - Do not manually edit]" + meta.Namespace + "/" + meta.Name,
			ZoneID:  zone.ID,
		}

		dnsRecord, createErr := provider.CreateRecord(params)
		if createErr != nil {
			log.Error().Err(createErr).Msgf("[DNS] [%s] Failed to create record", meta.Name)
		} else {
			log.Info().Msgf("[DNS] [%s] Record created", meta.Name)

			// Add the record to the cache
			existingRecords[domain] = dnsRecord
		}
	} else {
		// Ensure this service is the owner of the record
		expectedComment := "[greydns - Do not manually edit]" + meta.Namespace + "/" + meta.Name
		if existingRecords[domain].Comment != expectedComment {
			utils.Recorder.Eventf(
				service,
				v1.EventTypeWarning,
				"DuplicateDomain",
				"Duplicate domain entry, this domain is already owned by another service",
			)
			return
		}
		log.Debug().Msgf("[DNS] [%s] Record exists", meta.Name)

		// Cleanup old records for this service
		cleanupErr := provider.CleanupRecords(existingRecords, meta.Namespace, meta.Name, zone.ID, domain)
		if cleanupErr != nil {
			log.Error().Err(cleanupErr).Msgf("[DNS] [%s] Failed to cleanup records", meta.Name)
		}
	}
}

func HandleUpdates(
	provider types.Provider,
	existingRecords map[string]*types.DNSRecord,
	ingressDestination string,
	zonesToNames map[string]string,
	service *v1.Service,
	oldService *v1.Service,
) {
	meta := service.ObjectMeta
	oldMeta := oldService.ObjectMeta
	enabled := meta.Annotations["greydns.io/dns"]
	if enabled == "true" {
		log.Info().Msgf("[DNS] Service %s has DNS enabled", meta.Name)
	} else {
		return
	}

	// Check if the zone exists
	zone, err := provider.CheckZoneExists(meta.Annotations["greydns.io/zone"], zonesToNames)
	if err != nil {
		log.Error().Err(err).Msgf("[DNS] [%s] Zone does not exist", meta.Name)
		return
	}
	log.Debug().Msgf("[DNS] [%s] Belongs to zone: %s", meta.Name, zone.Name)

	oldDomain := oldMeta.Annotations["greydns.io/domain"]
	newDomain := meta.Annotations["greydns.io/domain"]

	// Check if the record exists
	existingRecord, exists := existingRecords[oldDomain]
	if !exists {
		log.Info().Msgf("[DNS] [%s] Record does not exist, attempting to create", meta.Name)

		HandleAnnotations(
			provider,
			existingRecords,
			ingressDestination,
			zonesToNames,
			service,
		)
	} else {
		// Ensure this service is the owner of the record
		expectedComment := "[greydns - Do not manually edit]" + meta.Namespace + "/" + meta.Name
		if existingRecord.Comment != expectedComment {
			utils.Recorder.Eventf(
				service,
				v1.EventTypeWarning,
				"DuplicateDomain",
				"Duplicate domain entry, this domain is already owned by another service",
			)
			return
		}
		log.Debug().Msgf("[DNS] [%s] Record exists attempting to update", meta.Name)

		ttl, ttlErr := strconv.Atoi(cfg.GetRequiredConfigValue("record-ttl"))
		if ttlErr != nil {
			log.Fatal().Err(ttlErr).Msg("[DNS] TTL is not a valid integer")
		}

		recordType := types.RecordType(cfg.GetRequiredConfigValue("record-type"))

		// Update the record
		params := types.UpdateRecordParams{
			RecordID: existingRecord.ID,
			Name:     newDomain,
			Type:     recordType,
			Content:  ingressDestination,
			TTL:      ttl,
			Comment:  "[greydns - Do not manually edit]" + meta.Namespace + "/" + meta.Name,
			ZoneID:   zone.ID,
		}

		dnsRecord, updateErr := provider.UpdateRecord(params)
		if updateErr != nil {
			log.Error().Err(updateErr).Msgf("[DNS] [%s] Failed to update record", meta.Name)
		} else {
			log.Info().Msgf("[DNS] [%s] Record updated", meta.Name)

			// Update the cache - remove old key, add new key if different
			if oldDomain != newDomain {
				delete(existingRecords, oldDomain)
			}
			existingRecords[newDomain] = dnsRecord
		}
	}
}

func HandleDeletions(
	provider types.Provider,
	existingRecords map[string]*types.DNSRecord,
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
	zone, err := provider.CheckZoneExists(meta.Annotations["greydns.io/zone"], zonesToNames)
	if err != nil {
		log.Error().Err(err).Msgf("[DNS] [%s] Zone does not exist", meta.Name)
		return
	}

	// Check if the record exists
	domain := meta.Annotations["greydns.io/domain"]
	log.Debug().Msgf("[DNS] [%s] Checking if record exists", meta.Name)
	record, exists := existingRecords[domain]
	if exists {
		// Ensure this service is the owner of the record
		expectedComment := "[greydns - Do not manually edit]" + meta.Namespace + "/" + meta.Name
		if record.Comment != expectedComment {
			log.Debug().Msgf("[DNS] [%s] Record does not belong to this service", meta.Name)
			return
		}

		log.Info().Msgf("[DNS] [%s] Record exists, attempting to delete", meta.Name)

		deleteErr := provider.DeleteRecord(record.ID, zone.ID)
		if deleteErr != nil {
			log.Error().Err(deleteErr).Msgf("[DNS] [%s] Failed to delete record", meta.Name)
		} else {
			log.Info().Msgf("[DNS] [%s] Record deleted", meta.Name)

			// Remove the record from the cache
			delete(existingRecords, domain)
		}
	} else {
		log.Debug().Msgf("[DNS] [%s] Record does not exist", meta.Name)
	}
}
