package health_test

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/math280h/greydns/internal/health"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	lis.Close()
	return addr
}

var testClient = &http.Client{Timeout: 500 * time.Millisecond} //nolint:gochecknoglobals // test-only

func waitForServer(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := testClient.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server never came up at %s", url)
}

func doGet(t *testing.T, url string) int {
	t.Helper()
	resp, err := testClient.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestHealthz_AlwaysOKWhileRunning(t *testing.T) {
	addr := freeAddr(t)
	srv := health.New(addr, func() bool { return false })

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-errCh
	})

	waitForServer(t, "http://"+addr+"/healthz")
	if got := doGet(t, "http://"+addr+"/healthz"); got != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200 while the server is running", got)
	}
}

func TestReadyz_ReflectsReadyFunc(t *testing.T) {
	addr := freeAddr(t)
	var ready atomic.Bool
	srv := health.New(addr, ready.Load)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-errCh
	})

	waitForServer(t, "http://"+addr+"/healthz")

	if got := doGet(t, "http://"+addr+"/readyz"); got != http.StatusServiceUnavailable {
		t.Fatalf("/readyz while not ready = %d, want 503", got)
	}

	ready.Store(true)
	if got := doGet(t, "http://"+addr+"/readyz"); got != http.StatusOK {
		t.Fatalf("/readyz after ready = %d, want 200", got)
	}
}

func TestRun_ShutsDownCleanlyOnContextCancel(t *testing.T) {
	addr := freeAddr(t)
	srv := health.New(addr, func() bool { return true })

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()

	waitForServer(t, "http://"+addr+"/healthz")

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error on shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
