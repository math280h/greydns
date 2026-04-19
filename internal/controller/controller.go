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

// DefaultResyncPeriod is the informer's resync interval. k8s clients
// re-deliver Add events at this cadence so the controller can recover
// if it ever misses one.
const DefaultResyncPeriod = 30 * time.Second

// Controller subscribes to Service events and dispatches them to a
// records.Reconciler. It is not safe for concurrent Start calls.
type Controller struct {
	clientset    kubernetes.Interface
	reconciler   *records.Reconciler
	factory      informers.SharedInformerFactory
	resyncPeriod time.Duration
}

// New returns a Controller that watches every namespace.
func New(clientset kubernetes.Interface, reconciler *records.Reconciler) *Controller {
	return &Controller{
		clientset:    clientset,
		reconciler:   reconciler,
		resyncPeriod: DefaultResyncPeriod,
	}
}

// Start registers the Service event handlers, starts the informer
// factory, and blocks until caches have synced. Events dispatched
// after Start returns run on the informer goroutine. stopCh halts
// the informer when closed.
func (c *Controller) Start(ctx context.Context, stopCh <-chan struct{}) error {
	c.factory = informers.NewSharedInformerFactory(c.clientset, c.resyncPeriod)
	serviceInformer := c.factory.Core().V1().Services().Informer()

	if _, err := serviceInformer.AddEventHandler(c.eventHandlers(ctx)); err != nil {
		return fmt.Errorf("controller: add event handler: %w", err)
	}
	c.factory.Start(stopCh)
	c.factory.WaitForCacheSync(stopCh)
	return nil
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

// AnnotationsChanged returns true when any greydns.io/* key differs
// between the two Services, including additions (present only on new)
// and removals (present only on old).
func AnnotationsChanged(service, oldService *v1.Service) bool {
	for _, key := range records.AnnotationKeys {
		if service.Annotations[key] != oldService.Annotations[key] {
			return true
		}
	}
	return false
}

// extractServiceFromDelete unwraps a DeleteFunc argument. client-go
// delivers either a *v1.Service or a cache.DeletedFinalStateUnknown
// tombstone when the informer missed the delete event; both must be
// handled or owned DNS records leak.
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
