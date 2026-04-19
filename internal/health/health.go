// Package health exposes the HTTP liveness and readiness endpoints
// that kubelet calls to probe the controller.
package health

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	DefaultAddr          = ":8080"
	readyShutdownTimeout = 5 * time.Second
)

type ReadyFunc func() bool

type Server struct {
	srv     *http.Server
	ready   ReadyFunc
	healthy atomic.Bool
}

// New builds a Server. ready is invoked from HTTP handler goroutines,
// so it must be safe for concurrent calls.
func New(addr string, ready ReadyFunc) *Server {
	s := &Server{ready: ready}
	s.healthy.Store(true)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)

	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// Run blocks until ctx is cancelled or the server errors. On
// cancellation both probes flip to 503 so kubelet stops routing traffic
// while in-flight requests drain.
func (s *Server) Run(ctx context.Context) error {
	lis, err := net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return err
	}
	log.Info().Str("addr", lis.Addr().String()).Msg("[Health] HTTP server listening")

	errCh := make(chan error, 1)
	go func() { errCh <- s.srv.Serve(lis) }()

	select {
	case <-ctx.Done():
		s.healthy.Store(false)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), readyShutdownTimeout)
		defer cancel()
		return s.srv.Shutdown(shutdownCtx)
	case serveErr := <-errCh:
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return serveErr
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	if !s.healthy.Load() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !s.healthy.Load() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	if !s.ready() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}
