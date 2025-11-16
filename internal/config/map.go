package config

import (
	"context"
	"os"

	"github.com/rs/zerolog/log"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	ConfigMap *v1.ConfigMap //nolint:gochecknoglobals // Required for configmap
)

func GetRequiredConfigValue(key string) string {
	value, ok := ConfigMap.Data[key]
	if !ok {
		log.Fatal().Msgf("[Config] Required key %s does not exist in configmap", key)
	}

	return value
}

func LoadConfigMap(
	clientset *kubernetes.Clientset,
) {
	namespace := os.Getenv("GREYDNS_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}

	var err error
	ConfigMap, err = clientset.CoreV1().ConfigMaps(
		namespace,
	).Get(context.Background(), "greydns-config", metav1.GetOptions{})
	if err != nil {
		log.Fatal().Err(err).Msg("[Config] Failed to get configmap")
	}
}
