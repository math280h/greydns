package config

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"

	"github.com/rs/zerolog/log"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// OnChangeFunc is invoked whenever the watched ConfigMap's data map
// changes. It runs synchronously on the informer's event goroutine;
// long-running work inside it blocks subsequent events.
type OnChangeFunc func(data map[string]string)

// Watch starts a ConfigMap informer scoped to a single namespace/name
// via FieldSelector so the in-memory cache holds at most one object.
// The onChange callback fires on every observed change, including the
// initial add once the cache syncs. Blocks until ctx is cancelled.
func Watch(
	ctx context.Context,
	clientset kubernetes.Interface,
	namespace, name string,
	onChange OnChangeFunc,
) error {
	if onChange == nil {
		return errors.New("config: onChange is required")
	}

	factory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		0,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", name).String()
		}),
	)
	informer := factory.Core().V1().ConfigMaps().Informer()

	var previous map[string]string
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			cm, ok := obj.(*v1.ConfigMap)
			if !ok || cm.Name != name {
				return
			}
			previous = cloneData(cm.Data)
			log.Info().Str("configmap", name).Msg("[Config] Initial ConfigMap loaded")
			onChange(cm.Data)
		},
		UpdateFunc: func(_, newObj any) {
			cm, ok := newObj.(*v1.ConfigMap)
			if !ok || cm.Name != name {
				return
			}
			if reflect.DeepEqual(previous, cm.Data) {
				return
			}
			previous = cloneData(cm.Data)
			log.Info().Str("configmap", name).Msg("[Config] ConfigMap changed; applying")
			onChange(cm.Data)
		},
	}
	if _, err := informer.AddEventHandler(handler); err != nil {
		return fmt.Errorf("config: add handler: %w", err)
	}

	factory.Start(ctx.Done())
	for _, synced := range factory.WaitForCacheSync(ctx.Done()) {
		if synced {
			continue
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("config: cache sync cancelled: %w", err)
		}
		return errors.New("config: cache sync failed")
	}

	<-ctx.Done()
	return nil
}

func cloneData(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}
