package usecase

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"narrative/internal/adapter/llm"
	"narrative/internal/adapter/storage"
	"narrative/internal/domain"
	"narrative/internal/slowlog"
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
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/characters/kakashi.md", "# Какаши\nспокойный"))

	ss := NewSessionStart(fs)
	mt := NewMaintenance(fs)
	fl := NewFirstLaunch(fs)
	npcm := NewNPCManager(fs)
	wt := NewWorldTransition(fs)
	cu := NewCharacterUpdate(fs, discardLogger(), slowlog.Discard())
	fake := &fakeLLM{}
	log, _ := newBufLogger()
	g := NewGM(GMConfig{
		Role:         llm.RoleConfig{Model: "test", MaxTokens: 100, Temperature: 0.5},
		SystemPrompt: "# rules",
		Compaction:   CompactionConfig{ContextWindow: 0, Threshold: 0.7, KeepRecent: 5},
	}, fs, fake, ss, mt, fl, npcm, wt, cu, nil, NewSystemState(fs, discardLogger(), slowlog.Discard()), slowlog.Discard(), "off", false, log)
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
	many := make([][]fakeChunk, 6)
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

func TestGM_EmptyContentSkipsReprompt(t *testing.T) {
	g, _, fake := newGMTestEnv(t)
	// Round that produces no content at all (model returned
	// just [DONE] with no deltas). This is the case that
	// triggered `reprompt_chars: 0` in the wild.
	fake.rounds = [][]fakeChunk{
		{{finish: "stop"}},
	}
	var got strings.Builder
	_, err := g.Reply(context.Background(), "chat1", "ping", deltaOnly(&got))
	require.NoError(t, err)
	// Critical: only ONE LLM call. A re-prompt would be a
	// wasted call (no content to fix) and would also need
	// its own fake round, blowing up `fake.calls`.
	assert.Equal(t, 1, fake.calls, "should not run a second round for an empty assistant turn")
	// The player should see *something* — a polite placeholder
	// so the "…" stream does not stay frozen.
	assert.Contains(t, got.String(), "не вернула ответ")
}

func TestGM_EmptyContentSendsPlaceholderDelta(t *testing.T) {
	g, _, fake := newGMTestEnv(t)
	fake.rounds = [][]fakeChunk{
		{{finish: "stop"}},
	}
	var got strings.Builder
	_, err := g.Reply(context.Background(), "chat1", "ping", deltaOnly(&got))
	require.NoError(t, err)
	// The placeholder must be the only text in the stream —
	// no other content from the model leaked through.
	assert.Equal(t, "⚠️ модель не вернула ответ — попробуй ещё раз", got.String())
}
