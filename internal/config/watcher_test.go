package config_test

import (
	"context"
	"errors"
	"maps"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"

	"github.com/math280h/greydns/internal/config"
)

const (
	tNS   = "default"
	tName = "greydns-config"
)

// cmCounter captures onChange invocations so tests can assert on
// timing and payload without racing against the informer goroutine.
type cmCounter struct {
	mu      sync.Mutex
	changes []map[string]string
}

func (c *cmCounter) record(data map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	copied := make(map[string]string, len(data))
	maps.Copy(copied, data)
	c.changes = append(c.changes, copied)
}

func (c *cmCounter) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.changes)
}

func (c *cmCounter) latest() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.changes) == 0 {
		return nil
	}
	return c.changes[len(c.changes)-1]
}

func newCM(data map[string]string) *v1.ConfigMap {
	return &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: tNS, Name: tName},
		Data:       data,
	}
}

// waitFor polls until cond returns true or times out.
func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startWatcher(t *testing.T, cs *kfake.Clientset, counter *cmCounter) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := config.Watch(ctx, cs, tNS, tName, counter.record); err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("Watch exited: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func TestWatch_FiresOnInitialAdd(t *testing.T) {
	cs := kfake.NewSimpleClientset(newCM(map[string]string{"record-ttl": "60"}))
	counter := &cmCounter{}
	startWatcher(t, cs, counter)

	waitFor(t, "initial Add callback", func() bool { return counter.len() >= 1 })
	if got := counter.latest()["record-ttl"]; got != "60" {
		t.Fatalf("initial data ttl = %q, want 60", got)
	}
}

func TestWatch_IgnoresNoopUpdate(t *testing.T) {
	cs := kfake.NewSimpleClientset(newCM(map[string]string{"record-ttl": "60"}))
	counter := &cmCounter{}
	startWatcher(t, cs, counter)
	waitFor(t, "initial Add", func() bool { return counter.len() == 1 })

	// Update with identical data. Should not fire.
	_, err := cs.CoreV1().ConfigMaps(tNS).Update(
		context.Background(),
		newCM(map[string]string{"record-ttl": "60"}),
		metav1.UpdateOptions{},
	)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	if got := counter.len(); got != 1 {
		t.Fatalf("no-op update fired %d callbacks, want 1 (only initial)", got)
	}
}

func TestWatch_FiresOnDataChange(t *testing.T) {
	cs := kfake.NewSimpleClientset(newCM(map[string]string{"record-ttl": "60"}))
	counter := &cmCounter{}
	startWatcher(t, cs, counter)
	waitFor(t, "initial Add", func() bool { return counter.len() == 1 })

	_, err := cs.CoreV1().ConfigMaps(tNS).Update(
		context.Background(),
		newCM(map[string]string{"record-ttl": "900"}),
		metav1.UpdateOptions{},
	)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	waitFor(t, "update callback", func() bool { return counter.len() == 2 })
	if got := counter.latest()["record-ttl"]; got != "900" {
		t.Fatalf("changed data ttl = %q, want 900", got)
	}
}

func TestWatch_CallbackReceivesIsolatedCopy(t *testing.T) {
	// Regression: onChange must receive a copy of the data map, not
	// the shared informer cache map. Mutating what the callback gets
	// must not affect subsequent callback payloads.
	cs := kfake.NewSimpleClientset(newCM(map[string]string{"record-ttl": "60"}))
	counter := &cmCounter{}
	var firstPayload map[string]string
	mu := sync.Mutex{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = config.Watch(ctx, cs, tNS, tName, func(data map[string]string) {
			mu.Lock()
			if firstPayload == nil {
				firstPayload = data
				// Callback mutates what it received; subsequent
				// callbacks must not see this mutation.
				data["record-ttl"] = "MUTATED"
			}
			counter.record(data)
			mu.Unlock()
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	waitFor(t, "initial Add", func() bool { return counter.len() == 1 })

	_, err := cs.CoreV1().ConfigMaps(tNS).Update(
		context.Background(),
		newCM(map[string]string{"record-ttl": "900"}),
		metav1.UpdateOptions{},
	)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	waitFor(t, "second callback", func() bool { return counter.len() == 2 })
	if got := counter.latest()["record-ttl"]; got != "900" {
		t.Fatalf("second callback ttl = %q, want 900 (callback received a fresh copy)", got)
	}
}
