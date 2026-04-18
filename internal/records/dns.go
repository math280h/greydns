// Package records contains the service-annotation handlers that drive DNS
// record lifecycle. Handlers operate against dnsprovider.Provider so they
// hold no provider-specific types or SDK dependencies.
package records

import (
	"context"
	"sync"

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

// AnnotationKeys is the exhaustive set of greydns.io/* annotations the
// controller reacts to. Callers (e.g. the informer's update filter) use
// this to detect additions, removals and changes in either direction.
//
//nolint:gochecknoglobals // immutable shared data
var AnnotationKeys = []string{AnnotationDNS, AnnotationZone, AnnotationDomain}

// Reconciler carries the shared state every handler needs and runtime
// settings that are fixed at startup. Informer callbacks invoke its
// HandleAnnotations / HandleUpdates / HandleDeletions methods.
//
// The record cache is guarded by mu; the background refresh goroutine
// replaces it wholesale while informer handlers concurrently read/write
// entries. External callers must go through ReplaceCache / SeedCache /
// CacheRecord / CacheLen rather than touching the map directly.
type Reconciler struct {
	Provider           dnsprovider.Provider
	ZoneNameToID       map[string]string
	IngressDestination string
	RecordTTL          int
	RecordType         dnsprovider.RecordType

	mu    sync.Mutex
	cache map[string]dnsprovider.Record
}

// NewReconciler returns a Reconciler with its cache initialised. Zero
// Reconciler values won't work: the internal map needs to exist before
// the first cache touch.
func NewReconciler(
	provider dnsprovider.Provider,
	zoneNameToID map[string]string,
	ingressDestination string,
	recordTTL int,
	recordType dnsprovider.RecordType,
) *Reconciler {
	return &Reconciler{
		Provider:           provider,
		ZoneNameToID:       zoneNameToID,
		IngressDestination: ingressDestination,
		RecordTTL:          recordTTL,
		RecordType:         recordType,
		cache:              make(map[string]dnsprovider.Record),
	}
}

// ReplaceCache atomically swaps the cache contents. Used by the
// background refresh goroutine after a successful full refresh.
func (r *Reconciler) ReplaceCache(next map[string]dnsprovider.Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache = next
}

// SeedCache inserts a record into the cache. Intended for tests and
// initial bootstrapping; informer handlers use the unexported setter.
func (r *Reconciler) SeedCache(name string, rec dnsprovider.Record) {
	r.cacheSet(name, rec)
}

// CacheRecord looks up a cached record by name.
func (r *Reconciler) CacheRecord(name string) (dnsprovider.Record, bool) {
	return r.cacheGet(name)
}

// CacheLen returns the number of cached records.
func (r *Reconciler) CacheLen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cache)
}

func (r *Reconciler) cacheGet(name string) (dnsprovider.Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.cache[name]
	return rec, ok
}

func (r *Reconciler) cacheSet(name string, rec dnsprovider.Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil {
		r.cache = make(map[string]dnsprovider.Record)
	}
	r.cache[name] = rec
}

func (r *Reconciler) cacheDelete(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, name)
}

// cacheOwnedBy returns a snapshot of every cached record whose OwnerRef
// matches (namespace, name). The snapshot is safe to iterate while
// mutating the cache from the same goroutine.
func (r *Reconciler) cacheOwnedBy(namespace, name string) []dnsprovider.Record {
	owner := dnsprovider.OwnerRefFor(namespace, name)
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]dnsprovider.Record, 0)
	for _, rec := range r.cache {
		if rec.OwnerRef == owner {
			out = append(out, rec)
		}
	}
	return out
}

func dnsEnabled(service *v1.Service) bool {
	return service.Annotations[AnnotationDNS] == "true"
}

func ownedBy(rec dnsprovider.Record, service *v1.Service) bool {
	return rec.OwnerRef == dnsprovider.OwnerRefFor(service.Namespace, service.Name)
}

// preflight validates the greydns annotations on service and resolves
// its zone. Returns (zoneID, domain, true) on success, or ("", "",
// false) to indicate the handler should no-op.
func (r *Reconciler) preflight(service *v1.Service) (string, string, bool) {
	if !dnsEnabled(service) {
		return "", "", false
	}
	log.Info().Msgf("[DNS] Service %s has DNS enabled", service.Name)

	zoneName := service.Annotations[AnnotationZone]
	if zoneName == "" {
		emitInvalidAnnotation(service, "greydns.io/zone is empty")
		return "", "", false
	}
	zoneID, ok := r.ZoneNameToID[zoneName]
	if !ok {
		log.Error().Msgf("[DNS] [%s] Zone %q not managed by provider", service.Name, zoneName)
		return "", "", false
	}

	domain := service.Annotations[AnnotationDomain]
	if domain == "" {
		emitInvalidAnnotation(service, "greydns.io/domain is empty")
		return "", "", false
	}
	return zoneID, domain, true
}

func emitDuplicateDomain(service *v1.Service) {
	utils.Recorder.Eventf(
		service,
		v1.EventTypeWarning,
		"DuplicateDomain",
		"Duplicate domain entry, this domain is already owned by another service",
	)
}

func emitInvalidAnnotation(service *v1.Service, detail string) {
	utils.Recorder.Eventf(
		service,
		v1.EventTypeWarning,
		"InvalidAnnotation",
		"Invalid greydns annotations: %s",
		detail,
	)
}

func (r *Reconciler) HandleAnnotations(ctx context.Context, service *v1.Service) {
	zoneID, domain, ok := r.preflight(service)
	if !ok {
		return
	}

	existing, exists := r.cacheGet(domain)

	if exists {
		if !ownedBy(existing, service) {
			emitDuplicateDomain(service)
			return
		}
		log.Debug().Msgf("[DNS] [%s] Record exists", service.Name)
		r.cleanupStaleRecords(ctx, service, domain)
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
	r.cacheSet(domain, created)
	r.cleanupStaleRecords(ctx, service, domain)
}

func (r *Reconciler) HandleUpdates(ctx context.Context, service, oldService *v1.Service) {
	zoneID, newDomain, ok := r.preflight(service)
	if !ok {
		return
	}

	oldDomain := oldService.Annotations[AnnotationDomain]
	existing, exists := r.cacheGet(oldDomain)
	if !exists {
		log.Info().Msgf("[DNS] [%s] Old record missing, creating fresh", service.Name)
		r.HandleAnnotations(ctx, service)
		return
	}
	if !ownedBy(existing, service) {
		emitDuplicateDomain(service)
		return
	}

	// Zone change: the existing record lives in existing.ZoneID, which
	// may not equal the zoneID we just resolved from the new annotation.
	// Record IDs are zone-scoped, so an in-place update would target
	// the wrong zone. Create the replacement in the new zone first;
	// only delete the old-zone record once the new one is confirmed,
	// so a create failure leaves the service still resolving.
	if existing.ZoneID != zoneID {
		log.Info().Msgf("[DNS] [%s] Zone changed, migrating record", service.Name)
		created, createErr := r.Provider.CreateRecord(ctx, dnsprovider.Record{
			ZoneID:   zoneID,
			Name:     newDomain,
			Type:     r.RecordType,
			Content:  r.IngressDestination,
			TTL:      r.RecordTTL,
			OwnerRef: dnsprovider.OwnerRefFor(service.Namespace, service.Name),
		})
		if createErr != nil {
			log.Error().Err(createErr).Msgf("[DNS] [%s] Failed to create record in new zone", service.Name)
			return
		}
		if delErr := r.Provider.DeleteRecord(ctx, existing.ZoneID, existing.ID); delErr != nil {
			// Replacement is live; log the orphan and continue. A
			// future cleanupStaleRecords pass will retry the delete.
			log.Error().Err(delErr).Msgf("[DNS] [%s] Failed to delete record in old zone after migration", service.Name)
		}
		if oldDomain != newDomain {
			r.cacheDelete(oldDomain)
		}
		r.cacheSet(newDomain, created)
		return
	}

	log.Debug().Msgf("[DNS] [%s] Updating record", service.Name)
	updated, err := r.Provider.UpdateRecord(ctx, dnsprovider.Record{
		ID:       existing.ID,
		ZoneID:   existing.ZoneID,
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
		r.cacheDelete(oldDomain)
	}
	r.cacheSet(newDomain, updated)
}

// HandleDeletions removes every cached record owned by the deleted
// service. It does not consult current annotations because a Service
// may be deleted after greydns.io/dns was set to "false" or a zone
// annotation was removed; driving cleanup from the cache prevents
// those records from being leaked.
func (r *Reconciler) HandleDeletions(ctx context.Context, service *v1.Service) {
	owned := r.cacheOwnedBy(service.Namespace, service.Name)
	if len(owned) == 0 {
		log.Debug().Msgf("[DNS] [%s] No owned records to delete", service.Name)
		return
	}

	for _, rec := range owned {
		if err := r.Provider.DeleteRecord(ctx, rec.ZoneID, rec.ID); err != nil {
			log.Error().Err(err).Msgf("[DNS] [%s] Failed to delete record %s", service.Name, rec.Name)
			continue
		}
		log.Info().Msgf("[DNS] [%s] Deleted record %s", service.Name, rec.Name)
		r.cacheDelete(rec.Name)
	}
}

// cleanupStaleRecords deletes any cached records still owned by service
// whose name differs from currentDomain. Handles the rename case: a
// service updates its greydns.io/domain annotation without greydns
// processing the intermediate delete. Each record is deleted in its
// own zone (rec.ZoneID), which may differ from the service's current
// zone if the service migrated zones and left records behind.
func (r *Reconciler) cleanupStaleRecords(
	ctx context.Context,
	service *v1.Service,
	currentDomain string,
) {
	for _, rec := range r.cacheOwnedBy(service.Namespace, service.Name) {
		if rec.Name == currentDomain {
			continue
		}
		log.Info().Msgf("[DNS] [%s] Cleaning up stale record %s", service.Name, rec.Name)
		if err := r.Provider.DeleteRecord(ctx, rec.ZoneID, rec.ID); err != nil {
			log.Error().Err(err).Msgf("[DNS] [%s] Failed to delete stale record", service.Name)
			continue
		}
		r.cacheDelete(rec.Name)
	}
}
