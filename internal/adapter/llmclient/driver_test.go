package llmclient_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/llmclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDriver records Stream calls and returns a canned result.
// It satisfies llm.Driver (Stream + Close).
type fakeDriver struct {
	streamFunc func(ctx context.Context, req llm.ChatRequest, onChunk func(llm.Chunk) error) error
	closeCount int
}

func (f *fakeDriver) Stream(ctx context.Context, req llm.ChatRequest, onChunk func(llm.Chunk) error) error {
	if f.streamFunc != nil {
		return f.streamFunc(ctx, req, onChunk)
	}

	return nil
}

func (f *fakeDriver) Close() error {
	f.closeCount++

	return nil
}

func TestDriver_Stream_ForwardsSuccess(t *testing.T) {
	t.Parallel()

	called := false
	drv := &fakeDriver{
		streamFunc: func(_ context.Context, _ llm.ChatRequest, _ func(llm.Chunk) error) error {
			called = true

			return nil
		},
	}

	adp := llmclient.New(drv)
	err := adp.Stream(context.Background(), llm.ChatRequest{}, nil)
	require.NoError(t, err)
	assert.True(t, called, "underlying driver.Stream was not invoked")
}

func TestDriver_Stream_WrapsError(t *testing.T) {
	t.Parallel()

	want := errors.New("boom")
	drv := &fakeDriver{
		streamFunc: func(_ context.Context, _ llm.ChatRequest, _ func(llm.Chunk) error) error {
			return want
		},
	}

	adp := llmclient.New(drv)
	err := adp.Stream(context.Background(), llm.ChatRequest{}, nil)
	require.ErrorIs(t, err, want, "adapter must wrap the original error so callers can match it with errors.Is")
	assert.Contains(t, err.Error(), "driver stream", "adapter must add context to the error message")
}

func TestDriver_DoesNotCallClose(t *testing.T) {
	t.Parallel()

	drv := &fakeDriver{}
	llmclient.New(drv)
	assert.Equal(t, 0, drv.closeCount, "adapter must not invoke Close on construction")
}
