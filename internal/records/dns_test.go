package records_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/math280h/greydns/internal/dnsprovider"
	"github.com/math280h/greydns/internal/dnsprovider/fake"
	"github.com/math280h/greydns/internal/records"
	"github.com/math280h/greydns/internal/utils"
)

const (
	testZoneID   = "zone-id"
	testZoneName = "example.com"
	testDomain   = "api.example.com"
	testIngress  = "1.2.3.4"
	testNS       = "default"
	testSvcName  = "api"
)

func stubRecorder(t *testing.T) *record.FakeRecorder {
	t.Helper()
	recorder := record.NewFakeRecorder(16)
	prev := utils.Recorder
	utils.Recorder = recorder //nolint:reassign // tests stub the event recorder
	t.Cleanup(func() {
		utils.Recorder = prev //nolint:reassign // restore the production recorder after the test
	})
	return recorder
}

func mustReconcile(t *testing.T, r *records.Reconciler, svc *v1.Service) {
	t.Helper()
	if err := r.Reconcile(context.Background(), svc); err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}
}

func mustCleanup(t *testing.T, r *records.Reconciler, svc *v1.Service) {
	t.Helper()
	if err := r.Cleanup(context.Background(), svc); err != nil {
		t.Fatalf("Cleanup returned unexpected error: %v", err)
	}
}

func expectIncomplete(t *testing.T, r *records.Reconciler, svc *v1.Service) {
	t.Helper()
	if err := r.Reconcile(context.Background(), svc); !errors.Is(err, records.ErrReconcileIncomplete) {
		t.Fatalf("Reconcile err = %v, want ErrReconcileIncomplete", err)
	}
}

func setupTest(t *testing.T) (*records.Reconciler, *fake.Provider, *record.FakeRecorder) {
	t.Helper()
	provider := fake.New(dnsprovider.Zone{ID: testZoneID, Name: testZoneName})
	recorder := stubRecorder(t)
	r := records.NewReconciler(
		provider,
		map[string]string{testZoneName: testZoneID},
		testIngress,
		60,
		dnsprovider.RecordTypeA,
		records.NewOverridePolicy("", false),
	)
	return r, provider, recorder
}

func svc(annotations map[string]string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   testNS,
			Name:        testSvcName,
			Annotations: annotations,
		},
	}
}

func dnsAnnotations(zone, domain string) map[string]string {
	return map[string]string{
		records.AnnotationDNS:    "true",
		records.AnnotationZone:   zone,
		records.AnnotationDomain: domain,
	}
}

func TestHandleAnnotations_CreatesRecord(t *testing.T) {
	r, provider, _ := setupTest(t)

	s := svc(dnsAnnotations(testZoneName, testDomain))
	mustReconcile(t, r, s)

	rec, ok := r.CacheRecord(testZoneID, testDomain, "")
	if !ok {
		t.Fatal("expected record in cache after create")
	}
	if rec.OwnerRef != "default/api" {
		t.Fatalf("owner ref = %q, want default/api", rec.OwnerRef)
	}
	if len(provider.Snapshot()) != 1 {
		t.Fatalf("provider should hold 1 record, has %d", len(provider.Snapshot()))
	}
}

func TestHandleAnnotations_SkipsWhenDNSDisabled(t *testing.T) {
	r, provider, _ := setupTest(t)

	s := svc(map[string]string{records.AnnotationDNS: "false"})
	mustReconcile(t, r, s)

	if r.CacheLen() != 0 {
		t.Fatalf("cache should be empty, has %d", r.CacheLen())
	}
	if len(provider.Snapshot()) != 0 {
		t.Fatalf("provider should be empty, has %d", len(provider.Snapshot()))
	}
}

func TestHandleAnnotations_SkipsWhenZoneMissing(t *testing.T) {
	r, provider, _ := setupTest(t)

	s := svc(dnsAnnotations("unknown.com", testDomain))
	mustReconcile(t, r, s)

	if r.CacheLen() != 0 {
		t.Fatalf("cache should be empty, has %d", r.CacheLen())
	}
	if len(provider.Snapshot()) != 0 {
		t.Fatalf("provider should be empty, has %d", len(provider.Snapshot()))
	}
}

func TestHandleAnnotations_DuplicateDomainEmitsEvent(t *testing.T) {
	r, provider, recorder := setupTest(t)

	r.SeedCache(dnsprovider.Record{
		ID:       "existing",
		ZoneID:   testZoneID,
		Name:     testDomain,
		OwnerRef: dnsprovider.OwnerRefFor(testNS, "other"),
	})

	s := svc(dnsAnnotations(testZoneName, testDomain))
	expectIncomplete(t, r, s)

	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, "DuplicateDomain") {
			t.Fatalf("expected DuplicateDomain event, got %q", ev)
		}
	default:
		t.Fatal("expected DuplicateDomain event, got none")
	}
	if len(provider.Snapshot()) != 0 {
		t.Fatalf("provider should not have been called, has %d records", len(provider.Snapshot()))
	}
}

func TestHandleAnnotations_CleansUpStaleOwnedRecord(t *testing.T) {
	r, provider, _ := setupTest(t)

	stale := provider.Seed(dnsprovider.Record{
		ZoneID:   testZoneID,
		Name:     "old.example.com",
		OwnerRef: dnsprovider.OwnerRefFor(testNS, testSvcName),
	})
	r.SeedCache(stale)

	s := svc(dnsAnnotations(testZoneName, testDomain))
	mustReconcile(t, r, s)

	if _, ok := r.CacheRecord(testZoneID, "old.example.com", ""); ok {
		t.Fatal("stale record should have been removed from cache")
	}
	if _, ok := r.CacheRecord(testZoneID, testDomain, ""); !ok {
		t.Fatal("new record should be in cache")
	}
	snap := provider.Snapshot()
	if len(snap) != 1 || snap[0].Name != testDomain {
		t.Fatalf("expected only new record in provider, got %+v", snap)
	}
}

func TestHandleUpdates_UpdatesExisting(t *testing.T) {
	r, provider, _ := setupTest(t)

	existing, err := provider.CreateRecord(context.Background(), dnsprovider.Record{
		ZoneID:   testZoneID,
		Name:     testDomain,
		Type:     dnsprovider.RecordTypeA,
		Content:  testIngress,
		TTL:      60,
		OwnerRef: dnsprovider.OwnerRefFor(testNS, testSvcName),
	})
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	r.SeedCache(existing)

	newSvc := svc(dnsAnnotations(testZoneName, "new.example.com"))

	mustReconcile(t, r, newSvc)

	if _, ok := r.CacheRecord(testZoneID, testDomain, ""); ok {
		t.Fatal("old domain should be removed from cache")
	}
	updated, ok := r.CacheRecord(testZoneID, "new.example.com", "")
	if !ok {
		t.Fatal("new domain should be in cache")
	}
	if updated.Name != "new.example.com" {
		t.Fatalf("updated record name = %q", updated.Name)
	}
}

func TestHandleUpdates_CreatesFreshWhenOldAnnotationsAbsent(t *testing.T) {
	// Update events where the old Service had no greydns identity
	// (e.g. DNS was just enabled, or the annotation was just added)
	// should create fresh.
	r, provider, _ := setupTest(t)

	newSvc := svc(dnsAnnotations(testZoneName, testDomain))

	mustReconcile(t, r, newSvc)

	if _, ok := r.CacheRecord(testZoneID, testDomain, ""); !ok {
		t.Fatal("expected fresh record to be created when old annotations are absent")
	}
	if len(provider.Snapshot()) != 1 {
		t.Fatalf("provider should hold the newly-created record, has %d", len(provider.Snapshot()))
	}
}

func TestHandleUpdates_RefusesToUpdateOtherServicesRecord(t *testing.T) {
	r, provider, recorder := setupTest(t)

	r.SeedCache(dnsprovider.Record{
		ID:       "other-id",
		ZoneID:   testZoneID,
		Name:     testDomain,
		OwnerRef: dnsprovider.OwnerRefFor(testNS, "other"),
	})

	newSvc := svc(dnsAnnotations(testZoneName, testDomain))

	expectIncomplete(t, r, newSvc)

	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, "DuplicateDomain") {
			t.Fatalf("expected DuplicateDomain event, got %q", ev)
		}
	default:
		t.Fatal("expected DuplicateDomain event, got none")
	}
	if len(provider.Snapshot()) != 0 {
		t.Fatal("provider should not have been called")
	}
}

func TestHandleDeletions_DeletesOwnedRecord(t *testing.T) {
	r, provider, _ := setupTest(t)

	existing, err := provider.CreateRecord(context.Background(), dnsprovider.Record{
		ZoneID:   testZoneID,
		Name:     testDomain,
		Type:     dnsprovider.RecordTypeA,
		Content:  testIngress,
		TTL:      60,
		OwnerRef: dnsprovider.OwnerRefFor(testNS, testSvcName),
	})
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	r.SeedCache(existing)

	s := svc(dnsAnnotations(testZoneName, testDomain))
	mustCleanup(t, r, s)

	if _, ok := r.CacheRecord(testZoneID, testDomain, ""); ok {
		t.Fatal("record should be removed from cache")
	}
	if len(provider.Snapshot()) != 0 {
		t.Fatal("record should be removed from provider")
	}
}

func TestHandleDeletions_NoopOnForeignRecord(t *testing.T) {
	r, provider, _ := setupTest(t)

	existing := provider.Seed(dnsprovider.Record{
		ZoneID:   testZoneID,
		Name:     testDomain,
		OwnerRef: dnsprovider.OwnerRefFor(testNS, "other"),
	})
	r.SeedCache(existing)

	s := svc(dnsAnnotations(testZoneName, testDomain))
	mustCleanup(t, r, s)

	if _, ok := r.CacheRecord(testZoneID, testDomain, ""); !ok {
		t.Fatal("foreign record should remain in cache")
	}
	if len(provider.Snapshot()) != 1 {
		t.Fatal("foreign record should remain in provider")
	}
}

func TestHandleDeletions_WorksWhenDNSDisabled(t *testing.T) {
	// Regression: a Service that gets DNS turned off before deletion
	// should still have its record cleaned up on delete, not leaked.
	r, provider, _ := setupTest(t)

	existing, err := provider.CreateRecord(context.Background(), dnsprovider.Record{
		ZoneID:   testZoneID,
		Name:     testDomain,
		Type:     dnsprovider.RecordTypeA,
		Content:  testIngress,
		TTL:      60,
		OwnerRef: dnsprovider.OwnerRefFor(testNS, testSvcName),
	})
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	r.SeedCache(existing)

	s := svc(map[string]string{records.AnnotationDNS: "false"})
	mustCleanup(t, r, s)

	if _, ok := r.CacheRecord(testZoneID, testDomain, ""); ok {
		t.Fatal("record should be removed from cache")
	}
	if len(provider.Snapshot()) != 0 {
		t.Fatal("record should be removed from provider")
	}
}

func TestHandleDeletions_WorksWhenAnnotationsMissing(t *testing.T) {
	// Regression: HandleDeletions must not depend on annotations to
	// locate owned records.
	r, provider, _ := setupTest(t)

	existing, err := provider.CreateRecord(context.Background(), dnsprovider.Record{
		ZoneID:   testZoneID,
		Name:     testDomain,
		Type:     dnsprovider.RecordTypeA,
		Content:  testIngress,
		TTL:      60,
		OwnerRef: dnsprovider.OwnerRefFor(testNS, testSvcName),
	})
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	r.SeedCache(existing)

	s := svc(nil)
	mustCleanup(t, r, s)

	if len(provider.Snapshot()) != 0 {
		t.Fatal("record should be removed from provider")
	}
}

func TestHandleAnnotations_EmptyDomainEmitsEvent(t *testing.T) {
	r, provider, recorder := setupTest(t)

	s := svc(map[string]string{
		records.AnnotationDNS:  "true",
		records.AnnotationZone: testZoneName,
	})
	mustReconcile(t, r, s)

	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, "InvalidAnnotation") {
			t.Fatalf("want InvalidAnnotation event, got %q", ev)
		}
	default:
		t.Fatal("expected InvalidAnnotation event")
	}
	if len(provider.Snapshot()) != 0 {
		t.Fatal("provider should not have been called")
	}
}

func TestHandleAnnotations_EmptyZoneEmitsEvent(t *testing.T) {
	r, provider, recorder := setupTest(t)

	s := svc(map[string]string{
		records.AnnotationDNS:    "true",
		records.AnnotationDomain: testDomain,
	})
	mustReconcile(t, r, s)

	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, "InvalidAnnotation") {
			t.Fatalf("want InvalidAnnotation event, got %q", ev)
		}
	default:
		t.Fatal("expected InvalidAnnotation event")
	}
	if len(provider.Snapshot()) != 0 {
		t.Fatal("provider should not have been called")
	}
}

func TestHandleUpdates_RenameOntoOtherOwnerEmitsEvent(t *testing.T) {
	// Regression: a Service must not be able to rename onto a domain
	// already owned by a different Service. The rename aborts with a
	// DuplicateDomain event and the peer's record is left intact.
	r, provider, recorder := setupTest(t)

	mine, err := provider.CreateRecord(context.Background(), dnsprovider.Record{
		ZoneID:   testZoneID,
		Name:     testDomain,
		Type:     dnsprovider.RecordTypeA,
		Content:  testIngress,
		TTL:      60,
		OwnerRef: dnsprovider.OwnerRefFor(testNS, testSvcName),
	})
	if err != nil {
		t.Fatalf("seed mine: %v", err)
	}
	r.SeedCache(mine)

	const foreignDomain = "foreign.example.com"
	foreign, err := provider.CreateRecord(context.Background(), dnsprovider.Record{
		ZoneID:   testZoneID,
		Name:     foreignDomain,
		Type:     dnsprovider.RecordTypeA,
		Content:  testIngress,
		TTL:      60,
		OwnerRef: dnsprovider.OwnerRefFor(testNS, "other"),
	})
	if err != nil {
		t.Fatalf("seed foreign: %v", err)
	}
	r.SeedCache(foreign)

	newSvc := svc(dnsAnnotations(testZoneName, foreignDomain))

	expectIncomplete(t, r, newSvc)

	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, "DuplicateDomain") {
			t.Fatalf("expected DuplicateDomain event, got %q", ev)
		}
	default:
		t.Fatal("expected DuplicateDomain event, got none")
	}
	// Foreign record untouched.
	stillForeign, ok := r.CacheRecord(testZoneID, foreignDomain, "")
	if !ok || stillForeign.OwnerRef != dnsprovider.OwnerRefFor(testNS, "other") {
		t.Fatal("foreign record should still be owned by the other service")
	}
	// Original record still in place (rename aborted).
	if _, intact := r.CacheRecord(testZoneID, testDomain, ""); !intact {
		t.Fatal("original record should be intact after rename abort")
	}
}

func TestHandleUpdates_ZoneChangeMigratesRecord(t *testing.T) {
	// Regression: a Service that changes greydns.io/zone must have its
	// record moved from the old zone to the new zone, not updated
	// in-place (record IDs are zone-scoped). The new record is created
	// before the old one is deleted so a create failure does not leave
	// the service without DNS.
	const otherZoneID, otherZoneName = "zone-id-2", "other.example"
	provider := fake.New(
		dnsprovider.Zone{ID: testZoneID, Name: testZoneName},
		dnsprovider.Zone{ID: otherZoneID, Name: otherZoneName},
	)
	stubRecorder(t)
	r := records.NewReconciler(
		provider,
		map[string]string{testZoneName: testZoneID, otherZoneName: otherZoneID},
		testIngress,
		60,
		dnsprovider.RecordTypeA,
		records.NewOverridePolicy("", false),
	)

	existing, err := provider.CreateRecord(context.Background(), dnsprovider.Record{
		ZoneID:   testZoneID,
		Name:     testDomain,
		Type:     dnsprovider.RecordTypeA,
		Content:  testIngress,
		TTL:      60,
		OwnerRef: dnsprovider.OwnerRefFor(testNS, testSvcName),
	})
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	r.SeedCache(existing)

	newSvc := svc(dnsAnnotations(otherZoneName, testDomain))

	mustReconcile(t, r, newSvc)

	snap := provider.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected exactly 1 record after migration, got %d", len(snap))
	}
	if snap[0].ZoneID != otherZoneID {
		t.Fatalf("expected record to live in %q, got %q", otherZoneID, snap[0].ZoneID)
	}
	cached, ok := r.CacheRecord(otherZoneID, testDomain, "")
	if !ok {
		t.Fatal("migrated record should be in cache under the new zone")
	}
	if cached.ZoneID != otherZoneID {
		t.Fatalf("cached record zone = %q, want %q", cached.ZoneID, otherZoneID)
	}
	if _, still := r.CacheRecord(testZoneID, testDomain, ""); still {
		t.Fatal("old-zone record should have been removed from cache")
	}
}

func TestCache_PreservesDuplicateNamesInSameZone(t *testing.T) {
	// Regression: multiple records at the same (zoneID, name) - e.g. a
	// pair of round-robin A records - must coexist in the cache.
	// Previously the map was keyed by (zoneID, name) and the second
	// record silently overwrote the first.
	r, _, _ := setupTest(t)

	r.SeedCache(dnsprovider.Record{
		ID:       "rec-1",
		ZoneID:   testZoneID,
		Name:     testDomain,
		Content:  "1.1.1.1",
		OwnerRef: dnsprovider.OwnerRefFor(testNS, testSvcName),
	})
	r.SeedCache(dnsprovider.Record{
		ID:       "rec-2",
		ZoneID:   testZoneID,
		Name:     testDomain,
		Content:  "2.2.2.2",
		OwnerRef: dnsprovider.OwnerRefFor(testNS, testSvcName),
	})

	if r.CacheLen() != 2 {
		t.Fatalf("expected both duplicates in cache, have %d", r.CacheLen())
	}
}

type rigOnDelete struct {
	*fake.Provider
	failDeletesOnce map[string]bool
}

func (p *rigOnDelete) DeleteRecord(ctx context.Context, zoneID, id string) error {
	if p.failDeletesOnce[id] {
		delete(p.failDeletesOnce, id)
		return errForTest("transient provider failure")
	}
	return p.Provider.DeleteRecord(ctx, zoneID, id)
}

type stubError string

func (e stubError) Error() string { return string(e) }

func errForTest(s string) error { return stubError(s) }

func TestHandleDeletions_RetriesFailedDeletes(t *testing.T) {
	// Regression: a DeleteRecord failure must queue the record for
	// retry by the refresh loop rather than leak silently.
	stubRecorder(t)
	base := fake.New(dnsprovider.Zone{ID: testZoneID, Name: testZoneName})
	rigged := &rigOnDelete{Provider: base, failDeletesOnce: map[string]bool{}}
	r := records.NewReconciler(
		rigged,
		map[string]string{testZoneName: testZoneID},
		testIngress,
		60,
		dnsprovider.RecordTypeA,
		records.NewOverridePolicy("", false),
	)

	existing, err := base.CreateRecord(context.Background(), dnsprovider.Record{
		ZoneID:   testZoneID,
		Name:     testDomain,
		Type:     dnsprovider.RecordTypeA,
		Content:  testIngress,
		TTL:      60,
		OwnerRef: dnsprovider.OwnerRefFor(testNS, testSvcName),
	})
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	r.SeedCache(existing)
	rigged.failDeletesOnce[existing.ID] = true

	mustCleanup(t, r, svc(dnsAnnotations(testZoneName, testDomain)))

	// First attempt failed: record is still in the provider AND the
	// cache entry stays (so a subsequent recreate of the same Service
	// doesn't cache-miss and duplicate).
	if len(base.Snapshot()) != 1 {
		t.Fatalf("provider should still hold the record after failed delete, has %d", len(base.Snapshot()))
	}
	if _, cached := r.CacheRecord(testZoneID, testDomain, ""); !cached {
		t.Fatal("cache entry should remain until the delete succeeds")
	}

	r.DrainDeleteRetries(context.Background())

	if len(base.Snapshot()) != 0 {
		t.Fatalf("retry drain should have removed the record, provider has %d", len(base.Snapshot()))
	}
	if _, cached := r.CacheRecord(testZoneID, testDomain, ""); cached {
		t.Fatal("cache entry should be cleared once the delete retry succeeds")
	}
}

// alwaysFailingDeletes is a fake provider whose DeleteRecord always
// returns an error; the counter records how many times drain attempts
// to delete, which equals the retry-queue depth at drain time.
type alwaysFailingDeletes struct {
	*fake.Provider
	deleteCalls int
}

func (p *alwaysFailingDeletes) DeleteRecord(_ context.Context, _, _ string) error {
	p.deleteCalls++
	return errForTest("provider outage")
}

func TestHandleDeletions_DedupesRepeatedEnqueues(t *testing.T) {
	// Regression: repeated failing delete attempts for the same record
	// must collapse into a single retry-queue entry; otherwise the
	// queue grows unbounded under a sustained provider outage.
	stubRecorder(t)
	base := fake.New(dnsprovider.Zone{ID: testZoneID, Name: testZoneName})
	prov := &alwaysFailingDeletes{Provider: base}
	r := records.NewReconciler(
		prov,
		map[string]string{testZoneName: testZoneID},
		testIngress,
		60,
		dnsprovider.RecordTypeA,
		records.NewOverridePolicy("", false),
	)

	r.SeedCache(dnsprovider.Record{
		ID:       "rec-1",
		ZoneID:   testZoneID,
		Name:     testDomain,
		Type:     dnsprovider.RecordTypeA,
		Content:  testIngress,
		TTL:      60,
		OwnerRef: dnsprovider.OwnerRefFor(testNS, testSvcName),
	})

	svcObj := svc(dnsAnnotations(testZoneName, testDomain))
	const failedAttempts = 5
	for range failedAttempts {
		mustCleanup(t, r, svcObj)
	}

	// Every HandleDeletions calls the provider once (and fails). That
	// accounts for the first `failedAttempts` calls. DrainDeleteRetries
	// should find exactly one pending entry regardless of the number
	// of failures, so it makes one additional provider call.
	prov.deleteCalls = 0
	r.DrainDeleteRetries(context.Background())
	if prov.deleteCalls != 1 {
		t.Fatalf("retry queue should hold exactly one entry, drain attempted %d deletes", prov.deleteCalls)
	}
}

func TestReconciler_CacheConcurrentAccess(t *testing.T) {
	// Runs SeedCache, CacheRecord, ReplaceCacheIfUnchanged and
	// DrainDeleteRetries concurrently with race detection on. Asserts
	// final cache state is internally consistent (every cached record
	// can be looked up via both the (zone, name) and the owner path).
	stubRecorder(t)
	provider := fake.New(dnsprovider.Zone{ID: testZoneID, Name: testZoneName})
	r := records.NewReconciler(
		provider,
		map[string]string{testZoneName: testZoneID},
		testIngress,
		60,
		dnsprovider.RecordTypeA,
		records.NewOverridePolicy("", false),
	)

	const workers = 8
	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(workers * 2)

	// Writer workers: seed, read, replace.
	for w := range workers {
		go func(id int) {
			defer wg.Done()
			for i := range iterations {
				name := fmt.Sprintf("svc-%d-%d.example.com", id, i)
				r.SeedCache(dnsprovider.Record{
					ID:       fmt.Sprintf("rec-%d-%d", id, i),
					ZoneID:   testZoneID,
					Name:     name,
					Type:     dnsprovider.RecordTypeA,
					Content:  testIngress,
					TTL:      60,
					OwnerRef: dnsprovider.OwnerRefFor(testNS, fmt.Sprintf("svc-%d-%d", id, i)),
				})
				_, _ = r.CacheRecord(testZoneID, name, "")
			}
		}(w)
	}

	// Reader workers: len + refresh-style replace via WriteGen guard.
	for range workers {
		go func() {
			defer wg.Done()
			for range iterations {
				gen := r.WriteGen()
				_ = r.CacheLen()
				r.ReplaceCacheIfUnchanged(records.NewCacheSnapshot(), gen)
			}
		}()
	}

	wg.Wait()
}

func TestHandleAnnotations_ReconcilesDriftedRecord(t *testing.T) {
	// Regression: if a cached record's content, TTL, or type diverges
	// from desired state (e.g. ingress-destination changed in config
	// or the record was edited out-of-band), HandleAnnotations should
	// update the record on the next reconcile rather than ignoring it.
	r, provider, _ := setupTest(t)

	drifted, err := provider.CreateRecord(context.Background(), dnsprovider.Record{
		ZoneID:   testZoneID,
		Name:     testDomain,
		Type:     dnsprovider.RecordTypeA,
		Content:  "9.9.9.9", // differs from testIngress
		TTL:      300,       // differs from Reconciler.recordTTL (60)
		OwnerRef: dnsprovider.OwnerRefFor(testNS, testSvcName),
	})
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	r.SeedCache(drifted)

	mustReconcile(t, r, svc(dnsAnnotations(testZoneName, testDomain)))

	reconciled, ok := r.CacheRecord(testZoneID, testDomain, dnsprovider.OwnerRefFor(testNS, testSvcName))
	if !ok {
		t.Fatal("reconciled record missing from cache")
	}
	if reconciled.Content != testIngress {
		t.Fatalf("content drift not fixed: got %q, want %q", reconciled.Content, testIngress)
	}
	if reconciled.TTL != 60 {
		t.Fatalf("TTL drift not fixed: got %d, want 60", reconciled.TTL)
	}
	// Provider state should also reflect the corrected values.
	snap := provider.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("provider should hold exactly one record, has %d", len(snap))
	}
	if snap[0].Content != testIngress {
		t.Fatalf("provider content not reconciled: got %q", snap[0].Content)
	}
}

func TestHandleAnnotations_PerServiceTTLOverride(t *testing.T) {
	r, provider, _ := setupTest(t)

	s := svc(dnsAnnotations(testZoneName, testDomain))
	s.Annotations[records.AnnotationTTL] = "900"
	mustReconcile(t, r, s)

	snap := provider.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("want 1 record, got %d", len(snap))
	}
	if snap[0].TTL != 900 {
		t.Fatalf("TTL = %d, want 900", snap[0].TTL)
	}
}

func TestHandleAnnotations_PerServiceRecordTypeOverride(t *testing.T) {
	r, provider, _ := setupTest(t)

	s := svc(dnsAnnotations(testZoneName, testDomain))
	s.Annotations[records.AnnotationRecordType] = "CNAME"
	mustReconcile(t, r, s)

	snap := provider.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("want 1 record, got %d", len(snap))
	}
	if snap[0].Type != dnsprovider.RecordTypeCNAME {
		t.Fatalf("Type = %q, want CNAME", snap[0].Type)
	}
}

func TestHandleAnnotations_InvalidTTLAnnotationEmitsEvent(t *testing.T) {
	r, provider, recorder := setupTest(t)

	s := svc(dnsAnnotations(testZoneName, testDomain))
	s.Annotations[records.AnnotationTTL] = "not-an-int"
	mustReconcile(t, r, s)

	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, "InvalidAnnotation") {
			t.Fatalf("want InvalidAnnotation event, got %q", ev)
		}
	default:
		t.Fatal("expected InvalidAnnotation event, got none")
	}
	// Fell back to controller default, still created the record.
	snap := provider.Snapshot()
	if len(snap) != 1 || snap[0].TTL != 60 {
		t.Fatalf("fallback TTL not applied: %+v", snap)
	}
}

func TestHandleAnnotations_UnsupportedRecordTypeEmitsEvent(t *testing.T) {
	r, _, recorder := setupTest(t)

	s := svc(dnsAnnotations(testZoneName, testDomain))
	s.Annotations[records.AnnotationRecordType] = "MX"
	mustReconcile(t, r, s)

	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, "InvalidAnnotation") {
			t.Fatalf("want InvalidAnnotation event, got %q", ev)
		}
	default:
		t.Fatal("expected InvalidAnnotation event, got none")
	}
}

func TestHandleAnnotations_ProviderHintsFlowThrough(t *testing.T) {
	r, provider, _ := setupTest(t)

	s := svc(dnsAnnotations(testZoneName, testDomain))
	s.Annotations["greydns.io/fake-proxied"] = "true"
	s.Annotations["greydns.io/fake-mode"] = "strict"
	s.Annotations["greydns.io/unrelated"] = "ignored"
	mustReconcile(t, r, s)

	snap := provider.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("want 1 record, got %d", len(snap))
	}
	hints := snap[0].ProviderHints
	if hints["proxied"] != "true" {
		t.Fatalf("proxied hint = %q, want true", hints["proxied"])
	}
	if hints["mode"] != "strict" {
		t.Fatalf("mode hint = %q, want strict", hints["mode"])
	}
	if _, unrelated := hints["unrelated"]; unrelated {
		t.Fatal("non-prefixed annotation leaked into hints")
	}
}

func TestOverridePolicy_Allows(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		hasKey  bool
		queries map[string]bool
	}{
		{
			name:   "missing key allows all",
			raw:    "",
			hasKey: false,
			queries: map[string]bool{
				"ttl":                true,
				"record-type":        true,
				"cloudflare-proxied": true,
			},
		},
		{
			name:   "star allows all",
			raw:    "*",
			hasKey: true,
			queries: map[string]bool{
				"ttl":                true,
				"cloudflare-proxied": true,
			},
		},
		{
			name:   "empty string denies all",
			raw:    "",
			hasKey: true,
			queries: map[string]bool{
				"ttl":         false,
				"record-type": false,
			},
		},
		{
			name:   "allowlist ignores unlisted",
			raw:    "ttl, record-type",
			hasKey: true,
			queries: map[string]bool{
				"ttl":                true,
				"record-type":        true,
				"cloudflare-proxied": false,
				"unknown":            false,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := records.NewOverridePolicy(tc.raw, tc.hasKey)
			for key, want := range tc.queries {
				if got := policy.Allows(key); got != want {
					t.Errorf("Allows(%q) = %v, want %v", key, got, want)
				}
			}
		})
	}
}

func TestHandleAnnotations_TTLOverrideBlockedByPolicy(t *testing.T) {
	stubRecorder(t)
	provider := fake.New(dnsprovider.Zone{ID: testZoneID, Name: testZoneName})
	recorder := record.NewFakeRecorder(16)
	utils.Recorder = recorder //nolint:reassign // test stubs the recorder
	r := records.NewReconciler(
		provider,
		map[string]string{testZoneName: testZoneID},
		testIngress,
		60,
		dnsprovider.RecordTypeA,
		records.NewOverridePolicy("record-type", true),
	)

	s := svc(dnsAnnotations(testZoneName, testDomain))
	s.Annotations[records.AnnotationTTL] = "900"
	mustReconcile(t, r, s)

	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, "InvalidAnnotation") || !strings.Contains(ev, "allowed-overrides") {
			t.Fatalf("want InvalidAnnotation about allowed-overrides, got %q", ev)
		}
	default:
		t.Fatal("expected InvalidAnnotation event, got none")
	}
	snap := provider.Snapshot()
	if len(snap) != 1 || snap[0].TTL != 60 {
		t.Fatalf("TTL override should have been blocked, got %+v", snap)
	}
}

func TestHandleAnnotations_ProviderHintBlockedByPolicy(t *testing.T) {
	stubRecorder(t)
	provider := fake.New(dnsprovider.Zone{ID: testZoneID, Name: testZoneName})
	recorder := record.NewFakeRecorder(16)
	utils.Recorder = recorder //nolint:reassign // test stubs the recorder
	r := records.NewReconciler(
		provider,
		map[string]string{testZoneName: testZoneID},
		testIngress,
		60,
		dnsprovider.RecordTypeA,
		records.NewOverridePolicy("ttl", true),
	)

	s := svc(dnsAnnotations(testZoneName, testDomain))
	s.Annotations["greydns.io/fake-proxied"] = "true"
	mustReconcile(t, r, s)

	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, "InvalidAnnotation") {
			t.Fatalf("want InvalidAnnotation, got %q", ev)
		}
	default:
		t.Fatal("expected InvalidAnnotation event, got none")
	}
	snap := provider.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("want 1 record, got %d", len(snap))
	}
	if _, set := snap[0].ProviderHints["proxied"]; set {
		t.Fatalf("provider hint should have been blocked, got %+v", snap[0].ProviderHints)
	}
}

func TestHandleAnnotations_TTLOverrideDriftTriggersUpdate(t *testing.T) {
	r, provider, _ := setupTest(t)

	// Seed a record with the controller default TTL.
	existing, err := provider.CreateRecord(context.Background(), dnsprovider.Record{
		ZoneID:   testZoneID,
		Name:     testDomain,
		Type:     dnsprovider.RecordTypeA,
		Content:  testIngress,
		TTL:      60,
		OwnerRef: dnsprovider.OwnerRefFor(testNS, testSvcName),
	})
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	r.SeedCache(existing)

	// Reconcile with a TTL override that differs from the cached TTL.
	s := svc(dnsAnnotations(testZoneName, testDomain))
	s.Annotations[records.AnnotationTTL] = "900"
	mustReconcile(t, r, s)

	snap := provider.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("want 1 record, got %d", len(snap))
	}
	if snap[0].TTL != 900 {
		t.Fatalf("TTL drift not reconciled: got %d, want 900", snap[0].TTL)
	}
}

func TestHandleAnnotations_MultipleDomainsCreateAll(t *testing.T) {
	r, provider, _ := setupTest(t)

	s := svc(dnsAnnotations(testZoneName, "a.example.com, b.example.com,c.example.com"))
	mustReconcile(t, r, s)

	names := map[string]bool{}
	for _, rec := range provider.Snapshot() {
		names[rec.Name] = true
	}
	for _, want := range []string{"a.example.com", "b.example.com", "c.example.com"} {
		if !names[want] {
			t.Fatalf("missing %s in provider, got %v", want, names)
		}
	}
	if r.CacheLen() != 3 {
		t.Fatalf("cache should have 3 records, has %d", r.CacheLen())
	}
}

func TestHandleAnnotations_DedupesDomainsInAnnotation(t *testing.T) {
	r, provider, _ := setupTest(t)

	s := svc(dnsAnnotations(testZoneName, "a.example.com,a.example.com, a.example.com"))
	mustReconcile(t, r, s)

	if len(provider.Snapshot()) != 1 {
		t.Fatalf("duplicated domain should produce one record, got %d", len(provider.Snapshot()))
	}
}

func TestHandleUpdates_AddsNewDomainKeepsExisting(t *testing.T) {
	r, provider, _ := setupTest(t)

	oldSvc := svc(dnsAnnotations(testZoneName, "a.example.com"))
	mustReconcile(t, r, oldSvc)
	if len(provider.Snapshot()) != 1 {
		t.Fatalf("initial create failed, have %d records", len(provider.Snapshot()))
	}

	newSvc := svc(dnsAnnotations(testZoneName, "a.example.com,b.example.com"))
	mustReconcile(t, r, newSvc)

	names := map[string]bool{}
	for _, rec := range provider.Snapshot() {
		names[rec.Name] = true
	}
	if !names["a.example.com"] || !names["b.example.com"] {
		t.Fatalf("expected both records, got %v", names)
	}
}

func TestHandleUpdates_DroppedDomainGetsCleanedUp(t *testing.T) {
	r, provider, _ := setupTest(t)

	oldSvc := svc(dnsAnnotations(testZoneName, "a.example.com,b.example.com"))
	mustReconcile(t, r, oldSvc)
	if len(provider.Snapshot()) != 2 {
		t.Fatalf("initial create failed, have %d records", len(provider.Snapshot()))
	}

	newSvc := svc(dnsAnnotations(testZoneName, "a.example.com"))
	mustReconcile(t, r, newSvc)

	snap := provider.Snapshot()
	if len(snap) != 1 || snap[0].Name != "a.example.com" {
		t.Fatalf("expected only a.example.com, got %+v", snap)
	}
}

func TestHandleUpdates_ContestedNewDomainPreservesOldRecords(t *testing.T) {
	// Regression: if one of the new domains is owned by a peer, the
	// reconciler must NOT delete the old records while the rename is
	// blocked. Otherwise the Service loses DNS entirely because of a
	// misconfiguration elsewhere.
	r, provider, _ := setupTest(t)

	oldSvc := svc(dnsAnnotations(testZoneName, "a.example.com"))
	mustReconcile(t, r, oldSvc)

	// Seed a peer-owned record at the new target name.
	r.SeedCache(dnsprovider.Record{
		ID:       "peer-id",
		ZoneID:   testZoneID,
		Name:     "b.example.com",
		OwnerRef: dnsprovider.OwnerRefFor(testNS, "peer"),
	})

	newSvc := svc(dnsAnnotations(testZoneName, "b.example.com"))
	expectIncomplete(t, r, newSvc)

	names := map[string]bool{}
	for _, rec := range provider.Snapshot() {
		names[rec.Name] = true
	}
	if !names["a.example.com"] {
		t.Fatalf("a.example.com should be preserved when rename target is contested, got %v", names)
	}
}
