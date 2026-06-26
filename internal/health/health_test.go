package health_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bestxp/narrative-ai-agent/internal/health"
)

// fakeReporter satisfies Reporter with a configurable snapshot.
type fakeReporter struct {
	mu     sync.Mutex
	report []health.Report
}

func (f *fakeReporter) Reports() []health.Report {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]health.Report, len(f.report))
	copy(out, f.report)

	return out
}

func (f *fakeReporter) set(reports []health.Report) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.report = reports
}

func TestLiveAlwaysOK(t *testing.T) {
	t.Parallel()

	s := health.New(":0", nil)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	defer func() { _ = s.Shutdown(context.Background()) }()

	url := "http://" + s.Addr() + "/healthz"

	resp, err := httpGetCtx(t.Context(), url)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestReadyzRequiresConnected(t *testing.T) {
	t.Parallel()

	r := &fakeReporter{}

	s := health.New(":0", r)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	defer func() { _ = s.Shutdown(context.Background()) }()

	s.MarkReady()

	r.set([]health.Report{
		{Name: "telegram", State: health.StatusReconnect},
		{Name: "vk", State: health.StatusStopped},
	})

	resp, _ := httpGetCtx(t.Context(), "http://"+s.Addr()+"/readyz")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}

	_ = resp.Body.Close()

	r.set([]health.Report{
		{Name: "telegram", State: health.StatusConnected},
		{Name: "vk", State: health.StatusStopped},
	})

	resp, _ = httpGetCtx(t.Context(), "http://"+s.Addr()+"/readyz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	defer func() { _ = resp.Body.Close() }()

	var body map[string]any

	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ready" {
		t.Fatalf("body[status] = %v, want ready", body["status"])
	}
}

func TestHealthEndpointAlwaysReturnsJSON(t *testing.T) {
	t.Parallel()

	r := &fakeReporter{}

	s := health.New(":0", r)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	defer func() { _ = s.Shutdown(context.Background()) }()

	s.MarkReady()
	r.set([]health.Report{{Name: "telegram", State: health.StatusConnected}})

	resp, err := httpGetCtx(t.Context(), "http://"+s.Addr()+"/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}

	var out map[string]any

	_ = json.NewDecoder(resp.Body).Decode(&out)
	if _, ok := out["clients"]; !ok {
		t.Fatalf("missing clients in body: %v", out)
	}
}

func TestShutdownDrains(t *testing.T) {
	t.Parallel()

	s := health.New(":0", &fakeReporter{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+s.Addr()+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()

		t.Fatal("expected connection error after Shutdown, got success")
	}
}

func httpGetCtx(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("http_get_ctx: NewRequestWithContext failed: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http_get_ctx: Do failed: %w", err)
	}

	return resp, nil
}
