package fake

import (
	"context"
	"errors"
	"testing"

	"github.com/math280h/greydns/internal/dnsprovider"
)

func TestFakeLifecycle(t *testing.T) {
	ctx := context.Background()
	zone := dnsprovider.Zone{ID: "z1", Name: "example.com"}
	p := New(zone)

	zones, err := p.ListZones(ctx)
	if err != nil {
		t.Fatalf("ListZones: %v", err)
	}
	if len(zones) != 1 || zones[0].ID != "z1" {
		t.Fatalf("unexpected zones: %+v", zones)
	}

	created, err := p.CreateRecord(ctx, dnsprovider.Record{
		ZoneID:   "z1",
		Name:     "api.example.com",
		Type:     dnsprovider.RecordTypeA,
		Content:  "1.2.3.4",
		TTL:      60,
		OwnerRef: "default/api",
	})
	if err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected non-empty ID from Create")
	}

	owned, err := p.ListOwnedRecords(ctx, "z1")
	if err != nil {
		t.Fatalf("ListOwnedRecords: %v", err)
	}
	if len(owned) != 1 {
		t.Fatalf("want 1 owned record, got %d", len(owned))
	}

	created.Content = "5.6.7.8"
	updated, err := p.UpdateRecord(ctx, created)
	if err != nil {
		t.Fatalf("UpdateRecord: %v", err)
	}
	if updated.Content != "5.6.7.8" {
		t.Fatalf("expected updated content, got %q", updated.Content)
	}

	if err := p.DeleteRecord(ctx, "z1", created.ID); err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}
	if len(p.Snapshot()) != 0 {
		t.Fatalf("expected empty snapshot, got %+v", p.Snapshot())
	}
}

func TestFakeErrInjection(t *testing.T) {
	p := New(dnsprovider.Zone{ID: "z1", Name: "example.com"})
	want := errors.New("boom")
	p.Err = want

	if _, err := p.ListZones(context.Background()); !errors.Is(err, want) {
		t.Fatalf("want injected error, got %v", err)
	}
}

func TestFakeSeedBypassesCreate(t *testing.T) {
	p := New(dnsprovider.Zone{ID: "z1", Name: "example.com"})
	seeded := p.Seed(dnsprovider.Record{ZoneID: "z1", Name: "x.example.com", OwnerRef: "default/x"})
	if seeded.ID == "" {
		t.Fatal("Seed should allocate an ID")
	}
	if len(p.Snapshot()) != 1 {
		t.Fatalf("want 1 record after seed, got %d", len(p.Snapshot()))
	}
}
