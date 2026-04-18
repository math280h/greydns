// Package records contains the service-annotation handlers that drive DNS
// record lifecycle. Handlers operate against dnsprovider.Provider so they
// hold no provider-specific types or SDK dependencies.
package records

import (
	"context"

	"github.com/rs/zerolog/log"
	v1 "k8s.io/api/core/v1"

	"github.com/math280h/greydns/internal/dnsprovider"
	"github.com/math280h/greydns/internal/utils"
)

const (
	AnnotationDNS    = "greydns.io/dns"
	AnnotationZone   = "greydns.io/zone"
	AnnotationDomain = "greydns.io/domain"
)

// Reconciler carries the shared state every handler needs and runtime
// settings that are fixed at startup. Informer callbacks invoke its
// HandleAnnotations / HandleUpdates / HandleDeletions methods.
type Reconciler struct {
	Provider           dnsprovider.Provider
	Cache              map[string]dnsprovider.Record
	ZonesToNames       map[string]string
	IngressDestination string
	RecordTTL          int
	RecordType         dnsprovider.RecordType
}

func dnsEnabled(service *v1.Service) bool {
	return service.Annotations[AnnotationDNS] == "true"
}

func ownedBy(rec dnsprovider.Record, service *v1.Service) bool {
	return rec.OwnerRef == dnsprovider.OwnerRefFor(service.Namespace, service.Name)
}

// preflight returns the zoneID for a service if greydns is enabled and the
// service's zone is managed by the active provider. Returns (zoneID, true)
// on success, ("", false) to indicate the handler should no-op.
func (r *Reconciler) preflight(service *v1.Service) (string, bool) {
	if !dnsEnabled(service) {
		return "", false
	}
	log.Info().Msgf("[DNS] Service %s has DNS enabled", service.Name)

	zoneID, ok := r.ZonesToNames[service.Annotations[AnnotationZone]]
	if !ok {
		log.Error().Msgf("[DNS] [%s] Zone %q not managed by provider",
			service.Name, service.Annotations[AnnotationZone])
		return "", false
	}
	return zoneID, true
}

func emitDuplicateDomain(service *v1.Service) {
	utils.Recorder.Eventf(
		service,
		v1.EventTypeWarning,
		"DuplicateDomain",
		"Duplicate domain entry, this domain is already owned by another service",
	)
}

func (r *Reconciler) HandleAnnotations(ctx context.Context, service *v1.Service) {
	zoneID, ok := r.preflight(service)
	if !ok {
		return
	}

	domain := service.Annotations[AnnotationDomain]
	existing, exists := r.Cache[domain]

	if exists {
		if !ownedBy(existing, service) {
			emitDuplicateDomain(service)
			return
		}
		log.Debug().Msgf("[DNS] [%s] Record exists", service.Name)
		r.cleanupStaleRecords(ctx, service, zoneID, domain)
		return
	}

	log.Info().Msgf("[DNS] [%s] Record does not exist, attempting to create", service.Name)
	created, err := r.Provider.CreateRecord(ctx, dnsprovider.Record{
		ZoneID:   zoneID,
		Name:     domain,
		Type:     r.RecordType,
		Content:  r.IngressDestination,
		TTL:      r.RecordTTL,
		OwnerRef: dnsprovider.OwnerRefFor(service.Namespace, service.Name),
	})
	if err != nil {
		log.Error().Err(err).Msgf("[DNS] [%s] Failed to create record", service.Name)
		return
	}
	log.Info().Msgf("[DNS] [%s] Record created", service.Name)
	r.Cache[domain] = created
	r.cleanupStaleRecords(ctx, service, zoneID, domain)
}

func (r *Reconciler) HandleUpdates(ctx context.Context, service, oldService *v1.Service) {
	zoneID, ok := r.preflight(service)
	if !ok {
		return
	}

	oldDomain := oldService.Annotations[AnnotationDomain]
	newDomain := service.Annotations[AnnotationDomain]

	existing, exists := r.Cache[oldDomain]
	if !exists {
		log.Info().Msgf("[DNS] [%s] Old record missing, creating fresh", service.Name)
		r.HandleAnnotations(ctx, service)
		return
	}
	if !ownedBy(existing, service) {
		emitDuplicateDomain(service)
		return
	}

	log.Debug().Msgf("[DNS] [%s] Updating record", service.Name)
	updated, err := r.Provider.UpdateRecord(ctx, dnsprovider.Record{
		ID:       existing.ID,
		ZoneID:   zoneID,
		Name:     newDomain,
		Type:     r.RecordType,
		Content:  r.IngressDestination,
		TTL:      r.RecordTTL,
		OwnerRef: dnsprovider.OwnerRefFor(service.Namespace, service.Name),
	})
	if err != nil {
		log.Error().Err(err).Msgf("[DNS] [%s] Failed to update record", service.Name)
		return
	}
	log.Info().Msgf("[DNS] [%s] Record updated", service.Name)
	if oldDomain != newDomain {
		delete(r.Cache, oldDomain)
	}
	r.Cache[newDomain] = updated
}

func (r *Reconciler) HandleDeletions(ctx context.Context, service *v1.Service) {
	zoneID, ok := r.preflight(service)
	if !ok {
		return
	}

	domain := service.Annotations[AnnotationDomain]
	rec, exists := r.Cache[domain]
	if !exists {
		log.Debug().Msgf("[DNS] [%s] Record does not exist", service.Name)
		return
	}
	if !ownedBy(rec, service) {
		log.Debug().Msgf("[DNS] [%s] Record does not belong to this service", service.Name)
		return
	}

	if err := r.Provider.DeleteRecord(ctx, zoneID, rec.ID); err != nil {
		log.Error().Err(err).Msgf("[DNS] [%s] Failed to delete record", service.Name)
		return
	}
	log.Info().Msgf("[DNS] [%s] Record deleted", service.Name)
	delete(r.Cache, domain)
}

// cleanupStaleRecords deletes any cached records still owned by service
// whose name differs from currentDomain. Handles the rename case: a service
// updates its greydns.io/domain annotation without greydns processing the
// intermediate delete.
func (r *Reconciler) cleanupStaleRecords(
	ctx context.Context,
	service *v1.Service,
	zoneID string,
	currentDomain string,
) {
	owner := dnsprovider.OwnerRefFor(service.Namespace, service.Name)
	for name, rec := range r.Cache {
		if name == currentDomain || rec.OwnerRef != owner {
			continue
		}
		log.Info().Msgf("[DNS] [%s] Cleaning up stale record %s", service.Name, name)
		if err := r.Provider.DeleteRecord(ctx, zoneID, rec.ID); err != nil {
			log.Error().Err(err).Msgf("[DNS] [%s] Failed to delete stale record", service.Name)
			continue
		}
		delete(r.Cache, name)
	}
}
