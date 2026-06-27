package app_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bestxp/narrative-ai-agent/cmd/bot/app"
	"github.com/bestxp/narrative-ai-agent/internal/messaging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// appStubClient implements messaging.Client with just
// enough surface for app_test.go to drive app.Run. The
// crucial field is runStarted: it bumps when the pool
// calls our Run(ctx) method. The regression test below
// fails if app.Run forgets to call pool.Run, because
// pool.Run is what invokes c.Run(ctx) for each client —
// without that, no client goroutine is ever spawned and
// no messages can arrive (this was the original vk /
// wschat regression).
type appStubClient struct {
	name       string
	runStarted atomic.Int32
	recvCh     chan messaging.IncomingMessage
}

func (s *appStubClient) Name() string { return s.name }

// Run is the "transport goroutine" — pool.Run spawns one
// goroutine per client that calls this. We bump
// runStarted and block on ctx so the test can assert the
// goroutine actually existed.
func (s *appStubClient) Run(ctx context.Context) error {
	s.runStarted.Add(1)
	<-ctx.Done()

	return ctx.Err()
}

func (s *appStubClient) Recv() <-chan messaging.IncomingMessage {
	return s.recvCh
}

func (s *appStubClient) Health() messaging.HealthReport {
	return messaging.HealthReport{Name: s.name}
}

func (s *appStubClient) Send(_ context.Context, _ messaging.OutgoingMessage) error {
	return nil
}

//nolint:ireturn // stub returns interface to satisfy messaging.Client
func (s *appStubClient) StartStream(_ context.Context, _ string, _ int) (messaging.StreamSession, error) {
	return nil, messaging.ErrStreamingDisabled
}

func (s *appStubClient) IsAllowed(_ string) bool { return true }
func (s *appStubClient) SetCommands(_ context.Context, _ []messaging.BotCommand) error {
	return nil
}

// TestRun_DrivesEveryTransport is the regression test
// for the "vk and wschat do not work" bug. app.Run must
// call pool.Run(ctx) in addition to handler.Run; without
// pool.Run, no client's Run(ctx) goroutine is ever
// spawned and no message arrives. This test fails loudly
// if a future refactor drops the pool.Run sibling.
func TestRun_DrivesEveryTransport(t *testing.T) {
	t.Parallel()

	vk := &appStubClient{name: "vk", recvCh: make(chan messaging.IncomingMessage)}
	ws := &appStubClient{name: "wschat", recvCh: make(chan messaging.IncomingMessage)}

	a := app.NewForTest(t, vk, ws)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan struct{})

	go func() {
		_ = a.Run(ctx)

		close(done)
	}()

	// Both transports must have entered Run() — pool.Run
	// spawned a per-client goroutine for each. If
	// pool.Run was forgotten, runStarted stays at 0.
	assert.Eventually(t, func() bool {
		return vk.runStarted.Load() == 1 && ws.runStarted.Load() == 1
	}, 2*time.Second, 10*time.Millisecond,
		"every transport client must be driven by pool.Run: vk=%d wschat=%d",
		vk.runStarted.Load(), ws.runStarted.Load())

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("app.Run did not return after ctx cancellation")
	}
}

// TestRun_CtxCancellationPropagatesToPool asserts that
// when ctx is cancelled, both pool.Run and handler.Run
// observe the cancellation and exit cleanly. Without
// this, a hung transport goroutine would prevent process
// shutdown.
func TestRun_CtxCancellationPropagatesToPool(t *testing.T) {
	t.Parallel()

	client := &appStubClient{name: "vk", recvCh: make(chan messaging.IncomingMessage)}
	a := app.NewForTest(t, client)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		_ = a.Run(ctx)

		close(done)
	}()

	// Wait until pool.Run has actually started the client.
	require.Eventually(t, func() bool {
		return client.runStarted.Load() == 1
	}, 2*time.Second, 10*time.Millisecond)

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("app.Run did not return after ctx cancellation")
	}
}
