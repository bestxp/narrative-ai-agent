package llm_test

import (
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
	"github.com/stretchr/testify/assert"
)

func TestEstimateTokens(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 0, llm.EstimateTokens(""))
	assert.Equal(t, 1, llm.EstimateTokens("abcd"))      // 4 chars → 1 token
	assert.Equal(t, 2, llm.EstimateTokens("abcdefgh"))  // 8 chars → 2 tokens
	assert.Equal(t, 3, llm.EstimateTokens("abcdefghi")) // 9 chars → 3 tokens (rounded up)
	assert.Equal(t, 25, llm.EstimateTokens(string(make([]byte, 100))))
}

func TestEstimateMessages(t *testing.T) {
	t.Parallel()
	// The numbers below are deliberately computed from
	// EstimateTokens so the test does not lock in any
	// particular "right" answer for the underlying rule of
	// thumb; the goal is consistency across messages.
	msgs := []llm.Message{
		{Role: "system", Content: "Ты — Game Master."}, // 18 chars
		{Role: "user", Content: "Привет"},              // 6 chars
		{Role: "tool", Name: "create_npc", Content: "ok"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{Function: llm.FunctionCall{Name: "end_day", Arguments: `{"day":1}`}},
		}},
	}
	got := llm.EstimateMessages(msgs)
	want := llm.EstimateTokens("Ты — Game Master.") +
		llm.EstimateTokens("Привет") +
		llm.EstimateTokens("ok") +
		llm.EstimateTokens("create_npc") +
		llm.EstimateTokens("end_day") +
		llm.EstimateTokens(`{"day":1}`)
	assert.Equal(t, want, got)
	assert.Positive(t, got)
}
