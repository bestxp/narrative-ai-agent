package handler_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bestxp/narrative-ai-agent/internal/config"
	"github.com/bestxp/narrative-ai-agent/internal/dispatcher"
	"github.com/bestxp/narrative-ai-agent/internal/messaging"
	"github.com/bestxp/narrative-ai-agent/internal/messaging/handler"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubClient implements the messaging.Client surface used
// by the handler. Unused methods return safe zero values
// so a test can wire only the bits it exercises.
type stubClient struct {
	name      string
	sent      int
	lastSend  messaging.OutgoingMessage
	streamErr error

	// recvStarted bumps once per call to Recv() — the
	// handler must call Recv() at most once per client.
	// If a future refactor drops the per-client goroutine,
	// this stays at 0.
	recvStarted atomic.Int32
	// recvCh blocks until a message is delivered;
	// tests close the channel to release the handler
	// goroutine without sending anything.
	recvCh chan messaging.IncomingMessage
}

func (s *stubClient) Name() string { return s.name }

// Run is what the pool calls when starting the transport.
// The handler never invokes Run directly — it only reads
// from Recv() — but the field exists so the stub satisfies
// the messaging.Client surface and can be plugged into a
// MultiClient.
func (s *stubClient) Run(ctx context.Context) error {
	<-ctx.Done()

	return ctx.Err()
}

func (s *stubClient) Recv() <-chan messaging.IncomingMessage {
	s.recvStarted.Add(1)

	return s.recvCh
}

func (s *stubClient) Health() messaging.HealthReport {
	return messaging.HealthReport{Name: s.name}
}

func (s *stubClient) Send(_ context.Context, m messaging.OutgoingMessage) error {
	s.sent++
	s.lastSend = m

	return nil
}

//nolint:ireturn // stub returns interface to satisfy messaging.Client
func (s *stubClient) StartStream(_ context.Context, _ string, _ int) (messaging.StreamSession, error) {
	if s.streamErr != nil {
		return nil, s.streamErr
	}

	return nil, messaging.ErrStreamingDisabled
}

func (s *stubClient) IsAllowed(_ string) bool { return true }
func (s *stubClient) SetCommands(_ context.Context, _ []messaging.BotCommand) error {
	return nil
}

func TestNew_RequiresConfig(t *testing.T) {
	t.Parallel()

	_, err := handler.New(nil, &dispatcher.Dispatcher{}, logOn())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Config")
}

func TestNew_RequiresDispatcher(t *testing.T) {
	t.Parallel()

	_, err := handler.New(&config.Config{}, nil, logOn())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Dispatcher")
}

func TestNew_RequiresLog(t *testing.T) {
	t.Parallel()

	_, err := handler.New(&config.Config{}, &dispatcher.Dispatcher{},
		zerolog.New(zerolog.NewConsoleWriter()).Level(zerolog.Disabled))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Log")
}

func TestNew_OK(t *testing.T) {
	t.Parallel()

	_, err := handler.New(
		&config.Config{},
		&dispatcher.Dispatcher{},
		logOn(),
	)
	require.NoError(t, err)
}

// TestRun_SpawnsGoroutinePerClient is the regression test
// for the "vk and wschat do not work" bug: handler.Run
// must launch one goroutine per transport client and
// each goroutine must call the client's Recv() at most
// once. If a future refactor drops the per-client
// goroutine (or skips the Recv() call), the
// recvStarted assertion fails loudly.
func TestRun_SpawnsGoroutinePerClient(t *testing.T) {
	t.Parallel()

	alpha := &stubClient{name: "alpha", recvCh: make(chan messaging.IncomingMessage)}
	bravo := &stubClient{name: "bravo", recvCh: make(chan messaging.IncomingMessage)}
	pool := messaging.NewMultiClient(alpha, bravo)

	h, err := handler.New(&config.Config{}, &dispatcher.Dispatcher{}, logOn())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		_ = h.Run(ctx, pool)

		close(done)
	}()

	assert.Eventually(t, func() bool {
		return alpha.recvStarted.Load() == 1 && bravo.recvStarted.Load() == 1
	}, 2*time.Second, 10*time.Millisecond,
		"each transport client must be driven by exactly one goroutine: alpha=%d bravo=%d",
		alpha.recvStarted.Load(), bravo.recvStarted.Load())

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler.Run did not return after ctx cancellation")
	}
}

// TestRun_NilPoolReturnsOnCtxDone verifies that handler.Run
// with a nil pool still respects ctx cancellation. The
// "no messaging transports configured" failure mode
// should not wedge the process.
func TestRun_NilPoolReturnsOnCtxDone(t *testing.T) {
	t.Parallel()

	h, err := handler.New(&config.Config{}, &dispatcher.Dispatcher{}, logOn())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Run starts

	done := make(chan error, 1)
	go func() {
		done <- h.Run(ctx, nil)
	}()

	select {
	case err := <-done:
		require.Error(t, err, "Run with nil pool and cancelled ctx must return ctx.Err()")
	case <-time.After(2 * time.Second):
		t.Fatal("Run with nil pool did not return on cancelled ctx")
	}
}

// logOn returns a non-Disabled zerolog logger. The handler
// rejects zerolog.Nop() (its level is Disabled) so tests
// must wire a real logger even when the test does not
// inspect log output.
func logOn() zerolog.Logger {
	return zerolog.New(zerolog.NewConsoleWriter()).Level(zerolog.InfoLevel)
}
