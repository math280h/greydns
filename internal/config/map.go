package config

import (
	"context"

	"github.com/rs/zerolog/log"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	ConfigMap *v1.ConfigMap //nolint:gochecknoglobals // Required for configmap
)

// GetRequiredConfigValue returns the value for key or fatally exits if the
// key is not set. Use for generic (top-level) settings. Provider-specific
// settings are namespaced and read via registry.ProviderConfig.
func GetRequiredConfigValue(key string) string {
	value, ok := ConfigMap.Data[key]
	if !ok {
		log.Fatal().Msgf("[Config] Required key %s does not exist in configmap", key)
	}

	return value
}

// GetOptionalConfigValue returns the value for key or defaultValue when unset.
func GetOptionalConfigValue(key, defaultValue string) string {
	if v, ok := ConfigMap.Data[key]; ok {
		return v
	}
	return defaultValue
}

// ProviderName returns the active DNS provider name (e.g. "cloudflare").
// Fatals if the "provider" key is missing; greydns cannot start without one.
func ProviderName() string {
	return GetRequiredConfigValue("provider")
}

// Data exposes the raw ConfigMap data map so the registry can hand it off
// to provider factories without coupling them to the k8s ConfigMap type.
func Data() map[string]string {
	if ConfigMap == nil {
		return nil
	}
	return ConfigMap.Data
}

func LoadConfigMap(
	clientset *kubernetes.Clientset,
	namespace string,
) {
	var err error
	ConfigMap, err = clientset.CoreV1().ConfigMaps(
		namespace,
	).Get(context.Background(), "greydns-config", metav1.GetOptions{})
	if err != nil {
		log.Fatal().Err(err).Msg("[Config] Failed to get configmap")
	}
}
