// Package metrics exposes greydns's Prometheus metrics. Callers
// increment the exported counters and observe the histograms directly;
// a single Register call wires the /metrics endpoint onto any mux.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const subsystem = "greydns"

// Outcome values for the reconcile counter. Kept small so label
// cardinality stays predictable.
const (
	OutcomeCreated           = "created"
	OutcomeUpdated           = "updated"
	OutcomeNoop              = "noop"
	OutcomeDuplicateDomain   = "duplicate_domain"
	OutcomeInvalidAnnotation = "invalid_annotation"
	OutcomeError             = "error"
)

// Operation values for the provider_calls counters.
const (
	OpListZones = "list_zones"
	OpListOwned = "list_owned"
	OpCreate    = "create"
	OpUpdate    = "update"
	OpDelete    = "delete"
)

//nolint:gochecknoglobals // Prometheus collectors are idiomatic package globals
var (
	Reconciles = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: subsystem,
		Name:      "reconciles_total",
		Help:      "Reconciliations attempted, labelled by outcome.",
	}, []string{"outcome"})

	ProviderCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: subsystem,
		Name:      "provider_calls_total",
		Help:      "Provider API calls, labelled by provider, operation, and outcome.",
	}, []string{"provider", "operation", "outcome"})

	ProviderCallDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: subsystem,
		Name:      "provider_call_duration_seconds",
		Help:      "Provider API call latency.",
		Buckets:   prometheus.ExponentialBuckets(0.01, 2, 10),
	}, []string{"provider", "operation"})

	CacheRecords = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: subsystem,
		Name:      "cache_records",
		Help:      "Number of DNS records currently in the controller cache.",
	})

	RetryQueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: subsystem,
		Name:      "retry_queue_depth",
		Help:      "Number of failed deletes pending retry.",
	})

	CacheRefreshDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: subsystem,
		Name:      "cache_refresh_duration_seconds",
		Help:      "Cache refresh (ListOwnedRecords across all zones) latency.",
		Buckets:   prometheus.ExponentialBuckets(0.05, 2, 10),
	})

	CacheRefreshLastSuccess = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: subsystem,
		Name:      "cache_refresh_last_success_timestamp_seconds",
		Help:      "Unix timestamp of the last successful cache refresh.",
	})

	WorkqueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: subsystem,
		Name:      "workqueue_depth",
		Help:      "Items currently waiting in the reconcile workqueue.",
	})

	WorkqueueAdds = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: subsystem,
		Name:      "workqueue_adds_total",
		Help:      "Total items added to the reconcile workqueue.",
	})

	WorkqueueRetries = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: subsystem,
		Name:      "workqueue_retries_total",
		Help:      "Total reconcile attempts that failed and were requeued.",
	})
)

// Register attaches the /metrics endpoint to mux using the default
// Prometheus gatherer. Safe to call exactly once at startup.
func Register(mux *http.ServeMux) {
	mux.Handle("/metrics", promhttp.Handler())
}

// ObserveProviderCall times a provider operation and records both a
// duration observation and a success/error counter bump. Call it with
// the returned func in a defer: `defer metrics.ObserveProviderCall(
// provider, operation)(&err)`.
func ObserveProviderCall(provider, operation string) func(*error) {
	start := time.Now()
	return func(errPtr *error) {
		ProviderCallDuration.WithLabelValues(provider, operation).Observe(time.Since(start).Seconds())
		outcome := "success"
		if errPtr != nil && *errPtr != nil {
			outcome = "error"
		}
		ProviderCalls.WithLabelValues(provider, operation, outcome).Inc()
	}
}
