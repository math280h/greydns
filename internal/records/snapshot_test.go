package records_test

import (
	"strings"
	"testing"

	"github.com/math280h/greydns/internal/dnsprovider"
	"github.com/math280h/greydns/internal/dnsprovider/fake"
	"github.com/math280h/greydns/internal/records"
)

func TestParseSnapshot_AcceptsValidConfig(t *testing.T) {
	provider := fake.New(dnsprovider.Zone{ID: "z1", Name: "example.com"})
	data := map[string]string{
		"record-ttl":          "120",
		"record-type":         "A",
		"ingress-destination": "1.2.3.4",
		"allowed-overrides":   "ttl",
	}
	snap, err := records.ParseSnapshot(data, provider)
	if err != nil {
		t.Fatalf("ParseSnapshot: %v", err)
	}
	if snap.RecordTTL != 120 {
		t.Fatalf("TTL = %d, want 120", snap.RecordTTL)
	}
	if snap.RecordType != dnsprovider.RecordTypeA {
		t.Fatalf("RecordType = %q, want A", snap.RecordType)
	}
	if !snap.OverridePolicy.Allows("ttl") {
		t.Fatal("allowlist should include ttl")
	}
	if snap.OverridePolicy.Allows("record-type") {
		t.Fatal("allowlist should exclude record-type")
	}
}

func TestParseSnapshot_RejectsInvalid(t *testing.T) {
	provider := fake.New(dnsprovider.Zone{ID: "z1", Name: "example.com"})
	base := func() map[string]string {
		return map[string]string{
			"record-ttl":          "60",
			"record-type":         "A",
			"ingress-destination": "1.2.3.4",
		}
	}
	cases := []struct {
		name     string
		mutate   func(m map[string]string)
		wantWord string
	}{
		{"missing ingress", func(m map[string]string) { delete(m, "ingress-destination") }, "ingress-destination"},
		{"empty ingress", func(m map[string]string) { m["ingress-destination"] = "" }, "ingress-destination"},
		{"missing ttl", func(m map[string]string) { delete(m, "record-ttl") }, "record-ttl"},
		{"ttl not int", func(m map[string]string) { m["record-ttl"] = "abc" }, "record-ttl"},
		{"ttl zero", func(m map[string]string) { m["record-ttl"] = "0" }, "record-ttl"},
		{"ttl negative", func(m map[string]string) { m["record-ttl"] = "-1" }, "record-ttl"},
		{"missing type", func(m map[string]string) { delete(m, "record-type") }, "record-type"},
		{"unsupported type", func(m map[string]string) { m["record-type"] = "MX" }, "record-type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			tc.mutate(m)
			_, err := records.ParseSnapshot(m, provider)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Fatalf("error %q should mention %q", err, tc.wantWord)
			}
		})
	}
}

func TestReconciler_UpdateSnapshot_TakesEffectOnNextReconcile(t *testing.T) {
	// Regression for hot-reload: swapping the snapshot must change the
	// TTL written to the provider on the next reconcile without
	// rebuilding the reconciler.
	r, provider, _ := setupTest(t)

	mustReconcile(t, r, svc(dnsAnnotations(testZoneName, testDomain)))
	snap := provider.Snapshot()
	if len(snap) != 1 || snap[0].TTL != 60 {
		t.Fatalf("initial record should have TTL 60, got %+v", snap)
	}

	next := r.Snapshot()
	next.RecordTTL = 900
	r.UpdateSnapshot(next)

	mustReconcile(t, r, svc(dnsAnnotations(testZoneName, testDomain)))
	snap = provider.Snapshot()
	if len(snap) != 1 || snap[0].TTL != 900 {
		t.Fatalf("after UpdateSnapshot, TTL should be 900, got %+v", snap)
	}
}

func TestReconciler_UpdateSnapshot_ChangesIngressDestination(t *testing.T) {
	r, provider, _ := setupTest(t)
	mustReconcile(t, r, svc(dnsAnnotations(testZoneName, testDomain)))

	next := r.Snapshot()
	next.IngressDestination = "9.9.9.9"
	r.UpdateSnapshot(next)

	mustReconcile(t, r, svc(dnsAnnotations(testZoneName, testDomain)))
	snap := provider.Snapshot()
	if len(snap) != 1 || snap[0].Content != "9.9.9.9" {
		t.Fatalf("ingress destination change not picked up, got %+v", snap)
	}
}
