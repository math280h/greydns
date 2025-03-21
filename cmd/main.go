package main

import (
	"context"
	"os"
	"strconv"
	"time"

	"github.com/cloudflare/cloudflare-go/v4/dns"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	cfg "github.com/math280h/greydns/internal/config"
	cf "github.com/math280h/greydns/internal/providers/cf"
	"github.com/math280h/greydns/internal/records"
	"github.com/math280h/greydns/internal/utils"
)

var (
	ingressDestination string                                //nolint:gochecknoglobals // Required for ingress destination
	zonesToNames       = make(map[string]string)             //nolint:gochecknoglobals // Required for zones
	existingRecords    = make(map[string]dns.RecordResponse) //nolint:gochecknoglobals // Required for existing records
)

func main() {
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

	// TODO:: Support multiple providers
	cf.Connect(secret)
	zonesToNames = cf.GetZoneNames()
	existingRecords = cf.RefreshRecordsCache(
		zonesToNames,
	)
	go func() {
		for {
			sleepTime, strconvErr := strconv.ParseInt(cfg.GetRequiredConfigValue("cache-refresh-seconds"), 0, 64)
			if strconvErr != nil {
				log.Fatal().Err(strconvErr).Msg("[Core] Sleep time is not a valid integer")
			}
			time.Sleep(time.Duration(sleepTime) * time.Second)
			existingRecords = cf.RefreshRecordsCache(
				zonesToNames,
			)
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
				existingRecords,
				ingressDestination,
				zonesToNames,
				service,
			)
		},
		UpdateFunc: func(_, newObj interface{}) {
			service, ok := newObj.(*v1.Service)
			if !ok {
				log.Error().Msg("[Core] Failed to cast object during update")
				return
			}
			records.HandleAnnotations(
				existingRecords,
				ingressDestination,
				zonesToNames,
				service,
			)
		},
		DeleteFunc: func(obj interface{}) {
			service, ok := obj.(*v1.Service)
			if !ok {
				log.Error().Msg("[Core] Failed to cast object during delete")
				return
			}
			records.HandleDeletions(
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
