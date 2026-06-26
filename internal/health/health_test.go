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
	"github.com/stretchr/testify/require"
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
	require.NoError(t, s.Start(), "Start")

	defer func() { _ = s.Shutdown(context.Background()) }()

	url := "http://" + s.Addr() + "/healthz"

	resp, err := httpGetCtx(t.Context(), url)
	require.NoError(t, err, "GET /healthz")

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestReadyzRequiresConnected(t *testing.T) {
	t.Parallel()

	r := &fakeReporter{}

	s := health.New(":0", r)
	require.NoError(t, s.Start(), "Start")

	defer func() { _ = s.Shutdown(context.Background()) }()

	s.MarkReady()

	r.set([]health.Report{
		{Name: "telegram", State: health.StatusReconnect},
		{Name: "vk", State: health.StatusStopped},
	})

	resp, _ := httpGetCtx(t.Context(), "http://"+s.Addr()+"/readyz")
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "expected 503")

	_ = resp.Body.Close()

	r.set([]health.Report{
		{Name: "telegram", State: health.StatusConnected},
		{Name: "vk", State: health.StatusStopped},
	})

	resp, _ = httpGetCtx(t.Context(), "http://"+s.Addr()+"/readyz")
	require.Equal(t, http.StatusOK, resp.StatusCode, "expected 200")

	defer func() { _ = resp.Body.Close() }()

	var body map[string]any

	_ = json.NewDecoder(resp.Body).Decode(&body)
	require.Equal(t, "ready", body["status"], "body[status]")
}

func TestHealthEndpointAlwaysReturnsJSON(t *testing.T) {
	t.Parallel()

	r := &fakeReporter{}

	s := health.New(":0", r)
	require.NoError(t, s.Start(), "Start")

	defer func() { _ = s.Shutdown(context.Background()) }()

	s.MarkReady()
	r.set([]health.Report{{Name: "telegram", State: health.StatusConnected}})

	resp, err := httpGetCtx(t.Context(), "http://"+s.Addr()+"/health")
	require.NoError(t, err, "GET /health")

	defer func() { _ = resp.Body.Close() }()

	ct := resp.Header.Get("Content-Type")
	require.True(t, strings.HasPrefix(ct, "application/json"), "content-type = %q, want application/json", ct)

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out map[string]any

	_ = json.NewDecoder(resp.Body).Decode(&out)
	_, ok := out["clients"]
	require.True(t, ok, "missing clients in body: %v", out)
}

func TestShutdownDrains(t *testing.T) {
	t.Parallel()

	s := health.New(":0", &fakeReporter{})
	require.NoError(t, s.Start(), "Start")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	require.NoError(t, s.Shutdown(ctx), "Shutdown")

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+s.Addr()+"/healthz", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()

		require.Fail(t, "expected connection error after Shutdown, got success")
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
