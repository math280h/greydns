// Package controller wires Kubernetes Service and Ingress informers to
// a records.Reconciler via a rate-limited workqueue.
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
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listersv1 "k8s.io/client-go/listers/core/v1"
	networkinglistersv1 "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/math280h/greydns/internal/dnsprovider"
	"github.com/math280h/greydns/internal/metrics"
	"github.com/math280h/greydns/internal/records"
)

const (
	DefaultResyncPeriod = 30 * time.Second
	DefaultWorkerCount  = 2
)

type Controller struct {
	clientset     kubernetes.Interface
	reconciler    *records.Reconciler
	factory       informers.SharedInformerFactory
	serviceLister listersv1.ServiceLister
	ingressLister networkinglistersv1.IngressLister
	queue         workqueue.TypedRateLimitingInterface[string]
	resyncPeriod  time.Duration
	workers       int

	readyOnce sync.Once
	ready     chan struct{}
	workersWG sync.WaitGroup
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
	c.serviceLister = serviceInformer.Lister()
	ingressInformer := c.factory.Networking().V1().Ingresses()
	c.ingressLister = ingressInformer.Lister()

	if _, err := serviceInformer.Informer().AddEventHandler(c.serviceEventHandlers()); err != nil {
		return fmt.Errorf("controller: add service event handler: %w", err)
	}
	if _, err := ingressInformer.Informer().AddEventHandler(c.ingressEventHandlers()); err != nil {
		return fmt.Errorf("controller: add ingress event handler: %w", err)
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

	for range c.workers {
		c.workersWG.Add(1)
		go func() {
			defer c.workersWG.Done()
			c.runWorker(ctx)
		}()
	}

	go func() {
		<-ctx.Done()
		c.queue.ShutDown()
	}()

	c.readyOnce.Do(func() { close(c.ready) })
	return nil
}

// Wait blocks until all workers exit. Call after ctx cancel if the
// caller needs to tear down shared state workers still touch.
func (c *Controller) Wait() {
	c.workersWG.Wait()
}

func (c *Controller) Ready() bool {
	select {
	case <-c.ready:
		return true
	default:
		return false
	}
}

func (c *Controller) serviceEventHandlers() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			svc, ok := obj.(*v1.Service)
			if !ok {
				log.Error().Msg("[Core] Failed to cast Service on add")
				return
			}
			c.enqueueKey(dnsprovider.KindService, svc.Namespace, svc.Name)
		},
		UpdateFunc: func(oldObj, newObj any) {
			service, ok := newObj.(*v1.Service)
			if !ok {
				log.Error().Msg("[Core] Failed to cast Service during update")
				return
			}
			oldService, ok := oldObj.(*v1.Service)
			if !ok {
				log.Error().Msg("[Core] Failed to cast old Service during update")
				return
			}
			if !AnnotationsChanged(service.Annotations, oldService.Annotations) {
				return
			}
			c.enqueueKey(dnsprovider.KindService, service.Namespace, service.Name)
		},
		DeleteFunc: func(obj any) {
			service := extractServiceFromDelete(obj)
			if service == nil {
				return
			}
			c.enqueueKey(dnsprovider.KindService, service.Namespace, service.Name)
		},
	}
}

func (c *Controller) ingressEventHandlers() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			ing, ok := obj.(*networkingv1.Ingress)
			if !ok {
				log.Error().Msg("[Core] Failed to cast Ingress on add")
				return
			}
			c.enqueueKey(dnsprovider.KindIngress, ing.Namespace, ing.Name)
		},
		UpdateFunc: func(oldObj, newObj any) {
			ing, ok := newObj.(*networkingv1.Ingress)
			if !ok {
				log.Error().Msg("[Core] Failed to cast Ingress during update")
				return
			}
			oldIng, ok := oldObj.(*networkingv1.Ingress)
			if !ok {
				log.Error().Msg("[Core] Failed to cast old Ingress during update")
				return
			}
			if !ingressChanged(ing, oldIng) {
				return
			}
			c.enqueueKey(dnsprovider.KindIngress, ing.Namespace, ing.Name)
		},
		DeleteFunc: func(obj any) {
			ing := extractIngressFromDelete(obj)
			if ing == nil {
				return
			}
			c.enqueueKey(dnsprovider.KindIngress, ing.Namespace, ing.Name)
		},
	}
}

func (c *Controller) enqueueKey(kind dnsprovider.Kind, namespace, name string) {
	key := string(kind) + "/" + namespace + "/" + name
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

	err := c.reconcileKey(ctx, key)
	switch {
	case err == nil:
		c.queue.Forget(key)
	case errors.Is(err, records.ErrReconcileIncomplete):
		// Expected steady state: contention or a peer-owned domain.
		// Requeue quietly; the reconcile counter already captures
		// the granular outcome.
		log.Info().Err(err).Str("key", key).Msg("[Core] Reconcile incomplete; requeuing")
		c.requeue(key)
	default:
		metrics.WorkqueueRetries.Inc()
		log.Error().Err(err).Str("key", key).Msg("[Core] Reconcile failed; requeuing")
		c.requeue(key)
	}
	return true
}

func (c *Controller) requeue(key string) {
	c.queue.AddRateLimited(key)
	metrics.WorkqueueAdds.Inc()
	metrics.WorkqueueDepth.Set(float64(c.queue.Len()))
}

func (c *Controller) reconcileKey(ctx context.Context, key string) error {
	kind, namespace, name, ok := splitKey(key)
	if !ok {
		return fmt.Errorf("controller: malformed workqueue key %q", key)
	}
	switch kind {
	case dnsprovider.KindService:
		return c.reconcileService(ctx, namespace, name)
	case dnsprovider.KindIngress:
		return c.reconcileIngress(ctx, namespace, name)
	default:
		return fmt.Errorf("controller: unknown kind %q in key %q", kind, key)
	}
}

func (c *Controller) reconcileService(ctx context.Context, namespace, name string) error {
	svc, err := c.serviceLister.Services(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		return c.reconciler.Cleanup(ctx, &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		})
	}
	if err != nil {
		return fmt.Errorf("controller: lookup service %s/%s: %w", namespace, name, err)
	}
	return c.reconciler.Reconcile(ctx, svc)
}

func (c *Controller) reconcileIngress(ctx context.Context, namespace, name string) error {
	ing, err := c.ingressLister.Ingresses(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		return c.reconciler.CleanupIngress(ctx, &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		})
	}
	if err != nil {
		return fmt.Errorf("controller: lookup ingress %s/%s: %w", namespace, name, err)
	}
	return c.reconciler.ReconcileIngress(ctx, ing)
}

// splitKey parses a "<kind>/<namespace>/<name>" workqueue key.
func splitKey(key string) (dnsprovider.Kind, string, string, bool) {
	kindStr, rest, ok := strings.Cut(key, "/")
	if !ok {
		return "", "", "", false
	}
	namespace, name, ok := strings.Cut(rest, "/")
	if !ok || namespace == "" || name == "" {
		return "", "", "", false
	}
	return dnsprovider.Kind(kindStr), namespace, name, true
}

// AnnotationsChanged catches additions and removals of greydns
// annotations, not just in-place edits. It's shared by the Service and
// Ingress handlers because the greydns annotation semantics are
// identical across kinds.
func AnnotationsChanged(newAnns, oldAnns map[string]string) bool {
	for k, v := range newAnns {
		if !strings.HasPrefix(k, records.AnnotationPrefix) {
			continue
		}
		if v != oldAnns[k] {
			return true
		}
	}
	for k, v := range oldAnns {
		if !strings.HasPrefix(k, records.AnnotationPrefix) {
			continue
		}
		if _, stillPresent := newAnns[k]; !stillPresent && v != "" {
			return true
		}
	}
	return false
}

// ingressChanged triggers a requeue on annotation changes or when the
// set of spec.rules[].host values shifts, since the Ingress host list
// drives the desired domain set.
func ingressChanged(newIng, oldIng *networkingv1.Ingress) bool {
	if AnnotationsChanged(newIng.Annotations, oldIng.Annotations) {
		return true
	}
	return !sameHosts(newIng.Spec.Rules, oldIng.Spec.Rules)
}

func sameHosts(a, b []networkingv1.IngressRule) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Host != b[i].Host {
			return false
		}
	}
	return true
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

func extractIngressFromDelete(obj any) *networkingv1.Ingress {
	if ing, ok := obj.(*networkingv1.Ingress); ok {
		return ing
	}
	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		log.Error().Msg("[Core] Failed to cast object during ingress delete")
		return nil
	}
	ing, ok := tombstone.Obj.(*networkingv1.Ingress)
	if !ok {
		log.Error().Msg("[Core] Tombstone during delete did not contain an Ingress")
		return nil
	}
	return ing
}
