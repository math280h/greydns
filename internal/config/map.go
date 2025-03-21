package config

import (
	"context"

	"github.com/rs/zerolog/log"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	ConfigMap *v1.ConfigMap
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
	var err error
	ConfigMap, err = clientset.CoreV1().ConfigMaps("default").Get(context.Background(), "greydns-config", metav1.GetOptions{})
	if err != nil {
		log.Fatal().Err(err).Msg("[Config] Failed to get configmap")
	}
}
