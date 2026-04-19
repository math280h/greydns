// Package records contains the service-annotation handlers that drive DNS
// record lifecycle. Handlers operate against dnsprovider.Provider so they
// hold no provider-specific types or SDK dependencies.
package records

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"
	v1 "k8s.io/api/core/v1"

	"github.com/math280h/greydns/internal/dnsprovider"
	"github.com/math280h/greydns/internal/utils"
)

const (
	AnnotationPrefix     = "greydns.io/"
	AnnotationDNS        = "greydns.io/dns"
	AnnotationZone       = "greydns.io/zone"
	AnnotationDomain     = "greydns.io/domain"
	AnnotationTTL        = "greydns.io/ttl"
	AnnotationRecordType = "greydns.io/record-type"
)

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
// OverridePolicy gates which per-Service annotation overrides the
// Reconciler will honour. Platform operators opt in to each one via
// the "allowed-overrides" ConfigMap key; the default (no key, or "*")
// is to accept every override.
//
// Enforcement happens in-process: because the Reconciler is the only
// reader of greydns annotations, no amount of k8s RBAC or Service
// editing bypasses the policy.
type OverridePolicy struct {
	allowAll bool
	allowed  map[string]struct{}
}

// NewOverridePolicy parses the raw "allowed-overrides" value. hasKey
// distinguishes "config key absent" (default: allow all) from the
// explicit empty string "" (deny all).
func NewOverridePolicy(raw string, hasKey bool) OverridePolicy {
	if !hasKey {
		return OverridePolicy{allowAll: true}
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "*" {
		return OverridePolicy{allowAll: true}
	}
	allowed := make(map[string]struct{})
	for item := range strings.SplitSeq(trimmed, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			allowed[item] = struct{}{}
		}
	}
	return OverridePolicy{allowed: allowed}
}

// Allows reports whether suffix (the part after "greydns.io/", e.g.
// "ttl" or "cloudflare-proxied") is in the allowlist.
func (p OverridePolicy) Allows(suffix string) bool {
	if p.allowAll {
		return true
	}
	_, ok := p.allowed[suffix]
	return ok
}

type Reconciler struct {
	provider           dnsprovider.Provider
	zoneNameToID       map[string]string
	ingressDestination string
	recordTTL          int
	recordType         dnsprovider.RecordType
	overridePolicy     OverridePolicy

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
	overridePolicy OverridePolicy,
) *Reconciler {
	return &Reconciler{
		provider:           provider,
		zoneNameToID:       zoneNameToID,
		ingressDestination: ingressDestination,
		recordTTL:          recordTTL,
		recordType:         recordType,
		overridePolicy:     overridePolicy,
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

// enqueueDeleteRetry appends p to the retry queue unless the same
// (zoneID, id) is already queued. Without this, repeated reconciles
// against a failing provider would grow the queue unbounded and
// re-retry the same record N times per cycle.
func (r *Reconciler) enqueueDeleteRetry(p pendingDelete) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.deleteRetries {
		if existing.ZoneID == p.ZoneID && existing.ID == p.ID {
			return
		}
	}
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

	desired := r.desiredRecord(service, zoneID, domain)
	existing, exists := r.CacheRecord(zoneID, domain, desired.OwnerRef)
	if exists {
		r.reconcileExistingRecord(ctx, service, existing, desired)
		return
	}

	log.Info().Msgf("[DNS] [%s] Record does not exist, attempting to create", service.Name)
	created, err := r.provider.CreateRecord(ctx, desired)
	if err != nil {
		log.Error().Err(err).Msgf("[DNS] [%s] Failed to create record", service.Name)
		return
	}
	log.Info().Msgf("[DNS] [%s] Record created", service.Name)
	r.cacheSet(CacheKey{ZoneID: created.ZoneID, ID: created.ID}, created)
	r.cleanupStaleRecords(ctx, service, created.ID, zoneID, domain)
}

func (r *Reconciler) desiredRecord(service *v1.Service, zoneID, domain string) dnsprovider.Record {
	return dnsprovider.Record{
		ZoneID:        zoneID,
		Name:          domain,
		Type:          r.resolveRecordType(service),
		Content:       r.ingressDestination,
		TTL:           r.resolveTTL(service),
		OwnerRef:      dnsprovider.OwnerRefFor(service.Namespace, service.Name),
		ProviderHints: r.collectProviderHints(service),
	}
}

func (r *Reconciler) resolveTTL(service *v1.Service) int {
	raw := strings.TrimSpace(service.Annotations[AnnotationTTL])
	if raw == "" {
		return r.recordTTL
	}
	if !r.overridePolicy.Allows("ttl") {
		emitInvalidAnnotation(service, AnnotationTTL+": override not in allowed-overrides")
		return r.recordTTL
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		emitInvalidAnnotation(service, AnnotationTTL+": must be a positive integer, got "+raw)
		return r.recordTTL
	}
	return v
}

func (r *Reconciler) resolveRecordType(service *v1.Service) dnsprovider.RecordType {
	raw := strings.TrimSpace(service.Annotations[AnnotationRecordType])
	if raw == "" {
		return r.recordType
	}
	if !r.overridePolicy.Allows("record-type") {
		emitInvalidAnnotation(service, AnnotationRecordType+": override not in allowed-overrides")
		return r.recordType
	}
	candidate := dnsprovider.RecordType(raw)
	if err := dnsprovider.ValidateRecordType(candidate, r.provider.SupportedRecordTypes()); err != nil {
		emitInvalidAnnotation(service, AnnotationRecordType+": "+err.Error())
		return r.recordType
	}
	return candidate
}

// collectProviderHints reads "greydns.io/<provider>-<key>" annotations
// into a <key>-keyed map so the active provider sees a clean namespace.
// Each full "<provider>-<key>" must be allowlisted.
func (r *Reconciler) collectProviderHints(service *v1.Service) map[string]string {
	prefix := AnnotationPrefix + r.provider.Name() + "-"
	hints := make(map[string]string)
	for k, v := range service.Annotations {
		suffix, ok := strings.CutPrefix(k, prefix)
		if !ok {
			continue
		}
		policyKey := r.provider.Name() + "-" + suffix
		if !r.overridePolicy.Allows(policyKey) {
			emitInvalidAnnotation(service, k+": override not in allowed-overrides")
			continue
		}
		hints[suffix] = v
	}
	if len(hints) == 0 {
		return nil
	}
	return hints
}

func (r *Reconciler) reconcileExistingRecord(
	ctx context.Context,
	service *v1.Service,
	existing, desired dnsprovider.Record,
) {
	if !ownedBy(existing, service) {
		emitDuplicateDomain(service)
		return
	}
	if !hasDrift(existing, desired) {
		log.Debug().Msgf("[DNS] [%s] Record exists and matches desired state", service.Name)
		r.cleanupStaleRecords(ctx, service, existing.ID, desired.ZoneID, desired.Name)
		return
	}
	log.Info().Msgf("[DNS] [%s] Record drift detected, updating", service.Name)
	update := desired
	update.ID = existing.ID
	update.ZoneID = existing.ZoneID
	updated, err := r.provider.UpdateRecord(ctx, update)
	if err != nil {
		log.Error().Err(err).Msgf("[DNS] [%s] Failed to update drifted record", service.Name)
		return
	}
	r.cacheSet(CacheKey{ZoneID: updated.ZoneID, ID: updated.ID}, updated)
	r.cleanupStaleRecords(ctx, service, updated.ID, desired.ZoneID, desired.Name)
}

// hasDrift compares fields the controller owns. Hints present only on
// existing are provider-internal defaults and ignored.
func hasDrift(existing, desired dnsprovider.Record) bool {
	if existing.Type != desired.Type ||
		existing.Content != desired.Content ||
		existing.TTL != desired.TTL {
		return true
	}
	for k, v := range desired.ProviderHints {
		if existing.ProviderHints[k] != v {
			return true
		}
	}
	return false
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

	desired := r.desiredRecord(service, zoneID, newDomain)

	// Zone change: the existing record lives in existing.ZoneID, which
	// may differ from the zoneID resolved from the new annotation.
	// Record IDs are zone-scoped, so an in-place update would target
	// the wrong zone. Create the replacement in the new zone first;
	// only delete the old-zone record once the new one is confirmed so
	// a create failure leaves the Service still resolving.
	if existing.ZoneID != zoneID {
		log.Info().Msgf("[DNS] [%s] Zone changed, migrating record", service.Name)
		created, createErr := r.provider.CreateRecord(ctx, desired)
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
	update := desired
	update.ID = existing.ID
	update.ZoneID = existing.ZoneID
	updated, err := r.provider.UpdateRecord(ctx, update)
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
