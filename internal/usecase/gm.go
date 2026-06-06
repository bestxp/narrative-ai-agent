package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"narrative/internal/adapter/llm"
	"narrative/internal/adapter/storage"
	"narrative/internal/domain"
	"narrative/internal/slowlog"
)

// LLMClient is the minimal surface GM needs from the LLM. It mirrors
// the production llm.Client behaviour but is an interface so tests
// can swap in a stub without a real HTTP server.
type LLMClient interface {
	Stream(ctx context.Context, req llm.ChatRequest, onChunk func(llm.Chunk) error) error
}

// StatusFunc is called between major GM phases. The transport
// layer uses it to rotate the "…" placeholder into something
// informative ("собираю контекст…", "NPC: Саске — 3 строки…")
// so the player sees the bot is alive, not just frozen on three
// dots. StatusFunc may be nil; in that case GM skips the calls.
type StatusFunc func(phase string, details map[string]any)

// OnCompactionFunc is called after the bot trims old
// conversation turns to fit under the configured context
// window. The transport layer uses it to surface a one-line
// notice ("🔄 компактирую историю...") as a separate Telegram
// message — the player gets confirmation that the context
// reset just happened and the model is reasoning with a
// tighter window. May be nil.
type OnCompactionFunc func(result CompactionResult)

// Callbacks groups the per-stream transport hooks. A zero value
// is valid — every field is optional and silently skipped when
// nil. Keeping them on a struct (rather than four positional
// arguments) leaves room for future hooks without churn.
type Callbacks struct {
	// OnDelta is called for every non-empty text delta the LLM
	// emits. Typical use: append the text to the outgoing
	// stream session.
	OnDelta func(string) error
	// OnStatus is called between major GM phases (context build,
	// LLM request, tool dispatch, ...). The transport layer uses
	// it to display an informative placeholder while the bot is
	// working. The string is a short phase label; the map is
	// optional per-phase detail (NPC names, tool name, ...).
	OnStatus StatusFunc
	// OnTokens is called once per LLM round (possibly several
	// times per Reply, when tool calls are in flight) with the
	// accumulated token usage for the round. The final call's
	// numbers are the ones the transport appends to the reply if
	// the operator enabled `llm.include_in_reply`.
	OnTokens func(llm.Usage)
	// OnCompaction is called once per Reply, after the bot has
	// trimmed old conversation turns to stay under the context
	// window. The transport layer surfaces a one-line notice so
	// the player sees the context reset. Nil is safe.
	OnCompaction OnCompactionFunc
}

// TokenUsage is the summary the GM emits at the end of a Reply.
// It is the union of provider-reported and locally-estimated
// numbers; Source is "usage" when the provider returned a block
// and "estimate" when the bot fell back. A source of "off" means
// the operator disabled accounting and the numbers are zero.
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Source           string // "usage" | "estimate" | "off"
}

// GM is the Game Master. It owns the conversation with the LLM and
// the dispatch of tool calls back into the usecase layer.
type GM struct {
	fs           *storage.FileStore
	llm          LLMClient
	role         llm.RoleConfig
	compaction   CompactionConfig
	staticPrompt string
	ss           *SessionStart
	mt           *Maintenance
	fl           *FirstLaunch
	npcm         *NPCManager
	wt           *WorldTransition
	cu           *CharacterUpdate
	summarizer   *Summarizer
	sysSt        *SystemState
	slow         *slowlog.Logger
	tracking     string // "off" | "estimate" | "usage"
	includeReply bool
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
	// Compaction is the role's context-window policy. The same
	// struct is in config.LLMRoleConfig; the GM receives its own
	// copy to keep the usecase layer independent of the config
	// package.
	Compaction CompactionConfig
}

// CompactionConfig is the slice of config.LLMRoleConfig the
// GM actually uses. Kept local so the usecase package can be
// tested without dragging in yaml.v3 dependencies.
type CompactionConfig struct {
	// ContextWindow is the soft cap on input tokens. 0 disables.
	ContextWindow int
	// Threshold is the fraction of ContextWindow at which the
	// preflight fires. 0.7 is the operator-friendly default.
	Threshold float64
	// KeepRecent is the number of freshest turns that survive
	// a compaction. 5 is the operator-friendly default.
	KeepRecent int
}

// NewGM constructs the GM. The supplied system prompt is sent as
// the first "system" message on every turn. The conversation state
// (per ChatID) is kept in a sync.Map; the GM is therefore safe to
// call from multiple goroutines.
func NewGM(cfg GMConfig, fs *storage.FileStore, llmCli LLMClient, ss *SessionStart, mt *Maintenance, fl *FirstLaunch, npcm *NPCManager, wt *WorldTransition, cu *CharacterUpdate, summarizer *Summarizer, sysSt *SystemState, slow *slowlog.Logger, tracking string, includeReply bool, log zerolog.Logger) *GM {
	log = log.With().Str("component", "gm").Logger()
	tools := make([]llm.ToolSchema, 0, len(domain.Tools()))
	for _, t := range domain.Tools() {
		raw, err := t.MarshalParameters()
		if err != nil {
			log.Warn().Err(err).Str("tool", t.Function.Name).Msg("tool schema marshal failed; skipping")
			continue
		}
		tools = append(tools, llm.ToolSchema{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  raw,
		})
	}
	return &GM{
		fs:           fs,
		llm:          llmCli,
		role:         cfg.Role,
		compaction:   cfg.Compaction,
		staticPrompt: cfg.SystemPrompt,
		ss:           ss,
		mt:           mt,
		fl:           fl,
		npcm:         npcm,
		wt:           wt,
		cu:           cu,
		summarizer:   summarizer,
		sysSt:        sysSt,
		slow:         slow,
		tracking:     tracking,
		includeReply: includeReply,
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
//
// The returned TokenUsage is the cumulative prompt/completion count
// across all rounds of this Reply. When the operator has
// token_tracking=off the struct is zero-valued and the
// dispatcher/transport can simply ignore it.
func (g *GM) Reply(ctx context.Context, chatID, userText string, cb Callbacks) (TokenUsage, error) {
	const maxToolRounds = 5
	var totals TokenUsage
	if g.tracking == "" || g.tracking == "off" {
		totals.Source = "off"
	}

	conv := g.getConversation(chatID)
	conv.mu.Lock()
	conv.messages = append(conv.messages, llm.Message{Role: "user", Content: userText})
	history := append([]llm.Message(nil), conv.messages...)
	conv.mu.Unlock()

	if cb.OnStatus != nil {
		cb.OnStatus("request_received", map[string]any{"text_len": len(userText)})
	}

	// Compaction preflight: if the conversation history has
	// grown past context_window * compaction_threshold, drop
	// the oldest turns. We run this once per Reply (not per
	// tool round) so the operator gets exactly one compaction
	// notice per user turn regardless of how many tool rounds
	// the model ran. The OnCompaction callback is the only
	// place the player sees the notice; system_state.md also
	// gets the row.
	//
	// When a summarizer role is configured, the dropped turns
	// are first compressed to a 200-400 token fact log and
	// appended to state.md so the next system prompt still
	// sees them. Without a summarizer we just drop (the
	// facts live in state.md/memorise.md/SOUL.md anyway,
	// and the operator sees a "🔄" notice all the same).
	if g.compaction.ContextWindow > 0 {
		ctxChars := len(g.staticPrompt)
		if NeedsCompaction(history, ctxChars, g.compaction.ContextWindow, g.compaction.Threshold) {
			dropCount := len(history) - g.compaction.KeepRecent
			if dropCount < 0 {
				dropCount = 0
			}
			var droppedTurns []llm.Message
			if dropCount > 0 {
				droppedTurns = history[:dropCount]
			}
			kept, res := CompactConversations(history, g.compaction.KeepRecent)
			if res.DroppedTurns > 0 {
				conv.mu.Lock()
				conv.messages = kept
				conv.mu.Unlock()
				history = kept
				now := time.Now().UTC()
				// Best-effort summary. The summarizer may be
				// unconfigured (single-role deployment) — in
				// that case the role call is skipped and we
				// still drop, just without a written fact
				// log. The "🔄" notice still fires.
				if g.summarizer != nil && g.summarizer.IsConfigured() && len(droppedTurns) > 0 {
					sumRes, sumErr := g.summarizer.SummarizeOldTurns(ctx, droppedTurns)
					if sumErr != nil {
						g.log.Warn().Err(sumErr).Msg("summary failed; dropping turns without state.md append")
					} else if sumRes.Text != "" {
						if err := g.mt.AppendHistoryToState(currentWorldName(g.fs), sumRes.Text, now); err != nil {
							g.log.Warn().Err(err).Msg("append history to state.md failed")
						} else {
							g.log.Info().Int("summary_chars", len(sumRes.Text)).Msg("summary appended to state.md")
						}
					}
				}
				if g.sysSt != nil {
					_, _ = g.sysSt.AppendCompaction(NewCompactionEvent(
						"narrative",
						res.BeforeTokens, res.AfterTokens,
						res.DroppedTurns, res.KeptRecent,
						now,
					))
				}
				if cb.OnCompaction != nil {
					cb.OnCompaction(res)
				}
				g.log.Info().
					Int("before", res.BeforeTokens).
					Int("after", res.AfterTokens).
					Int("dropped", res.DroppedTurns).
					Int("kept", res.KeptRecent).
					Msg("compaction fired")
			}
		}
	}

	for round := 0; round < maxToolRounds; round++ {
		if cb.OnStatus != nil {
			cb.OnStatus("build_context", nil)
		}
		// System prompt + current context is rebuilt on every tool
		// round so tool-modified state.md is visible to the model.
		ctxPrompt, err := g.buildContextPrompt()
		if err != nil {
			return totals, fmt.Errorf("gm: build context: %w", err)
		}
		messages := make([]llm.Message, 0, len(history)+1)
		messages = append(messages, llm.Message{Role: "system", Content: ctxPrompt})
		messages = append(messages, history...)

		if cb.OnStatus != nil {
			cb.OnStatus("llm_request", map[string]any{
				"model":         g.role.Model,
				"messages":      len(messages),
				"prompt_chars":  promptCharSize(messages),
			})
		}

		var (
			assistantBuf   strings.Builder
			toolCalls      []llm.ToolCall
			finishReason   string
			usageFromAPI   llm.Usage
			gotUsage       bool
			roundCompChars int
			rawTrace       []string
		)
		streamErr := g.llm.Stream(ctx, llm.ChatRequest{
			Model:       g.role.Model,
			Messages:    messages,
			Tools:       g.tools,
			Temperature: g.role.Temperature,
			MaxTokens:   g.role.MaxTokens,
		}, func(ch llm.Chunk) error {
			// Always grab the most recent raw trace so the
			// slowlog event below can include it on empty
			// turns. We overwrite rather than append because
			// every chunk already carries the running trace.
			if len(ch.RawTrace) > 0 {
				rawTrace = ch.RawTrace
			}
			if ch.Done {
				return nil
			}
			if ch.Content != "" {
				assistantBuf.WriteString(ch.Content)
				roundCompChars += len(ch.Content)
				if cb.OnDelta != nil {
					if err := cb.OnDelta(ch.Content); err != nil {
						return err
					}
				}
			}
			if len(ch.ToolCalls) > 0 {
				toolCalls = mergeToolCalls(toolCalls, ch.ToolCalls)
			}
			if ch.Finish != "" {
				finishReason = ch.Finish
			}
			if ch.Usage.TotalTokens > 0 || ch.Usage.PromptTokens > 0 {
				usageFromAPI = ch.Usage
				gotUsage = true
			}
			return nil
		})
		if streamErr != nil {
			return totals, streamErr
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
			Role: "assistant", Content: assistantBuf.String(), ToolCalls: toolCalls,
		})

		// Accumulate per-round token accounting.
		roundUsage := g.accountRound(messages, roundCompChars, assistantBuf.String(), gotUsage, usageFromAPI)
		totals.PromptTokens += roundUsage.PromptTokens
		totals.CompletionTokens += roundUsage.CompletionTokens
		totals.TotalTokens += roundUsage.TotalTokens
		if roundUsage.Source != "off" && totals.Source == "off" {
			totals.Source = roundUsage.Source
		} else if totals.Source == "" && roundUsage.Source != "" {
			totals.Source = roundUsage.Source
		}
		if cb.OnTokens != nil {
			cb.OnTokens(llm.Usage{
				PromptTokens:     roundUsage.PromptTokens,
				CompletionTokens: roundUsage.CompletionTokens,
				TotalTokens:      roundUsage.TotalTokens,
			})
		}
		if g.slow != nil {
			_ = g.slow.Write("llm.tokens", chatID, map[string]any{
				"round":             round,
				"prompt_tokens":     roundUsage.PromptTokens,
				"completion_tokens": roundUsage.CompletionTokens,
				"total_tokens":      roundUsage.TotalTokens,
				"source":            roundUsage.Source,
				"model":             g.role.Model,
			})
		// Diagnostic: capture finish reason + content preview so a
		// 0-byte assistant turn is visible in slow.log (not just
		// "4516 prompt, 0 completion"). Without this we can't tell
		// whether the model returned empty content, the stream
		// got cut off, or the provider sent a done frame only.
		preview := assistantBuf.String()
		if len(preview) > 500 {
			preview = preview[:500] + "…"
		}
		fields := map[string]any{
			"round":         round,
			"finish":        finishReason,
			"tool_calls":    len(toolCalls),
			"content_chars": len(assistantBuf.String()),
			"content_prev":  preview,
		}
		// When the assistant produced no text AND no tool calls,
		// the raw SSE trace is the only way to diagnose. We do
		// not log it on healthy rounds (would spam slow.log with
		// 3-5 entries per turn).
		if len(assistantBuf.String()) == 0 && len(toolCalls) == 0 {
			fields["raw_trace"] = rawTrace
		}
		_ = g.slow.Write("llm.round", chatID, fields)
		}

		// If the model only wanted to talk, we're done — but first
		// check format compliance and re-prompt on missing blocks.
		if len(toolCalls) == 0 || finishReason != "tool_calls" {
			// Skip the format gate entirely when the round
			// produced no content. Re-prompting on an empty
			// string would force us to send "[system note]
			// missing: 4 blocks" with no original to fix, and
			// the model would either repeat nothing or invent
			// text that ignores our blocks. Instead we surface
			// a polite "model returned empty" placeholder so
			// the user is not left hanging on a frozen "…".
			if len(assistantBuf.String()) == 0 {
				if cb.OnDelta != nil {
					_ = cb.OnDelta("⚠️ модель не вернула ответ — попробуй ещё раз")
				}
				g.log.Warn().
					Int("round", round).
					Str("finish", finishReason).
					Msg("llm returned empty content; skipping format gate")
				return totals, nil
			}
			if !g.isFormatCompliant(assistantBuf.String()) {
				fixed, fixedUsage, fixErr := g.repromptForFormat(ctx, chatID, history, assistantBuf.String(), cb)
				totals.PromptTokens += fixedUsage.PromptTokens
				totals.CompletionTokens += fixedUsage.CompletionTokens
				totals.TotalTokens += fixedUsage.TotalTokens
				if totals.Source == "" || totals.Source == "off" {
					totals.Source = fixedUsage.Source
				}
				if fixErr != nil {
					g.log.Warn().Err(fixErr).Msg("format re-prompt failed; returning original reply")
				} else if fixed != "" {
					if cb.OnDelta != nil {
						_ = cb.OnDelta("\n\n[формат восстановлен]\n\n" + fixed)
					}
					conv.mu.Lock()
					if len(conv.messages) > 0 {
						last := conv.messages[len(conv.messages)-1]
						if last.Role == "assistant" {
							last.Content = assistantBuf.String() + "\n\n[формат восстановлен]\n\n" + fixed
							conv.messages[len(conv.messages)-1] = last
						}
					}
					conv.mu.Unlock()
				}
			}
			return totals, nil
		}

		// Execute every tool the model requested and append the
		// tool-role messages so the next round sees the results.
		if cb.OnStatus != nil {
			names := make([]string, 0, len(toolCalls))
			for _, tc := range toolCalls {
				names = append(names, tc.Function.Name)
			}
			cb.OnStatus("tool_dispatch", map[string]any{"tools": names})
		}
		results := g.executeTools(ctx, toolCalls)
		conv.mu.Lock()
		conv.messages = append(conv.messages, results...)
		conv.mu.Unlock()
		history = append(history, results...)
		toolCalls = nil
	}
	return totals, fmt.Errorf("gm: tool loop exceeded %d rounds", maxToolRounds)
}

// accountRound converts the per-round counters into a TokenUsage.
// When tracking is "off" the result is zero and source stays "off".
// When tracking is "estimate" we always estimate regardless of
// what the provider reported. When tracking is "usage" we trust
// the provider; if it returned nothing, we estimate and warn.
func (g *GM) accountRound(messages []llm.Message, compChars int, compText string, gotUsage bool, usage llm.Usage) TokenUsage {
	switch g.tracking {
	case "", "off":
		return TokenUsage{Source: "off"}
	case "estimate":
		prompt := llm.EstimateMessages(messages)
		completion := llm.EstimateTokens(compText)
		return TokenUsage{
			PromptTokens:     prompt,
			CompletionTokens: completion,
			TotalTokens:      prompt + completion,
			Source:           "estimate",
		}
	case "usage":
		if gotUsage {
			return TokenUsage{
				PromptTokens:     usage.PromptTokens,
				CompletionTokens: usage.CompletionTokens,
				TotalTokens:      usage.TotalTokens,
				Source:           "usage",
			}
		}
		g.log.Warn().Msg("token_tracking=usage but provider returned no usage block — falling back to estimate")
		prompt := llm.EstimateMessages(messages)
		completion := llm.EstimateTokens(compText)
		return TokenUsage{
			PromptTokens:     prompt,
			CompletionTokens: completion,
			TotalTokens:      prompt + completion,
			Source:           "estimate",
		}
	}
	return TokenUsage{Source: "off"}
}

func promptCharSize(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	return n
}

// buildContextPrompt loads the current game-data and produces the
// "what's happening right now" half of the system message. The
// static skill rules are prepended by the caller.
//
// disableThinking forwards g.role.DisableThinking into
// BuildSystemPrompt so a /no_think sentinel is prepended when the
// role is configured to skip chain-of-thought. This is the
// in-prompt half of the dual switch — the wire-level
// chat_template_kwargs.think=false is the other half. Providers
// that ignore the wire flag (Ollama Cloud minimax-m3:cloud today)
// still respond to the prompt directive.
func (g *GM) buildContextPrompt() (string, error) {
	if !g.fs.Exists(storage.InfoFile) {
		return domain.BuildSystemPrompt(g.staticPrompt, domain.PromptContext{}, g.role.DisableThinking), nil
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
	return domain.BuildSystemPrompt(g.staticPrompt, ctx, g.role.DisableThinking), nil
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
		events := toStringSlice(args["events"])
		if moment == "" {
			return "", "update_state requires moment"
		}
		// Day number is taken from the current state.md; if absent,
		// default to 1 so the LLM can keep going.
		day := readCurrentDay(g.fs, currentWorldName(g.fs))
		if err := g.mt.UpdateState(StateSnapshot{
			Day: day, InFlight: inFlight, Moment: moment, NPCs: npcs,
			AppendEvents: events,
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
		// We need the from-world which lives in the registry (info.yaml).
		sc, err := g.ss.Start()
		if err != nil {
			return "", err.Error()
		}
		if _, err := g.wt.Leave(sc.World, to, skip, sc.Character); err != nil {
			return "", err.Error()
		}
		g.ResetConversation(sc.Character) // best-effort, no chatID available
		return okJSON("world switched"), ""
	case "update_character":
		if g.cu == nil {
			return "", "update_character: character updater not wired"
		}
		file := toString(args["file"])
		section := toString(args["section"])
		appendText := toString(args["append"])
		sc, err := g.ss.Start()
		if err != nil {
			return "", err.Error()
		}
		if err := g.cu.Append(sc.Character, file, section, appendText); err != nil {
			return "", err.Error()
		}
		return okJSON(map[string]any{
			"file":    file,
			"section": section,
		}), ""
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
	raw, _ := fs.ReadRaw(storage.InfoFile)
	if raw == "" {
		return ""
	}
	parsed, err := domain.ParseInfo(raw)
	if err != nil {
		return ""
	}
	return parsed.ActiveWorld
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

// requiredFormatHeaders is the contract the GM enforces on every
// assistant message. The model is told the four-block structure
// in prompts/narrative.md; this list is the same one the
// dispatcher reads. Keeping the names in code lets us detect
// drift between the prompt and the validator at boot.
var requiredFormatHeaders = []string{
	"**диалоги и действия**",
	"**КОНТЕКСТ И ИЗМЕНЕНИЯ**",
	"**БУДУЩЕЕ**",
	"**ВАЛИДАЦИЯ ПРАВИЛ**",
}

func (g *GM) isFormatCompliant(text string) bool {
	if text == "" {
		return false
	}
	for _, h := range requiredFormatHeaders {
		if !strings.Contains(text, h) {
			return false
		}
	}
	return true
}

func (g *GM) missingFormatHeaders(text string) []string {
	if text == "" {
		return requiredFormatHeaders
	}
	var missing []string
	for _, h := range requiredFormatHeaders {
		if !strings.Contains(text, h) {
			missing = append(missing, h)
		}
	}
	return missing
}

// repromptForFormat appends a corrective user message to history
// and runs one more LLM round. The corrective text is short and
// neutral — it only lists the headers that were missing, then
// asks the model to emit the full reply. The model sees the
// original reply in the history so it can re-emit it correctly
// rather than starting from scratch.
//
// Limit: at most one re-prompt. Long sessions occasionally
// produce 2-block replies; one nudge is enough. If the second
// reply also misses a header we return whatever we got — the
// operator can see it in the slowlog and tune the prompt.
func (g *GM) repromptForFormat(ctx context.Context, chatID string, history []llm.Message, original string, cb Callbacks) (string, TokenUsage, error) {
	var totals TokenUsage
	if g.tracking == "" || g.tracking == "off" {
		totals.Source = "off"
	}

	missing := g.missingFormatHeaders(original)
	if len(missing) == 0 {
		return "", totals, nil
	}

	missingList := strings.Join(missing, ", ")
	reprompt := fmt.Sprintf(
		"[system note] твой предыдущий ответ не содержал обязательных блоков: %s. "+
			"Перепиши ответ с нуля, включив все четыре блока **в этом порядке**: "+
			"**диалоги и действия**, **КОНТЕКСТ И ИЗМЕНЕНИЯ**, **БУДУЩЕЕ**, **ВАЛИДАЦИЯ ПРАВИЛ**. "+
			"Не пиши пятый блок (варианты действий / следующий ход / выбор игрока), "+
			"не задавай игроку вопрос в конце, не нумеруй опции.",
		missingList,
	)

	history = append(history,
		llm.Message{Role: "user", Content: reprompt},
	)

	convMessages := history
	ctxPrompt, err := g.buildContextPrompt()
	if err != nil {
		return "", totals, fmt.Errorf("reprompt: build context: %w", err)
	}
	messages := make([]llm.Message, 0, len(convMessages)+1)
	messages = append(messages, llm.Message{Role: "system", Content: ctxPrompt})
	messages = append(messages, convMessages...)

	var (
		buf          strings.Builder
		finishReason string
		usageFromAPI llm.Usage
		gotUsage     bool
		compChars    int
		rawTrace     []string
	)
	streamErr := g.llm.Stream(ctx, llm.ChatRequest{
		Model:       g.role.Model,
		Messages:    messages,
		Tools:       g.tools,
		Temperature: g.role.Temperature,
		MaxTokens:   g.role.MaxTokens,
	}, func(ch llm.Chunk) error {
		if len(ch.RawTrace) > 0 {
			rawTrace = ch.RawTrace
		}
		if ch.Done {
			return nil
		}
		if ch.Content != "" {
			buf.WriteString(ch.Content)
			compChars += len(ch.Content)
			if cb.OnDelta != nil {
				if err := cb.OnDelta(ch.Content); err != nil {
					return err
				}
			}
		}
		if ch.Finish != "" {
			finishReason = ch.Finish
		}
		if ch.Usage.TotalTokens > 0 || ch.Usage.PromptTokens > 0 {
			usageFromAPI = ch.Usage
			gotUsage = true
		}
		return nil
	})
	if streamErr != nil {
		return "", totals, streamErr
	}
	_ = finishReason
	_ = gotUsage
	_ = usageFromAPI
	_ = compChars

	// Persist the corrective turn as part of conversation
	// history so the model learns the pattern.
	conv := g.getConversation(chatID)
	conv.mu.Lock()
	conv.messages = append(conv.messages, llm.Message{Role: "user", Content: reprompt})
	conv.messages = append(conv.messages, llm.Message{Role: "assistant", Content: buf.String()})
	conv.mu.Unlock()

	if g.slow != nil {
		origPrev := original
		if len(origPrev) > 200 {
			origPrev = origPrev[:200] + "…"
		}
		fixPrev := buf.String()
		if len(fixPrev) > 200 {
			fixPrev = fixPrev[:200] + "…"
		}
		fields := map[string]any{
			"missing":        missing,
			"original_chars": len(original),
			"reprompt_chars": len(buf.String()),
			"original_prev":  origPrev,
			"reprompt_prev":  fixPrev,
		}
		// If the corrective round also produced no content, the
		// raw SSE trace is the only diagnostic. Without it the
		// operator would only see "reprompt_chars: 0" and have
		// to guess whether the model refused, timed out, or
		// crashed.
		if len(buf.String()) == 0 {
			fields["raw_trace"] = rawTrace
		}
		_ = g.slow.Write("format.reprompt", chatID, fields)
	}
	g.log.Info().Strs("missing", missing).Int("reprompt_chars", len(buf.String())).Msg("format re-prompt")

	// Return only the new chunk — caller will splice it into
	// the user-visible buffer with a clear separator.
	return buf.String(), totals, nil
}
