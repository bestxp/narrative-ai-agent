package messaging_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bestxp/narrative-ai-agent/internal/messaging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClient is a minimal messaging.Client used to drive the
// multi-client pool. It records lifecycle events so tests can assert
// on shutdown ordering.
type fakeClient struct {
	name      string
	allow     map[string]bool
	started   bool
	stopped   bool
	mu        sync.Mutex
	sendCount int
	commands  []messaging.BotCommand
}

func (f *fakeClient) Name() string { return f.name }
func (f *fakeClient) Run(ctx context.Context) error {
	f.mu.Lock()
	f.started = true
	f.mu.Unlock()
	<-ctx.Done()
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()

	return nil
}

func (f *fakeClient) Send(_ context.Context, _ messaging.OutgoingMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.sendCount++

	return nil
}

//nolint:ireturn // interface contract; fake returns interface to satisfy messaging.Client.
func (f *fakeClient) StartStream(_ context.Context, _ string, _ int) (messaging.StreamSession, error) {
	return nil, messaging.ErrStreamingDisabled
}

func (f *fakeClient) IsAllowed(id string) bool { return f.allow[id] }

func (f *fakeClient) SetCommands(_ context.Context, cmds []messaging.BotCommand) error {
	f.commands = append(f.commands, cmds...)

	return nil
}

func (f *fakeClient) Health() messaging.HealthReport {
	f.mu.Lock()
	defer f.mu.Unlock()

	state := messaging.StateUnknown
	if f.started && !f.stopped {
		state = messaging.StateConnected
	}

	if f.started && f.stopped {
		state = messaging.StateStopped
	}

	return messaging.HealthReport{Name: f.name, State: state}
}

func TestMultiClient_StartsAll(t *testing.T) {
	t.Parallel()

	a := &fakeClient{name: "a"}
	b := &fakeClient{name: "b"}
	pool := messaging.NewMultiClient(a, b)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_ = pool.Run(ctx)

	assert.True(t, a.started)
	assert.True(t, b.started)
}

func TestMultiClient_AllReturnsClients(t *testing.T) {
	t.Parallel()

	a := &fakeClient{name: "a"}
	b := &fakeClient{name: "b"}
	pool := messaging.NewMultiClient(a, b)
	all := pool.All()
	require.Len(t, all, 2)
	assert.Equal(t, "a", all[0].Name())
	assert.Equal(t, "b", all[1].Name())
}

func TestMultiClient_EmptyPool(t *testing.T) {
	t.Parallel()

	pool := messaging.NewMultiClient()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Should block on ctx.Done() without panicking.
	err := pool.Run(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}
