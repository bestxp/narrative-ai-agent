package telegram

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"narrative/internal/messaging"
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
	inner := &stubStream{}
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

// stubStream counts calls. It is enough for the throttle timing test
// and lets us avoid talking to Telegram's API.
type stubStream struct {
	appends int
	finals  int
}

func (s *stubStream) Append(_ context.Context, _ string) error {
	s.appends++
	return nil
}

func (s *stubStream) Final(_ context.Context, _ string) error {
	s.finals++
	return nil
}
