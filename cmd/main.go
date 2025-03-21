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
)

var (
	ingressDestination string
	zonesToNames       = make(map[string]string)
	existingRecords    = make(map[string]dns.RecordResponse) // Move to something like sqlite for cache
)

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

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

	// TODO:: Support multiple providers
	cf.Connect(secret)
	zonesToNames = cf.GetZoneNames()
	existingRecords = cf.RefreshRecordsCache(
		zonesToNames,
	)
	go func() {
		for {
			sleepTime, err := strconv.ParseInt(cfg.GetRequiredConfigValue("cache-refresh-seconds"), 0, 64)
			if err != nil {
				log.Fatal().Err(err).Msg("[Core] Sleep time is not a valid integer")
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
	serviceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
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
		UpdateFunc: func(oldObj, newObj interface{}) {
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
	})

	// Start the informer
	stopCh := make(chan struct{})
	defer close(stopCh)
	factory.Start(stopCh)

	// Keep running
	select {}
}
