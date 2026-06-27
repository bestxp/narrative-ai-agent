package handler

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestChatMutexPool_LockReturnsSameMutex(t *testing.T) {
	t.Parallel()

	p := NewChatMutexPool()
	a := p.Lock("chat-1")
	b := p.Lock("chat-1")
	c := p.Lock("chat-2")

	assert.Same(t, a, b, "same chatID must return the same *sync.Mutex instance")
	assert.NotSame(t, a, c, "different chatIDs must return different mutexes")
}

func TestFormatStatus(t *testing.T) {
	t.Parallel()

	t.Run("request_received", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "…принял", formatStatus("request_received", nil))
	})

	t.Run("build_context_no_world", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "…собираю контекст", formatStatus("build_context", nil))
	})

	t.Run("build_context_with_world", func(t *testing.T) {
		t.Parallel()
		got := formatStatus("build_context", map[string]any{"world": "midearth"})
		assert.Equal(t, "…собираю контекст (midearth)", got)
	})

	t.Run("llm_request_no_model", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "…спрашиваю LLM", formatStatus("llm_request", nil))
	})

	t.Run("llm_request_with_model", func(t *testing.T) {
		t.Parallel()
		got := formatStatus("llm_request", map[string]any{"model": "gpt-5"})
		assert.Equal(t, "…спрашиваю gpt-5", got)
	})

	t.Run("tool_dispatch_no_tools", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "…применяю инструменты", formatStatus("tool_dispatch", nil))
	})

	t.Run("tool_dispatch_with_tools", func(t *testing.T) {
		t.Parallel()
		got := formatStatus("tool_dispatch", map[string]any{"tools": []string{"search_npc", "update_state"}})
		assert.Equal(t, "…применяю search_npc,update_state", got)
	})

	t.Run("unknown_phase", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "…думаю", formatStatus("made_up_phase", nil))
	})
}
