package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEstimateTokens(t *testing.T) {
	assert.Equal(t, 0, EstimateTokens(""))
	assert.Equal(t, 1, EstimateTokens("abcd"))     // 4 chars → 1 token
	assert.Equal(t, 2, EstimateTokens("abcdefgh")) // 8 chars → 2 tokens
	assert.Equal(t, 3, EstimateTokens("abcdefghi")) // 9 chars → 3 tokens (rounded up)
	assert.Equal(t, 25, EstimateTokens(string(make([]byte, 100))))
}

func TestEstimateMessages(t *testing.T) {
	// The numbers below are deliberately computed from
	// EstimateTokens so the test does not lock in any
	// particular "right" answer for the underlying rule of
	// thumb; the goal is consistency across messages.
	msgs := []Message{
		{Role: "system", Content: "Ты — Game Master."}, // 18 chars
		{Role: "user", Content: "Привет"},              // 6 chars
		{Role: "tool", Name: "create_npc", Content: "ok"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{Function: FunctionCall{Name: "end_day", Arguments: `{"day":1}`}},
		}},
	}
	got := EstimateMessages(msgs)
	want := EstimateTokens("Ты — Game Master.") +
		EstimateTokens("Привет") +
		EstimateTokens("ok") +
		EstimateTokens("create_npc") +
		EstimateTokens("end_day") +
		EstimateTokens(`{"day":1}`)
	assert.Equal(t, want, got)
	assert.Greater(t, got, 0)
}
