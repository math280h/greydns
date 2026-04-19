// Package controller wires a Kubernetes Service informer to a
// records.Reconciler. It sits between cmd/main (which knows about
// k8s config, secrets, and providers) and internal/records (which
// knows about DNS semantics). Extracting the wiring here lets
// integration tests drive the whole informer -> reconciler flow
// against a fake clientset.
package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/math280h/greydns/internal/records"
)

// DefaultResyncPeriod is the informer's resync interval. k8s re-delivers
// Add events at this cadence so the controller recovers if it misses one.
const DefaultResyncPeriod = 30 * time.Second

type Controller struct {
	clientset    kubernetes.Interface
	reconciler   *records.Reconciler
	factory      informers.SharedInformerFactory
	resyncPeriod time.Duration

	ready chan struct{}
}

func New(clientset kubernetes.Interface, reconciler *records.Reconciler) *Controller {
	return &Controller{
		clientset:    clientset,
		reconciler:   reconciler,
		resyncPeriod: DefaultResyncPeriod,
		ready:        make(chan struct{}),
	}
}

// Start registers event handlers and blocks until caches have synced.
// The informer stops when ctx is cancelled.
func (c *Controller) Start(ctx context.Context) error {
	c.factory = informers.NewSharedInformerFactory(c.clientset, c.resyncPeriod)
	serviceInformer := c.factory.Core().V1().Services().Informer()

	if _, err := serviceInformer.AddEventHandler(c.eventHandlers(ctx)); err != nil {
		return fmt.Errorf("controller: add event handler: %w", err)
	}
	c.factory.Start(ctx.Done())
	c.factory.WaitForCacheSync(ctx.Done())
	close(c.ready)
	return nil
}

func (c *Controller) Ready() bool {
	select {
	case <-c.ready:
		return true
	default:
		return false
	}
}

func (c *Controller) eventHandlers(ctx context.Context) cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			service, ok := obj.(*v1.Service)
			if !ok {
				log.Error().Msg("[Core] Failed to cast object")
				return
			}
			c.reconciler.HandleAnnotations(ctx, service)
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
			if !AnnotationsChanged(service, oldService) {
				return
			}
			log.Info().Msgf("[Core] [%s] Annotations changed, updating records", service.Name)
			c.reconciler.HandleUpdates(ctx, service, oldService)
		},
		DeleteFunc: func(obj interface{}) {
			service := extractServiceFromDelete(obj)
			if service == nil {
				return
			}
			c.reconciler.HandleDeletions(ctx, service)
		},
	}
}

// AnnotationsChanged also catches additions and removals, not just
// in-place value changes.
func AnnotationsChanged(service, oldService *v1.Service) bool {
	for _, key := range records.AnnotationKeys {
		if service.Annotations[key] != oldService.Annotations[key] {
			return true
		}
	}
	return false
}

// extractServiceFromDelete also unwraps DeletedFinalStateUnknown
// tombstones, which client-go sends when the informer missed the raw
// delete; ignoring them would leak records.
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
