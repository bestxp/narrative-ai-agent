package usecase

import (
	"context"
	"testing"
	"time"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/prompts"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	yamlfs "github.com/bestxp/narrative-ai-agent/internal/storage/fs"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSummarizer_NotConfigured(t *testing.T) {
	t.Parallel()
	// nil summarizer is the "no summary role wired" case.
	var s *Summarizer
	assert.False(t, s.IsConfigured())
	res, err := s.SummarizeOldTurns(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "skipped", res.Source)
}

func TestSummarizer_EmptyMessagesSkips(t *testing.T) {
	t.Parallel()

	fake := &FakeLLM{}
	s := NewSummarizer(fake,
		llm.RoleConfig{Model: "summary", MaxTokens: 500, Temperature: 0.2},
		"system", slowlog.Discard(), DiscardLogger())
	assert.True(t, s.IsConfigured())
	res, err := s.SummarizeOldTurns(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "skipped", res.Source)
}

func TestSummarizer_HappyPath(t *testing.T) {
	t.Parallel()

	fake := &FakeLLM{}
	fake.rounds = [][]FakeChunk{
		{
			{Content: "- День 1: Аньбу остановила Маркуса, допрос", Finish: "stop"},
			{Content: "- День 2: Хокаге вызвал к себе", Finish: "stop"},
			{Content: "- Кагуя через портал вернулась", Finish: "stop"},
		},
	}
	s := NewSummarizer(fake,
		llm.RoleConfig{Model: "summary", MaxTokens: 500, Temperature: 0.2},
		"system-prompt", slowlog.Discard(), DiscardLogger())
	msgs := []llm.Message{
		{Role: "user", Content: "привет"},
		{
			Role:      "assistant",
			Content:   "Аньбу остановила",
			ToolCalls: []llm.ToolCall{{Function: llm.FunctionCall{Name: "end_day", Arguments: `{"day":1}`}}},
		},
		{Role: "tool", Name: "end_day", Content: `{"ok":true}`},
		{Role: "user", Content: "иду в деревню"},
		{Role: "assistant", Content: "Хокаге вызвал к себе"},
	}
	res, err := s.SummarizeOldTurns(context.Background(), msgs)
	require.NoError(t, err)
	assert.Equal(t, "summary", res.Source)
	assert.Positive(t, res.Tokens)
	assert.Contains(t, res.Text, "Аньбу")
	assert.Contains(t, res.Text, "Хокаге")
}

func TestSummarizer_FallbackMode(t *testing.T) {
	t.Parallel()

	fake := &FakeLLM{}
	fake.rounds = [][]FakeChunk{
		{{Content: "- compact summary", Finish: "stop"}},
	}
	// Build a fallback summarizer on top of a narrative-like
	// role with high temperature and big max_tokens. The
	// fallback constructor must clamp both.
	s := NewFallbackSummarizer(fake, llm.RoleConfig{
		Model: "narrative", MaxTokens: 4000, Temperature: 0.9,
	}, "system-prompt", slowlog.Discard(), DiscardLogger())
	assert.True(t, s.IsFallback())
	assert.Equal(t, 500, s.role.MaxTokens, "fallback must clamp max_tokens")
	assert.InDelta(t, 0.2, s.role.Temperature, 1e-9, "fallback must clamp temperature")

	res, err := s.SummarizeOldTurns(context.Background(), []llm.Message{
		{Role: "user", Content: "hello"},
	})
	require.NoError(t, err)
	assert.Equal(t, "summary-fallback", res.Source)
}

func TestSummarizer_FallbackKeepsLowValues(t *testing.T) {
	t.Parallel()
	// If the operator already configured a tame narrative
	// role (low temp, small max_tokens) the fallback
	// constructor should not inflate them.
	s := NewFallbackSummarizer(&FakeLLM{}, llm.RoleConfig{
		Model: "narrative", MaxTokens: 200, Temperature: 0.3,
	}, "system", slowlog.Discard(), DiscardLogger())
	assert.Equal(t, 200, s.role.MaxTokens)
	assert.InDelta(t, 0.3, s.role.Temperature, 1e-9, "fallback should not lower a temperature that is already in the safe range")
}

func TestSummarizer_StreamErrorReturned(t *testing.T) {
	t.Parallel()
	// Empty rounds means FakeLLM fires no chunks and returns
	// nil — our summarizer treats empty response as error.
	fake := &FakeLLM{}
	s := NewSummarizer(fake,
		llm.RoleConfig{Model: "summary", MaxTokens: 500, Temperature: 0.2},
		"system", slowlog.Discard(), DiscardLogger())
	_, err := s.SummarizeOldTurns(context.Background(), []llm.Message{{Role: "user", Content: "x"}})
	assert.Error(t, err)
}

func TestRenderTurnsForSummary_AllRoles(t *testing.T) {
	t.Parallel()

	msgs := []llm.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
		{Role: "tool", Name: "end_day", Content: `{"ok":true}`},
	}
	sum := prompts.NewOldTurnsSummaryData(projectMessages(msgs))
	out, err := prompts.RenderSummarizerUser("summarizer_old_turns_user.md.tmpl", sum)
	require.NoError(t, err)
	assert.Contains(t, out, "[Игрок]: hello")
	assert.Contains(t, out, "[GM]: hi there")
	assert.Contains(t, out, "[→ end_day]")
}

func TestRenderTurnsForSummary_AssistantWithToolCalls(t *testing.T) {
	t.Parallel()

	msgs := []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{Function: llm.FunctionCall{Name: "end_day", Arguments: `{}`}},
			{Function: llm.FunctionCall{Name: "update_state", Arguments: `{}`}},
		}},
	}
	sum := prompts.NewOldTurnsSummaryData(projectMessages(msgs))
	out, err := prompts.RenderSummarizerUser("summarizer_old_turns_user.md.tmpl", sum)
	require.NoError(t, err)
	assert.Contains(t, out, "(вызвал tools: end_day,update_state)")
}

func TestAppendHistoryToState_AppendsToEnd(t *testing.T) {
	t.Parallel()
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.WriteRawAtomic("info.yaml", "active_world: naruto\n"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.yaml",
		"state:\n  world: naruto\n  day: 5\n  in-flight: true\n  moment: допрос\n"+
			"  events:\n    - \"Ход 1: Аньбу подошла.\"\n"))
	yamlStore, _ := yamlfs.New(fs.Root())
	repos := api.NewYamlRepositories(yamlStore)
	tools := NewFileToolset(fs, repos, zerolog.Nop(), slowlog.Discard(), nil, nil, nil, nil)
	require.NoError(t, tools.AppendHistoryToState("naruto",
		"- Акацуки собраны\n- Хокаге вызвал",
		mustParseTime("2026-06-06T14:00:00Z")))

	got, _ := fs.ReadRaw("worlds/naruto/state.yaml")
	assert.Contains(t, got, "[history сжато 2026-06-06 14:00 UTC]")
	assert.Contains(t, got, "Акацуки собраны")
	assert.Contains(t, got, "Хокаге вызвал")
	// Existing content preserved.
	assert.Contains(t, got, "допрос")
	assert.Contains(t, got, "Ход 1: Аньбу подошла.")
}

func TestAppendHistoryToState_EmptySummaryNoop(t *testing.T) {
	t.Parallel()
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.yaml", "state:\n  world: naruto\n"))
	yamlStore, _ := yamlfs.New(fs.Root())
	repos := api.NewYamlRepositories(yamlStore)
	tools := NewFileToolset(fs, repos, zerolog.Nop(), slowlog.Discard(), nil, nil, nil, nil)
	require.NoError(t, tools.AppendHistoryToState("naruto", "", mustParseTime("2026-06-06T14:00:00Z")))

	got, _ := fs.ReadRaw("worlds/naruto/state.yaml")
	assert.Contains(t, got, "world: naruto", "empty summary should be a no-op")
}

func TestAppendHistoryToState_EmptyWorldErrors(t *testing.T) {
	t.Parallel()
	fs, _ := storage.NewFileStore(t.TempDir())
	yamlStore, _ := yamlfs.New(fs.Root())
	repos := api.NewYamlRepositories(yamlStore)
	tools := NewFileToolset(fs, repos, zerolog.Nop(), slowlog.Discard(), nil, nil, nil, nil)
	assert.Error(t, tools.AppendHistoryToState("", "stuff", mustParseTime("2026-06-06T14:00:00Z")))
}

func mustParseTime(s string) time.Time {
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}

	return parsed
}
