// Package health exposes an HTTP health/readiness server for k8s
// livenessProbe / readinessProbe probes and Docker HEALTHCHECK.
//
// The server is intentionally minimal (stdlib net/http only) — no
// framework, no metrics endpoint, no auth. It binds to a single
// port (config: health.listen_addr) and answers:
//
//	GET /healthz  — 200 if the process is up. Always 200 once
//	                Serve() has returned successfully.
//	GET /readyz   — 200 if at least one configured messaging
//	                client is reporting a healthy transport
//	                state. 503 otherwise.
//	GET /health   — same payload as /readyz, but a single endpoint
//	                returning a structured JSON body. Handy for
//	                human inspection.
//
// Clients self-report their health via the HealthReporter
// interface (see internal/messaging.Client).
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// Status is the health state reported by a transport client.
// It is intentionally a string enum (not a bool) so future states
// such as "degraded" or "auth_expired" can be added without breaking
// existing probe consumers.
type Status string

const (
	StatusUnknown   Status = "unknown"
	StatusStarting  Status = "starting"
	StatusConnected Status = "connected"
	StatusReconnect Status = "reconnecting"
	StatusStopped   Status = "stopped"
)

// Report is the per-client health snapshot.
type Report struct {
	Name      Status `json:"name"`
	State     Status `json:"state"`
	StartedAt string `json:"started_at,omitempty"`
	Message   string `json:"message,omitempty"`
}

// Reporter is what the HTTP handler asks for the current
// state of every transport. The bot wires a Reporter that
// snapshots the registered messaging.Client slice on each
// call (cheap, called only on probe).
type Reporter interface {
	Reports() []Report
}

// Server is the HTTP health server. Construct via New and call
// Start (in a goroutine) followed by Shutdown on context cancel.
type Server struct {
	addr     string
	reporter Reporter
	srv      *http.Server
	mu       sync.Mutex
	ready    bool
}

// New constructs a Server bound to addr (e.g. ":8080").
// Pass a nil reporter to make /readyz return 503 with no per-client
// detail (useful in tests or before clients are wired).
func New(addr string, r Reporter) *Server {
	s := &Server{addr: addr, reporter: r}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleLive)
	mux.HandleFunc("/readyz", s.handleReady)
	mux.HandleFunc("/health", s.handleHealth)
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	return s
}

// Addr returns the bound address. Useful when the server was
// configured with ":0" for ephemeral ports.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil || s.srv.Addr == "" {
		return s.addr
	}

	return s.srv.Addr
}

// MarkReady is called by main once the bot has at least one
// transport wired. Until then /readyz returns 503 — that lets
// k8s delay traffic to a half-initialised container.
func (s *Server) MarkReady() {
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
}

// Start binds the listener and serves in a background goroutine.
// The returned error is non-nil only if the bind itself fails
// (e.g. port in use); runtime errors are logged and the server
// stops accepting connections. Always pair with Shutdown.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr) //nolint:noctx // bind runs once at startup; cancellation handled by Shutdown
	if err != nil {
		return fmt.Errorf("start: Listen failed: %w", err)
	}
	if s.addr == ":0" || s.addr == "" {
		s.mu.Lock()
		s.srv.Addr = ln.Addr().String()
		s.mu.Unlock()
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// We deliberately do not expose this error through
			// a channel: the bot's main loop will see the
			// process exit and restart the container.
			fmt.Fprintf(os.Stderr, "health: serve: %v\n", err)
		}
	}()

	return nil
}

// Shutdown gracefully drains the HTTP server. Safe to call
// multiple times.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	return nil
}

func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()

	if !ready {
		http.Error(w, "starting", http.StatusServiceUnavailable)
		return
	}
	if s.reporter == nil {
		http.Error(w, "no clients configured", http.StatusServiceUnavailable)
		return
	}
	reports := s.reporter.Reports()
	connected := 0
	for _, r := range reports {
		if r.State == StatusConnected {
			connected++
		}
	}
	if connected == 0 {
		http.Error(w, "no transport connected", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:errchkjson // HTTP response stream is already started; encode errors are unobservable to the client
		"status":    "ready",
		"clients":   reports,
		"connected": connected,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	if s.reporter == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:errchkjson // HTTP response stream is already started; encode errors are unobservable to the client
			"status":  "no clients configured",
			"clients": []Report{},
		})

		return
	}
	reports := s.reporter.Reports()
	connected := 0
	for _, r := range reports {
		if r.State == StatusConnected {
			connected++
		}
	}
	body := map[string]any{
		"clients":   reports,
		"connected": connected,
	}
	if connected == 0 {
		body["status"] = "degraded"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(body) //nolint:errchkjson // HTTP response stream is already started; encode errors are unobservable to the client

		return
	}
	body["status"] = "ready"
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body) //nolint:errchkjson // HTTP response stream is already started; encode errors are unobservable to the client
}
