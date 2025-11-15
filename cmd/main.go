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
	"github.com/math280h/greydns/internal/providers"
	"github.com/math280h/greydns/internal/records"
	"github.com/math280h/greydns/internal/types"
	"github.com/math280h/greydns/internal/utils"
)

var (
	ingressDestination string                              //nolint:gochecknoglobals // Required for ingress destination
	zonesToNames       = make(map[string]string)           //nolint:gochecknoglobals // Required for zones
	existingRecords    = make(map[string]*types.DNSRecord) //nolint:gochecknoglobals // Required for existing records
	providerManager    *providers.Manager                  //nolint:gochecknoglobals // Required for provider management
)

func main() { //nolint:gocognit // Required for main function
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr}) //nolint:reassign // Required for logging

	// Create Kubernetes client
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal().Err(err).Msg("[Core] Failed to get cluster config")
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal().Err(err).Msg("[Core] Failed to create clientset")
	}

	cfg.LoadConfigMap(clientset)

	secret, err := clientset.CoreV1().Secrets("default").Get(context.Background(), "greydns-secret", metav1.GetOptions{})
	if err != nil {
		log.Fatal().Err(err).Msg("[Core] Failed to get secret")
	}

	ingressDestination = cfg.GetRequiredConfigValue("ingress-destination")

	utils.StartBroadcaster(
		clientset,
	)

	// Initialize DNS provider
	providerName := "cloudflare" // Default to Cloudflare for backward compatibility
	if configuredProvider, exists := cfg.ConfigMap.Data["provider"]; exists && configuredProvider != "" {
		providerName = configuredProvider
	}

	providerManager, err = providers.NewManager(providerName)
	if err != nil {
		log.Fatal().Err(err).Msg("[Core] Failed to create provider manager")
	}

	// Convert secret to credentials map
	credentials := make(map[string]string)
	for key, value := range secret.Data {
		credentials[key] = string(value)
	}

	err = providerManager.Connect(credentials)
	if err != nil {
		log.Fatal().Err(err).Msg("[Core] Failed to connect to DNS provider")
	}

	zonesToNames, err = providerManager.GetZones()
	if err != nil {
		log.Fatal().Err(err).Msg("[Core] Failed to get zones")
	}

	existingRecords, err = providerManager.RefreshRecordsCache(zonesToNames)
	if err != nil {
		log.Fatal().Err(err).Msg("[Core] Failed to refresh records cache")
	}

	go func() {
		for {
			sleepTime, strconvErr := strconv.ParseInt(cfg.GetRequiredConfigValue("cache-refresh-seconds"), 0, 64)
			if strconvErr != nil {
				log.Fatal().Err(strconvErr).Msg("[Core] Sleep time is not a valid integer")
			}
			time.Sleep(time.Duration(sleepTime) * time.Second)
			refreshedRecords, refreshErr := providerManager.RefreshRecordsCache(zonesToNames)
			if refreshErr != nil {
				log.Error().Err(refreshErr).Msg("[Core] Failed to refresh records cache")
			} else {
				existingRecords = refreshedRecords
			}
		}
	}()

	// Set up informer to watch Service resources
	factory := informers.NewSharedInformerFactory(clientset, 30*time.Second)
	serviceInformer := factory.Core().V1().Services().Informer()

	// Define event handlers
	_, err = serviceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			service, ok := obj.(*v1.Service)
			if !ok {
				log.Error().Msg("[Core] Failed to cast object")
				return
			}
			records.HandleAnnotations(
				providerManager.GetProvider(),
				existingRecords,
				ingressDestination,
				zonesToNames,
				service,
			)
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

			annotationsChanged := false
			for key, value := range service.Annotations {
				if !strings.Contains(key, "greydns.io") {
					continue
				}
				if value != oldService.Annotations[key] {
					annotationsChanged = true
					break
				}
			}

			if annotationsChanged {
				log.Info().Msgf("[Core] [%s] Annotations changed, updating records", service.Name)
				records.HandleUpdates(
					providerManager.GetProvider(),
					existingRecords,
					ingressDestination,
					zonesToNames,
					service,
					oldService,
				)
			}
		},
		DeleteFunc: func(obj interface{}) {
			service, ok := obj.(*v1.Service)
			if !ok {
				log.Error().Msg("[Core] Failed to cast object during delete")
				return
			}
			records.HandleDeletions(
				providerManager.GetProvider(),
				existingRecords,
				zonesToNames,
				service,
			)
		},
	})
	if err != nil {
		log.Fatal().Err(err).Msg("[Core] Failed to add event handler")
		return
	}

	// Start the informer
	stopCh := make(chan struct{})
	defer close(stopCh)
	factory.Start(stopCh)

	// Keep running
	select {}
}
