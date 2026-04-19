package metrics_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/math280h/greydns/internal/metrics"
)

func TestRegister_ExposesMetricsEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	metrics.Register(mux)

	metrics.Reconciles.WithLabelValues(metrics.OutcomeCreated).Inc()
	metrics.CacheRecords.Set(7)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("get /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(body)
	for _, want := range []string{
		`greydns_reconciles_total{outcome="created"} `,
		`greydns_cache_records 7`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics output missing %q", want)
		}
	}
}

func TestObserveProviderCall_RecordsOutcome(t *testing.T) {
	done := metrics.ObserveProviderCall("unit-test", metrics.OpCreate)
	var err error
	done(&err)

	boom := errors.New("boom")
	doneErr := metrics.ObserveProviderCall("unit-test", metrics.OpCreate)
	doneErr(&boom)

	mux := http.NewServeMux()
	metrics.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("get /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	for _, want := range []string{
		`greydns_provider_calls_total{operation="create",outcome="success",provider="unit-test"} `,
		`greydns_provider_calls_total{operation="create",outcome="error",provider="unit-test"} `,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics output missing %q", want)
		}
	}
}
