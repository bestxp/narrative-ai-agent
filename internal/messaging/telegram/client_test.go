package telegram

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bestxp/narrative-ai-agent/internal/messaging"
)

// Compile-time check: telegram.Client satisfies messaging.Client.
var _ messaging.Client = (*Client)(nil)

func discardLogger() zerolog.Logger {
	return zerolog.New(nil)
}

func TestIsAllowed(t *testing.T) {
	c := &Client{cfg: Config{AllowedUserIDs: []int{1, 2, 3}}}
	assert.True(t, c.IsAllowed("1"))
	assert.True(t, c.IsAllowed("2"))
	assert.False(t, c.IsAllowed("4"))
	assert.False(t, c.IsAllowed("not-a-number"))
}

func TestName(t *testing.T) {
	c := &Client{}
	assert.Equal(t, "telegram", c.Name())
}

func TestParseChatID(t *testing.T) {
	assert.Equal(t, int64(12345), parseChatID("12345"))
	assert.Equal(t, int64(0), parseChatID("garbage"))
	assert.Equal(t, int64(-1), parseChatID("-1"))
}

func TestNew_RejectsEmptyToken(t *testing.T) {
	_, err := New(Config{Token: ""}, discardLogger())
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "token"))
}

func TestThrottledStream_FirstAppendGoesImmediately(t *testing.T) {
	inner := &recordingStream{}
	th := NewThrottledStream(inner)

	start := time.Now()
	require.NoError(t, th.Append(context.Background(), "hello"))
	firstCallLatency := time.Since(start)
	// First call should not be blocked by the throttle.
	assert.Less(t, firstCallLatency, 50*time.Millisecond)

	require.NoError(t, th.Final(context.Background(), "done"))
	assert.Equal(t, 1, inner.appends)
	assert.Equal(t, 1, inner.finals)
}

// TestThrottledStream_FinalIsIdempotent asserts that ThrottledStream
// does not block on the second Final call. The inner stream's
// own closed-flag short-circuits the duplicate Telegram edit;
// from the throttle layer's perspective both calls return
// promptly without sleeping.
func TestThrottledStream_FinalIsIdempotent(t *testing.T) {
	rec := &recordingStream{}
	th := NewThrottledStream(rec)
	start := time.Now()
	require.NoError(t, th.Final(context.Background(), "x"))
	require.NoError(t, th.Final(context.Background(), "y"))
	elapsed := time.Since(start)
	assert.Less(t, elapsed, 50*time.Millisecond, "second Final should not be throttled")
}

type recordingStream struct {
	mu      sync.Mutex
	appends int
	finals  int
	events  []string
}

func (r *recordingStream) Append(_ context.Context, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appends++
	r.events = append(r.events, "append:"+text)
	return nil
}

func (r *recordingStream) Final(_ context.Context, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finals++
	r.events = append(r.events, "final:"+text)
	return nil
}
