package usecase

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"narrative/internal/adapter/llm"
	"narrative/internal/adapter/storage"
	"narrative/internal/slowlog"
)

func TestSummarizer_NotConfigured(t *testing.T) {
	// nil summarizer is the "no summary role wired" case.
	var s *Summarizer
	assert.False(t, s.IsConfigured())
	res, err := s.SummarizeOldTurns(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "skipped", res.Source)
}

func TestSummarizer_EmptyMessagesSkips(t *testing.T) {
	fake := &fakeLLM{}
	s := NewSummarizer(fake, llm.RoleConfig{Model: "summary", MaxTokens: 500, Temperature: 0.2}, "system", slowlog.Discard(), discardLogger())
	assert.True(t, s.IsConfigured())
	res, err := s.SummarizeOldTurns(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "skipped", res.Source)
}

func TestSummarizer_HappyPath(t *testing.T) {
	fake := &fakeLLM{}
	fake.rounds = [][]fakeChunk{
		{
			{content: "- День 1: Аньбу остановила Маркуса, допрос", finish: "stop"},
			{content: "- День 2: Хокаге вызвал к себе", finish: "stop"},
			{content: "- Кагуя через портал вернулась", finish: "stop"},
		},
	}
	s := NewSummarizer(fake, llm.RoleConfig{Model: "summary", MaxTokens: 500, Temperature: 0.2}, "system-prompt", slowlog.Discard(), discardLogger())
	msgs := []llm.Message{
		{Role: "user", Content: "привет"},
		{Role: "assistant", Content: "Аньбу остановила", ToolCalls: []llm.ToolCall{{Function: llm.FunctionCall{Name: "end_day", Arguments: `{"day":1}`}}}},
		{Role: "tool", Name: "end_day", Content: `{"ok":true}`},
		{Role: "user", Content: "иду в деревню"},
		{Role: "assistant", Content: "Хокаге вызвал к себе"},
	}
	res, err := s.SummarizeOldTurns(context.Background(), msgs)
	require.NoError(t, err)
	assert.Equal(t, "summary", res.Source)
	assert.Greater(t, res.Tokens, 0)
	assert.Contains(t, res.Text, "Аньбу")
	assert.Contains(t, res.Text, "Хокаге")
}

func TestSummarizer_FallbackMode(t *testing.T) {
	fake := &fakeLLM{}
	fake.rounds = [][]fakeChunk{
		{{content: "- compact summary", finish: "stop"}},
	}
	// Build a fallback summarizer on top of a narrative-like
	// role with high temperature and big max_tokens. The
	// fallback constructor must clamp both.
	s := NewFallbackSummarizer(fake, llm.RoleConfig{
		Model: "narrative", MaxTokens: 4000, Temperature: 0.9,
	}, "system-prompt", slowlog.Discard(), discardLogger())
	assert.True(t, s.IsFallback())
	assert.Equal(t, 500, s.role.MaxTokens, "fallback must clamp max_tokens")
	assert.Equal(t, 0.2, s.role.Temperature, "fallback must clamp temperature")

	res, err := s.SummarizeOldTurns(context.Background(), []llm.Message{
		{Role: "user", Content: "hello"},
	})
	require.NoError(t, err)
	assert.Equal(t, "summary-fallback", res.Source)
}

func TestSummarizer_FallbackKeepsLowValues(t *testing.T) {
	// If the operator already configured a tame narrative
	// role (low temp, small max_tokens) the fallback
	// constructor should not inflate them.
	s := NewFallbackSummarizer(&fakeLLM{}, llm.RoleConfig{
		Model: "narrative", MaxTokens: 200, Temperature: 0.3,
	}, "system", slowlog.Discard(), discardLogger())
	assert.Equal(t, 200, s.role.MaxTokens)
	assert.Equal(t, 0.3, s.role.Temperature, "fallback should not lower a temperature that is already in the safe range")
}

func TestSummarizer_StreamErrorReturned(t *testing.T) {
	// Empty rounds means fakeLLM fires no chunks and returns
	// nil — our summarizer treats empty response as error.
	fake := &fakeLLM{}
	s := NewSummarizer(fake, llm.RoleConfig{Model: "summary", MaxTokens: 500, Temperature: 0.2}, "system", slowlog.Discard(), discardLogger())
	_, err := s.SummarizeOldTurns(context.Background(), []llm.Message{{Role: "user", Content: "x"}})
	assert.Error(t, err)
}

func TestRenderTurnsForSummary_AllRoles(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
		{Role: "tool", Name: "end_day", Content: `{"ok":true}`},
	}
	out := renderTurnsForSummary(msgs)
	assert.Contains(t, out, "[Игрок]: hello")
	assert.Contains(t, out, "[GM]: hi there")
	assert.Contains(t, out, "[→ end_day]")
}

func TestRenderTurnsForSummary_AssistantWithToolCalls(t *testing.T) {
	msgs := []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{Function: llm.FunctionCall{Name: "end_day", Arguments: `{}`}},
			{Function: llm.FunctionCall{Name: "update_state", Arguments: `{}`}},
		}},
	}
	out := renderTurnsForSummary(msgs)
	assert.Contains(t, out, "(вызвал tools: end_day,update_state)")
}

func TestAppendHistoryToState_AppendsToEnd(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.WriteRawAtomic("info.yaml", "active_world: naruto\n"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md",
		"# Состояние мира: naruto\n\n## Текущий момент\nДень 5 (в процессе).\nМомент: допрос.\n\n## Хронология дня\n- Ход 1: Аньбу подошла.\n"))
	m := NewMaintenance(fs)
	require.NoError(t, m.AppendHistoryToState("naruto", "- Акацуки собраны\n- Хокаге вызвал", mustParseTime("2026-06-06T14:00:00Z")))
	got, _ := fs.ReadRaw("worlds/naruto/state.md")
	assert.Contains(t, got, "[history сжато 2026-06-06 14:00]")
	assert.Contains(t, got, "- Акацуки собраны")
	assert.Contains(t, got, "- Хокаге вызвал")
	// Existing content preserved.
	assert.Contains(t, got, "Момент: допрос.")
	assert.Contains(t, got, "Ход 1: Аньбу подошла.")
}

func TestAppendHistoryToState_EmptySummaryNoop(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md", "old"))
	m := NewMaintenance(fs)
	require.NoError(t, m.AppendHistoryToState("naruto", "", mustParseTime("2026-06-06T14:00:00Z")))
	got, _ := fs.ReadRaw("worlds/naruto/state.md")
	assert.Equal(t, "old", got, "empty summary should be a no-op")
}

func TestAppendHistoryToState_EmptyWorldErrors(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	m := NewMaintenance(fs)
	assert.Error(t, m.AppendHistoryToState("", "stuff", mustParseTime("2026-06-06T14:00:00Z")))
}

func mustParseTime(s string) time.Time {
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return parsed
}
