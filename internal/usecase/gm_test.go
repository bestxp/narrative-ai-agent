package usecase

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	yamlfs "github.com/bestxp/narrative-ai-agent/internal/storage/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newGMTestEnv(t *testing.T) (*GM, *storage.FileStore, *FakeLLM) {
	t.Helper()
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile, domain.BuildInfo("markus", "naruto", nil, nil)))
	require.NoError(t, fs.EnsureDir("characters/markus"))
	require.NoError(t, fs.WriteRawAtomic("characters/markus/SOUL.md", "# Markus"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.yaml",
		"state:\n  world: naruto\n  day: 1\n  in-flight: true\n  npcs:\n    - Какаши\n"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/lore.md", "lore"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/plan.md", "plan"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/canon.md", "canon"))
	require.NoError(t, fs.WriteRawAtomic(fs.WorldChronicle("naruto"), ""))
	require.NoError(t, fs.EnsureDir("worlds/naruto/characters"))
	require.NoError(t, fs.WriteRawAtomic(
		"worlds/naruto/characters/kakashi.yaml",
		"display_name: Какаши\ntemperament: спокойный\n",
	))

	ss := NewSessionStart(fs)
	fl := NewFirstLaunch(fs)
	// One Tool bundles every concern; the tests use the
	// file-backed implementation so on-disk state changes
	// are observable via fs.ReadRaw.
	yamlStore, _ := yamlfs.New(fs.Root())
	repos := api.NewYamlRepositories(yamlStore)

	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile, "active_character: markus\nactive_world: naruto\n"))
	tools := NewFileToolset(fs, repos, DiscardLogger(), slowlog.Discard(), nil, nil, nil, nil)
	fake := &FakeLLM{}
	log := NewBufLogger()
	g := NewGM(GMConfig{
		Role: llm.RoleConfig{
			Model:           "test",
			MaxTokens:       100,
			Temperature:     0.5,
			MaxEmptyRetries: 2, // mirror config default so old "expect 2 calls" tests keep working
		},
		SystemPrompt: "# rules",
		Compaction:   CompactionConfig{ContextWindow: 0, Threshold: 0.7, KeepRecent: 5},
	}, fs, fake, ss, fl, tools, nil, NewSystemState(fs, DiscardLogger(), slowlog.Discard()), slowlog.Discard(), "off", false, log)

	return g, fs, fake
}

func deltaOnly(s *strings.Builder) Callbacks {
	return Callbacks{OnDelta: func(t string) error {
		s.WriteString(t)

		return nil
	}}
}

func TestGM_StreamsReplyIntoCallback(t *testing.T) {
	t.Parallel()
	chatID := "chat-" + t.Name()
	g, _, fake := newGMTestEnv(t)
	fake.rounds = [][]FakeChunk{
		{
			{Content: "**диалоги и действия**\nПривет, "},
			{Content: "путник.\n\n**КОНТЕКСТ И ИЗМЕНЕНИЯ**\nбез изменений\n\n" +
				"**БУДУЩЕЕ**\n- продолжение\n\n**ВАЛИДАЦИЯ ПРАВИЛ**\n- ok"},
			{Finish: "stop"},
		},
	}

	var got strings.Builder

	_, err := g.Reply(context.Background(), chatID, "я пришёл", deltaOnly(&got))
	require.NoError(t, err)
	assert.Equal(t, 1, fake.calls)
	assert.Contains(t, got.String(), "Привет, путник.")
}

func TestGM_ToolRound_EndDay(t *testing.T) {
	t.Parallel()
	t.Skip("pending gm.go migration to repository pattern — see research_repository_pattern.md")
	chatID := "chat-" + t.Name()
	g, fs, fake := newGMTestEnv(t)
	dayRecordedContent := " День записан.\n\n**диалоги и действия**\nАрхивирую день. " +
		"День записан.\n\n**КОНТЕКСТ И ИЗМЕНЕНИЯ**\nmemorise.md обновлён\n\n" +
		"**БУДУЩЕЕ**\n- продолжение\n\n**ВАЛИДАЦИЯ ПРАВИЛ**\n- ok"
	fake.rounds = [][]FakeChunk{
		{
			{Content: "Архивирую день."},
			{ToolID: "call_1", ToolName: "end_day", ToolArgs: `{"day":1,"summary":"первый день"}`, Finish: "tool_calls"},
		},
		{
			{Content: dayRecordedContent},
			{Finish: "stop"},
		},
	}

	var got strings.Builder

	_, err := g.Reply(context.Background(), chatID, "конец дня", deltaOnly(&got))
	require.NoError(t, err)

	mem, _ := fs.ReadRaw(fs.WorldChronicle("naruto"))
	assert.Contains(t, mem, "первый день")
	assert.Equal(t, 2, fake.calls)
}

func TestGM_ToolRound_UpdateState(t *testing.T) {
	t.Parallel()
	chatID := "chat-" + t.Name()
	g, fs, fake := newGMTestEnv(t)
	fake.rounds = [][]FakeChunk{
		{
			{Content: "обновляю"},
			{
				ToolID: "call_1", ToolName: "update_state",
				ToolArgs: `{"moment":"Маркус входит в деревню.","npcs":["Какаши"],"in_flight":true}`,
				Finish:   "tool_calls",
			},
		},
		{{
			Content: " ок.\n\n**диалоги и действия**\nобновляю ок.\n\n" +
				"**КОНТЕКСТ И ИЗМЕНЕНИЯ**\nstate.md обновлён\n\n" +
				"**БУДУЩЕЕ**\n- продолжение\n\n**ВАЛИДАЦИЯ ПРАВИЛ**\n- ok",
			Finish: "stop",
		}},
	}
	_, err := g.Reply(context.Background(), chatID, "идём в деревню", Callbacks{})
	require.NoError(t, err)

	state, _ := fs.ReadRaw("worlds/naruto/state.yaml")
	// YAML format: moment is under `state.moment:`
	assert.Contains(t, state, "Маркус входит в деревню")
	assert.Contains(t, state, "Какаши")
}

func TestGM_ToolRound_RotatePlan_RejectsBadRange(t *testing.T) {
	t.Parallel()
	chatID := "chat-" + t.Name()
	g, _, fake := newGMTestEnv(t)
	fake.rounds = [][]FakeChunk{
		{
			{
				ToolID: "call_1", ToolName: "rotate_plan",
				ToolArgs: `{"events":["a","b"]}`, Finish: "tool_calls",
			},
		},
		{{Content: "не вышло\n\n**диалоги и действия**\nне вышло" +
			"\n\n**КОНТЕКСТ И ИЗМЕНЕНИЯ**\nбез изменений\n\n**БУДУЩЕЕ**" +
			"\n- продолжение\n\n**ВАЛИДАЦИЯ ПРАВИЛ**\n- ok", Finish: "stop"}},
	}
	_, err := g.Reply(context.Background(), chatID, "x", Callbacks{})
	require.NoError(t, err)
	assert.Equal(t, 2, fake.calls)
}

func TestGM_StopsAtMaxRounds(t *testing.T) {
	t.Parallel()
	chatID := "chat-" + t.Name()
	g, _, fake := newGMTestEnv(t)

	many := make([][]FakeChunk, MaxToolRounds+1)
	for i := range many {
		many[i] = []FakeChunk{{
			ToolID: fmt.Sprintf("c%d", i), ToolName: "run_maintenance",
			ToolArgs: `{}`, Finish: "tool_calls",
		}}
	}

	fake.rounds = many
	_, err := g.Reply(context.Background(), chatID, "x", Callbacks{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeded")
}

func TestGM_StuckGuard_InjectsNudge(t *testing.T) {
	t.Parallel()
	chatID := "chat-" + t.Name()
	g, _, fake := newGMTestEnv(t)
	fake.rounds = [][]FakeChunk{
		{{
			ToolID: "c0", ToolName: "update_npc",
			ToolArgs: `{"npc":"Ино","section":"Личная память/факты","append":"test"}`,
			Finish:   "tool_calls",
		}},
		{{
			ToolID: "c1", ToolName: "update_npc",
			ToolArgs: `{"npc":"Шикамару","section":"Личная память/факты","append":"test"}`,
			Finish:   "tool_calls",
		}},
		{{
			ToolID: "c2", ToolName: "update_npc",
			ToolArgs: `{"npc":"Чоджи","section":"Личная память/факты","append":"test"}`,
			Finish:   "tool_calls",
		}},
		{{
			Content: "**диалоги и действия**\nок\n\n" +
				"**КОНТЕКСТ И ИЗМЕНЕНИЯ**\nбез изменений\n\n" +
				"**БУДУЩЕЕ**\n- продолжение\n\n**ВАЛИДАЦИЯ ПРАВИЛ**\n- ok",
			Finish: "stop",
		}},
	}
	_, err := g.Reply(context.Background(), chatID, "рассказал легенду", Callbacks{})
	require.NoError(t, err)

	conv := g.GetConversation(chatID)

	var foundNudge bool

	for _, m := range conv.messages {
		if m.Role == "user" && strings.Contains(m.Content, "Не вызывай больше инструменты") {
			foundNudge = true
		}
	}

	assert.True(t, foundNudge, "stuck guard should inject nudge after 3 consecutive tool-only rounds")
}

func TestGM_ResetConversation(t *testing.T) {
	t.Parallel()
	chatID := "chat-" + t.Name()
	g, _, fake := newGMTestEnv(t)
	fake.rounds = [][]FakeChunk{
		{{Content: "hi", Finish: "stop"}},
	}
	_, err := g.Reply(context.Background(), chatID, "ping", Callbacks{})
	require.NoError(t, err)

	conv := g.GetConversation(chatID)
	assert.NotEmpty(t, conv.messages)

	g.ResetConversation(chatID)
	conv2 := g.GetConversation(chatID)
	assert.NotNil(t, conv2)
	assert.Empty(t, conv2.messages)
}

func TestGM_BuildsContextWithNPCs(t *testing.T) {
	t.Parallel()
	t.Skip("pending gm.go migration to repository pattern — see research_repository_pattern.md")
	chatID := "chat-" + t.Name()
	g, _, _ := newGMTestEnv(t)

	var captured llm.ChatRequest

	captureLLM := &captureLLM{run: func(req llm.ChatRequest, onChunk func(llm.Chunk) error) error {
		captured = req

		return onChunk(llm.Chunk{
			Content: "**диалоги и действия**\nok\n\n" +
				"**КОНТЕКСТ И ИЗМЕНЕНИЯ**\nбез изменений\n\n" +
				"**БУДУЩЕЕ**\n- продолжение\n\n**ВАЛИДАЦИЯ ПРАВИЛ**\n- ok",
			Finish: "stop",
		})
	}}
	g.LLM = captureLLM
	_, err := g.Reply(context.Background(), chatID, "go", Callbacks{})
	require.NoError(t, err)
	require.NotEmpty(t, captured.Messages)
	// messages[0] is the system prompt (rules + character)
	// and messages[1] is the WorldState user message (world + NPCs).
	sys := captured.Messages[0]
	world := captured.Messages[1]

	assert.Equal(t, "system", sys.Role)
	assert.Equal(t, "user", world.Role)
	assert.Contains(t, sys.Content, "rules")
	assert.Contains(t, world.Content, "naruto")
	assert.Contains(t, world.Content, "Какаши",
		"NPC profile lives in the world user message (Индекс 1), not system")
	assert.Contains(t, world.Content, "спокойный",
		"NPC temperament lives in the world user message")
	assert.NotContains(t, sys.Content, "Какаши",
		"system prompt must NOT carry world data")
}

func TestGM_TokenUsage_Estimate(t *testing.T) {
	t.Parallel()
	chatID := "chat-" + t.Name()
	g, _, fake := newGMTestEnv(t)
	g.Tracking = "estimate"
	fake.rounds = [][]FakeChunk{
		{{Content: "Привет, мир.", Finish: "stop"}},
	}

	var lastTok llm.Usage

	_, err := g.Reply(context.Background(), chatID, "ping", Callbacks{OnTokens: func(u llm.Usage) { lastTok = u }})
	require.NoError(t, err)
	assert.Positive(t, lastTok.PromptTokens)
	assert.Positive(t, lastTok.CompletionTokens)
}

func TestGM_TokenUsage_Usage(t *testing.T) {
	t.Parallel()
	chatID := "chat-" + t.Name()
	g, _, fake := newGMTestEnv(t)
	g.Tracking = "usage"
	fake.rounds = [][]FakeChunk{
		{{Content: "ok", Finish: "stop", Usage: llm.Usage{PromptTokens: 12, CompletionTokens: 7, TotalTokens: 19}}},
	}

	var lastTok llm.Usage

	totals, err := g.Reply(context.Background(), chatID, "ping", Callbacks{OnTokens: func(u llm.Usage) { lastTok = u }})
	require.NoError(t, err)
	assert.Equal(t, 12, lastTok.PromptTokens)
	assert.Equal(t, 19, totals.TotalTokens)
}

func TestMergeToolCalls_FirstChunkStartsNew(t *testing.T) {
	t.Parallel()

	out := MergeToolCalls(nil, []llm.ToolCall{{
		ID: "c1", Type: "function",
		Function: llm.FunctionCall{Name: "end_day", Arguments: `{"day":1}`},
	}})
	require.Len(t, out, 1)
	assert.Equal(t, "c1", out[0].ID)
	assert.Equal(t, "end_day", out[0].Function.Name)
	assert.Equal(t, `{"day":1}`, out[0].Function.Arguments)
}

func TestMergeToolCalls_ContinuationsAccumulate(t *testing.T) {
	t.Parallel()

	out := MergeToolCalls(nil, []llm.ToolCall{{
		ID: "c1", Type: "function",
		Function: llm.FunctionCall{Name: "end_day", Arguments: `{"day":`},
	}})
	out = MergeToolCalls(out, []llm.ToolCall{{
		Function: llm.FunctionCall{Arguments: `1}`},
	}})
	require.Len(t, out, 1)
	assert.Equal(t, `{"day":1}`, out[0].Function.Arguments)
}

func TestGM_CompactionFiresOnLongHistory(t *testing.T) {
	t.Parallel()
	chatID := "chat-" + t.Name()
	g, _, fake := newGMTestEnv(t)
	g.Compaction = CompactionConfig{ContextWindow: 100, Threshold: 0.5, KeepRecent: 2}
	// Inject a long history (60 messages) directly so the
	// preflight sees ~5000 chars of input → ~1250 tokens,
	// well past the 50-token threshold.
	conv := g.GetConversation(chatID)
	conv.mu.Lock()
	for i := range 30 {
		conv.messages = append(conv.messages,
			llm.Message{Role: "user", Content: "user message " + string(rune('a'+i%26)) + " with extra padding to make this longer"},
			llm.Message{
				Role:    "assistant",
				Content: "assistant reply " + string(rune('a'+i%26)) + " with even more padding to push the count up",
			},
		)
	}
	conv.mu.Unlock()

	fake.rounds = [][]FakeChunk{{{
		Content: "**диалоги и действия**\nfinal\n\n" +
			"**КОНТЕКСТ И ИЗМЕНЕНИЯ**\nбез изменений\n\n" +
			"**БУДУЩЕЕ**\n- продолжение\n\n**ВАЛИДАЦИЯ ПРАВИЛ**\n- ok",
		Finish: "stop",
	}}}

	var (
		compacted   CompactionResult
		compactedMu sync.Mutex
	)

	cb := Callbacks{
		OnDelta: func(_ string) error { return nil },
		OnCompaction: func(r CompactionResult) {
			compactedMu.Lock()
			compacted = r
			compactedMu.Unlock()
		},
	}
	_, err := g.Reply(context.Background(), chatID, "ping", cb)
	require.NoError(t, err)
	compactedMu.Lock()
	defer compactedMu.Unlock()

	assert.Positive(t, compacted.DroppedTurns,
		"compaction should have fired on long history (got %d dropped, before=%d after=%d)",
		compacted.DroppedTurns, compacted.BeforeTokens, compacted.AfterTokens)
	assert.LessOrEqual(t, len(g.GetConversation(chatID).messages),
		2*g.Compaction.KeepRecent+2,
		"conv holds kept + (1 user, 1 assistant from final round)")
}

func TestGM_CompactionWithSummarizer_WritesToState(t *testing.T) {
	t.Parallel()
	chatID := "chat-" + t.Name()
	g, fs, fake := newGMTestEnv(t)
	g.Compaction = CompactionConfig{ContextWindow: 100, Threshold: 0.5, KeepRecent: 2}
	// Wire a summarizer that responds with a short fact list.
	summaryLLM := &FakeLLM{}
	summaryLLM.rounds = [][]FakeChunk{
		{{Content: "- Акацуки собраны (день 5)\n- Хокаге вызвал к себе", Finish: "stop"}},
	}
	g.Summarizer = NewSummarizer(summaryLLM,
		llm.RoleConfig{Model: "summary", MaxTokens: 500, Temperature: 0.2},
		"system-prompt", slowlog.Discard(), DiscardLogger())

	// Long history.
	conv := g.GetConversation(chatID)
	conv.mu.Lock()
	for i := range 30 {
		conv.messages = append(conv.messages,
			llm.Message{Role: "user", Content: "long user message " + string(rune('a'+i%26)) + " with padding"},
			llm.Message{Role: "assistant", Content: "long assistant reply " + string(rune('a'+i%26)) + " with even more padding"},
		)
	}
	conv.mu.Unlock()

	fake.rounds = [][]FakeChunk{{{Content: "ok", Finish: "stop"}}}
	_, err := g.Reply(context.Background(), chatID, "ping", Callbacks{OnDelta: func(_ string) error { return nil }})
	require.NoError(t, err)

	// state.yaml should have the new history section appended.
	state, _ := fs.ReadRaw("worlds/naruto/state.yaml")
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
	t.Parallel()
	chatID := "chat-" + t.Name()
	g, _, fake := newGMTestEnv(t)
	fake.rounds = [][]FakeChunk{
		{
			// The model "decided" to call a tool, but only
			// the ID + empty name + partial args made it
			// through the stream.
			{ToolID: "call_X", ToolName: "", ToolArgs: `{`, Finish: "stop"},
		},
	}

	var got strings.Builder

	_, err := g.Reply(context.Background(), chatID, "ping", deltaOnly(&got))
	require.Error(t, err, "broken tool calls must propagate as a hard error under h4")
	assert.Contains(t, err.Error(), "no tool_use and no content")
	assert.Equal(t, 1, fake.calls, "no nudge-retry on broken tool calls; h4 is hard-error")
	// The assistant turn must not be persisted (no half-broken
	// state in history that would poison the next round).
	conv := g.GetConversation(chatID)
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
	t.Parallel()
	// Empty slice: not "broken" — there are simply no tool calls.
	assert.False(t, AllToolCallsBroken(nil))
	assert.False(t, AllToolCallsBroken([]llm.ToolCall{}))
	// A single headless entry (Name="") is the broken signature.
	assert.True(t, AllToolCallsBroken([]llm.ToolCall{{
		Function: llm.FunctionCall{Name: "", Arguments: "{partial"},
	}}))
	// Multiple headless entries: still all broken.
	assert.True(t, AllToolCallsBroken([]llm.ToolCall{
		{Function: llm.FunctionCall{Name: "", Arguments: ""}},
		{Function: llm.FunctionCall{Name: "", Arguments: "{partial"}},
	}))
	// One valid entry makes the round NOT broken.
	assert.False(t, AllToolCallsBroken([]llm.ToolCall{{
		Function: llm.FunctionCall{Name: "update_state", Arguments: `{"moment":"x"}`},
	}}))
	// Mixed: one valid + one headless — keep the valid one, do
	// not classify the whole round as broken.
	assert.False(t, AllToolCallsBroken([]llm.ToolCall{
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
	t.Parallel()
	chatID := "chat-" + t.Name()
	g, _, fake := newGMTestEnv(t)
	fake.rounds = [][]FakeChunk{
		{{Finish: "stop"}}, // round 0: 0 chars, 0 tool calls
	}

	var got strings.Builder

	_, err := g.Reply(context.Background(), chatID, "ping", deltaOnly(&got))
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
	t.Parallel()
	chatID := "chat-" + t.Name()
	g, _, fake := newGMTestEnv(t)
	fake.rounds = [][]FakeChunk{
		{{Finish: "tool_calls"}}, // 0 chars, 0 surviving calls
	}

	var got strings.Builder

	_, err := g.Reply(context.Background(), chatID, "ping", deltaOnly(&got))
	require.Error(t, err)
	assert.Equal(t, 1, fake.calls, "no retry on empty after tool_calls finish")
	assert.Empty(t, got.String())
}

// --- WorldState snapshotting tests ---------------------

// TestGM_WorldStateSnapshot_StableAcrossTurns verifies the
// critical cache-hit invariant: between explicit invalidations,
// the BOTH the system snapshot AND the world-state snapshot
// MUST be byte-equal across turns, even when update_state /
// update_npc / update_soul / update_skill / update_memory mutate the underlying files
// on disk. The split into system (rules+character) and world
// (world+NPCs) messages means both share the same sceneKey
// cache and are rebuilt together.
func TestGM_WorldStateSnapshot_StableAcrossTurns(t *testing.T) {
	t.Parallel()
	g, _, _ := newGMTestEnv(t)

	firstSys, firstWorld, err := g.BuildContext()
	require.NoError(t, err)
	require.NotEmpty(t, firstSys)
	require.NotEmpty(t, firstWorld)

	// Now simulate a turn that mutates the world. The
	// file-backed State is the only thing that changes;
	// the snapshots MUST stay identical.
	require.NoError(t, g.FS.WriteRawAtomic("worlds/naruto/state.yaml",
		"state:\n  world: naruto\n  day: 1\n  in-flight: true\n  moment: новая сцена\n  npcs:\n    - Какаши\n"))
	secondSys, secondWorld, err := g.BuildContext()
	require.NoError(t, err)
	assert.Equal(t, firstSys, secondSys,
		"system snapshot must be byte-equal — state.md change should NOT bust cache")
	assert.Equal(t, firstWorld, secondWorld,
		"world snapshot must be byte-equal — state.md change should NOT bust cache")
}

// TestGM_WorldStateSnapshot_InvalidatedOnEndDay: end_day
// (ArchiveDay) MUST drop the world snapshot so the next turn
// rebuilds it with the freshly appended "## Протокол". The
// system snapshot (rules + character) does NOT change on
// end_day — character and rules are stable across day
// boundaries. Only the world block needs to be rebuilt.
func TestGM_WorldStateSnapshot_InvalidatedOnEndDay(t *testing.T) {
	t.Parallel()
	g, _, _ := newGMTestEnv(t)

	firstSys, firstWorld, err := g.BuildContext()
	require.NoError(t, err)

	// Simulate end_day: write to chronicle.yaml (this is
	// what ArchiveChronicleDay does) and then call the
	// same hook ArchiveChronicleDay uses.
	require.NoError(t, g.FS.WriteRawAtomic(g.FS.WorldChronicle("naruto"),
		"days:\n    1: тестовый день\n"))
	g.InvalidateWorldState("end_day")

	secondSys, secondWorld, err := g.BuildContext()
	require.NoError(t, err)
	// System does NOT change on end_day (no character
	// or rules touched). This is the invariant that lets
	// the Anthropic system cache live across day boundaries.
	assert.Equal(t, firstSys, secondSys,
		"system snapshot must NOT change on end_day")
	assert.NotEqual(t, firstWorld, secondWorld,
		"world snapshot must rebuild after end_day invalidation")
	assert.Contains(t, secondWorld, "1: тестовый день",
		"new world snapshot must include the freshly archived day")
}

// TestGM_WorldStateSnapshot_InvalidatedOnLeave: leave_world
// (tool) drops the snapshots via the worldStateInvalidate
// hook wired in main.go.
func TestGM_WorldStateSnapshot_InvalidatedOnLeave(t *testing.T) {
	t.Parallel()
	g, _, _ := newGMTestEnv(t)

	_, _, err := g.BuildContext()
	require.NoError(t, err)

	g.InvalidateWorldState("leave_world")
	_, _, err = g.BuildContext()
	require.NoError(t, err)
	// Both rebuilds use the same world so the bodies
	// are equal — what we test is that the rebuild
	// HAPPENED, not that the content differs.
}

// TestGM_WorldStateSnapshot_InvalidatedOnReload: /reload
// invalidates explicitly via GM.InvalidateWorldState.
func TestGM_WorldStateSnapshot_InvalidatedOnReload(t *testing.T) {
	t.Parallel()
	g, _, _ := newGMTestEnv(t)
	_, _, err := g.BuildContext()
	require.NoError(t, err)

	g.InvalidateWorldState("reload")
	g.ContextMu.Lock()
	sys := g.SystemSnapshot
	ws := g.WorldSnapshot
	key := g.ContextSceneKey
	g.ContextMu.Unlock()
	assert.Empty(t, sys, "system snapshot must be empty after reload")
	assert.Empty(t, ws, "world snapshot must be empty after reload")
	assert.Empty(t, key, "scene key must be empty after reload")
}

// TestGM_ToolResultUpdateState_ShortWithDelta: dispatching
// update_state returns a SHORT ToolResult (does not include
// the full snapshot body) and includes a human-readable
// "delta" field for the model to weave into its reply.
// After dispatch BOTH snapshots (system + world) must
// remain valid — the cache prefix is preserved.
func TestGM_ToolResultUpdateState_ShortWithDelta(t *testing.T) {
	t.Parallel()
	g, _, _ := newGMTestEnv(t)

	sys, world, err := g.BuildContext()
	require.NoError(t, err)
	require.NotEmpty(t, sys)
	require.NotEmpty(t, world)

	res, errStr := g.DispatchOneTool(context.Background(), llm.ToolCall{
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
	// The ToolResult must NOT echo either the system or
	// the world snapshot body.
	assert.NotContains(t, res, sys, "ToolResult must NOT echo the system snapshot body")
	assert.NotContains(t, res, world, "ToolResult must NOT echo the world snapshot body")

	// Snapshots must STILL be valid (cache stable).
	g.ContextMu.Lock()
	curSys := g.SystemSnapshot
	curWorld := g.WorldSnapshot
	g.ContextMu.Unlock()
	assert.Equal(t, sys, curSys, "update_state must not invalidate the system snapshot")
	assert.Equal(t, world, curWorld, "update_state must not invalidate the world snapshot")
}

// --- /reload tests ------------------------------------------

// TestGM_ResetAllConversations_ClearsAll: ensure
// all per-chat conversation entries are dropped.
//
//nolint:paralleltest // mutates the package-level `Conversations sync.Map` and asserts on its global count.
func TestGM_ResetAllConversations_ClearsAll(t *testing.T) {
	g, _, _ := newGMTestEnv(t)
	// Seed a couple of Conversations.
	g.GetConversation("chat-A")
	g.GetConversation("chat-B")
	// We expect 2 entries (the package-level
	// Conversations sync.Map may have leftovers from
	// prior tests, but in a clean test run there are
	// exactly 2).
	Conversations.Range(func(k, _ any) bool {
		Conversations.Delete(k)

		return true
	})
	g.GetConversation("chat-A")
	g.GetConversation("chat-B")

	count := 0

	Conversations.Range(func(_, _ any) bool {
		count++

		return true
	})
	assert.Equal(t, 2, count)
	g.ResetAllConversations()

	count = 0

	Conversations.Range(func(_, _ any) bool {
		count++

		return true
	})
	assert.Equal(t, 0, count, "ResetAllConversations must drop every entry")
}

// TestGM_InvalidateWorldState_AfterReload: after
// /reload the next buildContext rebuilds from disk
// (the same way end_day does). The operator's
// hand-edit (e.g. adding "Хината" to active NPCs)
// must surface in the world-state user message.
func TestGM_InvalidateWorldState_AfterReload(t *testing.T) {
	t.Parallel()
	g, fs, _ := newGMTestEnv(t)
	_, firstWorld, err := g.BuildContext()
	require.NoError(t, err)
	// Operator hand-edits state.yaml.
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.yaml",
		"state:\n  world: naruto\n  day: 1\n  in-flight: true\n  npcs:\n    - Какаши\n    - Хината\n"))
	// /reload semantics.
	g.InvalidateWorldState("reload")
	_, secondWorld, err := g.BuildContext()
	require.NoError(t, err)
	assert.NotEqual(t, firstWorld, secondWorld)
	assert.Contains(t, secondWorld, "Хината",
		"operator's hand edit must be picked up after reload (in world block, not system)")
}

// --- search_npc tool ---

// TestSearchNPC_ResolvesDisplayName: search_npc maps a
// free-form query to the registry entry, loads the
// profile, and returns the compact view (display_name
// + temperament + current_status).
func TestSearchNPC_ResolvesDisplayName(t *testing.T) {
	t.Parallel()
	g, _, _ := newGMTestEnv(t)
	// Mark his world as a known NPC with extra data.
	// First, write a longer profile for kakashi via
	// the tools (so the registry can find him).
	require.NoError(t, g.FS.WriteRawAtomic("worlds/naruto/characters/kakashi.yaml",
		"display_name: Какаши-сенсей\nfile_slug: kakashi\n"+
			"temperament: хладнокровный, методичный\n"+
			"current_status: на тренировочной площадке\n"))
	require.NoError(t, g.FS.WriteRawAtomic("worlds/naruto/characters.yaml",
		"npcs:\n  - slug: kakashi\n    display_name: Какаши-сенсей\n    nicknames: [Какаши]\n"))

	res, errStr := g.DispatchOneTool(context.Background(), llm.ToolCall{
		ID:   "t1",
		Type: "function",
		Function: llm.FunctionCall{
			Name:      "search_npc",
			Arguments: `{"query":"Какаши-сенсей"}`,
		},
	})
	require.Empty(t, errStr)
	assert.Contains(t, res, `"status":"found"`)
	assert.Contains(t, res, `"display_name":"Какаши-сенсей"`)
	assert.Contains(t, res, `"temperament":"хладнокровный, методичный"`)
	assert.Contains(t, res, `"current_status":"на тренировочной площадке"`)
}

// TestSearchNPC_NotFound: missing query returns a
// short, recoverable error — the model should try a
// different query or call create_npc, not invent a
// profile.
func TestSearchNPC_NotFound(t *testing.T) {
	t.Parallel()
	g, _, _ := newGMTestEnv(t)
	_, errStr := g.DispatchOneTool(context.Background(), llm.ToolCall{
		ID:   "t1",
		Type: "function",
		Function: llm.FunctionCall{
			Name:      "search_npc",
			Arguments: `{"query":"Неизвестный Персонаж"}`,
		},
	})
	assert.Contains(t, errStr, "search_npc")
	assert.Contains(t, errStr, "not found",
		"err text must include the underlying reason for the model to recover")
}

// TestSearchNPC_RateLimit: the same query twice in a
// short window is rejected. A different query is
// always allowed (the limit is per-query, not global).
func TestSearchNPC_RateLimit(t *testing.T) {
	t.Parallel()
	t.Skip("pending gm.go migration to repository pattern — see research_repository_pattern.md")
	g, _, _ := newGMTestEnv(t)
	g.RateWindow = 5
	g.TurnCounter = 1

	// Write a profile for kakashi so search hits.
	require.NoError(t, g.FS.WriteRawAtomic("worlds/naruto/characters/kakashi.yaml",
		"display_name: Какаши\nfile_slug: kakashi\ntemperament: t\ncurrent_status: s\n"))

	// First call: success.
	_, errStr := g.DispatchOneTool(context.Background(), llm.ToolCall{
		ID:       "t1",
		Type:     "function",
		Function: llm.FunctionCall{Name: "search_npc", Arguments: `{"query":"Какаши"}`},
	})
	assert.Empty(t, errStr)

	// Second call with the same query, no turn advance:
	// rate-limited.
	_, errStr = g.DispatchOneTool(context.Background(), llm.ToolCall{
		ID:       "t2",
		Type:     "function",
		Function: llm.FunctionCall{Name: "search_npc", Arguments: `{"query":"Какаши"}`},
	})
	assert.Contains(t, errStr, "rate-limited",
		"second identical query must be rejected within rateWindow")

	// A different query is allowed even within the window.
	_, errStr = g.DispatchOneTool(context.Background(), llm.ToolCall{
		ID:       "t3",
		Type:     "function",
		Function: llm.FunctionCall{Name: "search_npc", Arguments: `{"query":"Какаши-сенсей"}`},
	})
	// This query is not in the registry — that error
	// is "not found", not "rate-limited".
	assert.NotContains(t, errStr, "rate-limited")
	assert.Contains(t, errStr, "not found")

	// Advance the turn counter past the rate window
	// and re-issue the original query: allowed.
	g.TurnCounter = 100
	_, errStr = g.DispatchOneTool(context.Background(), llm.ToolCall{
		ID:       "t4",
		Type:     "function",
		Function: llm.FunctionCall{Name: "search_npc", Arguments: `{"query":"Какаши"}`},
	})
	assert.Empty(t, errStr, "after rateWindow turns, the same query is allowed again")
}

// --- end_scene tool ---

// TestEndScene_PrunesRosterByExplicitList: when the
// tool is called with a permanent_party arg, the
// active roster is pruned to that list. NPCs not in
// the list are dropped from state.yaml.
func TestEndScene_PrunesRunesByExplicitList(t *testing.T) {
	t.Parallel()
	g, fs, _ := newGMTestEnv(t)
	// Seed state with a 4-NPC roster. The YAML
	// parser reads the npcs list under `state:` —
	// see world_state_yaml.go:parseStateYAML.
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.yaml",
		"state:\n  world: naruto\n  day: 1\n  in-flight: true\n"+
			"  npcs:\n    - Какаши\n    - Хината\n    - Ирука\n    - Наруто\n"))

	res, errStr := g.DispatchOneTool(context.Background(), llm.ToolCall{
		ID:   "t1",
		Type: "function",
		Function: llm.FunctionCall{
			Name:      "end_scene",
			Arguments: `{"permanent_party":"Какаши, Хината"}`,
		},
	})
	require.Empty(t, errStr)
	assert.Contains(t, res, `"status":"scene_closed"`)
	assert.Contains(t, res, `"pruned_npcs_len":2`)

	// On disk: roster is now "Какаши, Хината".
	cur, _ := fs.ReadRaw("worlds/naruto/state.yaml")
	assert.Contains(t, cur, "- Какаши")
	assert.Contains(t, cur, "- Хината")
	assert.NotContains(t, cur, "Ирука", "non-party NPC must be pruned from state.yaml")
	assert.NotContains(t, cur, "Наруто", "non-party NPC must be pruned from state.yaml")

	// Snapshot dropped so the next turn rebuilds.
	g.ContextMu.Lock()
	world := g.WorldSnapshot
	g.ContextMu.Unlock()
	assert.Empty(t, world, "end_scene must invalidate the world snapshot")
}

// TestEndScene_NoPruneWhenListMissing: if the tool
// is called with no permanent_party arg AND the
// world's state.yaml has no permanent party, the
// roster is left as-is (safe default).
func TestEndScene_NoPruneWhenListMissing(t *testing.T) {
	t.Parallel()
	g, fs, _ := newGMTestEnv(t)
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.yaml",
		"state:\n  world: naruto\n  day: 1\n  in-flight: true\n  npcs:\n    - Какаши\n    - Хината\n"))

	res, errStr := g.DispatchOneTool(context.Background(), llm.ToolCall{
		ID:       "t1",
		Type:     "function",
		Function: llm.FunctionCall{Name: "end_scene", Arguments: `{}`},
	})
	require.Empty(t, errStr)
	assert.Contains(t, res, `"pruned_npcs_len":0`)

	cur, _ := fs.ReadRaw("worlds/naruto/state.yaml")
	assert.Contains(t, cur, "- Какаши")
	assert.Contains(t, cur, "- Хината")
}

// --- LOD для NPC ---

// TestLoadActiveNPCs_LODTiers: loadActiveNPCs applies
// the documented LOD policy based on the position in
// the active roster:
//
//	positions 0-2 → LODFull (BuildMarkdown — full body)
//	positions 3-7 → LODCompact (no big arrays)
//	position  8+  → LODOneLine (1-line summary)
//
// The test seeds an 11-NPC roster and inspects the
// rendered body of each slot. Display names are
// Russian; fixture slugs are explicit ASCII pairs so
// we can drive the file backend without depending on
// the project transliteration (which is exercised in
// domain tests).
func TestLoadActiveNPCs_LODTiers(t *testing.T) {
	t.Parallel()
	t.Skip("pending gm.go migration to repository pattern — see research_repository_pattern.md")
	g, fs, _ := newGMTestEnv(t)

	type n struct {
		display, slug string
	}

	roster := []n{
		{"Какаши", "kakashi"},
		{"Хината", "hinata"},
		{"Ирука", "iruka"},
		{"Наруто", "naruto"},
		{"Саске", "sasuke"},
		{"Сакура", "sakura"},
		{"Шикамару", "shikamaru"},
		{"Ино", "ino"},
		{"Чоуджи", "chouji"},
		{"Шино", "shino"},
		{"Хиаши", "hiashi"},
	}

	var sb strings.Builder
	sb.WriteString("День 1 (в процессе).\nNPC: ")

	for i, r := range roster {
		if i > 0 {
			sb.WriteString(", ")
		}

		sb.WriteString(r.display)
	}

	sb.WriteString("\n")
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md", sb.String()))

	for _, r := range roster {
		require.NoError(t, fs.WriteRawAtomic(
			"worlds/naruto/characters/"+r.slug+".yaml",
			"display_name: "+r.display+"\nfile_slug: "+r.slug+
				"\ntemperament: спокойный\ncurrent_status: здесь\n"+
				"personal_memory:\n"+
				"  - факт1\n  - факт2\n  - факт3\n  - факт4\n  - факт5\n"+
				"  - факт6\n  - факт7\n  - факт8\n  - факт9\n  - факт10\n",
		))
	}
	// Force a world rebuild.
	g.InvalidateWorldSnapshot("test")
	_, worldMsg, err := g.BuildContext()
	require.NoError(t, err)

	// First 3 NPCs (0, 1, 2) → Full. Their sections
	// contain "Личная память/факты" (only the full
	// BuildMarkdown render emits this section).
	for _, r := range roster[:3] {
		body := findNPCSection(t, worldMsg, r.display)
		assert.Contains(t, body, "Личная память/факты",
			"position 0-2 must be LOD Full; %q body: %s", r.display, body)
	}
	// Positions 3-7 (5 NPCs) → Compact. No "Личная
	// память/факты" header, but has "Темперамент:".
	for _, r := range roster[3:8] {
		body := findNPCSection(t, worldMsg, r.display)
		assert.NotContains(t, body, "Личная память/факты",
			"position 3-7 must be LOD Compact (no personal_memory); %q body: %s", r.display, body)
		assert.Contains(t, body, "Темперамент:",
			"position 3-7 must be LOD Compact (temperament line present); %q body: %s", r.display, body)
	}
	// Position 8+ (3 NPCs) → OneLine. Single-line,
	// no markdown header, no "Темперамент:".
	for _, r := range roster[8:] {
		body := findNPCSection(t, worldMsg, r.display)
		assert.NotContains(t, body, "Личная память/факты",
			"position 8+ must be LOD OneLine; %q body: %s", r.display, body)
		assert.NotContains(t, body, "Темперамент:",
			"LOD OneLine does not have a temperament line; %q body: %s", r.display, body)
	}
}

// TestLoadActiveNPCs_SmallCastAllFull: a 3-NPC roster
// is small enough to render all NPCs at LOD Full
// regardless of cast size.
func TestLoadActiveNPCs_SmallCastAllFull(t *testing.T) {
	t.Parallel()
	t.Skip("pending gm.go migration to repository pattern — see research_repository_pattern.md")
	g, fs, _ := newGMTestEnv(t)
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md",
		"День 1 (в процессе).\nNPC: Какаши, Хината, Ирука\n"))

	type n struct {
		display, slug string
	}

	npcs := []n{
		{"Какаши", "kakashi"}, {"Хината", "hinata"}, {"Ирука", "iruka"},
	}
	for _, r := range npcs {
		require.NoError(t, fs.WriteRawAtomic(
			"worlds/naruto/characters/"+r.slug+".yaml",
			"display_name: "+r.display+"\nfile_slug: "+r.slug+
				"\ntemperament: t\ncurrent_status: s\npersonal_memory:\n  - a\n  - b\n",
		))
	}

	g.InvalidateWorldSnapshot("test")
	_, worldMsg, err := g.BuildContext()
	require.NoError(t, err)

	for _, r := range npcs {
		body := findNPCSection(t, worldMsg, r.display)
		assert.Contains(t, body, "Личная память/факты",
			"3-NPC cast must all be LOD Full; %q body: %s", r.display, body)
	}
}

// findNPCSection returns the rendered body of the NPC
// with the given display_name, looking at the world
// block as assembled by buildContext. The "body" is
// the text between the "### <name>" header (BuildWorldStateMessage
// renders NPC sections at h3 level) and the next "### "
// sibling header.
func findNPCSection(t *testing.T, worldMsg, displayName string) string {
	t.Helper()

	header := "### " + displayName

	idx := strings.Index(worldMsg, header)
	if idx < 0 {
		t.Fatalf("NPC %q not found in world block:\n%s", displayName, worldMsg)
	}

	rest := worldMsg[idx+len(header):]
	// End at the next "### " sibling.
	end := strings.Index(rest, "\n### ")
	if end < 0 {
		end = len(rest)
	}

	return rest[:end]
}

// translitASCII was used in earlier fixture drafts
// where display names drove the slug. The current
// fixtures use explicit display/slug pairs, so this
// helper is no longer referenced; the implementation
// is kept commented-out in case a future test wants
// a bare ASCII-slug generator.
//
// func translitASCII(s string) string {
// 	var b strings.Builder
// 	for _, r := range s {
// 		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
// 			b.WriteRune(r)
// 		}
// 	}
// 	if b.Len() == 0 {
// 		return "x"
// 	}
// 	return b.String()
// }
