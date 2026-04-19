package records_test

import (
	"context"
	"strings"
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
	r.HandleAnnotations(context.Background(), s)

	rec, ok := r.CacheRecord(testZoneID, testDomain)
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
	r.HandleAnnotations(context.Background(), s)

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
	r.HandleAnnotations(context.Background(), s)

	if r.CacheLen() != 0 {
		t.Fatalf("cache should be empty, has %d", r.CacheLen())
	}
	if len(provider.Snapshot()) != 0 {
		t.Fatalf("provider should be empty, has %d", len(provider.Snapshot()))
	}
}

func TestHandleAnnotations_DuplicateDomainEmitsEvent(t *testing.T) {
	r, provider, recorder := setupTest(t)

	r.SeedCache(testZoneID, testDomain, dnsprovider.Record{
		ID:       "existing",
		ZoneID:   testZoneID,
		Name:     testDomain,
		OwnerRef: dnsprovider.OwnerRefFor(testNS, "other"),
	})

	s := svc(dnsAnnotations(testZoneName, testDomain))
	r.HandleAnnotations(context.Background(), s)

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
	r.SeedCache(testZoneID, "old.example.com", stale)

	s := svc(dnsAnnotations(testZoneName, testDomain))
	r.HandleAnnotations(context.Background(), s)

	if _, ok := r.CacheRecord(testZoneID, "old.example.com"); ok {
		t.Fatal("stale record should have been removed from cache")
	}
	if _, ok := r.CacheRecord(testZoneID, testDomain); !ok {
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
	r.SeedCache(testZoneID, testDomain, existing)

	oldSvc := svc(dnsAnnotations(testZoneName, testDomain))
	newSvc := svc(dnsAnnotations(testZoneName, "new.example.com"))

	r.HandleUpdates(context.Background(), newSvc, oldSvc)

	if _, ok := r.CacheRecord(testZoneID, testDomain); ok {
		t.Fatal("old domain should be removed from cache")
	}
	updated, ok := r.CacheRecord(testZoneID, "new.example.com")
	if !ok {
		t.Fatal("new domain should be in cache")
	}
	if updated.Name != "new.example.com" {
		t.Fatalf("updated record name = %q", updated.Name)
	}
}

func TestHandleUpdates_FallsThroughWhenOldMissing(t *testing.T) {
	r, _, _ := setupTest(t)

	oldSvc := svc(dnsAnnotations(testZoneName, testDomain))
	newSvc := svc(dnsAnnotations(testZoneName, testDomain))

	r.HandleUpdates(context.Background(), newSvc, oldSvc)

	if _, ok := r.CacheRecord(testZoneID, testDomain); !ok {
		t.Fatal("expected fresh record to be created via fall-through")
	}
}

func TestHandleUpdates_RefusesToUpdateOtherServicesRecord(t *testing.T) {
	r, provider, recorder := setupTest(t)

	r.SeedCache(testZoneID, testDomain, dnsprovider.Record{
		ID:       "other-id",
		ZoneID:   testZoneID,
		Name:     testDomain,
		OwnerRef: dnsprovider.OwnerRefFor(testNS, "other"),
	})

	oldSvc := svc(dnsAnnotations(testZoneName, testDomain))
	newSvc := svc(dnsAnnotations(testZoneName, testDomain))

	r.HandleUpdates(context.Background(), newSvc, oldSvc)

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
	r.SeedCache(testZoneID, testDomain, existing)

	s := svc(dnsAnnotations(testZoneName, testDomain))
	r.HandleDeletions(context.Background(), s)

	if _, ok := r.CacheRecord(testZoneID, testDomain); ok {
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
	r.SeedCache(testZoneID, testDomain, existing)

	s := svc(dnsAnnotations(testZoneName, testDomain))
	r.HandleDeletions(context.Background(), s)

	if _, ok := r.CacheRecord(testZoneID, testDomain); !ok {
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
	r.SeedCache(testZoneID, testDomain, existing)

	s := svc(map[string]string{records.AnnotationDNS: "false"})
	r.HandleDeletions(context.Background(), s)

	if _, ok := r.CacheRecord(testZoneID, testDomain); ok {
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
	r.SeedCache(testZoneID, testDomain, existing)

	s := svc(nil)
	r.HandleDeletions(context.Background(), s)

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
	r.HandleAnnotations(context.Background(), s)

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
	r.HandleAnnotations(context.Background(), s)

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
	r.SeedCache(testZoneID, testDomain, existing)

	oldSvc := svc(dnsAnnotations(testZoneName, testDomain))
	newSvc := svc(dnsAnnotations(otherZoneName, testDomain))

	r.HandleUpdates(context.Background(), newSvc, oldSvc)

	snap := provider.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected exactly 1 record after migration, got %d", len(snap))
	}
	if snap[0].ZoneID != otherZoneID {
		t.Fatalf("expected record to live in %q, got %q", otherZoneID, snap[0].ZoneID)
	}
	cached, ok := r.CacheRecord(otherZoneID, testDomain)
	if !ok {
		t.Fatal("migrated record should be in cache under the new zone")
	}
	if cached.ZoneID != otherZoneID {
		t.Fatalf("cached record zone = %q, want %q", cached.ZoneID, otherZoneID)
	}
	if _, still := r.CacheRecord(testZoneID, testDomain); still {
		t.Fatal("old-zone record should have been removed from cache")
	}
}
