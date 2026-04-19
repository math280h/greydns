// Package controller wires a Kubernetes Service informer to a
// records.Reconciler via a rate-limited workqueue.
package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/math280h/greydns/internal/metrics"
	"github.com/math280h/greydns/internal/records"
)

const (
	DefaultResyncPeriod = 30 * time.Second
	DefaultWorkerCount  = 2
)

type Controller struct {
	clientset    kubernetes.Interface
	reconciler   *records.Reconciler
	factory      informers.SharedInformerFactory
	lister       listersv1.ServiceLister
	queue        workqueue.TypedRateLimitingInterface[string]
	resyncPeriod time.Duration
	workers      int

	readyOnce sync.Once
	ready     chan struct{}
}

func New(clientset kubernetes.Interface, reconciler *records.Reconciler) *Controller {
	return &Controller{
		clientset:    clientset,
		reconciler:   reconciler,
		resyncPeriod: DefaultResyncPeriod,
		workers:      DefaultWorkerCount,
		ready:        make(chan struct{}),
	}
}

// Start returns once informer caches are synced. Workers keep running
// in background goroutines and stop when ctx is cancelled.
func (c *Controller) Start(ctx context.Context) error {
	c.queue = workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[string](),
	)
	c.factory = informers.NewSharedInformerFactory(c.clientset, c.resyncPeriod)
	serviceInformer := c.factory.Core().V1().Services()
	c.lister = serviceInformer.Lister()

	if _, err := serviceInformer.Informer().AddEventHandler(c.eventHandlers()); err != nil {
		return fmt.Errorf("controller: add event handler: %w", err)
	}
	c.factory.Start(ctx.Done())
	for _, ok := range c.factory.WaitForCacheSync(ctx.Done()) {
		if ok {
			continue
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("controller: cache sync cancelled: %w", err)
		}
		return errors.New("controller: cache sync failed")
	}

	var wg sync.WaitGroup
	for range c.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.runWorker(ctx)
		}()
	}

	go func() {
		<-ctx.Done()
		c.queue.ShutDown()
	}()
	go func() {
		wg.Wait()
		log.Info().Msg("[Core] Controller workers drained")
	}()

	c.readyOnce.Do(func() { close(c.ready) })
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

func (c *Controller) eventHandlers() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			c.enqueueService(obj)
		},
		UpdateFunc: func(oldObj, newObj any) {
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
			c.enqueueKey(service.Namespace, service.Name)
		},
		DeleteFunc: func(obj any) {
			service := extractServiceFromDelete(obj)
			if service == nil {
				return
			}
			c.enqueueKey(service.Namespace, service.Name)
		},
	}
}

func (c *Controller) enqueueService(obj any) {
	service, ok := obj.(*v1.Service)
	if !ok {
		log.Error().Msg("[Core] Failed to cast object")
		return
	}
	c.enqueueKey(service.Namespace, service.Name)
}

func (c *Controller) enqueueKey(namespace, name string) {
	key := namespace + "/" + name
	c.queue.Add(key)
	metrics.WorkqueueAdds.Inc()
	metrics.WorkqueueDepth.Set(float64(c.queue.Len()))
}

func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextItem(ctx) {
	}
}

func (c *Controller) processNextItem(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)
	defer metrics.WorkqueueDepth.Set(float64(c.queue.Len()))

	if err := c.reconcileKey(ctx, key); err != nil {
		metrics.WorkqueueRetries.Inc()
		log.Error().Err(err).Str("key", key).Msg("[Core] Reconcile failed; requeuing")
		c.queue.AddRateLimited(key)
		return true
	}
	c.queue.Forget(key)
	return true
}

func (c *Controller) reconcileKey(ctx context.Context, key string) error {
	namespace, name, ok := strings.Cut(key, "/")
	if !ok {
		return fmt.Errorf("controller: malformed workqueue key %q", key)
	}
	svc, err := c.lister.Services(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		return c.reconciler.Cleanup(ctx, &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		})
	}
	if err != nil {
		return fmt.Errorf("controller: lookup %s: %w", key, err)
	}
	return c.reconciler.Reconcile(ctx, svc)
}

// AnnotationsChanged catches additions and removals too, not just
// in-place edits.
func AnnotationsChanged(service, oldService *v1.Service) bool {
	for k, v := range service.Annotations {
		if !strings.HasPrefix(k, records.AnnotationPrefix) {
			continue
		}
		if v != oldService.Annotations[k] {
			return true
		}
	}
	for k, v := range oldService.Annotations {
		if !strings.HasPrefix(k, records.AnnotationPrefix) {
			continue
		}
		if _, stillPresent := service.Annotations[k]; !stillPresent && v != "" {
			return true
		}
	}
	return false
}

// extractServiceFromDelete also unwraps DeletedFinalStateUnknown
// tombstones, which client-go sends when the informer missed the
// raw delete.
func extractServiceFromDelete(obj any) *v1.Service {
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
