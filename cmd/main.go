package main

import (
	"context"
	"os"
	"strconv"
	"strings"
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
	refreshInterval := time.Duration(
		mustAtoi(cfg.GetRequiredConfigValue("cache-refresh-seconds"), "cache-refresh-seconds"),
	) * time.Second
	recordType := dnsprovider.RecordType(cfg.GetRequiredConfigValue("record-type"))
	if typeErr := dnsprovider.ValidateRecordType(recordType, provider.SupportedRecordTypes()); typeErr != nil {
		log.Fatal().Err(typeErr).Str("provider", provider.Name()).Msg("[Core] record-type rejected by provider")
	}

	ctx := context.Background()
	zonesToNames := mustListZones(ctx, provider)

	reconciler := &records.Reconciler{
		Provider:           provider,
		Cache:              refreshCache(ctx, provider, zonesToNames),
		ZonesToNames:       zonesToNames,
		IngressDestination: cfg.GetRequiredConfigValue("ingress-destination"),
		RecordTTL:          ttl,
		RecordType:         recordType,
	}

	go func() {
		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()
		for range ticker.C {
			reconciler.Cache = refreshCache(ctx, provider, zonesToNames)
		}
	}()

	factory := informers.NewSharedInformerFactory(clientset, 30*time.Second)
	serviceInformer := factory.Core().V1().Services().Informer()

	_, err = serviceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
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
			service, ok := obj.(*v1.Service)
			if !ok {
				log.Error().Msg("[Core] Failed to cast object during delete")
				return
			}
			reconciler.HandleDeletions(ctx, service)
		},
	})
	if err != nil {
		log.Fatal().Err(err).Msg("[Core] Failed to add event handler")
		return
	}

	stopCh := make(chan struct{})
	defer close(stopCh)
	factory.Start(stopCh)

	select {}
}

func greydnsAnnotationsChanged(service, oldService *v1.Service) bool {
	for key, value := range service.Annotations {
		if !strings.HasPrefix(key, "greydns.io/") {
			continue
		}
		if value != oldService.Annotations[key] {
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

func refreshCache(
	ctx context.Context,
	provider dnsprovider.Provider,
	zones map[string]string,
) map[string]dnsprovider.Record {
	out := make(map[string]dnsprovider.Record)
	for _, id := range zones {
		recs, err := provider.ListOwnedRecords(ctx, id)
		if err != nil {
			log.Error().Err(err).Str("zone", id).Msg("[Core] Failed to list records")
			continue
		}
		for _, r := range recs {
			out[r.Name] = r
			log.Debug().Msgf("[Core] Refresh found record: %s (ID: %s)", r.Name, r.ID)
		}
	}
	log.Info().Msgf("[Core] Refresh found %d records", len(out))
	return out
}
