package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
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
	"github.com/math280h/greydns/internal/leader"
	"github.com/math280h/greydns/internal/metrics"
	_ "github.com/math280h/greydns/internal/providers" // registers all DNS provider factories
	"github.com/math280h/greydns/internal/records"
	"github.com/math280h/greydns/internal/utils"
)

const defaultNamespace = "default"

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

	namespace := namespaceFromEnv()
	cfg.LoadConfigMap(clientset, namespace)
	secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, "greydns-secret", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get greydns-secret: %w", err)
	}

	utils.StartBroadcaster(clientset)

	// Health probes flip to ready as soon as startup (provider build,
	// zone discovery, initial cache warm) succeeds, regardless of
	// leadership. Followers stay probe-green so they can take over fast.
	ready.Store(true)
	log.Info().Msg("[Core] greydns is ready")

	identity, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("resolve pod identity: %w", err)
	}

	leaderErr := leader.Run(ctx, leader.Config{
		Clientset: clientset,
		Namespace: namespace,
		LeaseName: leader.DefaultLeaseName,
		Identity:  identity,
		OnBecome: func(leaderCtx context.Context) {
			if runErr := runAsLeader(leaderCtx, clientset, namespace, secret.Data); runErr != nil {
				log.Error().Err(runErr).Msg("[Core] Leader run exited with error")
			}
		},
	})
	if leaderErr != nil {
		return fmt.Errorf("leader election: %w", leaderErr)
	}

	log.Info().Msg("[Core] Shutdown signal received; draining")
	ready.Store(false)
	if healthErr := <-healthDone; healthErr != nil && !errors.Is(healthErr, context.Canceled) {
		log.Error().Err(healthErr).Msg("[Health] Server exited with error")
	}
	log.Info().Msg("[Core] Shutdown complete")
	return nil
}

// runAsLeader builds the provider-backed reconciler and runs the
// controller plus refresh loop until leaderCtx cancels (leadership lost
// or shutdown). Called from leader.Run's OnBecome so only one pod at a
// time mutates DNS.
func runAsLeader(
	leaderCtx context.Context,
	clientset kubernetes.Interface,
	namespace string,
	secretData map[string][]byte,
) error {
	provider := mustBuildProvider(secretData)
	refreshInterval := mustRefreshInterval()

	initialSnapshot, err := records.ParseSnapshot(cfg.Data(), provider)
	if err != nil {
		return fmt.Errorf("parse initial config: %w", err)
	}

	zoneNameToID := mustListZones(leaderCtx, provider)
	initialCache, refreshErr := refreshCache(leaderCtx, provider, zoneNameToID)
	if refreshErr != nil {
		return fmt.Errorf("initial cache refresh: %w", refreshErr)
	}

	reconciler := records.NewReconciler(provider, zoneNameToID, initialSnapshot)
	reconciler.ReplaceCache(initialCache)

	ctrl := controller.New(clientset, reconciler)
	if startErr := ctrl.Start(leaderCtx); startErr != nil {
		return fmt.Errorf("start controller: %w", startErr)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		runRefreshLoop(leaderCtx, provider, zoneNameToID, reconciler, refreshInterval)
	}()
	go func() {
		defer wg.Done()
		runConfigWatcher(leaderCtx, clientset, namespace, provider, reconciler, ctrl.ResyncAll)
	}()

	<-leaderCtx.Done()
	wg.Wait()
	ctrl.Wait()
	return nil
}

// runConfigWatcher re-parses greydns-config on every change and swaps
// the reconciler's snapshot. Invalid data (bad TTL, unsupported record
// type) is logged and ignored; the reconciler keeps its previous
// snapshot so a typo in the ConfigMap can't break running reconciles.
func runConfigWatcher(
	ctx context.Context,
	clientset kubernetes.Interface,
	namespace string,
	provider dnsprovider.Provider,
	reconciler *records.Reconciler,
	resync func() error,
) {
	initialApplied := false
	err := cfg.Watch(ctx, clientset, namespace, "greydns-config", func(data map[string]string) {
		next, parseErr := records.ParseSnapshot(data, provider)
		if parseErr != nil {
			log.Error().Err(parseErr).Msg("[Config] Rejecting invalid ConfigMap update; keeping previous snapshot")
			return
		}
		reconciler.UpdateSnapshot(next)
		log.Info().Msg("[Config] Snapshot updated from ConfigMap")
		// Skip the resync on the initial load: the reconciler was just
		// built with the same snapshot and the informer will emit an
		// Add for every Service/Ingress anyway.
		if !initialApplied {
			initialApplied = true
			return
		}
		if resyncErr := resync(); resyncErr != nil {
			log.Error().Err(resyncErr).Msg("[Config] Resync after snapshot swap failed")
		}
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error().Err(err).Msg("[Config] ConfigMap watcher exited with error")
	}
}

// namespaceFromEnv honours the POD_NAMESPACE downward-API env var so
// the lease is created in the pod's own namespace; falls back to
// "default" to match the existing ConfigMap/Secret lookup path.
func namespaceFromEnv() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return defaultNamespace
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
		out[strings.ToLower(z.Name)] = z.ID
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
	callCtx, cancel := dnsprovider.WithCallTimeout(ctx)
	defer cancel()
	out, err := provider.ListOwnedRecords(callCtx, zoneID)
	return out, err
}

func listZones(ctx context.Context, provider dnsprovider.Provider) ([]dnsprovider.Zone, error) {
	var err error
	defer metrics.ObserveProviderCall(provider.Name(), metrics.OpListZones)(&err)
	callCtx, cancel := dnsprovider.WithCallTimeout(ctx)
	defer cancel()
	out, err := provider.ListZones(callCtx)
	return out, err
}
