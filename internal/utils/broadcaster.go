package utils

import (
	"github.com/rs/zerolog/log"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
)

var (
	Recorder record.EventRecorder //nolint:gochecknoglobals // Required for event recording
)

func StartBroadcaster(
	clientset *kubernetes.Clientset,
) {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(log.Info().Msgf)

	eventBroadcaster.StartRecordingToSink(&typedv1.EventSinkImpl{
		Interface: clientset.CoreV1().Events(""),
	})

	Recorder = eventBroadcaster.NewRecorder(
		scheme.Scheme,
		v1.EventSource{Component: "greydns-controller"},
	)
}
