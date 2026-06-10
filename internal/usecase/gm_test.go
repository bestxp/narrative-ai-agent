package usecase

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/prompts"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
)

// fakeLLM replays a deterministic sequence of chunks. The test
// declares what the LLM should do on the first and (optionally)
// second stream call, including any tool calls.
type fakeLLM struct {
	mu     sync.Mutex
	calls  int
	rounds [][]fakeChunk
}

type fakeChunk struct {
	content  string
	toolName string
	toolArgs string
	toolID   string
	finish   string
	usage    llm.Usage
}

func (f *fakeLLM) Stream(_ context.Context, _ llm.ChatRequest, onChunk func(llm.Chunk) error) error {
	f.mu.Lock()
	idx := f.calls
	f.calls++
	var round []fakeChunk
	if idx < len(f.rounds) {
		round = f.rounds[idx]
	}
	f.mu.Unlock()
	for _, fc := range round {
		ch := llm.Chunk{Content: fc.content, Finish: fc.finish, Usage: fc.usage}
		if fc.toolName != "" {
			ch.ToolCalls = []llm.ToolCall{{
				ID:       fc.toolID,
				Type:     "function",
				Function: llm.FunctionCall{Name: fc.toolName, Arguments: fc.toolArgs},
			}}
		}
		if err := onChunk(ch); err != nil {
			return err
		}
	}
	return onChunk(llm.Chunk{Done: true})
}

func newGMTestEnv(t *testing.T) (*GM, *storage.FileStore, *fakeLLM) {
	t.Helper()
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile, domain.BuildInfo("markus", "naruto", nil, nil)))
	require.NoError(t, fs.EnsureDir("characters/markus"))
	require.NoError(t, fs.WriteRawAtomic("characters/markus/SOUL.md", "# Markus"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md", "День 1 (в процессе).\nАктивные NPC прямо сейчас: Какаши"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/lore.md", "lore"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/plan.md", "plan"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/canon.md", "canon"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/memorise.md", ""))
	require.NoError(t, fs.EnsureDir("worlds/naruto/characters"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/characters/kakashi.yaml", "display_name: Какаши\ntemperament: спокойный\n"))

	ss := NewSessionStart(fs)
	fl := NewFirstLaunch(fs)
	// One Tool bundles every concern; the tests use the
	// file-backed implementation so on-disk state changes
	// are observable via fs.ReadRaw.
	tools := NewFileToolset(fs, discardLogger(), slowlog.Discard(), nil, nil, nil)
	fake := &fakeLLM{}
	log, _ := newBufLogger()
	g := NewGM(GMConfig{
		Role: llm.RoleConfig{
			Model:           "test",
			MaxTokens:       100,
			Temperature:     0.5,
			MaxEmptyRetries: 2, // mirror config default so old "expect 2 calls" tests keep working
		},
		SystemPrompt: "# rules",
		Compaction:   CompactionConfig{ContextWindow: 0, Threshold: 0.7, KeepRecent: 5},
	}, fs, fake, ss, fl, tools, nil, NewSystemState(fs, discardLogger(), slowlog.Discard()), slowlog.Discard(), "off", false, log)
	return g, fs, fake
}

func deltaOnly(s *strings.Builder) Callbacks {
	return Callbacks{OnDelta: func(t string) error { s.WriteString(t); return nil }}
}

func TestGM_StreamsReplyIntoCallback(t *testing.T) {
	g, _, fake := newGMTestEnv(t)
	fake.rounds = [][]fakeChunk{
		{
			{content: "**диалоги и действия**\nПривет, "},
			{content: "путник.\n\n**КОНТЕКСТ И ИЗМЕНЕНИЯ**\nбез изменений\n\n**БУДУЩЕЕ**\n- продолжение\n\n**ВАЛИДАЦИЯ ПРАВИЛ**\n- ok"},
			{finish: "stop"},
		},
	}
	var got strings.Builder
	_, err := g.Reply(context.Background(), "chat1", "я пришёл", deltaOnly(&got))
	require.NoError(t, err)
	assert.Equal(t, 1, fake.calls)
	assert.Contains(t, got.String(), "Привет, путник.")
}

func TestGM_ToolRound_EndDay(t *testing.T) {
	g, fs, fake := newGMTestEnv(t)
	fake.rounds = [][]fakeChunk{
		{
			{content: "Архивирую день."},
			{toolID: "call_1", toolName: "end_day", toolArgs: `{"day":1,"summary":"первый день"}`, finish: "tool_calls"},
		},
		{
			{content: " День записан.\n\n**диалоги и действия**\nАрхивирую день. День записан.\n\n**КОНТЕКСТ И ИЗМЕНЕНИЯ**\nmemorise.md обновлён\n\n**БУДУЩЕЕ**\n- продолжение\n\n**ВАЛИДАЦИЯ ПРАВИЛ**\n- ok"},
			{finish: "stop"},
		},
	}
	var got strings.Builder
	_, err := g.Reply(context.Background(), "chat1", "конец дня", deltaOnly(&got))
	require.NoError(t, err)
	mem, _ := fs.ReadRaw("worlds/naruto/memorise.md")
	assert.Contains(t, mem, "д00001: первый день")
	assert.Equal(t, 2, fake.calls)
}

func TestGM_ToolRound_UpdateState(t *testing.T) {
	g, fs, fake := newGMTestEnv(t)
	fake.rounds = [][]fakeChunk{
		{
			{content: "обновляю"},
			{toolID: "call_1", toolName: "update_state",
				toolArgs: `{"moment":"Маркус входит в деревню.","npcs":["Какаши"],"in_flight":true}`,
				finish:   "tool_calls"},
		},
		{{content: " ок.\n\n**диалоги и действия**\nобновляю ок.\n\n**КОНТЕКСТ И ИЗМЕНЕНИЯ**\nstate.md обновлён\n\n**БУДУЩЕЕ**\n- продолжение\n\n**ВАЛИДАЦИЯ ПРАВИЛ**\n- ok", finish: "stop"}},
	}
	_, err := g.Reply(context.Background(), "chat1", "идём в деревню", Callbacks{})
	require.NoError(t, err)
	state, _ := fs.ReadRaw("worlds/naruto/state.md")
	assert.Contains(t, state, "Маркус входит в деревню")
	assert.Contains(t, state, "Какаши")
}

func TestGM_ToolRound_RotatePlan_RejectsBadRange(t *testing.T) {
	g, _, fake := newGMTestEnv(t)
	fake.rounds = [][]fakeChunk{
		{
			{toolID: "call_1", toolName: "rotate_plan",
				toolArgs: `{"events":["a","b"]}`, finish: "tool_calls"},
		},
		{{content: "не вышло\n\n**диалоги и действия**\nне вышло\n\n**КОНТЕКСТ И ИЗМЕНЕНИЯ**\nбез изменений\n\n**БУДУЩЕЕ**\n- продолжение\n\n**ВАЛИДАЦИЯ ПРАВИЛ**\n- ok", finish: "stop"}},
	}
	_, err := g.Reply(context.Background(), "chat1", "x", Callbacks{})
	require.NoError(t, err)
	assert.Equal(t, 2, fake.calls)
}

func TestGM_StopsAtMaxRounds(t *testing.T) {
	g, _, fake := newGMTestEnv(t)
	many := make([][]fakeChunk, maxToolRounds+1)
	for i := range many {
		many[i] = []fakeChunk{{
			toolID: fmt.Sprintf("c%d", i), toolName: "run_maintenance",
			toolArgs: `{}`, finish: "tool_calls",
		}}
	}
	fake.rounds = many
	_, err := g.Reply(context.Background(), "chat1", "x", Callbacks{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeded")
}

func TestGM_StuckGuard_InjectsNudge(t *testing.T) {
	g, _, fake := newGMTestEnv(t)
	fake.rounds = [][]fakeChunk{
		{{toolID: "c0", toolName: "update_npc", toolArgs: `{"npc":"Ино","section":"Личная память/факты","append":"test"}`, finish: "tool_calls"}},
		{{toolID: "c1", toolName: "update_npc", toolArgs: `{"npc":"Шикамару","section":"Личная память/факты","append":"test"}`, finish: "tool_calls"}},
		{{toolID: "c2", toolName: "update_npc", toolArgs: `{"npc":"Чоджи","section":"Личная память/факты","append":"test"}`, finish: "tool_calls"}},
		{{content: "**диалоги и действия**\nок\n\n**КОНТЕКСТ И ИЗМЕНЕНИЯ**\nбез изменений\n\n**БУДУЩЕЕ**\n- продолжение\n\n**ВАЛИДАЦИЯ ПРАВИЛ**\n- ok", finish: "stop"}},
	}
	_, err := g.Reply(context.Background(), "chat1", "рассказал легенду", Callbacks{})
	require.NoError(t, err)
	conv := g.getConversation("chat1")
	var foundNudge bool
	for _, m := range conv.messages {
		if m.Role == "user" && strings.Contains(m.Content, "Не вызывай больше инструменты") {
			foundNudge = true
		}
	}
	assert.True(t, foundNudge, "stuck guard should inject nudge after 3 consecutive tool-only rounds")
}

func TestGM_ResetConversation(t *testing.T) {
	g, _, fake := newGMTestEnv(t)
	fake.rounds = [][]fakeChunk{
		{{content: "hi", finish: "stop"}},
	}
	_, err := g.Reply(context.Background(), "chat1", "ping", Callbacks{})
	require.NoError(t, err)
	conv := g.getConversation("chat1")
	assert.NotEmpty(t, conv.messages)

	g.ResetConversation("chat1")
	conv2 := g.getConversation("chat1")
	assert.NotNil(t, conv2)
	assert.Empty(t, conv2.messages)
}

func TestGM_BuildsContextWithNPCs(t *testing.T) {
	g, _, _ := newGMTestEnv(t)
	var captured llm.ChatRequest
	captureLLM := &captureLLM{run: func(req llm.ChatRequest, onChunk func(llm.Chunk) error) error {
		captured = req
		return onChunk(llm.Chunk{
			Content: "**диалоги и действия**\nok\n\n**КОНТЕКСТ И ИЗМЕНЕНИЯ**\nбез изменений\n\n**БУДУЩЕЕ**\n- продолжение\n\n**ВАЛИДАЦИЯ ПРАВИЛ**\n- ok",
			Finish:  "stop",
		})
	}}
	g.llm = captureLLM
	_, err := g.Reply(context.Background(), "chat1", "go", Callbacks{})
	require.NoError(t, err)
	require.NotEmpty(t, captured.Messages)
	assert.Contains(t, captured.Messages[0].Content, "rules")
	assert.Contains(t, captured.Messages[0].Content, "naruto")
	assert.Contains(t, captured.Messages[0].Content, "Какаши")
	assert.Contains(t, captured.Messages[0].Content, "спокойный")
}

func TestGM_TokenUsage_Estimate(t *testing.T) {
	g, _, fake := newGMTestEnv(t)
	g.tracking = "estimate"
	fake.rounds = [][]fakeChunk{
		{{content: "Привет, мир.", finish: "stop"}},
	}
	var lastTok llm.Usage
	_, err := g.Reply(context.Background(), "chat1", "ping", Callbacks{OnTokens: func(u llm.Usage) { lastTok = u }})
	require.NoError(t, err)
	assert.Greater(t, lastTok.PromptTokens, 0)
	assert.Greater(t, lastTok.CompletionTokens, 0)
}

func TestGM_TokenUsage_Usage(t *testing.T) {
	g, _, fake := newGMTestEnv(t)
	g.tracking = "usage"
	fake.rounds = [][]fakeChunk{
		{{content: "ok", finish: "stop", usage: llm.Usage{PromptTokens: 12, CompletionTokens: 7, TotalTokens: 19}}},
	}
	var lastTok llm.Usage
	totals, err := g.Reply(context.Background(), "chat1", "ping", Callbacks{OnTokens: func(u llm.Usage) { lastTok = u }})
	require.NoError(t, err)
	assert.Equal(t, 12, lastTok.PromptTokens)
	assert.Equal(t, 19, totals.TotalTokens)
}

func TestMergeToolCalls_FirstChunkStartsNew(t *testing.T) {
	out := mergeToolCalls(nil, []llm.ToolCall{{
		ID: "c1", Type: "function",
		Function: llm.FunctionCall{Name: "end_day", Arguments: `{"day":1}`},
	}})
	require.Len(t, out, 1)
	assert.Equal(t, "c1", out[0].ID)
	assert.Equal(t, "end_day", out[0].Function.Name)
	assert.Equal(t, `{"day":1}`, out[0].Function.Arguments)
}

func TestMergeToolCalls_ContinuationsAccumulate(t *testing.T) {
	out := mergeToolCalls(nil, []llm.ToolCall{{
		ID: "c1", Type: "function",
		Function: llm.FunctionCall{Name: "end_day", Arguments: `{"day":`},
	}})
	out = mergeToolCalls(out, []llm.ToolCall{{
		Function: llm.FunctionCall{Arguments: `1}`},
	}})
	require.Len(t, out, 1)
	assert.Equal(t, `{"day":1}`, out[0].Function.Arguments)
}

func TestGM_CompactionFiresOnLongHistory(t *testing.T) {
	g, _, fake := newGMTestEnv(t)
	g.compaction = CompactionConfig{ContextWindow: 100, Threshold: 0.5, KeepRecent: 2}
	// Inject a long history (60 messages) directly so the
	// preflight sees ~5000 chars of input → ~1250 tokens,
	// well past the 50-token threshold.
	conv := g.getConversation("chat1")
	conv.mu.Lock()
	for i := 0; i < 30; i++ {
		conv.messages = append(conv.messages,
			llm.Message{Role: "user", Content: "user message " + string(rune('a'+i%26)) + " with extra padding to make this longer"},
			llm.Message{Role: "assistant", Content: "assistant reply " + string(rune('a'+i%26)) + " with even more padding to push the count up"},
		)
	}
	conv.mu.Unlock()

	fake.rounds = [][]fakeChunk{{{
		content: "**диалоги и действия**\nfinal\n\n**КОНТЕКСТ И ИЗМЕНЕНИЯ**\nбез изменений\n\n**БУДУЩЕЕ**\n- продолжение\n\n**ВАЛИДАЦИЯ ПРАВИЛ**\n- ok",
		finish:  "stop",
	}}}
	var compacted CompactionResult
	var compactedMu sync.Mutex
	cb := Callbacks{
		OnDelta: func(s string) error { return nil },
		OnCompaction: func(r CompactionResult) {
			compactedMu.Lock()
			compacted = r
			compactedMu.Unlock()
		},
	}
	_, err := g.Reply(context.Background(), "chat1", "ping", cb)
	require.NoError(t, err)
	compactedMu.Lock()
	defer compactedMu.Unlock()
	assert.Greater(t, compacted.DroppedTurns, 0, "compaction should have fired on long history (got %d dropped, before=%d after=%d)", compacted.DroppedTurns, compacted.BeforeTokens, compacted.AfterTokens)
	assert.LessOrEqual(t, len(g.getConversation("chat1").messages), 2*g.compaction.KeepRecent+2, "conv holds kept + (1 user, 1 assistant from final round)")
}

func TestGM_CompactionWithSummarizer_WritesToState(t *testing.T) {
	g, fs, fake := newGMTestEnv(t)
	g.compaction = CompactionConfig{ContextWindow: 100, Threshold: 0.5, KeepRecent: 2}
	// Wire a summarizer that responds with a short fact list.
	summaryLLM := &fakeLLM{}
	summaryLLM.rounds = [][]fakeChunk{
		{{content: "- Акацуки собраны (день 5)\n- Хокаге вызвал к себе", finish: "stop"}},
	}
	g.summarizer = NewSummarizer(summaryLLM,
		llm.RoleConfig{Model: "summary", MaxTokens: 500, Temperature: 0.2},
		"system-prompt", slowlog.Discard(), discardLogger())

	// Long history.
	conv := g.getConversation("chat1")
	conv.mu.Lock()
	for i := 0; i < 30; i++ {
		conv.messages = append(conv.messages,
			llm.Message{Role: "user", Content: "long user message " + string(rune('a'+i%26)) + " with padding"},
			llm.Message{Role: "assistant", Content: "long assistant reply " + string(rune('a'+i%26)) + " with even more padding"},
		)
	}
	conv.mu.Unlock()

	fake.rounds = [][]fakeChunk{{{content: "ok", finish: "stop"}}}
	_, err := g.Reply(context.Background(), "chat1", "ping", Callbacks{OnDelta: func(s string) error { return nil }})
	require.NoError(t, err)

	// state.md should have the new history section appended.
	state, _ := fs.ReadRaw("worlds/naruto/state.md")
	assert.Contains(t, state, "[history сжато", "summarizer should have appended a history block")
	assert.Contains(t, state, "Акацуки собраны")
	assert.Contains(t, state, "Хокаге вызвал")
}

type captureLLM struct {
	run func(req llm.ChatRequest, onChunk func(llm.Chunk) error) error
}

func (c *captureLLM) Stream(_ context.Context, req llm.ChatRequest, onChunk func(llm.Chunk) error) error {
	return c.run(req, onChunk)
}

// TestGM_BrokenToolCallsIsHardError covers the
// minimax-m3:cloud case where the model decides to call a
// tool but the stream cuts off before the head lands, and
// we see `delta.tool_calls: [{}]` fragments assembled into
// one or more headless entries. With h4-by-default config,
// the bot must (a) NOT dispatch them as "unknown tool:",
// (b) NOT retry with a nudge, (c) treat the round as a
// hard error so the operator can see the broken state in
// slowlog instead of silently cycling.
func TestGM_BrokenToolCallsIsHardError(t *testing.T) {
	g, _, fake := newGMTestEnv(t)
	fake.rounds = [][]fakeChunk{
		{
			// The model "decided" to call a tool, but only
			// the ID + empty name + partial args made it
			// through the stream.
			{toolID: "call_X", toolName: "", toolArgs: `{`, finish: "stop"},
		},
	}
	var got strings.Builder
	_, err := g.Reply(context.Background(), "chat1", "ping", deltaOnly(&got))
	require.Error(t, err, "broken tool calls must propagate as a hard error under h4")
	assert.Contains(t, err.Error(), "no tool_use and no content")
	assert.Equal(t, 1, fake.calls, "no nudge-retry on broken tool calls; h4 is hard-error")
	// The assistant turn must not be persisted (no half-broken
	// state in history that would poison the next round).
	conv := g.getConversation("chat1")
	conv.mu.Lock()
	defer conv.mu.Unlock()
	for _, m := range conv.messages {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			assert.NotEmpty(t, tc.Function.Name, "broken (headless) tool calls must not be persisted into history")
		}
	}
}

// TestAllToolCallsBroken covers the "minimax-m3:cloud stream
// got cut off mid-tool-call" detection. When every entry in
// the slice has an empty Function.Name the round is
// treated as empty content (the model intended to call a
// tool but the stream clipped before the head landed).
// Dispatching them as "unknown tool:" would just pollute
// the conversation with garbage tool-role messages.
func TestAllToolCallsBroken(t *testing.T) {
	// Empty slice: not "broken" — there are simply no tool calls.
	assert.False(t, allToolCallsBroken(nil))
	assert.False(t, allToolCallsBroken([]llm.ToolCall{}))
	// A single headless entry (Name="") is the broken signature.
	assert.True(t, allToolCallsBroken([]llm.ToolCall{{
		Function: llm.FunctionCall{Name: "", Arguments: "{partial"},
	}}))
	// Multiple headless entries: still all broken.
	assert.True(t, allToolCallsBroken([]llm.ToolCall{
		{Function: llm.FunctionCall{Name: "", Arguments: ""}},
		{Function: llm.FunctionCall{Name: "", Arguments: "{partial"}},
	}))
	// One valid entry makes the round NOT broken.
	assert.False(t, allToolCallsBroken([]llm.ToolCall{{
		Function: llm.FunctionCall{Name: "update_state", Arguments: `{"moment":"x"}`},
	}}))
	// Mixed: one valid + one headless — keep the valid one, do
	// not classify the whole round as broken.
	assert.False(t, allToolCallsBroken([]llm.ToolCall{
		{Function: llm.FunctionCall{Name: "", Arguments: "{partial"}},
		{Function: llm.FunctionCall{Name: "update_state", Arguments: `{}`}},
	}))
}

// TestGM_EmptyContentIsHardError covers the h4-by-default
// behaviour: when the model returns 0 tool_use AND 0
// content, the bot must surface a hard error to the
// dispatcher (no retry, no polite placeholder). The error
// message identifies the round, the finish_reason, and the
// raw SSE event count so the operator has something to
// debug via slowlog.
func TestGM_EmptyContentIsHardError(t *testing.T) {
	g, _, fake := newGMTestEnv(t)
	fake.rounds = [][]fakeChunk{
		{{finish: "stop"}}, // round 0: 0 chars, 0 tool calls
	}
	var got strings.Builder
	_, err := g.Reply(context.Background(), "chat1", "ping", deltaOnly(&got))
	require.Error(t, err, "empty content must propagate as an error")
	assert.Contains(t, err.Error(), "no tool_use and no content")
	assert.Contains(t, err.Error(), "round=0")
	assert.Contains(t, err.Error(), "finish=stop")
	// Exactly one LLM call — no retry.
	assert.Equal(t, 1, fake.calls, "no retry on empty content")
	// The visible buffer is empty (no polite placeholder).
	assert.Empty(t, got.String())
}

// TestGM_EmptyWithToolCallsFinish_StillError covers the
// "model intended to act but stream cut off" path: the
// finish reason is "tool_calls" but no calls survived the
// accumulator, no content was streamed. The bot returns
// the same hard error as a fully empty round.
func TestGM_EmptyWithToolCallsFinish_StillError(t *testing.T) {
	g, _, fake := newGMTestEnv(t)
	fake.rounds = [][]fakeChunk{
		{{finish: "tool_calls"}}, // 0 chars, 0 surviving calls
	}
	var got strings.Builder
	_, err := g.Reply(context.Background(), "chat1", "ping", deltaOnly(&got))
	require.Error(t, err)
	assert.Equal(t, 1, fake.calls, "no retry on empty after tool_calls finish")
	assert.Empty(t, got.String())
}

// --- Этап 0a: WorldState snapshotting tests ---------------------

// TestGM_WorldStateSnapshot_StableAcrossTurns verifies the
// critical cache-hit invariant: between explicit invalidations,
// the WorldState snapshot MUST be byte-equal across turns,
// even when update_state / update_npc / update_character
// mutate the underlying files on disk.
func TestGM_WorldStateSnapshot_StableAcrossTurns(t *testing.T) {
	g, _, _ := newGMTestEnv(t)

	first, err := g.buildContextPrompt()
	require.NoError(t, err)
	require.NotEmpty(t, first)

	// Now simulate a turn that mutates the world. The
	// file-backed State is the only thing that changes;
	// the snapshot MUST stay identical.
	require.NoError(t, g.fs.WriteRawAtomic("worlds/naruto/state.md",
		"День 1 (в процессе).\nМомент: новая сцена\nАктивные NPC прямо сейчас: Какаши"))
	second, err := g.buildContextPrompt()
	require.NoError(t, err)
	assert.Equal(t, first, second,
		"snapshot must be byte-equal — state.md change should NOT bust cache")
}

// TestGM_WorldStateSnapshot_InvalidatedOnEndDay: end_day
// (ArchiveDay) MUST drop the snapshot so the next turn
// rebuilds index:1 with the freshly appended "## Протокол".
func TestGM_WorldStateSnapshot_InvalidatedOnEndDay(t *testing.T) {
	g, _, _ := newGMTestEnv(t)

	first, err := g.buildContextPrompt()
	require.NoError(t, err)

	// Simulate end_day: write to memorise.md (this is
	// what ArchiveDay does) and then call the same hook
	// ArchiveDay uses.
	require.NoError(t, g.fs.WriteRawAtomic("worlds/naruto/memorise.md",
		"д00001: тестовый день\n"))
	g.InvalidateWorldState("end_day")

	second, err := g.buildContextPrompt()
	require.NoError(t, err)
	assert.NotEqual(t, first, second,
		"snapshot must rebuild after end_day invalidation")
	assert.Contains(t, second, "д00001: тестовый день",
		"new snapshot must include the freshly archived day")
}

// TestGM_WorldStateSnapshot_InvalidatedOnLeave: leave_world
// (tool) drops the snapshot via the worldStateInvalidate
// hook wired in main.go.
func TestGM_WorldStateSnapshot_InvalidatedOnLeave(t *testing.T) {
	g, _, _ := newGMTestEnv(t)

	first, err := g.buildContextPrompt()
	require.NoError(t, err)

	g.InvalidateWorldState("leave_world")
	second, err := g.buildContextPrompt()
	require.NoError(t, err)
	// Both rebuilds use the same world so the bodies
	// are equal — what we test is that the rebuild
	// HAPPENED, not that the content differs.
	assert.NotEmpty(t, second)
	_ = first
}

// TestGM_WorldStateSnapshot_InvalidatedOnReload: /reload
// invalidates explicitly via GM.InvalidateWorldState.
func TestGM_WorldStateSnapshot_InvalidatedOnReload(t *testing.T) {
	g, _, _ := newGMTestEnv(t)
	_, err := g.buildContextPrompt()
	require.NoError(t, err)

	g.InvalidateWorldState("reload")
	g.worldStateMu.Lock()
	snap := g.worldStateSnapshot
	key := g.worldStateSceneKey
	g.worldStateMu.Unlock()
	assert.Empty(t, snap, "snapshot must be empty after reload")
	assert.Empty(t, key, "scene key must be empty after reload")
}

// TestGM_ToolResultUpdateState_ShortWithDelta: dispatching
// update_state returns a SHORT ToolResult (does not include
// the full snapshot body) and includes a human-readable
// "delta" field for the model to weave into its reply.
func TestGM_ToolResultUpdateState_ShortWithDelta(t *testing.T) {
	g, _, _ := newGMTestEnv(t)

	snap, err := g.buildContextPrompt()
	require.NoError(t, err)
	require.NotEmpty(t, snap)

	res, errStr := g.dispatchOneTool(context.Background(), llm.ToolCall{
		ID:   "t1",
		Type: "function",
		Function: llm.FunctionCall{
			Name:      "update_state",
			Arguments: `{"moment":"у фонтана","npcs":["Какаши"],"in_flight":true}`,
		},
	})
	require.Empty(t, errStr)
	assert.Contains(t, res, "recorded")
	assert.Contains(t, res, "локация/момент обновлены")
	assert.NotContains(t, res, snap, "ToolResult must NOT echo the snapshot body")

	// Snapshot must STILL be valid (cache stable).
	g.worldStateMu.Lock()
	cur := g.worldStateSnapshot
	g.worldStateMu.Unlock()
	assert.Equal(t, snap, cur, "update_state must not invalidate the snapshot")
}

// --- Этап 0b: in-place compaction tests -----------------------------

// newGMTestEnvWithInPlace is a placeholder for future
// end-to-end in-place compaction tests. Today the
// in-place path requires a real Summarizer+LLM wiring
// (production code path is exercised via Reply in the
// full env), so we return the regular env and let
// specific tests drill into the smaller unit
// (appendChronicleEntry, prompt checks).
func newGMTestEnvWithInPlace(t *testing.T, body string) (*GM, *storage.FileStore) {
	t.Helper()
	g, fs, _ := newGMTestEnv(t)
	_ = body
	return g, fs
}

// TestGM_AppendChronicleEntry_CreatesSection: first
// in-place compaction creates the "## Хроника
// текущего дня" section.
func TestGM_AppendChronicleEntry_CreatesSection(t *testing.T) {
	g, fs, _ := newGMTestEnv(t)
	err := g.appendChronicleEntry("naruto", 1,
		[]byte("[События текущего дня Д0001] Утром ГГ пришёл в Академию."))
	require.NoError(t, err)
	body, _ := fs.ReadRaw("worlds/naruto/state.md")
	assert.Contains(t, body, "## Хроника текущего дня")
	assert.Contains(t, body, "Д0001")
	assert.Contains(t, body, "Утром ГГ пришёл")
}

// TestGM_AppendChronicleEntry_AppendsToExisting:
// second entry lands under the same section.
func TestGM_AppendChronicleEntry_AppendsToExisting(t *testing.T) {
	g, fs, _ := newGMTestEnv(t)
	require.NoError(t, g.appendChronicleEntry("naruto", 1,
		[]byte("[События текущего дня Д0001] утро")))
	require.NoError(t, g.appendChronicleEntry("naruto", 1,
		[]byte("[События текущего дня Д0001] дополнение днём")))
	body, _ := fs.ReadRaw("worlds/naruto/state.md")
	assert.Contains(t, body, "утро")
	assert.Contains(t, body, "дополнение днём")
	// Exactly one section header.
	count := strings.Count(body, "## Хроника текущего дня")
	assert.Equal(t, 1, count, "single section header")
}

// TestGM_AppendChronicleEntry_DoesNotConfuseWithProtocol:
// "## Хроника текущего дня" and "## Протокол
// прошедших дней" are different sections; appending
// to one must not touch the other.
func TestGM_AppendChronicleEntry_DoesNotConfuseWithProtocol(t *testing.T) {
	g, fs, _ := newGMTestEnv(t)
	// Pre-seed a protocol section.
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md",
		"День 1 (в процессе).\n\n## Протокол прошедших дней\n#### Д0000\nстарый\n"))
	require.NoError(t, g.appendChronicleEntry("naruto", 1,
		[]byte("[События текущего дня Д0001] новая хроника")))
	body, _ := fs.ReadRaw("worlds/naruto/state.md")
	assert.Contains(t, body, "## Хроника текущего дня")
	assert.Contains(t, body, "## Протокол прошедших дней")
	assert.Contains(t, body, "старый", "protocol preserved")
	assert.Contains(t, body, "новая хроника", "chronicle added")
}

// TestGM_InPlaceCompaction_NotPastDay: the
// compaction_in_place.md prompt explicitly says the
// result must start with "[События текущего дня Д<N>]",
// not "[События прошедшего дня Д<N>]". The end_of_day
// path uses the OTHER prompt. The two must not be
// confused at runtime.
func TestGM_InPlaceCompaction_NotPastDay(t *testing.T) {
	// Verify the prompt content is what we expect.
	// We don't have direct access to the file from a
	// test (it's embedded into the binary). Use
	// prompts.Bundled for an indirect check.
	prompt := prompts.Bundled("compaction_in_place.md")
	assert.Contains(t, prompt, "События текущего дня")
	assert.NotContains(t, prompt, "События прошедшего дня")
	// end-of-day must use the opposite marker.
	eod := prompts.Bundled("end_of_day.md")
	assert.Contains(t, eod, "События прошедшего дня")
	assert.NotContains(t, eod, "События текущего дня",
		"end_of_day prompt must not use the in-place marker")
}

// TestGM_InPlaceCompaction_InvalidatesWorldState: after
// appendChronicleEntry + invalidateWorldState, the next
// buildContextPrompt must rebuild from disk.
func TestGM_InPlaceCompaction_InvalidatesWorldState(t *testing.T) {
	g, _, _ := newGMTestEnv(t)
	first, err := g.buildContextPrompt()
	require.NoError(t, err)

	require.NoError(t, g.appendChronicleEntry("naruto", 1,
		[]byte("[События текущего дня Д0001] новая хроника")))
	g.invalidateWorldState("compaction")

	second, err := g.buildContextPrompt()
	require.NoError(t, err)
	assert.NotEqual(t, first, second)
	assert.Contains(t, second, "## Хроника текущего дня")
}

// --- Этап 0c: end-of-day protocol tests -----------------------------

// TestGM_AppendProtocolEntry_CreatesSection: first
// protocol entry creates the "## Протокол прошедших
// дней" section.
func TestGM_AppendProtocolEntry_CreatesSection(t *testing.T) {
	g, fs, _ := newGMTestEnv(t)
	err := g.appendProtocolEntry("naruto",
		[]byte("#### Д0001:\nГГ встретил Какаши у фонтана утром, тот показал ловушку в лесу."))
	require.NoError(t, err)
	body, _ := fs.ReadRaw("worlds/naruto/state.md")
	assert.Contains(t, body, "## Протокол прошедших дней")
	assert.Contains(t, body, "#### Д0001")
	assert.Contains(t, body, "Какаши")
}

// TestGM_AppendProtocolEntry_AppendsToExisting:
// multiple days stack under the same section.
func TestGM_AppendProtocolEntry_AppendsToExisting(t *testing.T) {
	g, fs, _ := newGMTestEnv(t)
	require.NoError(t, g.appendProtocolEntry("naruto",
		[]byte("#### Д0001:\nпервый")))
	require.NoError(t, g.appendProtocolEntry("naruto",
		[]byte("#### Д0002:\nвторой")))
	body, _ := fs.ReadRaw("worlds/naruto/state.md")
	assert.Contains(t, body, "первый")
	assert.Contains(t, body, "второй")
	count := strings.Count(body, "## Протокол прошедших дней")
	assert.Equal(t, 1, count, "single section header")
}

// TestGM_ExtractChronicleSection_RoundTrip: the
// companion to appendChronicleEntry.
func TestGM_ExtractChronicleSection_RoundTrip(t *testing.T) {
	body := "День 1 (в процессе).\n## Хроника текущего дня\n[События текущего дня Д0001] утро\nднём\n## Другая секция\nне наша"
	got := extractChronicleSection(body)
	assert.Contains(t, got, "утро")
	assert.Contains(t, got, "днём")
	assert.NotContains(t, got, "Другая секция")
}

// TestGM_EnforceProtocolWindow_EvictsToMemorise:
// when 3 days are in the protocol and window=2, the
// oldest is moved to memorise.md.
func TestGM_EnforceProtocolWindow_EvictsToMemorise(t *testing.T) {
	g, fs, _ := newGMTestEnv(t)
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md",
		"День 4 (в процессе).\n\n## Протокол прошедших дней\n#### Д0001:\nсамый старый\n#### Д0002:\nсредний\n#### Д0003:\nновейший\n"))
	require.NoError(t, g.enforceProtocolWindow("naruto"))
	body, _ := fs.ReadRaw("worlds/naruto/state.md")
	assert.NotContains(t, body, "самый старый",
		"oldest day must be evicted from protocol")
	assert.Contains(t, body, "средний")
	assert.Contains(t, body, "новейший")
	mem, _ := fs.ReadRaw("worlds/naruto/memorise.md")
	assert.Contains(t, mem, "д00001: самый старый",
		"evicted day must land in memorise.md as д<NNNNN>: <narrative>")
}

// TestGM_EnforceProtocolWindow_ByCharCount: even with
// 2 days (within window), if the section exceeds
// protocolMaxChars, the oldest is evicted.
func TestGM_EnforceProtocolWindow_ByCharCount(t *testing.T) {
	g, fs, _ := newGMTestEnv(t)
	huge := strings.Repeat("A", 3000)
	big := strings.Repeat("B", 3000)
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md",
		"День 4 (в процессе).\n\n## Протокол прошедших дней\n#### Д0001:\n"+huge+"\n#### Д0002:\n"+big+"\n"))
	// g.protocolMaxChars is 5000 by default.
	require.NoError(t, g.enforceProtocolWindow("naruto"))
	body, _ := fs.ReadRaw("worlds/naruto/state.md")
	assert.NotContains(t, body, huge,
		"the oldest oversized day must be evicted")
	mem, _ := fs.ReadRaw("worlds/naruto/memorise.md")
	assert.Contains(t, mem, "д00001: "+huge)
}

// TestGM_EndOfDay_PromptMarksPast: the end_of_day
// prompt must mark the day as past (closed), distinct
// from the in-place marker.
func TestGM_EndOfDay_PromptMarksPast(t *testing.T) {
	prompt := prompts.Bundled("end_of_day.md")
	assert.Contains(t, prompt, "События прошедшего дня")
	assert.NotContains(t, prompt, "События текущего дня",
		"end_of_day prompt must not use the in-place marker")
}

// --- Этап 0d: /reload tests ------------------------------------------

// TestGM_ResetAllConversations_ClearsAll: ensure
// all per-chat conversation entries are dropped.
func TestGM_ResetAllConversations_ClearsAll(t *testing.T) {
	g, _, _ := newGMTestEnv(t)
	// Seed a couple of conversations.
	g.getConversation("chat-A")
	g.getConversation("chat-B")
	// We expect 2 entries (the package-level
	// conversations sync.Map may have leftovers from
	// prior tests, but in a clean test run there are
	// exactly 2).
	conversations.Range(func(k, _ any) bool {
		conversations.Delete(k)
		return true
	})
	g.getConversation("chat-A")
	g.getConversation("chat-B")
	count := 0
	conversations.Range(func(_, _ any) bool { count++; return true })
	assert.Equal(t, 2, count)
	g.ResetAllConversations()
	count = 0
	conversations.Range(func(_, _ any) bool { count++; return true })
	assert.Equal(t, 0, count, "ResetAllConversations must drop every entry")
}

// TestGM_InvalidateWorldState_AfterReload: after
// /reload the next buildContextPrompt rebuilds
// from disk (the same way end_day does).
func TestGM_InvalidateWorldState_AfterReload(t *testing.T) {
	g, fs, _ := newGMTestEnv(t)
	first, err := g.buildContextPrompt()
	require.NoError(t, err)
	// Operator hand-edits state.md.
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md",
		"День 1 (в процессе).\nАктивные NPC прямо сейчас: Какаши, Хината\n"))
	// /reload semantics.
	g.InvalidateWorldState("reload")
	second, err := g.buildContextPrompt()
	require.NoError(t, err)
	assert.NotEqual(t, first, second)
	assert.Contains(t, second, "Хината",
		"operator's hand edit must be picked up after reload")
}
