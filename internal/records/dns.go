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
// controller reacts to.
//
//nolint:gochecknoglobals // immutable shared data
var AnnotationKeys = []string{AnnotationDNS, AnnotationZone, AnnotationDomain}

// CacheKey identifies a record in the cache. Keying by zone and the
// provider-assigned record ID (rather than by name) lets the cache
// represent multiple records at the same (zoneID, name) - for example
// round-robin A records or manually-added duplicates - without silently
// dropping the extras.
type CacheKey struct {
	ZoneID string
	ID     string
}

// BuildCacheEntry returns the canonical cache key for a record.
func BuildCacheEntry(rec dnsprovider.Record) (CacheKey, dnsprovider.Record) {
	return CacheKey{ZoneID: rec.ZoneID, ID: rec.ID}, rec
}

// NewCacheSnapshot returns an empty cache snapshot suitable for
// populating via BuildCacheEntry and handing to ReplaceCache.
func NewCacheSnapshot() map[CacheKey]dnsprovider.Record {
	return make(map[CacheKey]dnsprovider.Record)
}

// pendingDelete is a record the controller failed to delete and that
// the refresh loop should retry on its next tick. The cache entry is
// left in place while the delete is pending so a new Service with the
// same namespace/name can't get a stale cache-miss and accidentally
// create a duplicate record in the provider.
type pendingDelete struct {
	ZoneID string
	ID     string
	Name   string
}

// nameKey indexes records by their DNS identity (zone + fully-qualified
// name), independent of their provider-assigned ID.
type nameKey struct {
	ZoneID string
	Name   string
}

// Reconciler carries the shared state every handler needs and runtime
// settings that are fixed at startup. Informer callbacks invoke its
// HandleAnnotations / HandleUpdates / HandleDeletions methods.
//
// The record cache is guarded by mu; the background refresh goroutine
// swaps it wholesale while informer handlers concurrently read/write
// entries. Every mutation bumps writeGen so ReplaceCacheIfUnchanged can
// refuse to clobber a handler write that landed during the refresh
// round-trip. byName and byOwner are secondary indexes so CacheRecord
// and cacheOwnedBy don't need to scan the full cache under the mutex.
// Failed deletes are parked in deleteRetries for the refresh goroutine
// to retry because the originating Service is usually gone by then.
type Reconciler struct {
	provider           dnsprovider.Provider
	zoneNameToID       map[string]string
	ingressDestination string
	recordTTL          int
	recordType         dnsprovider.RecordType

	mu            sync.Mutex
	cache         map[CacheKey]dnsprovider.Record
	byName        map[nameKey][]CacheKey
	byOwner       map[string][]CacheKey
	writeGen      uint64
	deleteRetries []pendingDelete
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
		provider:           provider,
		zoneNameToID:       zoneNameToID,
		ingressDestination: ingressDestination,
		recordTTL:          recordTTL,
		recordType:         recordType,
		cache:              make(map[CacheKey]dnsprovider.Record),
		byName:             make(map[nameKey][]CacheKey),
		byOwner:            make(map[string][]CacheKey),
	}
}

// ReplaceCache atomically swaps the cache contents and rebuilds
// secondary indexes.
func (r *Reconciler) ReplaceCache(next map[CacheKey]dnsprovider.Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache = next
	r.rebuildIndexes()
}

// WriteGen returns the current mutation generation counter.
func (r *Reconciler) WriteGen() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writeGen
}

// ReplaceCacheIfUnchanged swaps the cache to next only when no handler
// mutation has occurred since genAtStart was captured.
func (r *Reconciler) ReplaceCacheIfUnchanged(next map[CacheKey]dnsprovider.Record, genAtStart uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.writeGen != genAtStart {
		return false
	}
	r.cache = next
	r.rebuildIndexes()
	return true
}

// rebuildIndexes recomputes byName and byOwner from r.cache. Caller
// must hold r.mu.
func (r *Reconciler) rebuildIndexes() {
	r.byName = make(map[nameKey][]CacheKey, len(r.cache))
	r.byOwner = make(map[string][]CacheKey, len(r.cache))
	for k, rec := range r.cache {
		r.byName[nameKey{ZoneID: rec.ZoneID, Name: rec.Name}] = append(
			r.byName[nameKey{ZoneID: rec.ZoneID, Name: rec.Name}], k,
		)
		if rec.OwnerRef != "" {
			r.byOwner[rec.OwnerRef] = append(r.byOwner[rec.OwnerRef], k)
		}
	}
}

// SeedCache inserts a record into the cache under its (ZoneID, ID) key.
// Intended for tests and initial bootstrapping.
func (r *Reconciler) SeedCache(rec dnsprovider.Record) {
	r.cacheSet(CacheKey{ZoneID: rec.ZoneID, ID: rec.ID}, rec)
}

// CacheRecord returns a cached record matching (zoneID, name),
// preferring one whose OwnerRef matches ownerRef when multiple records
// share that key. ownerRef may be empty to accept any match. Lookup is
// O(k) where k is the number of records at that name (typically 1).
func (r *Reconciler) CacheRecord(zoneID, name, ownerRef string) (dnsprovider.Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := r.byName[nameKey{ZoneID: zoneID, Name: name}]
	var fallback dnsprovider.Record
	haveFallback := false
	for _, k := range keys {
		rec, ok := r.cache[k]
		if !ok {
			continue
		}
		if ownerRef != "" && rec.OwnerRef == ownerRef {
			return rec, true
		}
		if !haveFallback {
			fallback = rec
			haveFallback = true
		}
	}
	return fallback, haveFallback
}

// CacheLen returns the number of cached records.
func (r *Reconciler) CacheLen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cache)
}

// DrainDeleteRetries attempts each previously-failed delete once. Any
// that still fail are re-queued for the next cycle, and their cache
// entries stay in place. Successful retries remove the record from the
// cache. Called by the background refresh goroutine.
func (r *Reconciler) DrainDeleteRetries(ctx context.Context) {
	r.mu.Lock()
	pending := r.deleteRetries
	r.deleteRetries = nil
	r.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	log.Info().Int("pending", len(pending)).Msg("[DNS] Retrying failed record deletes")
	for _, p := range pending {
		if err := r.provider.DeleteRecord(ctx, p.ZoneID, p.ID); err != nil {
			log.Error().Err(err).Msgf("[DNS] Retry delete failed for record %s", p.Name)
			r.enqueueDeleteRetry(p)
			continue
		}
		log.Info().Msgf("[DNS] Retry delete succeeded for record %s", p.Name)
		r.cacheDelete(CacheKey{ZoneID: p.ZoneID, ID: p.ID})
	}
}

func (r *Reconciler) enqueueDeleteRetry(p pendingDelete) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleteRetries = append(r.deleteRetries, p)
}

func (r *Reconciler) cacheSet(k CacheKey, rec dnsprovider.Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil {
		r.cache = make(map[CacheKey]dnsprovider.Record)
	}
	if r.byName == nil {
		r.byName = make(map[nameKey][]CacheKey)
	}
	if r.byOwner == nil {
		r.byOwner = make(map[string][]CacheKey)
	}
	if prev, ok := r.cache[k]; ok {
		r.indexRemove(k, prev)
	}
	r.cache[k] = rec
	r.indexInsert(k, rec)
	r.writeGen++
}

func (r *Reconciler) cacheDelete(k CacheKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.cache[k]; ok {
		r.indexRemove(k, prev)
		delete(r.cache, k)
	}
	r.writeGen++
}

// cacheOwnedBy returns a snapshot of every cached record whose OwnerRef
// matches (namespace, name). Lookup is O(k) where k is the number of
// records owned by the service.
func (r *Reconciler) cacheOwnedBy(namespace, name string) []dnsprovider.Record {
	owner := dnsprovider.OwnerRefFor(namespace, name)
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := r.byOwner[owner]
	out := make([]dnsprovider.Record, 0, len(keys))
	for _, k := range keys {
		if rec, ok := r.cache[k]; ok {
			out = append(out, rec)
		}
	}
	return out
}

// indexInsert adds k to the secondary indexes for rec. Caller must
// hold r.mu.
func (r *Reconciler) indexInsert(k CacheKey, rec dnsprovider.Record) {
	nk := nameKey{ZoneID: rec.ZoneID, Name: rec.Name}
	r.byName[nk] = append(r.byName[nk], k)
	if rec.OwnerRef != "" {
		r.byOwner[rec.OwnerRef] = append(r.byOwner[rec.OwnerRef], k)
	}
}

// indexRemove removes k from the secondary indexes for rec. Caller
// must hold r.mu.
func (r *Reconciler) indexRemove(k CacheKey, rec dnsprovider.Record) {
	nk := nameKey{ZoneID: rec.ZoneID, Name: rec.Name}
	r.byName[nk] = removeCacheKey(r.byName[nk], k)
	if len(r.byName[nk]) == 0 {
		delete(r.byName, nk)
	}
	if rec.OwnerRef != "" {
		r.byOwner[rec.OwnerRef] = removeCacheKey(r.byOwner[rec.OwnerRef], k)
		if len(r.byOwner[rec.OwnerRef]) == 0 {
			delete(r.byOwner, rec.OwnerRef)
		}
	}
}

func removeCacheKey(s []CacheKey, k CacheKey) []CacheKey {
	for i, v := range s {
		if v == k {
			return append(s[:i], s[i+1:]...)
		}
	}
	return s
}

func dnsEnabled(service *v1.Service) bool {
	return service.Annotations[AnnotationDNS] == "true"
}

func ownedBy(rec dnsprovider.Record, service *v1.Service) bool {
	return rec.OwnerRef == dnsprovider.OwnerRefFor(service.Namespace, service.Name)
}

// preflight validates the greydns annotations on service and resolves
// its zone.
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
	zoneID, ok := r.zoneNameToID[zoneName]
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

	ownerRef := dnsprovider.OwnerRefFor(service.Namespace, service.Name)
	existing, exists := r.CacheRecord(zoneID, domain, ownerRef)
	if exists {
		r.reconcileExistingRecord(ctx, service, existing, zoneID, domain, ownerRef)
		return
	}

	log.Info().Msgf("[DNS] [%s] Record does not exist, attempting to create", service.Name)
	created, err := r.provider.CreateRecord(ctx, dnsprovider.Record{
		ZoneID:   zoneID,
		Name:     domain,
		Type:     r.recordType,
		Content:  r.ingressDestination,
		TTL:      r.recordTTL,
		OwnerRef: ownerRef,
	})
	if err != nil {
		log.Error().Err(err).Msgf("[DNS] [%s] Failed to create record", service.Name)
		return
	}
	log.Info().Msgf("[DNS] [%s] Record created", service.Name)
	r.cacheSet(CacheKey{ZoneID: created.ZoneID, ID: created.ID}, created)
	r.cleanupStaleRecords(ctx, service, created.ID, zoneID, domain)
}

// reconcileExistingRecord handles the branch of HandleAnnotations where
// a cached record already exists at (zoneID, domain). If it's owned by
// another Service, emit DuplicateDomain. If it's ours but has drifted
// from desired state, update it. In all success cases, sweep stale
// records owned by this Service.
func (r *Reconciler) reconcileExistingRecord(
	ctx context.Context,
	service *v1.Service,
	existing dnsprovider.Record,
	zoneID, domain, ownerRef string,
) {
	if !ownedBy(existing, service) {
		emitDuplicateDomain(service)
		return
	}
	if !r.hasDrift(existing) {
		log.Debug().Msgf("[DNS] [%s] Record exists and matches desired state", service.Name)
		r.cleanupStaleRecords(ctx, service, existing.ID, zoneID, domain)
		return
	}
	log.Info().Msgf("[DNS] [%s] Record drift detected, updating", service.Name)
	updated, err := r.provider.UpdateRecord(ctx, dnsprovider.Record{
		ID:       existing.ID,
		ZoneID:   existing.ZoneID,
		Name:     domain,
		Type:     r.recordType,
		Content:  r.ingressDestination,
		TTL:      r.recordTTL,
		OwnerRef: ownerRef,
	})
	if err != nil {
		log.Error().Err(err).Msgf("[DNS] [%s] Failed to update drifted record", service.Name)
		return
	}
	r.cacheSet(CacheKey{ZoneID: updated.ZoneID, ID: updated.ID}, updated)
	r.cleanupStaleRecords(ctx, service, updated.ID, zoneID, domain)
}

// hasDrift reports whether an existing cached record differs from the
// controller's desired state (record type, content, TTL). Ownership
// and name are implied by the lookup path so they aren't compared here.
func (r *Reconciler) hasDrift(rec dnsprovider.Record) bool {
	return rec.Type != r.recordType ||
		rec.Content != r.ingressDestination ||
		rec.TTL != r.recordTTL
}

// resolveUpdateExisting locates the record HandleUpdates should mutate
// and performs the destination-ownership check that would otherwise
// let a Service take over a peer's domain by changing its annotations.
func (r *Reconciler) resolveUpdateExisting(
	service *v1.Service,
	oldZoneID, oldDomain, newZoneID, newDomain string,
) (dnsprovider.Record, bool) {
	ownerRef := dnsprovider.OwnerRefFor(service.Namespace, service.Name)
	existing, exists := r.CacheRecord(oldZoneID, oldDomain, ownerRef)
	if !exists {
		// Cache miss despite the old Service having greydns
		// annotations. The cache is likely stale. The record may
		// already live at the new destination; if it does and we
		// own it, proceed with that. Otherwise skip rather than
		// create - a fresh create could duplicate if the provider
		// still holds the original.
		dest, destExists := r.CacheRecord(newZoneID, newDomain, ownerRef)
		switch {
		case destExists && ownedBy(dest, service):
			return dest, true
		case destExists:
			emitDuplicateDomain(service)
			return dnsprovider.Record{}, false
		default:
			log.Warn().Msgf(
				"[DNS] [%s] Expected record missing from cache; skipping to avoid a duplicate create",
				service.Name,
			)
			return dnsprovider.Record{}, false
		}
	}
	if !ownedBy(existing, service) {
		emitDuplicateDomain(service)
		return dnsprovider.Record{}, false
	}

	// Rename or migration: if the destination key is different and
	// already owned by a peer, abort so a Service can't take over
	// another's domain by mutating its annotations.
	if existing.ZoneID != newZoneID || oldDomain != newDomain {
		dest, destExists := r.CacheRecord(newZoneID, newDomain, ownerRef)
		if destExists && !ownedBy(dest, service) {
			emitDuplicateDomain(service)
			return dnsprovider.Record{}, false
		}
	}
	return existing, true
}

func (r *Reconciler) HandleUpdates(ctx context.Context, service, oldService *v1.Service) {
	zoneID, newDomain, ok := r.preflight(service)
	if !ok {
		return
	}

	oldDomain := oldService.Annotations[AnnotationDomain]
	oldZoneID, haveOldZoneID := r.zoneNameToID[oldService.Annotations[AnnotationZone]]

	if !haveOldZoneID || oldDomain == "" {
		log.Info().Msgf("[DNS] [%s] Old record absent, creating fresh", service.Name)
		r.HandleAnnotations(ctx, service)
		return
	}

	existing, found := r.resolveUpdateExisting(service, oldZoneID, oldDomain, zoneID, newDomain)
	if !found {
		return
	}

	// Zone change: the existing record lives in existing.ZoneID, which
	// may differ from the zoneID resolved from the new annotation.
	// Record IDs are zone-scoped, so an in-place update would target
	// the wrong zone. Create the replacement in the new zone first;
	// only delete the old-zone record once the new one is confirmed so
	// a create failure leaves the Service still resolving.
	if existing.ZoneID != zoneID {
		log.Info().Msgf("[DNS] [%s] Zone changed, migrating record", service.Name)
		created, createErr := r.provider.CreateRecord(ctx, dnsprovider.Record{
			ZoneID:   zoneID,
			Name:     newDomain,
			Type:     r.recordType,
			Content:  r.ingressDestination,
			TTL:      r.recordTTL,
			OwnerRef: dnsprovider.OwnerRefFor(service.Namespace, service.Name),
		})
		if createErr != nil {
			log.Error().Err(createErr).Msgf("[DNS] [%s] Failed to create record in new zone", service.Name)
			return
		}
		r.cacheSet(CacheKey{ZoneID: created.ZoneID, ID: created.ID}, created)
		r.deleteRecordOrRetry(ctx, service.Name, existing)
		r.cleanupStaleRecords(ctx, service, created.ID, zoneID, newDomain)
		return
	}

	log.Debug().Msgf("[DNS] [%s] Updating record", service.Name)
	updated, err := r.provider.UpdateRecord(ctx, dnsprovider.Record{
		ID:       existing.ID,
		ZoneID:   existing.ZoneID,
		Name:     newDomain,
		Type:     r.recordType,
		Content:  r.ingressDestination,
		TTL:      r.recordTTL,
		OwnerRef: dnsprovider.OwnerRefFor(service.Namespace, service.Name),
	})
	if err != nil {
		log.Error().Err(err).Msgf("[DNS] [%s] Failed to update record", service.Name)
		return
	}
	log.Info().Msgf("[DNS] [%s] Record updated", service.Name)
	r.cacheSet(CacheKey{ZoneID: updated.ZoneID, ID: updated.ID}, updated)
	r.cleanupStaleRecords(ctx, service, updated.ID, zoneID, newDomain)
}

// HandleDeletions removes every cached record owned by the deleted
// service. Records whose deletion fails are parked in the retry queue
// and reattempted by the refresh goroutine, since the originating
// Service is gone and won't produce further events.
func (r *Reconciler) HandleDeletions(ctx context.Context, service *v1.Service) {
	owned := r.cacheOwnedBy(service.Namespace, service.Name)
	if len(owned) == 0 {
		log.Debug().Msgf("[DNS] [%s] No owned records to delete", service.Name)
		return
	}
	for _, rec := range owned {
		r.deleteRecordOrRetry(ctx, service.Name, rec)
	}
}

// cleanupStaleRecords deletes any cached records still owned by service
// other than the canonical (currentZoneID, currentDomain, currentID)
// triplet. Handles rename residue and zone-migration residue from
// prior failed reconciles. Failed deletes are queued for retry.
func (r *Reconciler) cleanupStaleRecords(
	ctx context.Context,
	service *v1.Service,
	currentID, currentZoneID, currentDomain string,
) {
	for _, rec := range r.cacheOwnedBy(service.Namespace, service.Name) {
		if rec.ID == currentID && rec.ZoneID == currentZoneID && rec.Name == currentDomain {
			continue
		}
		log.Info().Msgf("[DNS] [%s] Cleaning up stale record %s", service.Name, rec.Name)
		r.deleteRecordOrRetry(ctx, service.Name, rec)
	}
}

// deleteRecordOrRetry removes rec from the provider and the cache. On
// failure the cache entry is left in place and the delete is queued
// for the refresh goroutine to retry; the entry is only dropped once
// the provider confirms the delete. Keeping the cache entry around
// means a Service recreated with the same namespace/name while the
// provider call is still failing won't get a cache-miss and produce a
// duplicate create.
func (r *Reconciler) deleteRecordOrRetry(ctx context.Context, serviceName string, rec dnsprovider.Record) {
	if err := r.provider.DeleteRecord(ctx, rec.ZoneID, rec.ID); err != nil {
		log.Error().Err(err).Msgf("[DNS] [%s] Failed to delete record %s; will retry", serviceName, rec.Name)
		r.enqueueDeleteRetry(pendingDelete{ZoneID: rec.ZoneID, ID: rec.ID, Name: rec.Name})
		return
	}
	log.Info().Msgf("[DNS] [%s] Deleted record %s", serviceName, rec.Name)
	r.cacheDelete(CacheKey{ZoneID: rec.ZoneID, ID: rec.ID})
}
