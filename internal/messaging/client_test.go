package messaging

import (
	"context"
	"sync"
	"testing"
	"time"

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
	commands  []BotCommand
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

func (f *fakeClient) Send(ctx context.Context, msg OutgoingMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendCount++
	return nil
}

func (f *fakeClient) StartStream(ctx context.Context, chatID string, replyToMessageID int) (StreamSession, error) {
	return nil, nil
}

func (f *fakeClient) IsAllowed(id string) bool { return f.allow[id] }

func (f *fakeClient) SetCommands(ctx context.Context, cmds []BotCommand) error {
	f.commands = append(f.commands, cmds...)
	return nil
}

func (f *fakeClient) Health() HealthReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := StateUnknown
	if f.started && !f.stopped {
		state = StateConnected
	}
	if f.started && f.stopped {
		state = StateStopped
	}
	return HealthReport{Name: f.name, State: state}
}

func TestMultiClient_StartsAll(t *testing.T) {
	t.Parallel()
	a := &fakeClient{name: "a"}
	b := &fakeClient{name: "b"}
	pool := NewMultiClient(a, b)

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
	pool := NewMultiClient(a, b)
	all := pool.All()
	require.Len(t, all, 2)
	assert.Equal(t, "a", all[0].Name())
	assert.Equal(t, "b", all[1].Name())
}

func TestMultiClient_EmptyPool(t *testing.T) {
	t.Parallel()
	pool := NewMultiClient()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Should block on ctx.Done() without panicking.
	err := pool.Run(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}
