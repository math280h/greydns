// Package records contains the service-annotation handlers that drive DNS
// record lifecycle. Handlers operate against dnsprovider.Provider so they
// hold no provider-specific types or SDK dependencies.
package records

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"
	v1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/math280h/greydns/internal/dnsprovider"
	"github.com/math280h/greydns/internal/metrics"
	"github.com/math280h/greydns/internal/utils"
)

// ErrReconcileIncomplete signals a non-terminal reconcile result: a
// DuplicateDomain block on another Service or a transient provider
// error. Callers requeue with backoff; the next attempt re-evaluates
// from fresh state.
var ErrReconcileIncomplete = errors.New("reconcile incomplete")

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
	metrics.CacheRecords.Set(float64(len(r.cache)))
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
	metrics.RetryQueueDepth.Set(0)
	if len(pending) == 0 {
		return
	}
	log.Info().Int("pending", len(pending)).Msg("[DNS] Retrying failed record deletes")
	for _, p := range pending {
		err := r.callProviderDelete(ctx, p.ZoneID, p.ID)
		if err != nil {
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
	metrics.RetryQueueDepth.Set(float64(len(r.deleteRetries)))
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
	metrics.CacheRecords.Set(float64(len(r.cache)))
}

func (r *Reconciler) cacheDelete(k CacheKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.cache[k]; ok {
		r.indexRemove(k, prev)
		delete(r.cache, k)
	}
	r.writeGen++
	metrics.CacheRecords.Set(float64(len(r.cache)))
}

// cacheOwnedByRef returns a snapshot of every cached record whose
// OwnerRef matches the given canonical owner reference. Lookup is O(k)
// where k is the number of records owned by that ref.
func (r *Reconciler) cacheOwnedByRef(ownerRef string) []dnsprovider.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := r.byOwner[ownerRef]
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

// target is the neutral shape every reconcile path operates on.
// Service and Ingress handlers translate their resource into a target
// so the core logic stays kind-agnostic; the kind is preserved in the
// owner ref so cross-kind domain collisions surface as DuplicateDomain.
type target struct {
	kind        dnsprovider.Kind
	namespace   string
	name        string
	annotations map[string]string
	domains     []string
	object      runtime.Object
}

func targetFromService(svc *v1.Service) target {
	return target{
		kind:        dnsprovider.KindService,
		namespace:   svc.Namespace,
		name:        svc.Name,
		annotations: svc.Annotations,
		domains:     parseDomains(svc.Annotations[AnnotationDomain]),
		object:      svc,
	}
}

func targetFromIngress(ing *networkingv1.Ingress) target {
	return target{
		kind:        dnsprovider.KindIngress,
		namespace:   ing.Namespace,
		name:        ing.Name,
		annotations: ing.Annotations,
		domains:     uniqueHosts(ing.Spec.Rules),
		object:      ing,
	}
}

// uniqueHosts collects the set of non-empty spec.rules[].host values,
// preserving the order of first appearance.
func uniqueHosts(rules []networkingv1.IngressRule) []string {
	if len(rules) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(rules))
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		host := strings.TrimSpace(r.Host)
		if host == "" {
			continue
		}
		if _, dup := seen[host]; dup {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	return out
}

func (t target) ownerRef() string {
	return dnsprovider.OwnerRefFor(t.kind, t.namespace, t.name)
}

func (t target) dnsEnabled() bool {
	return t.annotations[AnnotationDNS] == "true"
}

// preflight validates the greydns annotations on t and resolves its
// zone. Domain resolution is done by the caller (Service reads the
// greydns.io/domain CSV; Ingress reads spec.rules[].host), so preflight
// only checks that at least one domain made it through.
func (r *Reconciler) preflight(t target) (string, bool) {
	if !t.dnsEnabled() {
		return "", false
	}
	log.Info().Msgf("[DNS] %s %s/%s has DNS enabled", t.kind, t.namespace, t.name)

	zoneName := t.annotations[AnnotationZone]
	if zoneName == "" {
		emitInvalidAnnotation(t, "greydns.io/zone is empty")
		return "", false
	}
	zoneID, ok := r.zoneNameToID[zoneName]
	if !ok {
		log.Error().Msgf("[DNS] [%s/%s] Zone %q not managed by provider", t.namespace, t.name, zoneName)
		return "", false
	}

	if len(t.domains) == 0 {
		emitInvalidAnnotation(t, r.emptyDomainMessage(t))
		return "", false
	}
	return zoneID, true
}

// emptyDomainMessage tailors the InvalidAnnotation reason to the kind
// so Ingress users aren't told to set a CSV that doesn't apply.
func (r *Reconciler) emptyDomainMessage(t target) string {
	if t.kind == dnsprovider.KindIngress {
		return "spec.rules[].host is empty"
	}
	return "greydns.io/domain is empty"
}

// parseDomains splits a CSV annotation value into trimmed, deduped,
// non-empty entries while preserving order.
func parseDomains(raw string) []string {
	if raw == "" {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, 1)
	for item := range strings.SplitSeq(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, dup := seen[item]; dup {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func emitDuplicateDomain(t target) {
	utils.Recorder.Eventf(
		t.object,
		v1.EventTypeWarning,
		"DuplicateDomain",
		"Duplicate domain entry, this domain is already owned by another resource",
	)
}

func emitInvalidAnnotation(t target, detail string) {
	utils.Recorder.Eventf(
		t.object,
		v1.EventTypeWarning,
		"InvalidAnnotation",
		"Invalid greydns annotations: %s",
		detail,
	)
	metrics.Reconciles.WithLabelValues(metrics.OutcomeInvalidAnnotation).Inc()
}

// Reconcile ensures the provider holds exactly one record per
// greydns.io/domain entry on service and nothing else owned by this
// Service. Returns ErrReconcileIncomplete when some provider call
// could not complete so callers (the workqueue) can requeue with
// backoff; returns nil for terminal outcomes (annotation-rejected,
// fully reconciled) that require a new event to progress.
func (r *Reconciler) Reconcile(ctx context.Context, service *v1.Service) error {
	return r.reconcileTarget(ctx, targetFromService(service))
}

// ReconcileIngress is the Ingress-side counterpart to Reconcile. The
// desired domain set is derived from spec.rules[].host; greydns.io/*
// annotations (dns toggle, zone, ttl, overrides) still apply.
func (r *Reconciler) ReconcileIngress(ctx context.Context, ingress *networkingv1.Ingress) error {
	return r.reconcileTarget(ctx, targetFromIngress(ingress))
}

func (r *Reconciler) reconcileTarget(ctx context.Context, t target) error {
	zoneID, ok := r.preflight(t)
	if !ok {
		return nil
	}

	desiredNames := make(map[string]struct{}, len(t.domains))
	allReconciled := true
	for _, domain := range t.domains {
		desiredNames[domain] = struct{}{}
		if !r.reconcileDomain(ctx, t, r.desiredRecord(t, zoneID, domain)) {
			allReconciled = false
		}
	}

	// Skip cleanup if any desired reconcile was blocked; deleting stale
	// records while a rename target is contested would leave the target
	// with no DNS at all.
	if !allReconciled {
		return ErrReconcileIncomplete
	}
	r.cleanupOwnedOutsideDesired(ctx, t, zoneID, desiredNames)
	return nil
}

// reconcileDomain returns true when the domain is in its desired state
// (created, updated, or already matching), false when something
// blocked it (DuplicateDomain or a provider error).
func (r *Reconciler) reconcileDomain(
	ctx context.Context,
	t target,
	desired dnsprovider.Record,
) bool {
	existing, exists := r.CacheRecord(desired.ZoneID, desired.Name, desired.OwnerRef)
	if !exists {
		log.Info().Msgf("[DNS] [%s/%s] Creating record %s", t.namespace, t.name, desired.Name)
		created, err := r.callProviderCreate(ctx, desired)
		if err != nil {
			metrics.Reconciles.WithLabelValues(metrics.OutcomeError).Inc()
			log.Error().Err(err).Msgf("[DNS] [%s/%s] Failed to create %s", t.namespace, t.name, desired.Name)
			return false
		}
		r.cacheSet(CacheKey{ZoneID: created.ZoneID, ID: created.ID}, created)
		metrics.Reconciles.WithLabelValues(metrics.OutcomeCreated).Inc()
		return true
	}
	if existing.OwnerRef != t.ownerRef() {
		emitDuplicateDomain(t)
		metrics.Reconciles.WithLabelValues(metrics.OutcomeDuplicateDomain).Inc()
		return false
	}
	if !hasDrift(existing, desired) {
		log.Debug().Msgf("[DNS] [%s/%s] %s matches desired state", t.namespace, t.name, desired.Name)
		metrics.Reconciles.WithLabelValues(metrics.OutcomeNoop).Inc()
		return true
	}
	log.Info().Msgf("[DNS] [%s/%s] Drift on %s, updating", t.namespace, t.name, desired.Name)
	update := desired
	update.ID = existing.ID
	update.ZoneID = existing.ZoneID
	updated, err := r.callProviderUpdate(ctx, update)
	if err != nil {
		metrics.Reconciles.WithLabelValues(metrics.OutcomeError).Inc()
		log.Error().Err(err).Msgf("[DNS] [%s/%s] Failed to update %s", t.namespace, t.name, desired.Name)
		return false
	}
	r.cacheSet(CacheKey{ZoneID: updated.ZoneID, ID: updated.ID}, updated)
	metrics.Reconciles.WithLabelValues(metrics.OutcomeUpdated).Inc()
	return true
}

// cleanupOwnedOutsideDesired deletes any record owned by t that isn't
// at one of the desired names in the current zone. Handles annotation
// removal (a domain dropped from the CSV or an Ingress rule), zone
// migration (old-zone records no longer desired), and residue from
// prior failed reconciles.
func (r *Reconciler) cleanupOwnedOutsideDesired(
	ctx context.Context,
	t target,
	currentZoneID string,
	desiredNames map[string]struct{},
) {
	for _, rec := range r.cacheOwnedByRef(t.ownerRef()) {
		if rec.ZoneID == currentZoneID {
			if _, keep := desiredNames[rec.Name]; keep {
				continue
			}
		}
		log.Info().Msgf("[DNS] [%s/%s] Cleaning up stale record %s", t.namespace, t.name, rec.Name)
		r.deleteRecordOrRetry(ctx, t.namespace+"/"+t.name, rec)
	}
}

func (r *Reconciler) desiredRecord(t target, zoneID, domain string) dnsprovider.Record {
	return dnsprovider.Record{
		ZoneID:        zoneID,
		Name:          domain,
		Type:          r.resolveRecordType(t),
		Content:       r.ingressDestination,
		TTL:           r.resolveTTL(t),
		OwnerRef:      t.ownerRef(),
		ProviderHints: r.collectProviderHints(t),
	}
}

func (r *Reconciler) resolveTTL(t target) int {
	raw := strings.TrimSpace(t.annotations[AnnotationTTL])
	if raw == "" {
		return r.recordTTL
	}
	if !r.overridePolicy.Allows("ttl") {
		emitInvalidAnnotation(t, AnnotationTTL+": override not in allowed-overrides")
		return r.recordTTL
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		emitInvalidAnnotation(t, AnnotationTTL+": must be a positive integer, got "+raw)
		return r.recordTTL
	}
	return v
}

func (r *Reconciler) resolveRecordType(t target) dnsprovider.RecordType {
	raw := strings.TrimSpace(t.annotations[AnnotationRecordType])
	if raw == "" {
		return r.recordType
	}
	if !r.overridePolicy.Allows("record-type") {
		emitInvalidAnnotation(t, AnnotationRecordType+": override not in allowed-overrides")
		return r.recordType
	}
	candidate := dnsprovider.RecordType(raw)
	if err := dnsprovider.ValidateRecordType(candidate, r.provider.SupportedRecordTypes()); err != nil {
		emitInvalidAnnotation(t, AnnotationRecordType+": "+err.Error())
		return r.recordType
	}
	return candidate
}

// collectProviderHints reads "greydns.io/<provider>-<key>" annotations
// into a <key>-keyed map so the active provider sees a clean namespace.
// Each full "<provider>-<key>" must be allowlisted.
func (r *Reconciler) collectProviderHints(t target) map[string]string {
	prefix := AnnotationPrefix + r.provider.Name() + "-"
	hints := make(map[string]string)
	for k, v := range t.annotations {
		suffix, ok := strings.CutPrefix(k, prefix)
		if !ok {
			continue
		}
		policyKey := r.provider.Name() + "-" + suffix
		if !r.overridePolicy.Allows(policyKey) {
			emitInvalidAnnotation(t, k+": override not in allowed-overrides")
			continue
		}
		hints[suffix] = v
	}
	if len(hints) == 0 {
		return nil
	}
	return hints
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

// Cleanup removes every record owned by service. Individual delete
// failures enter the internal retry queue; Cleanup itself returns nil
// even when some deletes fail so callers don't double-retry. The
// refresh loop's DrainDeleteRetries is the outer retry path for those.
func (r *Reconciler) Cleanup(ctx context.Context, service *v1.Service) error {
	return r.cleanupOwnerRef(ctx, dnsprovider.OwnerRefFor(dnsprovider.KindService, service.Namespace, service.Name))
}

// CleanupIngress is the Ingress-side counterpart to Cleanup.
func (r *Reconciler) CleanupIngress(ctx context.Context, ingress *networkingv1.Ingress) error {
	return r.cleanupOwnerRef(ctx, dnsprovider.OwnerRefFor(dnsprovider.KindIngress, ingress.Namespace, ingress.Name))
}

func (r *Reconciler) cleanupOwnerRef(ctx context.Context, ownerRef string) error {
	owned := r.cacheOwnedByRef(ownerRef)
	if len(owned) == 0 {
		log.Debug().Msgf("[DNS] [%s] No owned records to delete", ownerRef)
		return nil
	}
	for _, rec := range owned {
		r.deleteRecordOrRetry(ctx, ownerRef, rec)
	}
	return nil
}

// deleteRecordOrRetry removes rec from the provider and the cache. On
// failure the cache entry is left in place and the delete is queued
// for the refresh goroutine to retry; the entry is only dropped once
// the provider confirms the delete. Keeping the cache entry around
// means a Service recreated with the same namespace/name while the
// provider call is still failing won't get a cache-miss and produce a
// duplicate create.
func (r *Reconciler) deleteRecordOrRetry(ctx context.Context, serviceName string, rec dnsprovider.Record) {
	if err := r.callProviderDelete(ctx, rec.ZoneID, rec.ID); err != nil {
		log.Error().Err(err).Msgf("[DNS] [%s] Failed to delete record %s; will retry", serviceName, rec.Name)
		r.enqueueDeleteRetry(pendingDelete{ZoneID: rec.ZoneID, ID: rec.ID, Name: rec.Name})
		return
	}
	log.Info().Msgf("[DNS] [%s] Deleted record %s", serviceName, rec.Name)
	r.cacheDelete(CacheKey{ZoneID: rec.ZoneID, ID: rec.ID})
}

// callProviderCreate / callProviderUpdate / callProviderDelete wrap
// the reconciler's provider calls with timing + outcome metrics. The
// refresh loop in cmd/main wraps its own list-path calls separately.
func (r *Reconciler) callProviderCreate(ctx context.Context, rec dnsprovider.Record) (dnsprovider.Record, error) {
	var err error
	defer metrics.ObserveProviderCall(r.provider.Name(), metrics.OpCreate)(&err)
	out, err := r.provider.CreateRecord(ctx, rec)
	return out, err
}

func (r *Reconciler) callProviderUpdate(ctx context.Context, rec dnsprovider.Record) (dnsprovider.Record, error) {
	var err error
	defer metrics.ObserveProviderCall(r.provider.Name(), metrics.OpUpdate)(&err)
	out, err := r.provider.UpdateRecord(ctx, rec)
	return out, err
}

func (r *Reconciler) callProviderDelete(ctx context.Context, zoneID, id string) error {
	var err error
	defer metrics.ObserveProviderCall(r.provider.Name(), metrics.OpDelete)(&err)
	err = r.provider.DeleteRecord(ctx, zoneID, id)
	return err
}
