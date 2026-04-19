package main

import (
	"context"
	"os"
	"strconv"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	cfg "github.com/math280h/greydns/internal/config"
	"github.com/math280h/greydns/internal/dnsprovider"
	"github.com/math280h/greydns/internal/dnsprovider/registry"
	_ "github.com/math280h/greydns/internal/providers" // registers all DNS provider factories
	"github.com/math280h/greydns/internal/records"
	"github.com/math280h/greydns/internal/utils"
)

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr}) //nolint:reassign // Required for logging

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal().Err(err).Msg("[Core] Failed to get cluster config")
	}

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		log.Fatal().Err(err).Msg("[Core] Failed to create clientset")
	}

	cfg.LoadConfigMap(clientset)

	secret, err := clientset.CoreV1().Secrets("default").Get(context.Background(), "greydns-secret", metav1.GetOptions{})
	if err != nil {
		log.Fatal().Err(err).Msg("[Core] Failed to get secret")
	}

	utils.StartBroadcaster(clientset)

	providerName := cfg.ProviderName()
	provider, err := registry.Build(
		providerName,
		registry.NewProviderConfig(providerName, cfg.Data()),
		secret.Data,
	)
	if err != nil {
		log.Fatal().Err(err).Str("provider", providerName).Msg("[Core] Failed to build provider")
	}
	log.Info().Str("provider", provider.Name()).Msg("[Core] DNS provider ready")

	ttl := mustAtoi(cfg.GetRequiredConfigValue("record-ttl"), "record-ttl")
	refreshSeconds := mustAtoi(cfg.GetRequiredConfigValue("cache-refresh-seconds"), "cache-refresh-seconds")
	if refreshSeconds <= 0 {
		log.Fatal().
			Int("cache-refresh-seconds", refreshSeconds).
			Msg("[Core] cache-refresh-seconds must be greater than 0")
	}
	refreshInterval := time.Duration(refreshSeconds) * time.Second

	recordType := dnsprovider.RecordType(cfg.GetRequiredConfigValue("record-type"))
	if typeErr := dnsprovider.ValidateRecordType(recordType, provider.SupportedRecordTypes()); typeErr != nil {
		log.Fatal().Err(typeErr).Str("provider", provider.Name()).Msg("[Core] record-type rejected by provider")
	}

	ctx := context.Background()
	zoneNameToID := mustListZones(ctx, provider)

	initialCache, refreshErr := refreshCache(ctx, provider, zoneNameToID)
	if refreshErr != nil {
		log.Fatal().Err(refreshErr).Msg("[Core] Initial cache refresh failed")
	}

	reconciler := records.NewReconciler(
		provider,
		zoneNameToID,
		cfg.GetRequiredConfigValue("ingress-destination"),
		ttl,
		recordType,
	)
	reconciler.ReplaceCache(initialCache)

	go runRefreshLoop(ctx, provider, zoneNameToID, reconciler, refreshInterval)

	factory := informers.NewSharedInformerFactory(clientset, 30*time.Second)
	serviceInformer := factory.Core().V1().Services().Informer()

	if _, evtErr := serviceInformer.AddEventHandler(serviceEventHandlers(ctx, reconciler)); evtErr != nil {
		log.Fatal().Err(evtErr).Msg("[Core] Failed to add event handler")
	}

	stopCh := make(chan struct{})
	defer close(stopCh)
	factory.Start(stopCh)

	select {}
}

func runRefreshLoop(
	ctx context.Context,
	provider dnsprovider.Provider,
	zoneNameToID map[string]string,
	reconciler *records.Reconciler,
	interval time.Duration,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		// Capture the mutation generation before the network round-trip
		// so any handler writes that land while refresh is in flight
		// will be detected and keep us from clobbering them.
		gen := reconciler.WriteGen()
		next, err := refreshCache(ctx, provider, zoneNameToID)
		if err != nil {
			log.Error().Err(err).Msg("[Core] Cache refresh failed; keeping previous cache")
			continue
		}
		if !reconciler.ReplaceCacheIfUnchanged(next, gen) {
			log.Info().Msg("[Core] Cache mutated during refresh; skipping this cycle")
		}
	}
}

// extractServiceFromDelete unwraps a DeleteFunc argument. client-go
// delivers either a *v1.Service or a cache.DeletedFinalStateUnknown
// tombstone when the informer missed the delete event; both must be
// cleaned up or owned DNS records leak.
func extractServiceFromDelete(obj interface{}) *v1.Service {
	if svc, ok := obj.(*v1.Service); ok {
		return svc
	}
	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		log.Error().Msg("[Core] Failed to cast object during delete")
		return nil
	}
	svc, ok := tombstone.Obj.(*v1.Service)
	if !ok {
		log.Error().Msg("[Core] Tombstone during delete did not contain a Service")
		return nil
	}
	return svc
}

func serviceEventHandlers(ctx context.Context, reconciler *records.Reconciler) cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			service, ok := obj.(*v1.Service)
			if !ok {
				log.Error().Msg("[Core] Failed to cast object")
				return
			}
			reconciler.HandleAnnotations(ctx, service)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			service, ok := newObj.(*v1.Service)
			if !ok {
				log.Error().Msg("[Core] Failed to cast object during update")
				return
			}
			oldService, ok := oldObj.(*v1.Service)
			if !ok {
				log.Error().Msg("[Core] Failed to cast old object during update")
				return
			}
			if !greydnsAnnotationsChanged(service, oldService) {
				return
			}
			log.Info().Msgf("[Core] [%s] Annotations changed, updating records", service.Name)
			reconciler.HandleUpdates(ctx, service, oldService)
		},
		DeleteFunc: func(obj interface{}) {
			service := extractServiceFromDelete(obj)
			if service == nil {
				return
			}
			reconciler.HandleDeletions(ctx, service)
		},
	}
}

// greydnsAnnotationsChanged returns true when any greydns.io/* key
// differs between the two Services, including additions (present only
// on new) and removals (present only on old).
func greydnsAnnotationsChanged(service, oldService *v1.Service) bool {
	for _, key := range records.AnnotationKeys {
		if service.Annotations[key] != oldService.Annotations[key] {
			return true
		}
	}
	return false
}

func mustAtoi(raw, name string) int {
	v, err := strconv.Atoi(raw)
	if err != nil {
		log.Fatal().Err(err).Str("key", name).Msg("[Core] Config value is not a valid integer")
	}
	return v
}

func mustListZones(ctx context.Context, provider dnsprovider.Provider) map[string]string {
	zones, err := provider.ListZones(ctx)
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

// refreshCache rebuilds the cache snapshot by listing owned records
// across every managed zone. It returns an error if any zone fails to
// list: the caller must then keep the previous cache rather than swap
// in a partial view, which would cause the controller to "forget"
// records and incorrectly try to recreate or leak them.
func refreshCache(
	ctx context.Context,
	provider dnsprovider.Provider,
	zones map[string]string,
) (map[records.CacheKey]dnsprovider.Record, error) {
	out := records.NewCacheSnapshot()
	total := 0
	for _, id := range zones {
		recs, err := provider.ListOwnedRecords(ctx, id)
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
	log.Info().Msgf("[Core] Refresh found %d records", total)
	return out, nil
}
