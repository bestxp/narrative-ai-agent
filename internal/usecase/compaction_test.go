package usecase_test

import (
	"testing"
	"time"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/usecase"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEstimateConversationTokens_Empty(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 0, usecase.EstimateConversationTokens(nil, 0))
}

func TestEstimateConversationTokens_AccountsForSystemAndBody(t *testing.T) {
	t.Parallel()

	msgs := []llm.Message{
		{Role: "user", Content: "hello world"}, // 11 chars
		{
			Role: "assistant", Content: "hi",
			ToolCalls: []llm.ToolCall{{Function: llm.FunctionCall{Name: "end_day", Arguments: `{"day":1}`}}},
		},
	}
	tok := usecase.EstimateConversationTokens(msgs, 200) // 200 system chars
	// 200 + 11 + 2 + 7 + 9 = 229 → 58 with (229+3)/4=58 (rounded up).
	// Assert the formula rather than a hard number so a future
	// tweak to the estimator doesn't break the test.
	want := (200 + 11 + 2 + 7 + 9 + 3) / 4
	assert.Equal(t, want, tok)
}

func TestNeedsCompaction_BelowThreshold(t *testing.T) {
	t.Parallel()

	msgs := []llm.Message{{Role: "user", Content: "x"}}
	assert.False(t, usecase.NeedsCompaction(msgs, 100, 1000, 0.7))
}

func TestNeedsCompaction_AboveThreshold(t *testing.T) {
	t.Parallel()
	// 4000 chars system + a long user message → ~1001 tokens.
	msgs := []llm.Message{{Role: "user", Content: string(make([]byte, 400))}}
	// 1000/4000 = 0.25, so 0.7 threshold of 4000-window is 2800 — we are way under.
	// But with system 4000 + body 400 = 4400 chars / 4 = 1100 tokens.
	// 1100 / 4000-window = 0.275, still under 0.7.
	assert.False(t, usecase.NeedsCompaction(msgs, 4000, 4000, 0.7))
	// If window is 1500, 1100/1500 = 0.73 → triggers.
	assert.True(t, usecase.NeedsCompaction(msgs, 4000, 1500, 0.7))
}

func TestNeedsCompaction_ZeroWindowDisables(t *testing.T) {
	t.Parallel()
	assert.False(t, usecase.NeedsCompaction(nil, 999999, 0, 0.7))
	assert.False(t, usecase.NeedsCompaction(nil, 999999, 1000, 0))
}

func TestCompactConversations_KeepsRecent(t *testing.T) {
	t.Parallel()

	msgs := []llm.Message{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "u2"},
		{Role: "assistant", Content: "a2"},
		{Role: "user", Content: "u3"},
		{Role: "assistant", Content: "a3"},
		{Role: "user", Content: "u4"},
		{Role: "assistant", Content: "a4"},
	}
	// 8 messages, keep last 3 → [a3, u4, a4].
	kept, res := usecase.CompactConversations(msgs, 3)
	assert.Len(t, kept, 3)
	assert.Equal(t, "a3", kept[0].Content)
	assert.Equal(t, "u4", kept[1].Content)
	assert.Equal(t, "a4", kept[2].Content)
	assert.Equal(t, 5, res.DroppedTurns)
	assert.Greater(t, res.BeforeTokens, res.AfterTokens)
}

func TestCompactConversations_NothingToDo(t *testing.T) {
	t.Parallel()

	msgs := []llm.Message{{Role: "user", Content: "hi"}}
	kept, res := usecase.CompactConversations(msgs, 5)
	assert.Equal(t, msgs, kept)
	assert.Equal(t, 0, res.DroppedTurns)
}

func TestCompactConversations_KeepZeroReturnsEmpty(t *testing.T) {
	t.Parallel()

	msgs := []llm.Message{{Role: "user", Content: "x"}}
	kept, res := usecase.CompactConversations(msgs, 0)
	assert.Empty(t, kept)
	assert.Equal(t, 1, res.DroppedTurns)
}

func TestNewCompactionEvent_HasExpectedShape(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 6, 14, 0, 0, 0, time.UTC)
	ev := usecase.NewCompactionEvent("narrative", 22000, 5500, 23, 5, now)
	assert.Equal(t, "narrative", ev.Role)
	assert.Equal(t, "context_window*0.7", ev.Trigger)
	assert.Equal(t, 22000, ev.BeforeTokens)
	assert.Equal(t, 5500, ev.AfterTokens)
	assert.Equal(t, 23, ev.DroppedTurns)
	assert.Equal(t, 5, ev.KeptRecent)
	assert.True(t, ev.At.Equal(now))
}

func TestDescribeCompaction_Short(t *testing.T) {
	t.Parallel()

	res := usecase.CompactionResult{BeforeTokens: 22000, AfterTokens: 5500, DroppedTurns: 23, KeptRecent: 5}
	out := usecase.DescribeCompaction(res, "narrative", false)
	assert.Contains(t, out, "🔄")
	assert.Contains(t, out, "22k")
	assert.Contains(t, out, "5k")
	assert.Contains(t, out, "−23 ходов")
}

func TestDescribeCompaction_Verbose(t *testing.T) {
	t.Parallel()

	res := usecase.CompactionResult{BeforeTokens: 22000, AfterTokens: 5500, DroppedTurns: 23, KeptRecent: 5}
	out := usecase.DescribeCompaction(res, "narrative", true)
	assert.Contains(t, out, "было:  22000")
	assert.Contains(t, out, "стало: 5500")
	assert.Contains(t, out, "роль: narrative")
}

func TestDescribeCompaction_NoDropReturnsEmpty(t *testing.T) {
	t.Parallel()

	res := usecase.CompactionResult{DroppedTurns: 0}
	assert.Empty(t, usecase.DescribeCompaction(res, "narrative", true))
}

func TestCompactionFlow_GMReply(t *testing.T) {
	t.Parallel()
	// Smoke test: build a long history, run the compact+estimate
	// pipeline, confirm the kept slice is well under the cap.
	window := 100
	threshold := 0.5

	msgs := make([]llm.Message, 0, 60)
	for i := range 30 {
		msgs = append(msgs,
			llm.Message{Role: "user", Content: "user message " + string(rune('a'+i%26)) + " with extra padding to make this longer"},
			llm.Message{Role: "assistant", Content: "assistant reply " + string(rune('a'+i%26)) +
				" with even more padding to push the count up"},
		)
	}

	require.True(t, usecase.NeedsCompaction(msgs, 0, window, threshold), "long history should need compaction")
	kept, res := usecase.CompactConversations(msgs, 2)
	// 2 messages × ~85 chars / 4 ≈ 43 tokens, well under 50 threshold.
	assert.False(t, usecase.NeedsCompaction(kept, 0, window, threshold), "kept slice should fit")
	assert.Less(t, res.AfterTokens, 50)

	_ = domain.CompactionEvent{}
}
