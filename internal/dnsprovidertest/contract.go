// Package dnsprovidertest defines a contract-test suite every
// dnsprovider.Provider implementation is expected to pass. Real
// providers (Cloudflare, Route53, etc.) wire a Harness through
// RunContractTests; the fake in internal/dnsprovider/fake serves as
// the reference implementation.
package dnsprovidertest

import (
	"context"
	"testing"

	"github.com/math280h/greydns/internal/dnsprovider"
)

// Harness wires a provider under test to the contract suite. Zones
// contains zones the Provider should already know about; each subtest
// creates and cleans up its own records. SeedUnmanaged is optional
// and lets the suite insert records that bypass greydns ownership so
// ListOwnedRecords filtering can be exercised. Providers without a
// test hook can leave SeedUnmanaged nil; filtering tests skip.
type Harness struct {
	Provider      dnsprovider.Provider
	Zones         []dnsprovider.Zone
	SeedUnmanaged func(rec dnsprovider.Record)
}

// Factory returns a fresh Harness per subtest. Real-backend
// implementations should use t.Cleanup to tear down any records they
// create outside the suite.
type Factory func(t *testing.T) Harness

// RunContractTests runs every contract subtest against factory.
// Subtests are independent; each calls factory for a clean provider.
func RunContractTests(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("Name", func(t *testing.T) { testName(t, factory(t)) })
	t.Run("SupportedRecordTypes", func(t *testing.T) { testSupportedRecordTypes(t, factory(t)) })
	t.Run("ListZones", func(t *testing.T) { testListZones(t, factory(t)) })
	t.Run("CreateReturnsIDAndPreservesFields", func(t *testing.T) { testCreatePreservesFields(t, factory(t)) })
	t.Run("CreateInUnknownZoneErrors", func(t *testing.T) { testCreateUnknownZone(t, factory(t)) })
	t.Run("ListOwnedRecordsIncludesCreated", func(t *testing.T) { testListIncludesCreated(t, factory(t)) })
	t.Run("ListOwnedRecordsFiltersUnmanaged", func(t *testing.T) { testListFiltersUnmanaged(t, factory(t)) })
	t.Run("UpdateModifiesFields", func(t *testing.T) { testUpdateModifies(t, factory(t)) })
	t.Run("DeleteRemovesFromList", func(t *testing.T) { testDeleteRemoves(t, factory(t)) })
	t.Run("DeleteUnknownIDErrors", func(t *testing.T) { testDeleteUnknown(t, factory(t)) })
}

func testName(t *testing.T, h Harness) {
	if h.Provider.Name() == "" {
		t.Fatal("Name() must be non-empty")
	}
}

func testSupportedRecordTypes(t *testing.T, h Harness) {
	if len(h.Provider.SupportedRecordTypes()) == 0 {
		t.Fatal("SupportedRecordTypes() must return at least one type")
	}
}

func testListZones(t *testing.T, h Harness) {
	zones, err := h.Provider.ListZones(context.Background())
	if err != nil {
		t.Fatalf("ListZones: %v", err)
	}
	byID := make(map[string]string, len(zones))
	for _, z := range zones {
		byID[z.ID] = z.Name
	}
	for _, want := range h.Zones {
		got, ok := byID[want.ID]
		if !ok {
			t.Fatalf("ListZones missing %s (%s); got %+v", want.ID, want.Name, zones)
		}
		if got != want.Name {
			t.Fatalf("zone %s name = %q, want %q", want.ID, got, want.Name)
		}
	}
}

func testCreatePreservesFields(t *testing.T, h Harness) {
	zone := firstZone(t, h)
	rt := firstSupportedType(t, h)
	desired := dnsprovider.Record{
		ZoneID:   zone.ID,
		Name:     "api.example.com",
		Type:     rt,
		Content:  recordContentFor(rt),
		TTL:      60,
		OwnerRef: dnsprovider.OwnerRefFor(dnsprovider.KindService, "default", "api"),
	}
	created, err := h.Provider.CreateRecord(context.Background(), desired)
	if err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	if created.ID == "" {
		t.Fatal("CreateRecord returned empty ID")
	}
	if created.Name != desired.Name {
		t.Fatalf("Name = %q, want %q", created.Name, desired.Name)
	}
	if created.Type != desired.Type {
		t.Fatalf("Type = %q, want %q", created.Type, desired.Type)
	}
	if created.Content != desired.Content {
		t.Fatalf("Content = %q, want %q", created.Content, desired.Content)
	}
	if created.OwnerRef != desired.OwnerRef {
		t.Fatalf("OwnerRef = %q, want %q", created.OwnerRef, desired.OwnerRef)
	}
}

func testCreateUnknownZone(t *testing.T, h Harness) {
	rt := firstSupportedType(t, h)
	_, err := h.Provider.CreateRecord(context.Background(), dnsprovider.Record{
		ZoneID:   "zone-that-does-not-exist",
		Name:     "api.example.com",
		Type:     rt,
		Content:  recordContentFor(rt),
		TTL:      60,
		OwnerRef: dnsprovider.OwnerRefFor(dnsprovider.KindService, "default", "api"),
	})
	if err == nil {
		t.Fatal("CreateRecord in unknown zone must return an error")
	}
}

func testListIncludesCreated(t *testing.T, h Harness) {
	zone := firstZone(t, h)
	rt := firstSupportedType(t, h)
	owner := dnsprovider.OwnerRefFor(dnsprovider.KindService, "default", "api")
	created, err := h.Provider.CreateRecord(context.Background(), dnsprovider.Record{
		ZoneID: zone.ID, Name: "api.example.com", Type: rt,
		Content: recordContentFor(rt), TTL: 60, OwnerRef: owner,
	})
	if err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	listed, err := h.Provider.ListOwnedRecords(context.Background(), zone.ID)
	if err != nil {
		t.Fatalf("ListOwnedRecords: %v", err)
	}
	if !containsID(listed, created.ID) {
		t.Fatalf("ListOwnedRecords missing %s; got %+v", created.ID, listed)
	}
}

func testListFiltersUnmanaged(t *testing.T, h Harness) {
	if h.SeedUnmanaged == nil {
		t.Skip("harness does not expose SeedUnmanaged")
	}
	zone := firstZone(t, h)
	rt := firstSupportedType(t, h)
	h.SeedUnmanaged(dnsprovider.Record{
		ID: "unmanaged-1", ZoneID: zone.ID, Name: "external.example.com",
		Type: rt, Content: recordContentFor(rt), TTL: 60,
		// Deliberately no OwnerRef: the provider must filter this out.
	})
	listed, err := h.Provider.ListOwnedRecords(context.Background(), zone.ID)
	if err != nil {
		t.Fatalf("ListOwnedRecords: %v", err)
	}
	for _, rec := range listed {
		if rec.OwnerRef == "" {
			t.Fatalf("ListOwnedRecords returned un-owned record %s; ownership filtering is mandatory", rec.ID)
		}
	}
}

func testUpdateModifies(t *testing.T, h Harness) {
	zone := firstZone(t, h)
	rt := firstSupportedType(t, h)
	owner := dnsprovider.OwnerRefFor(dnsprovider.KindService, "default", "api")
	created, err := h.Provider.CreateRecord(context.Background(), dnsprovider.Record{
		ZoneID: zone.ID, Name: "api.example.com", Type: rt,
		Content: recordContentFor(rt), TTL: 60, OwnerRef: owner,
	})
	if err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	next := created
	next.TTL = 300
	next.Content = altContentFor(rt)
	updated, err := h.Provider.UpdateRecord(context.Background(), next)
	if err != nil {
		t.Fatalf("UpdateRecord: %v", err)
	}
	if updated.TTL != 300 {
		t.Fatalf("TTL = %d, want 300", updated.TTL)
	}
	if updated.Content != next.Content {
		t.Fatalf("Content = %q, want %q", updated.Content, next.Content)
	}
}

func testDeleteRemoves(t *testing.T, h Harness) {
	zone := firstZone(t, h)
	rt := firstSupportedType(t, h)
	owner := dnsprovider.OwnerRefFor(dnsprovider.KindService, "default", "api")
	created, err := h.Provider.CreateRecord(context.Background(), dnsprovider.Record{
		ZoneID: zone.ID, Name: "api.example.com", Type: rt,
		Content: recordContentFor(rt), TTL: 60, OwnerRef: owner,
	})
	if err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	if delErr := h.Provider.DeleteRecord(context.Background(), zone.ID, created.ID); delErr != nil {
		t.Fatalf("DeleteRecord: %v", delErr)
	}
	listed, err := h.Provider.ListOwnedRecords(context.Background(), zone.ID)
	if err != nil {
		t.Fatalf("ListOwnedRecords: %v", err)
	}
	if containsID(listed, created.ID) {
		t.Fatalf("ListOwnedRecords still contains %s after delete", created.ID)
	}
}

func testDeleteUnknown(t *testing.T, h Harness) {
	zone := firstZone(t, h)
	err := h.Provider.DeleteRecord(context.Background(), zone.ID, "definitely-not-a-real-id")
	if err == nil {
		t.Fatal("DeleteRecord with unknown ID must return an error")
	}
}

func firstZone(t *testing.T, h Harness) dnsprovider.Zone {
	t.Helper()
	if len(h.Zones) == 0 {
		t.Fatal("harness has no seeded zones")
	}
	return h.Zones[0]
}

func firstSupportedType(t *testing.T, h Harness) dnsprovider.RecordType {
	t.Helper()
	types := h.Provider.SupportedRecordTypes()
	if len(types) == 0 {
		t.Fatal("provider reports no supported record types")
	}
	return types[0]
}

// recordContentFor returns a syntactically valid content value for t so
// real providers accept CreateRecord without extra validation wiring.
func recordContentFor(t dnsprovider.RecordType) string {
	switch t {
	case dnsprovider.RecordTypeAAAA:
		return "2001:db8::1"
	case dnsprovider.RecordTypeCNAME:
		return "target.example.com"
	case dnsprovider.RecordTypeA:
		fallthrough
	default:
		return "1.2.3.4"
	}
}

func altContentFor(t dnsprovider.RecordType) string {
	switch t {
	case dnsprovider.RecordTypeAAAA:
		return "2001:db8::2"
	case dnsprovider.RecordTypeCNAME:
		return "other.example.com"
	case dnsprovider.RecordTypeA:
		fallthrough
	default:
		return "5.6.7.8"
	}
}

func containsID(recs []dnsprovider.Record, id string) bool {
	for _, r := range recs {
		if r.ID == id {
			return true
		}
	}
	return false
}
