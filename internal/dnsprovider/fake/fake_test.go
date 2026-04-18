package fake_test

import (
	"context"
	"errors"
	"testing"

	"github.com/math280h/greydns/internal/dnsprovider"
	"github.com/math280h/greydns/internal/dnsprovider/fake"
)

func TestFakeLifecycle(t *testing.T) {
	ctx := context.Background()
	zone := dnsprovider.Zone{ID: "z1", Name: "example.com"}
	p := fake.New(zone)

	zones, zonesErr := p.ListZones(ctx)
	if zonesErr != nil {
		t.Fatalf("ListZones: %v", zonesErr)
	}
	if len(zones) != 1 || zones[0].ID != "z1" {
		t.Fatalf("unexpected zones: %+v", zones)
	}

	created, createErr := p.CreateRecord(ctx, dnsprovider.Record{
		ZoneID:   "z1",
		Name:     "api.example.com",
		Type:     dnsprovider.RecordTypeA,
		Content:  "1.2.3.4",
		TTL:      60,
		OwnerRef: "default/api",
	})
	if createErr != nil {
		t.Fatalf("CreateRecord: %v", createErr)
	}
	if created.ID == "" {
		t.Fatal("expected non-empty ID from Create")
	}

	owned, ownedErr := p.ListOwnedRecords(ctx, "z1")
	if ownedErr != nil {
		t.Fatalf("ListOwnedRecords: %v", ownedErr)
	}
	if len(owned) != 1 {
		t.Fatalf("want 1 owned record, got %d", len(owned))
	}

	created.Content = "5.6.7.8"
	updated, updateErr := p.UpdateRecord(ctx, created)
	if updateErr != nil {
		t.Fatalf("UpdateRecord: %v", updateErr)
	}
	if updated.Content != "5.6.7.8" {
		t.Fatalf("expected updated content, got %q", updated.Content)
	}

	if deleteErr := p.DeleteRecord(ctx, "z1", created.ID); deleteErr != nil {
		t.Fatalf("DeleteRecord: %v", deleteErr)
	}
	if len(p.Snapshot()) != 0 {
		t.Fatalf("expected empty snapshot, got %+v", p.Snapshot())
	}
}

func TestFakeErrInjection(t *testing.T) {
	p := fake.New(dnsprovider.Zone{ID: "z1", Name: "example.com"})
	want := errors.New("boom") //nolint:err113 // sentinel for test comparison
	p.Err = want

	if _, err := p.ListZones(context.Background()); !errors.Is(err, want) {
		t.Fatalf("want injected error, got %v", err)
	}
}

func TestFakeSeedBypassesCreate(t *testing.T) {
	p := fake.New(dnsprovider.Zone{ID: "z1", Name: "example.com"})
	seeded := p.Seed(dnsprovider.Record{ZoneID: "z1", Name: "x.example.com", OwnerRef: "default/x"})
	if seeded.ID == "" {
		t.Fatal("Seed should allocate an ID")
	}
	if len(p.Snapshot()) != 1 {
		t.Fatalf("want 1 record after seed, got %d", len(p.Snapshot()))
	}
}
