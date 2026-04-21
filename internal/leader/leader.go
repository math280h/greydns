// Package leader wraps client-go leader election so only one greydns
// pod at a time runs the reconciler. Followers stay up on the same
// health probes and take over on lease expiry, unlocking replicas > 1
// for availability without risk of two pods racing to write DNS.
package leader

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

const (
	DefaultLeaseDuration = 15 * time.Second
	DefaultRenewDeadline = 10 * time.Second
	DefaultRetryPeriod   = 2 * time.Second
	DefaultLeaseName     = "greydns-leader"
)

// Config wires a leader election run. Identity must be unique per pod;
// the pod name (hostname) is the idiomatic choice. OnBecome is invoked
// with a context that cancels when this pod loses leadership, letting
// the caller stop the reconciler cleanly before another pod takes over.
type Config struct {
	Clientset kubernetes.Interface
	Namespace string
	LeaseName string
	Identity  string
	OnBecome  func(ctx context.Context)
}

// Run blocks until ctx cancels. It loops client-go's leader elector so
// a pod that loses its lease rejoins the election on the next iteration
// instead of sitting idle (the default RunOrDie pattern crashes the
// process on loss; we cooperate with graceful shutdown instead). When
// the outer ctx cancels, ReleaseOnCancel drops the lease so a sibling
// can take over immediately instead of waiting LeaseDuration.
func Run(ctx context.Context, cfg Config) error {
	if cfg.OnBecome == nil {
		return errors.New("leader: OnBecome is required")
	}
	if cfg.Identity == "" {
		return errors.New("leader: Identity is required")
	}
	if cfg.Namespace == "" {
		return errors.New("leader: Namespace is required")
	}
	if cfg.LeaseName == "" {
		cfg.LeaseName = DefaultLeaseName
	}

	for ctx.Err() == nil {
		le, err := newElector(cfg)
		if err != nil {
			return err
		}
		le.Run(ctx)
	}
	return nil
}

func newElector(cfg Config) (*leaderelection.LeaderElector, error) {
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      cfg.LeaseName,
			Namespace: cfg.Namespace,
		},
		Client: cfg.Clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: cfg.Identity,
		},
	}
	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   DefaultLeaseDuration,
		RenewDeadline:   DefaultRenewDeadline,
		RetryPeriod:     DefaultRetryPeriod,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaderCtx context.Context) {
				log.Info().Str("identity", cfg.Identity).Msg("[Leader] Acquired leadership")
				cfg.OnBecome(leaderCtx)
			},
			OnStoppedLeading: func() {
				log.Warn().Str("identity", cfg.Identity).Msg("[Leader] Lost leadership")
			},
			OnNewLeader: func(identity string) {
				if identity == cfg.Identity {
					return
				}
				log.Info().Str("leader", identity).Msg("[Leader] Observed new leader")
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("leader: new elector: %w", err)
	}
	return le, nil
}
