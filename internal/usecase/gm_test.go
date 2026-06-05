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
		ch := llm.Chunk{Content: fc.content, Finish: fc.finish}
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
	fake := &fakeLLM{}
	log, _ := newBufLogger()
	g := NewGM(GMConfig{
		Role:         llm.RoleConfig{Model: "test", MaxTokens: 100, Temperature: 0.5},
		SystemPrompt: "# rules",
	}, fs, fake, ss, mt, fl, npcm, wt, log)
	return g, fs, fake
}

func TestGM_StreamsReplyIntoCallback(t *testing.T) {
	g, _, fake := newGMTestEnv(t)
	fake.rounds = [][]fakeChunk{
		{
			{content: "Привет, "},
			{content: "путник."},
			{finish: "stop"},
		},
	}
	var got strings.Builder
	require.NoError(t, g.Reply(context.Background(), "chat1", "я пришёл", func(s string) error {
		got.WriteString(s)
		return nil
	}))
	assert.Equal(t, "Привет, путник.", got.String())
	assert.Equal(t, 1, fake.calls)
}

func TestGM_ToolRound_EndDay(t *testing.T) {
	g, fs, fake := newGMTestEnv(t)
	fake.rounds = [][]fakeChunk{
		{
			{content: "Архивирую день."},
			{toolID: "call_1", toolName: "end_day", toolArgs: `{"day":1,"summary":"первый день"}`, finish: "tool_calls"},
		},
		{
			{content: " День записан."},
			{finish: "stop"},
		},
	}
	var got strings.Builder
	require.NoError(t, g.Reply(context.Background(), "chat1", "конец дня", func(s string) error {
		got.WriteString(s)
		return nil
	}))
	mem, _ := fs.ReadRaw("worlds/naruto/memorise.md")
	assert.Contains(t, mem, "д00001: первый день")
	assert.Equal(t, 2, fake.calls)
	assert.Equal(t, "Архивирую день. День записан.", got.String())
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
		{{content: " ок.", finish: "stop"}},
	}
	require.NoError(t, g.Reply(context.Background(), "chat1", "идём в деревню", func(string) error { return nil }))
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
		{{content: "не вышло", finish: "stop"}},
	}
	require.NoError(t, g.Reply(context.Background(), "chat1", "x", func(string) error { return nil }))
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
	err := g.Reply(context.Background(), "chat1", "x", func(string) error { return nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeded")
}

func TestGM_ResetConversation(t *testing.T) {
	g, _, fake := newGMTestEnv(t)
	fake.rounds = [][]fakeChunk{
		{{content: "hi", finish: "stop"}},
	}
	require.NoError(t, g.Reply(context.Background(), "chat1", "ping", func(string) error { return nil }))
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
		return onChunk(llm.Chunk{Content: "ok", Finish: "stop"})
	}}
	g.llm = captureLLM
	require.NoError(t, g.Reply(context.Background(), "chat1", "go", func(string) error { return nil }))
	require.NotEmpty(t, captured.Messages)
	assert.Contains(t, captured.Messages[0].Content, "rules")
	assert.Contains(t, captured.Messages[0].Content, "naruto")
	assert.Contains(t, captured.Messages[0].Content, "Какаши")
	assert.Contains(t, captured.Messages[0].Content, "спокойный")
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

type captureLLM struct {
	run func(req llm.ChatRequest, onChunk func(llm.Chunk) error) error
}

func (c *captureLLM) Stream(_ context.Context, req llm.ChatRequest, onChunk func(llm.Chunk) error) error {
	return c.run(req, onChunk)
}
