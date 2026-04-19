package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	cfg "github.com/math280h/greydns/internal/config"
	"github.com/math280h/greydns/internal/controller"
	"github.com/math280h/greydns/internal/dnsprovider"
	"github.com/math280h/greydns/internal/dnsprovider/registry"
	"github.com/math280h/greydns/internal/health"
	"github.com/math280h/greydns/internal/metrics"
	_ "github.com/math280h/greydns/internal/providers" // registers all DNS provider factories
	"github.com/math280h/greydns/internal/records"
	"github.com/math280h/greydns/internal/utils"
)

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr}) //nolint:reassign // Required for logging
	if err := run(); err != nil {
		log.Fatal().Err(err).Msg("[Core] greydns exited with error")
	}
}

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Health server goes up first so kubelet probes can reach /healthz
	// while slow initialisation (cache warm-up, provider build) is
	// still running. /readyz stays 503 until ready.Store(true) below.
	// If the health server exits unexpectedly (e.g. bind failure) we
	// cancel the run ctx so the rest of startup bails out instead of
	// continuing without probes.
	var ready atomic.Bool
	healthSrv := health.New(health.DefaultAddr, ready.Load)
	metrics.Register(healthSrv.Mux())
	healthDone := make(chan error, 1)
	go func() {
		err := healthSrv.Run(ctx)
		healthDone <- err
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
			cancel()
		}
	}()

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("get cluster config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("create clientset: %w", err)
	}

	cfg.LoadConfigMap(clientset)
	secret, err := clientset.CoreV1().Secrets("default").Get(ctx, "greydns-secret", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get greydns-secret: %w", err)
	}

	utils.StartBroadcaster(clientset)

	provider := mustBuildProvider(secret.Data)
	ttl := mustAtoi(cfg.GetRequiredConfigValue("record-ttl"), "record-ttl")
	refreshInterval := mustRefreshInterval()
	recordType := mustRecordType(provider)

	zoneNameToID := mustListZones(ctx, provider)
	initialCache, refreshErr := refreshCache(ctx, provider, zoneNameToID)
	if refreshErr != nil {
		return fmt.Errorf("initial cache refresh: %w", refreshErr)
	}

	overrideRaw, hasOverrideKey := cfg.Data()["allowed-overrides"]
	reconciler := records.NewReconciler(
		provider,
		zoneNameToID,
		cfg.GetRequiredConfigValue("ingress-destination"),
		ttl,
		recordType,
		records.NewOverridePolicy(overrideRaw, hasOverrideKey),
	)
	reconciler.ReplaceCache(initialCache)

	ctrl := controller.New(clientset, reconciler)
	if startErr := ctrl.Start(ctx); startErr != nil {
		return fmt.Errorf("start controller: %w", startErr)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runRefreshLoop(ctx, provider, zoneNameToID, reconciler, refreshInterval)
	}()

	ready.Store(true)
	log.Info().Msg("[Core] greydns is ready")

	<-ctx.Done()
	log.Info().Msg("[Core] Shutdown signal received; draining")

	ready.Store(false)
	wg.Wait()
	if healthErr := <-healthDone; healthErr != nil && !errors.Is(healthErr, context.Canceled) {
		log.Error().Err(healthErr).Msg("[Health] Server exited with error")
	}
	log.Info().Msg("[Core] Shutdown complete")
	return nil
}

func mustBuildProvider(secretData map[string][]byte) dnsprovider.Provider {
	providerName := cfg.ProviderName()
	provider, err := registry.Build(
		providerName,
		registry.NewProviderConfig(providerName, cfg.Data()),
		secretData,
	)
	if err != nil {
		log.Fatal().Err(err).Str("provider", providerName).Msg("[Core] Failed to build provider")
	}
	log.Info().Str("provider", provider.Name()).Msg("[Core] DNS provider ready")
	return provider
}

func mustRefreshInterval() time.Duration {
	seconds := mustAtoi(cfg.GetRequiredConfigValue("cache-refresh-seconds"), "cache-refresh-seconds")
	if seconds <= 0 {
		log.Fatal().
			Int("cache-refresh-seconds", seconds).
			Msg("[Core] cache-refresh-seconds must be greater than 0")
	}
	return time.Duration(seconds) * time.Second
}

func mustRecordType(provider dnsprovider.Provider) dnsprovider.RecordType {
	rt := dnsprovider.RecordType(cfg.GetRequiredConfigValue("record-type"))
	if err := dnsprovider.ValidateRecordType(rt, provider.SupportedRecordTypes()); err != nil {
		log.Fatal().Err(err).Str("provider", provider.Name()).Msg("[Core] record-type rejected by provider")
	}
	return rt
}

// refreshAttemptBudget bounds per-tick retries when handler writes
// race the snapshot, so a busy cluster can't spin the goroutine.
const refreshAttemptBudget = 3

func runRefreshLoop(
	ctx context.Context,
	provider dnsprovider.Provider,
	zoneNameToID map[string]string,
	reconciler *records.Reconciler,
	interval time.Duration,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconciler.DrainDeleteRetries(ctx)
			refreshOnce(ctx, provider, zoneNameToID, reconciler)
		}
	}
}

func refreshOnce(
	ctx context.Context,
	provider dnsprovider.Provider,
	zoneNameToID map[string]string,
	reconciler *records.Reconciler,
) {
	for attempt := 1; attempt <= refreshAttemptBudget; attempt++ {
		if ctx.Err() != nil {
			return
		}
		// Capture writeGen before the round-trip so handler writes
		// landing during refresh cause a retry instead of a clobber.
		gen := reconciler.WriteGen()
		next, err := refreshCache(ctx, provider, zoneNameToID)
		if err != nil {
			log.Error().Err(err).Msg("[Core] Cache refresh failed; keeping previous cache")
			return
		}
		if reconciler.ReplaceCacheIfUnchanged(next, gen) {
			return
		}
		log.Info().Int("attempt", attempt).Msg("[Core] Cache mutated during refresh; retrying")
	}
	log.Warn().Int("budget", refreshAttemptBudget).Msg("[Core] Refresh retry budget exhausted; next tick will try again")
}

func mustAtoi(raw, name string) int {
	v, err := strconv.Atoi(raw)
	if err != nil {
		log.Fatal().Err(err).Str("key", name).Msg("[Core] Config value is not a valid integer")
	}
	return v
}

func mustListZones(ctx context.Context, provider dnsprovider.Provider) map[string]string {
	zones, err := listZones(ctx, provider)
	if err != nil {
		log.Fatal().Err(err).Msg("[Core] Failed to list zones")
	}
	out := make(map[string]string, len(zones))
	for _, z := range zones {
		out[z.Name] = z.ID
		log.Debug().Msgf("[Core] Found zone: %s (ID: %s)", z.Name, z.ID)
	}
	log.Info().Msgf("[Core] Found %d zones", len(out))
	return out
}

// refreshCache fails the whole snapshot if any zone errors; a partial
// snapshot would make the controller "forget" records and leak them.
func refreshCache(
	ctx context.Context,
	provider dnsprovider.Provider,
	zones map[string]string,
) (map[records.CacheKey]dnsprovider.Record, error) {
	start := time.Now()
	out := records.NewCacheSnapshot()
	total := 0
	for _, id := range zones {
		recs, err := listOwnedRecords(ctx, provider, id)
		if err != nil {
			return nil, err
		}
		for _, rec := range recs {
			key, value := records.BuildCacheEntry(rec)
			out[key] = value
			total++
			log.Debug().Msgf("[Core] Refresh found record: %s (ID: %s)", rec.Name, rec.ID)
		}
	}
	metrics.CacheRefreshDuration.Observe(time.Since(start).Seconds())
	metrics.CacheRefreshLastSuccess.SetToCurrentTime()
	log.Info().Msgf("[Core] Refresh found %d records", total)
	return out, nil
}

func listOwnedRecords(
	ctx context.Context,
	provider dnsprovider.Provider,
	zoneID string,
) ([]dnsprovider.Record, error) {
	var err error
	defer metrics.ObserveProviderCall(provider.Name(), metrics.OpListOwned)(&err)
	out, err := provider.ListOwnedRecords(ctx, zoneID)
	return out, err
}

func listZones(ctx context.Context, provider dnsprovider.Provider) ([]dnsprovider.Zone, error) {
	var err error
	defer metrics.ObserveProviderCall(provider.Name(), metrics.OpListZones)(&err)
	out, err := provider.ListZones(ctx)
	return out, err
}
