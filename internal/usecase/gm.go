package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/rs/zerolog"

	"narrative/internal/adapter/llm"
	"narrative/internal/adapter/storage"
	"narrative/internal/domain"
)

// LLMClient is the minimal surface GM needs from the LLM. It mirrors
// the production llm.Client behaviour but is an interface so tests
// can swap in a stub without a real HTTP server.
type LLMClient interface {
	Stream(ctx context.Context, req llm.ChatRequest, onChunk func(llm.Chunk) error) error
}

// GM is the Game Master. It owns the conversation with the LLM and
// the dispatch of tool calls back into the usecase layer.
type GM struct {
	fs           *storage.FileStore
	llm          LLMClient
	role         llm.RoleConfig
	staticPrompt string
	ss           *SessionStart
	mt           *Maintenance
	fl           *FirstLaunch
	npcm         *NPCManager
	wt           *WorldTransition
	log          zerolog.Logger

	// tools cached on construction; immutable.
	tools []llm.ToolSchema
}

// GMConfig carries everything GM needs to bootstrap. Kept separate
// from the constructor so tests can populate it without touching the
// file store directly.
type GMConfig struct {
	Role         llm.RoleConfig
	SystemPrompt string // raw text loaded from prompts/narrative.md
}

// NewGM constructs the GM. The supplied system prompt is sent as
// the first "system" message on every turn. The conversation state
// (per ChatID) is kept in a sync.Map; the GM is therefore safe to
// call from multiple goroutines.
func NewGM(cfg GMConfig, fs *storage.FileStore, llmCli LLMClient, ss *SessionStart, mt *Maintenance, fl *FirstLaunch, npcm *NPCManager, wt *WorldTransition, log zerolog.Logger) *GM {
	log = log.With().Str("component", "gm").Logger()
	tools := make([]llm.ToolSchema, 0, len(domain.Tools()))
	for _, t := range domain.Tools() {
		tools = append(tools, llm.ToolSchema{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	return &GM{
		fs:           fs,
		llm:          llmCli,
		role:         cfg.Role,
		staticPrompt: cfg.SystemPrompt,
		ss:           ss,
		mt:           mt,
		fl:           fl,
		npcm:         npcm,
		wt:           wt,
		log:          log,
		tools:        tools,
	}
}

// conversation keeps per-chat history. Kept private to the GM.
type conversation struct {
	mu       sync.Mutex
	messages []llm.Message
}

var conversations sync.Map // map[chatID]*conversation

func (g *GM) getConversation(chatID string) *conversation {
	v, ok := conversations.Load(chatID)
	if ok {
		return v.(*conversation)
	}
	c := &conversation{}
	actual, _ := conversations.LoadOrStore(chatID, c)
	return actual.(*conversation)
}

// ResetConversation clears the per-chat history. Called when the
// player switches worlds or starts a new session.
func (g *GM) ResetConversation(chatID string) {
	conversations.Delete(chatID)
}

// Reply is the streaming entry point. It builds the prompt context,
// calls the LLM, dispatches any tool calls, and pushes the resulting
// text to the supplied callback. The callback returns an error to
// abort mid-stream (typically the transport's context cancellation).
//
// maxToolRounds caps the number of tool-call rounds so a runaway
// model cannot loop forever. 5 is enough for any realistic session.
func (g *GM) Reply(ctx context.Context, chatID, userText string, onDelta func(string) error) error {
	const maxToolRounds = 5

	conv := g.getConversation(chatID)
	conv.mu.Lock()
	conv.messages = append(conv.messages, llm.Message{Role: "user", Content: userText})
	history := append([]llm.Message(nil), conv.messages...)
	conv.mu.Unlock()

	for round := 0; round < maxToolRounds; round++ {
		// System prompt + current context is rebuilt on every tool
		// round so tool-modified state.md is visible to the model.
		ctxPrompt, err := g.buildContextPrompt()
		if err != nil {
			return fmt.Errorf("gm: build context: %w", err)
		}
		messages := make([]llm.Message, 0, len(history)+1)
		messages = append(messages, llm.Message{Role: "system", Content: ctxPrompt})
		messages = append(messages, history...)

		var (
			assistantBuf strings.Builder
			toolCalls    []llm.ToolCall
			finishReason string
		)
		streamErr := g.llm.Stream(ctx, llm.ChatRequest{
			Model:       g.role.Model,
			Messages:    messages,
			Tools:       g.tools,
			Temperature: g.role.Temperature,
			MaxTokens:   g.role.MaxTokens,
		}, func(ch llm.Chunk) error {
			if ch.Done {
				return nil
			}
			if ch.Content != "" {
				assistantBuf.WriteString(ch.Content)
				if err := onDelta(ch.Content); err != nil {
					return err
				}
			}
			if len(ch.ToolCalls) > 0 {
				toolCalls = mergeToolCalls(toolCalls, ch.ToolCalls)
			}
			if ch.Finish != "" {
				finishReason = ch.Finish
			}
			return nil
		})
		if streamErr != nil {
			return streamErr
		}

		// Persist the assistant turn regardless of finish reason.
		conv.mu.Lock()
		conv.messages = append(conv.messages, llm.Message{
			Role:      "assistant",
			Content:   assistantBuf.String(),
			ToolCalls: toolCalls,
		})
		conv.mu.Unlock()
		history = append(history, llm.Message{
			Role: "assistant",
			Content: assistantBuf.String(), ToolCalls: toolCalls,
		})

		// If the model only wanted to talk, we're done.
		if len(toolCalls) == 0 || finishReason != "tool_calls" {
			return nil
		}

		// Execute every tool the model requested and append the
		// tool-role messages so the next round sees the results.
		results := g.executeTools(ctx, toolCalls)
		conv.mu.Lock()
		conv.messages = append(conv.messages, results...)
		conv.mu.Unlock()
		history = append(history, results...)
		toolCalls = nil
	}
	return fmt.Errorf("gm: tool loop exceeded %d rounds", maxToolRounds)
}

// buildContextPrompt loads the current game-data and produces the
// "what's happening right now" half of the system message. The
// static skill rules are prepended by the caller.
func (g *GM) buildContextPrompt() (string, error) {
	if !g.fs.Exists("info.md") {
		return domain.BuildSystemPrompt(g.staticPrompt, domain.PromptContext{}), nil
	}
	sc, err := g.ss.Start()
	if err != nil {
		return "", err
	}
	ctx := domain.PromptContext{
		Character:         sc.Character,
		World:             sc.World,
		CharacterSOUL:     safeRead(g.fs, "characters/"+sc.Character+"/SOUL.md"),
		CharacterSKILL:    safeRead(g.fs, "characters/"+sc.Character+"/SKILL.md"),
		CharacterMemory:   safeRead(g.fs, "characters/"+sc.Character+"/memory.md"),
		WorldCanon:        safeRead(g.fs, "worlds/"+sc.World+"/canon.md"),
		WorldState:        safeRead(g.fs, "worlds/"+sc.World+"/state.md"),
		WorldLore:         safeRead(g.fs, "worlds/"+sc.World+"/lore.md"),
		WorldPlan:         safeRead(g.fs, "worlds/"+sc.World+"/plan.md"),
		WorldMemoriseTail: tailMemorise(safeRead(g.fs, "worlds/"+sc.World+"/memorise.md"), 20),
		NPCs:              g.loadActiveNPCs(sc.World, sc.State),
	}
	return domain.BuildSystemPrompt(g.staticPrompt, ctx), nil
}

func safeRead(fs *storage.FileStore, rel string) string {
	s, _ := fs.ReadRaw(rel)
	return s
}

// tailMemorise returns the last N lines of the memorise file so the
// LLM sees recent days without burning context on the full archive.
func tailMemorise(body string, n int) string {
	if body == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) <= n {
		return body
	}
	return strings.Join(lines[len(lines)-n:], "\n") + "\n"
}

// loadActiveNPCs reads the world state, extracts the comma-separated
// list from the "Активные NPC прямо сейчас" line, and loads each
// NPC's profile file. Info-isolation is enforced upstream by the
// state.md editor: NPCs that should not appear simply aren't
// mentioned.
func (g *GM) loadActiveNPCs(world, state string) []domain.NPCSnapshot {
	marker := "Активные NPC прямо сейчас:"
	idx := strings.Index(state, marker)
	if idx < 0 {
		return nil
	}
	rest := state[idx+len(marker):]
	end := strings.Index(rest, "\n")
	if end < 0 {
		end = len(rest)
	}
	names := strings.Split(rest[:end], ",")
	var out []domain.NPCSnapshot
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		body, err := g.npcm.Load(world, name)
		if err != nil {
			g.log.Warn().Err(err).Str("npc", name).Msg("skip npc load")
			continue
		}
		out = append(out, domain.NPCSnapshot{DisplayName: name, Profile: body})
	}
	return out
}

// executeTools dispatches every requested tool call and returns the
// tool-role messages ready to be appended to the conversation.
func (g *GM) executeTools(ctx context.Context, calls []llm.ToolCall) []llm.Message {
	out := make([]llm.Message, 0, len(calls))
	for _, tc := range calls {
		result, errText := g.dispatchOneTool(ctx, tc)
		out = append(out, llm.Message{
			Role:       "tool",
			Name:       tc.Function.Name,
			ToolCallID: tc.ID,
			Content:    result,
		})
		if errText != "" {
			g.log.Warn().Str("tool", tc.Function.Name).Str("err", errText).Msg("tool error")
		}
	}
	return out
}

// dispatchOneTool is the tool-to-usecase bridge. The argument JSON
// is decoded into the matching usecase type and the result is
// rendered as a short JSON-ish text the model can read.
func (g *GM) dispatchOneTool(_ context.Context, tc llm.ToolCall) (string, string) {
	args := map[string]any{}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return "", "invalid arguments: " + err.Error()
	}
	switch tc.Function.Name {
	case "end_day":
		day := toInt(args["day"])
		summary := toString(args["summary"])
		if day == 0 || summary == "" {
			return "", "end_day requires day and summary"
		}
		if err := g.mt.ArchiveDay(currentWorldName(g.fs), day, summary); err != nil {
			return "", err.Error()
		}
		return okJSON("archived"), ""
	case "run_maintenance":
		touched, err := g.mt.CompactNPCs(currentWorldName(g.fs))
		if err != nil {
			return "", err.Error()
		}
		return okJSON(map[string]any{"compacted": touched}), ""
	case "update_state":
		moment := toString(args["moment"])
		inFlight := toBool(args["in_flight"])
		npcs := toStringSlice(args["npcs"])
		if moment == "" {
			return "", "update_state requires moment"
		}
		// Day number is taken from the current state.md; if absent,
		// default to 1 so the LLM can keep going.
		day := readCurrentDay(g.fs, currentWorldName(g.fs))
		if err := g.mt.UpdateState(StateSnapshot{
			Day: day, InFlight: inFlight, Moment: moment, NPCs: npcs,
		}); err != nil {
			return "", err.Error()
		}
		return okJSON("state updated"), ""
	case "rotate_plan":
		events := toStringSlice(args["events"])
		if err := g.mt.RotatePlan(currentWorldName(g.fs), events); err != nil {
			return "", err.Error()
		}
		return okJSON("plan rotated"), ""
	case "create_npc":
		spec := NPCProfile{
			DisplayName: toString(args["display_name"]),
			File:        toString(args["file_slug"]),
			Temperament: toString(args["temperament"]),
			Relations:   toString(args["relations"]),
			Abilities:   toString(args["abilities"]),
			Nicknames:   toStringSlice(args["nicknames"]),
		}
		if err := g.npcm.Create(currentWorldName(g.fs), spec); err != nil {
			return "", err.Error()
		}
		return okJSON("npc created"), ""
	case "leave_world":
		to := toString(args["to_world"])
		skip := toString(args["skip_note"])
		// We need the from-world which lives in info.md.
		sc, err := g.ss.Start()
		if err != nil {
			return "", err.Error()
		}
		if _, err := g.wt.Leave(sc.World, to, skip, sc.Character); err != nil {
			return "", err.Error()
		}
		g.ResetConversation(sc.Character) // best-effort, no chatID available
		return okJSON("world switched"), ""
	}
	return "", "unknown tool: " + tc.Function.Name
}

// --- helpers ---

// mergeToolCalls stitches together the partial tool-call chunks
// that OpenAI emits across a stream. Each chunk may carry the full
// id+name (the "head") or just an arguments fragment (the "tail");
// tails accumulate into the most recent head.
func mergeToolCalls(acc, incoming []llm.ToolCall) []llm.ToolCall {
	for _, ic := range incoming {
		if ic.ID != "" || ic.Function.Name != "" {
			// Head: start a new tool call.
			acc = append(acc, ic)
			continue
		}
		// Tail: append arguments to the most recent head.
		if len(acc) == 0 {
			acc = append(acc, ic)
			continue
		}
		acc[len(acc)-1].Function.Arguments += ic.Function.Arguments
	}
	return acc
}

func okJSON(payload any) string {
	b, _ := json.Marshal(map[string]any{"ok": true, "result": payload})
	return string(b)
}

func toInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}

func toBool(v any) bool {
	b, _ := v.(bool)
	return b
}

func toStringSlice(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		s, ok := x.(string)
		if !ok {
			continue
		}
		out = append(out, s)
	}
	return out
}

func currentWorldName(fs *storage.FileStore) string {
	raw, _ := fs.ReadRaw("info.md")
	if raw == "" {
		return ""
	}
	_, w, err := domain.ParseInfo(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(w.Pointer, "worlds/")
}

func readCurrentDay(fs *storage.FileStore, world string) int {
	raw, _ := fs.ReadRaw("worlds/" + world + "/state.md")
	idx := strings.Index(raw, "День ")
	if idx < 0 {
		return 1
	}
	rest := raw[idx+len("День "):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 1
	}
	n, _ := strconv.Atoi(rest[:end])
	if n == 0 {
		return 1
	}
	return n
}
