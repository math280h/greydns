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

// CacheKey identifies a record in the cache. Keying by zone + name
// (instead of name alone) avoids silent collisions when overlapping
// hosted zones legitimately host the same FQDN (e.g. "example.com" and
// "sub.example.com" both holding a record named "sub.example.com").
type CacheKey struct {
	ZoneID string
	Name   string
}

// BuildCacheEntry returns the canonical cache key for a record. Used by
// the background refresh goroutine when composing a fresh snapshot.
func BuildCacheEntry(rec dnsprovider.Record) (CacheKey, dnsprovider.Record) {
	return CacheKey{ZoneID: rec.ZoneID, Name: rec.Name}, rec
}

// NewCacheSnapshot returns an empty cache snapshot suitable for
// populating via BuildCacheEntry and handing to ReplaceCache.
func NewCacheSnapshot() map[CacheKey]dnsprovider.Record {
	return make(map[CacheKey]dnsprovider.Record)
}

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
	cache map[CacheKey]dnsprovider.Record
}

// NewReconciler returns a Reconciler with its cache initialised.
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
		cache:              make(map[CacheKey]dnsprovider.Record),
	}
}

// ReplaceCache atomically swaps the cache contents.
func (r *Reconciler) ReplaceCache(next map[CacheKey]dnsprovider.Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache = next
}

// SeedCache inserts a record into the cache. Intended for tests and
// initial bootstrapping.
func (r *Reconciler) SeedCache(zoneID, name string, rec dnsprovider.Record) {
	r.cacheSet(CacheKey{ZoneID: zoneID, Name: name}, rec)
}

// CacheRecord looks up a cached record by zone and name.
func (r *Reconciler) CacheRecord(zoneID, name string) (dnsprovider.Record, bool) {
	return r.cacheGet(CacheKey{ZoneID: zoneID, Name: name})
}

// CacheLen returns the number of cached records.
func (r *Reconciler) CacheLen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cache)
}

func (r *Reconciler) cacheGet(k CacheKey) (dnsprovider.Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.cache[k]
	return rec, ok
}

func (r *Reconciler) cacheSet(k CacheKey, rec dnsprovider.Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil {
		r.cache = make(map[CacheKey]dnsprovider.Record)
	}
	r.cache[k] = rec
}

func (r *Reconciler) cacheDelete(k CacheKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, k)
}

// cacheOwnedBy returns a snapshot of every cached record whose OwnerRef
// matches (namespace, name).
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
// its zone. Returns (zoneID, domain, true) on success.
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

	key := CacheKey{ZoneID: zoneID, Name: domain}
	existing, exists := r.cacheGet(key)

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
	r.cacheSet(key, created)
	r.cleanupStaleRecords(ctx, service, zoneID, domain)
}

func (r *Reconciler) HandleUpdates(ctx context.Context, service, oldService *v1.Service) {
	zoneID, newDomain, ok := r.preflight(service)
	if !ok {
		return
	}

	oldDomain := oldService.Annotations[AnnotationDomain]
	oldZoneID, haveOldZoneID := r.ZoneNameToID[oldService.Annotations[AnnotationZone]]
	var existing dnsprovider.Record
	exists := false
	if haveOldZoneID && oldDomain != "" {
		existing, exists = r.cacheGet(CacheKey{ZoneID: oldZoneID, Name: oldDomain})
	}
	if !exists {
		log.Info().Msgf("[DNS] [%s] Old record missing, creating fresh", service.Name)
		r.HandleAnnotations(ctx, service)
		return
	}
	if !ownedBy(existing, service) {
		emitDuplicateDomain(service)
		return
	}

	// Rename or migration: the destination differs from the current
	// cache entry. If some other Service already owns (zoneID,
	// newDomain), abort so a Service can't take over a peer's domain
	// by mutating its annotations. In-place updates (same zone, same
	// name) skip this check because they modify the record we already
	// own.
	movingKey := existing.ZoneID != zoneID || oldDomain != newDomain
	if movingKey {
		if dest, destExists := r.cacheGet(CacheKey{ZoneID: zoneID, Name: newDomain}); destExists && !ownedBy(dest, service) {
			emitDuplicateDomain(service)
			return
		}
	}

	// Zone change: the existing record lives in existing.ZoneID, which
	// may differ from the zoneID resolved from the new annotation.
	// Record IDs are zone-scoped, so an in-place update would target
	// the wrong zone. Create the replacement in the new zone first;
	// only delete the old-zone record once the new one is confirmed so
	// a create failure leaves the Service still resolving.
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
			log.Error().Err(delErr).Msgf("[DNS] [%s] Failed to delete record in old zone after migration", service.Name)
		} else {
			r.cacheDelete(CacheKey{ZoneID: existing.ZoneID, Name: oldDomain})
		}
		r.cacheSet(CacheKey{ZoneID: zoneID, Name: newDomain}, created)
		r.cleanupStaleRecords(ctx, service, zoneID, newDomain)
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
		r.cacheDelete(CacheKey{ZoneID: existing.ZoneID, Name: oldDomain})
	}
	r.cacheSet(CacheKey{ZoneID: zoneID, Name: newDomain}, updated)
	r.cleanupStaleRecords(ctx, service, zoneID, newDomain)
}

// HandleDeletions removes every cached record owned by the deleted
// service, ignoring current annotations so records aren't leaked when
// the Service is deleted after DNS was disabled or annotations were
// removed.
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
		r.cacheDelete(CacheKey{ZoneID: rec.ZoneID, Name: rec.Name})
	}
}

// cleanupStaleRecords deletes any cached records still owned by service
// whose (zone, name) differs from the current (zoneID, currentDomain).
// Handles rename and zone-migration residue from prior failed
// reconciles. Each record is deleted in its own zone.
func (r *Reconciler) cleanupStaleRecords(
	ctx context.Context,
	service *v1.Service,
	currentZoneID string,
	currentDomain string,
) {
	for _, rec := range r.cacheOwnedBy(service.Namespace, service.Name) {
		if rec.ZoneID == currentZoneID && rec.Name == currentDomain {
			continue
		}
		log.Info().Msgf("[DNS] [%s] Cleaning up stale record %s", service.Name, rec.Name)
		if err := r.Provider.DeleteRecord(ctx, rec.ZoneID, rec.ID); err != nil {
			log.Error().Err(err).Msgf("[DNS] [%s] Failed to delete stale record", service.Name)
			continue
		}
		r.cacheDelete(CacheKey{ZoneID: rec.ZoneID, Name: rec.Name})
	}
}
