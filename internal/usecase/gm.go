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
	"gopkg.in/yaml.v3"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/charprofile"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/staging"
	"github.com/bestxp/narrative-ai-agent/internal/structured"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools/files"
)

// DefaultProtocolWindowDays is the number of past days for
// which a full day-protocol (200-400 word narrative) is kept
// in the WorldState (index:1). Days older than this window
// are still on disk in memorise.md as shorter summaries —
// the model can recall the **facts** via the per-NPC
// personal_memory field, but no longer the verbatim
// conversation. 2 is the "remember yesterday and the day
// before" default.
const DefaultProtocolWindowDays = 2

// DefaultProtocolMaxChars is the hard cap on the size of
// the "## Протокол прошедших дней" section in WorldState.
// When the section grows past this, the oldest day is
// evicted to memorise.md. 5000 chars ≈ 1500 tokens; a
// 200-400 word narrative per day leaves room for 2-3 days
// even with multi-NPC scenes.
const DefaultProtocolMaxChars = 5000

// LLMClient is the minimal surface GM needs from the LLM. It
// is an interface so tests can swap in a stub without a real
// HTTP server, and so production can wire in either the
// legacy *llm.Client or the openai-go *llmopenai.Client.
//
// In package usecase the interface is intentionally narrow:
// Stream only. Resource lifecycles (openai-go connection
// pool, log file handles) are owned by main.go and released
// there. Tests that need to implement this interface only
// have to provide Stream.
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
	fl           *FirstLaunch
	// tools is the single file-backed Tool the GM reaches
	// for when it dispatches a tool call. Replacing the five
	// per-concern fields with one interface is the key
	// simplification of the 2026-06 refactor: a future mock
	// or alternate backend satisfies one interface, not five.
	tools        Tool
	summarizer   *Summarizer
	sysSt        *SystemState
	slow         *slowlog.Logger
	tracking     string // "off" | "estimate" | "usage"
	includeReply bool
	log          zerolog.Logger

	// toolSpecs cached on construction; immutable.
	toolSpecs []llm.ToolSchema

	// search_npc rate-limit. The same query string is
	// only allowed once per rateWindow turns. A
	// different query is always allowed — the limit
	// guards against "re-asking the same NPC over and
	// over", not against broad exploration. Counter
	// is the turn when the query was last asked.
	rateWindow    int            // default 5
	npcSearchRate map[string]int // query → last turn
	turnCounter   int            // increments per Reply

	// System prompt and world-state user message are snapshotted
	// separately. The system prompt is rules + character (static
	// for the entire conversation); the world-state user message
	// is everything world-scoped (changes on end_day / leave_world
	// / /reload / compaction). Both are rebuilt together on the
	// same sceneKey, so they are always consistent. Mutating
	// tools (update_state, update_npc, ...) do NOT invalidate
	// either snapshot — the model reads the delta in the short
	// ToolResult.
	contextMu          sync.Mutex
	systemSnapshot     string // rules + character
	worldSnapshot      string // [WORLD_STATE] world + NPCs
	contextSceneKey    string // "world|character|day|in_flight" — the inputs the snapshot was built from
	contextBuiltAt     time.Time
	protocolWindowDays int // default 2
	protocolMaxChars   int // default 5000
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

// NewGM constructs the GM. The supplied system prompt is
// sent as the first "system" message on every turn. The
// conversation state (per ChatID) is kept in a sync.Map; the
// GM is therefore safe to call from multiple goroutines.
//
// The single Tool parameter bundles every domain operation
// the GM needs. main.go wires the file-backed implementation
// (NewFileToolset); tests pass a mock.
func NewGM(cfg GMConfig, fs *storage.FileStore, llmCli LLMClient, ss *SessionStart, fl *FirstLaunch, tools Tool, summarizer *Summarizer, sysSt *SystemState, slow *slowlog.Logger, tracking string, includeReply bool, log zerolog.Logger) *GM {
	log = log.With().Str("component", "gm").Logger()
	toolSpecs := make([]llm.ToolSchema, 0, len(domain.Tools()))
	for _, t := range domain.Tools() {
		raw, err := t.MarshalParameters()
		if err != nil {
			log.Warn().Err(err).Str("tool", t.Function.Name).Msg("tool schema marshal failed; skipping")
			continue
		}
		toolSpecs = append(toolSpecs, llm.ToolSchema{
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
		fl:           fl,
		tools:        tools,
		summarizer:   summarizer,
		sysSt:        sysSt,
		slow:         slow,
		tracking:     tracking,
		includeReply: includeReply,
		log:          log,
		toolSpecs:    toolSpecs,
		// Protocol window defaults. Operators can override
		// via config (we will thread the cfg through
		// GMConfig in a follow-up if requested; for now
		// the constants live in gm.go).
		protocolWindowDays: DefaultProtocolWindowDays,
		protocolMaxChars:   DefaultProtocolMaxChars,
		npcSearchRate:      make(map[string]int),
		rateWindow:         5,
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

// ResetAllConversations drops every per-chat in-memory
// history. Used by /reload when the operator edited
// game-data by hand and wants the next turn to start
// with a clean dialogue but the freshly-edited
// WorldState. The next Reply will lazily create a new
// conversation entry for that chat ID.
func (g *GM) ResetAllConversations() {
	conversations.Range(func(k, _ any) bool {
		conversations.Delete(k)
		return true
	})
}

// Reply is the streaming entry point. It builds the prompt context,
// calls the LLM, dispatches any tool calls, and pushes the resulting
// text to the supplied callback. The callback returns an error to
// abort mid-stream (typically the transport's context cancellation).
//
// maxToolRounds caps the number of tool-call rounds so a runaway
// model cannot loop forever. 10 gives the model room to update
// multiple NPC files and still produce a narrative response.
const maxToolRounds = 10

func (g *GM) Reply(ctx context.Context, chatID, userText string, cb Callbacks) (TokenUsage, error) {
	var totals TokenUsage
	if g.tracking == "" || g.tracking == "off" {
		totals.Source = "off"
	}

	// Bump the per-Reply turn counter (used by
	// search_npc rate-limit). The counter starts at
	// 0, so the first Reply sees turnCounter=1.
	g.turnCounter++

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
						if err := g.tools.AppendHistoryToState(currentWorldName(g.fs), sumRes.Text, now); err != nil {
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
	// In-place compaction path. Differs from the legacy
	// token-drop compaction above in that the dropped turns
	// are compressed into a 150-300 word narrative that
	// goes into "## Хроника текущего дня" in state.md (not
	// the legacy "## История (сжато)" tail). This keeps
	// index:1 current with the day's events without forcing
	// end_day. The legacy path stays for callers that do
	// not wire the in-place prompt (test suite, single-role
	// deployments).
	if g.compaction.ContextWindow > 0 && g.shouldUseInPlaceCompaction() {
		ctxChars := len(g.staticPrompt)
		if NeedsCompaction(history, ctxChars, g.compaction.ContextWindow, g.compaction.Threshold) {
			keptMsgs, _ := CompactConversations(history, g.compaction.KeepRecent)
			dropped := len(history) - g.compaction.KeepRecent
			if dropped < 0 {
				dropped = 0
			}
			var droppedTurns []llm.Message
			if dropped > 0 && dropped <= len(history) {
				droppedTurns = history[:dropped]
			}
			day := readCurrentDay(g.fs, currentWorldName(g.fs))
			world := currentWorldName(g.fs)
			// Summarizer call. Best-effort: errors log and
			// fall through to the drop path. Empty body
			// (model said "too thin") is a no-op — we still
			// drop, just without a Хроника append.
			if g.summarizer != nil && g.summarizer.IsConfigured() && len(droppedTurns) > 0 {
				res, sumErr := g.summarizer.SummarizeInPlace(ctx, world, day, droppedTurns)
				if sumErr != nil {
					g.log.Warn().Err(sumErr).Msg("in-place compaction failed; dropping turns without Хроника")
				} else if len(res.Body) > 0 {
					if err := g.appendChronicleEntry(world, day, res.Body); err != nil {
						g.log.Warn().Err(err).Msg("append Хроника to state.md failed")
					} else {
						g.log.Info().Int("chars", res.OutputChars).Int("day", day).Msg("in-place: Хроника appended")
					}
				}
			}
			conv.mu.Lock()
			conv.messages = keptMsgs
			conv.mu.Unlock()
			history = keptMsgs
			// Force the next turn to rebuild index:1 —
			// we just appended to state.md, the snapshot
			// is stale.
			g.invalidateWorldState("compaction")
			g.log.Info().Int("dropped", dropped).Int("kept", len(keptMsgs)).Msg("in-place compaction fired")
		}
	}

	consecutiveToolOnlyRounds := 0

	for round := 0; round < maxToolRounds; round++ {
		if cb.OnStatus != nil {
			cb.OnStatus("build_context", nil)
		}
		// System prompt (rules+character) and WorldState user
		// message (world+NPCs) are rebuilt on every tool round
		// so tool-modified state.md / world files are visible
		// to the model. They are snapshotted together on a
		// single sceneKey — see buildContext for the cache
		// contract. World state is a SEPARATE user message
		// (user[0]), not part of the system message. Anthropic
		// driver attaches cache_control to user[0]; OpenAI
		// uses prefix-cache on the same prefix.
		systemMsg, worldMsg, err := g.buildContext()
		if err != nil {
			return totals, fmt.Errorf("gm: build context: %w", err)
		}
		messages := make([]llm.Message, 0, len(history)+2)
		messages = append(messages, llm.Message{Role: "system", Content: systemMsg})
		messages = append(messages, llm.Message{Role: "user", Content: worldMsg})
		messages = append(messages, history...)

		if cb.OnStatus != nil {
			cb.OnStatus("llm_request", map[string]any{
				"model":        g.role.Model,
				"messages":     len(messages),
				"prompt_chars": promptCharSize(messages),
			})
		}

		// Slowlog: log the full request before sending so an
		// operator can reproduce the exact payload that produced
		// a broken or empty response. This is the only place
		// where we see the wire-shape of every message and tool
		// spec — the Driver does not emit its own request log.
		if g.slow != nil {
			reqFields := map[string]any{
				"round":            round,
				"model":            g.role.Model,
				"temperature":      g.role.Temperature,
				"max_tokens":       g.role.MaxTokens,
				"messages":         len(messages),
				"prompt_chars":     promptCharSize(messages),
				"tool_count":       len(g.toolSpecs),
				"tool_names":       toolNames(g.toolSpecs),
				"reasoning_effort": g.role.ReasoningEffort,
			}
			reqMsgs := make([]map[string]any, 0, len(messages))
			for _, m := range messages {
				entry := map[string]any{
					"role":        m.Role,
					"content_len": len(m.Content),
				}
				if m.Content != "" {
					entry["content"] = m.Content
				}
				if len(m.ToolCalls) > 0 {
					calls := make([]map[string]any, 0, len(m.ToolCalls))
					for _, tc := range m.ToolCalls {
						calls = append(calls, map[string]any{
							"name": tc.Function.Name,
							"args": tc.Function.Arguments,
						})
					}
					entry["tool_calls"] = calls
				}
				if m.Name != "" {
					entry["name"] = m.Name
				}
				if m.ToolCallID != "" {
					entry["tool_call_id"] = m.ToolCallID
				}
				reqMsgs = append(reqMsgs, entry)
			}
			reqFields["request_messages"] = reqMsgs
			_ = g.slow.Write("llm.request", chatID, reqFields)
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
			Tools:       g.toolSpecs,
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

		// Slowlog: log the full response immediately after the
		// stream finishes. This captures everything the model
		// produced — content, tool calls, finish reason, and
		// usage — in a single structured event. Combined with
		// llm.request above, an operator has the full round-trip.
		if g.slow != nil {
			respFields := map[string]any{
				"round":           round,
				"finish":          finishReason,
				"content_len":     assistantBuf.Len(),
				"tool_call_count": len(toolCalls),
			}
			if len(toolCalls) > 0 {
				tcNames := make([]string, 0, len(toolCalls))
				for _, tc := range toolCalls {
					tcNames = append(tcNames, tc.Function.Name)
				}
				respFields["tool_call_names"] = tcNames
				// Log full tool call arguments when tool_calls
				// were emitted — this is the only way to see
				// what the model tried to do on a broken call.
				tcDetails := make([]map[string]any, 0, len(toolCalls))
				for _, tc := range toolCalls {
					tcDetails = append(tcDetails, map[string]any{
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
						"id":        tc.ID,
					})
				}
				respFields["tool_call_details"] = tcDetails
			}
			if assistantBuf.Len() > 0 {
				// Full content on every round — operators need it
				// to diagnose format compliance issues and empty
				// tool_calls rounds where the model wrote
				// directive text in the content field instead of
				// using native tool calls.
				respFields["content"] = assistantBuf.String()
			}
			if gotUsage {
				respFields["usage_prompt_tokens"] = usageFromAPI.PromptTokens
				respFields["usage_completion_tokens"] = usageFromAPI.CompletionTokens
				respFields["usage_total_tokens"] = usageFromAPI.TotalTokens
			}
			if len(rawTrace) > 0 {
				respFields["raw_trace"] = rawTrace
			}
			_ = g.slow.Write("llm.response", chatID, respFields)
		}

		// Persist the assistant turn regardless of finish reason.
		// Tool calls with empty names are dropped here so the
		// broken-cloud-stream case does not poison the next
		// round with `[{"name":""}]` history entries.
		storedCalls := toolCalls
		if allToolCallsBroken(storedCalls) {
			storedCalls = nil
		}
		conv.mu.Lock()
		conv.messages = append(conv.messages, llm.Message{
			Role:      "assistant",
			Content:   assistantBuf.String(),
			ToolCalls: storedCalls,
		})
		conv.mu.Unlock()
		history = append(history, llm.Message{
			Role: "assistant", Content: assistantBuf.String(), ToolCalls: storedCalls,
		})

		// Context-directive parser: fallback when
		// native tool_calls did not fire. With
		// tool_choice=required (probe confirmed
		// working on minimax-m3:cloud, 2026-06-08)
		// the model calls tools natively; we only
		// parse markers when tools were skipped
		// (empty/broken round). Errors here are
		// non-fatal; the user-visible narrative
		// has already been streamed.
		if len(toolCalls) == 0 {
			cmds := extractContextCommands(assistantBuf.String())
			g.executeExtractedCommands(ctx, chatID, currentWorldName(g.fs), cmds)
		}

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
			fullContent := assistantBuf.String()
			preview := fullContent
			if len(preview) > 500 {
				preview = preview[:500] + "…"
			}
			fields := map[string]any{
				"round":         round,
				"finish":        finishReason,
				"tool_calls":    len(toolCalls),
				"content_chars": len(fullContent),
				"content_prev":  preview,
			}
			// When the assistant produced no text AND no tool calls,
			// the raw SSE trace is the only way to diagnose. We do
			// not log it on healthy rounds (would spam slow.log with
			// 3-5 entries per turn).
			if len(fullContent) == 0 && len(toolCalls) == 0 {
				fields["raw_trace"] = rawTrace
			}
			// When the format gate will fail (no ВАЛИДАЦИЯ ПРАВИЛ
			// header), the operator needs the FULL assistant turn
			// to confirm whether the model emitted the block at
			// all, emitted it with a typo, or got cut off mid-list.
			// The 500-char preview truncates the ВАЛИДАЦИЯ block
			// almost every time (it sits at the end). We log the
			// full body only on the failing path to keep healthy
			// rounds readable.
			if len(fullContent) > 0 {
				fields["content_full"] = fullContent
			}
			fields["request_tools"] = formatToolsForLog(g.toolSpecs)
			_ = g.slow.Write("llm.round", chatID, fields)
		}

		// If the model only wanted to talk, we're done — but first
		// check format compliance and re-prompt on missing blocks.
		if len(toolCalls) == 0 || finishReason != "tool_calls" {
			// Detect a "broken" tool-call round: the model
			// emitted tool_calls but the stream cut off
			// before any of them got a name. minimax-m3:cloud
			// (Ollama Cloud) does this occasionally when
			// its reasoning block runs past the response
			// budget — we get `delta.tool_calls: [{}]`
			// fragments that never assemble into a
			// callable name. Treat this as the same
			// "empty content" case below: the model did
			// intend to act but the stream was clipped, so
			// retrying the same prompt is the right move.
			if allToolCallsBroken(toolCalls) {
				toolCalls = nil
				g.log.Warn().
					Int("round", round).
					Str("finish", finishReason).
					Msg("llm emitted broken tool calls (no names) — treating as empty")
			}
			// Skip the format gate entirely when the round
			// produced no content. With the h4-by-default
			// config (8 tools, tool_choice=auto) the model
			// is expected to ALWAYS return either a
			// tool_call round or a non-empty content
			// round. An empty round is a hard error:
			//   1. retrying the same request risks the
			//      same broken response, AND burns 30-90s
			//      of the player's time;
			//   2. retrying with a "nudge" prompt silently
			//      papers over real provider bugs;
			//   3. returning a polite placeholder hides
			//      the failure from the operator.
			//
			// We log the full SSE trace to slowlog so the
			// operator has something to debug, then return
			// the error to the dispatcher (which renders
			// it to the player as "⚠️ <err>").
			if len(assistantBuf.String()) == 0 {
				if g.slow != nil {
					_ = g.slow.Write("llm.empty", chatID, map[string]any{
						"round":          round,
						"finish":         finishReason,
						"tool_calls_raw": len(storedCalls),
						"all_broken":     allToolCallsBroken(storedCalls),
					})
				}
				g.log.Error().
					Int("round", round).
					Str("finish", finishReason).
					Int("raw_trace_count", len(rawTrace)).
					Msg("llm returned empty content (no tools, no text) — hard error, no retry")
				return totals, fmt.Errorf("model returned no tool_use and no content (round=%d, finish=%s, raw_events=%d)", round, finishReason, len(rawTrace))
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
		// Post-tool guard: if the model returned a narrative that
		// mentions facts about the player (age, name, skills) or
		// new NPC names but did NOT call update_character /
		// create_npc, the player's persistent files are now
		// out-of-date. We log a warning so the operator can
		// see the gap in slow.log, and inject a one-shot hint
		// onto the next user turn via the conv history so the
		// model gets a chance to self-correct.
		g.missedToolGuard(ctx, chatID, toolCalls, assistantBuf.String(), currentWorldName(g.fs))

		// Stuck guard: when the model issues tool calls N rounds
		// in a row without ever producing narrative content, it
		// is likely iterating through NPC files one at a time.
		// After maxStuckToolRounds consecutive tool-only rounds we
		// inject a nudge so the model knows to produce narrative.
		if assistantBuf.Len() == 0 {
			consecutiveToolOnlyRounds++
		} else {
			consecutiveToolOnlyRounds = 0
		}
		const maxStuckToolRounds = 3
		if consecutiveToolOnlyRounds >= maxStuckToolRounds {
			g.log.Warn().
				Int("round", round).
				Int("consecutive", consecutiveToolOnlyRounds).
				Msg("model issued tool calls without narrative for N rounds — injecting nudge")
			if g.slow != nil {
				_ = g.slow.Write("gm.stuck_nudge", chatID, map[string]any{
					"round":       round,
					"consecutive": consecutiveToolOnlyRounds,
				})
			}
			nudge := llm.Message{
				Role:    "user",
				Content: "[система] Все инструменты выполнены. Теперь обязательно напиши нарративный ответ игроку в формате JSON (narration, context, future, validation). Не вызывай больше инструменты.",
			}
			conv.mu.Lock()
			conv.messages = append(conv.messages, results...)
			conv.messages = append(conv.messages, nudge)
			conv.mu.Unlock()
			history = append(history, results...)
			history = append(history, nudge)
			consecutiveToolOnlyRounds = 0
			toolCalls = nil
			continue
		}

		conv.mu.Lock()
		conv.messages = append(conv.messages, results...)
		conv.mu.Unlock()
		history = append(history, results...)
		toolCalls = nil
	}
	return totals, fmt.Errorf("gm: tool loop exceeded %d rounds", maxToolRounds)
}

// missedToolGuard inspects the assistant turn + the tool calls
// the model issued and warns (via slowlog) when the narrative
// clearly called for update_character / create_npc but no
// such call was made. The check is heuristic on purpose — it
// errs on the side of "no warning" rather than spamming the
// slowlog on every turn. Operators who want a tighter contract
// can add a stricter checker here.
func (g *GM) missedToolGuard(_ context.Context, chatID string, calls []llm.ToolCall, answer string, world string) {
	if world == "" {
		return
	}
	called := make(map[string]bool, len(calls))
	for _, tc := range calls {
		called[tc.Function.Name] = true
	}
	lower := strings.ToLower(answer)
	// Missing per-character file tools: the assistant
	// turn mentions the player's name, age, or a
	// clear self-disclosure ("мне N лет", "меня зовут",
	// "я умею"). These are the triggers the prompt's
	// checklist calls out, so the absence of the
	// matching tool is a real signal. We check the
	// union of the four file tools (update_soul /
	// update_skill / update_memory) — a turn that
	// mentions player facts and calls NONE of them
	// is the bug case. The h5 refactor split the
	// legacy update_character into one tool per
	// file, but the trigger language is the same.
	if !called["update_soul"] && !called["update_skill"] && !called["update_memory"] {
		triggers := []string{
			"мне ", "лет", "меня зовут", "я умею", "мой навык", "моя цель",
			"я родился", "я из ", "моё прошлое", "в моём мире", "в моей школе",
		}
		for _, t := range triggers {
			if strings.Contains(lower, t) {
				g.log.Warn().
					Str("trigger", t).
					Str("chat", chatID).
					Msg("narrative mentions player facts but no per-character file tool was called")
				if g.slow != nil {
					_ = g.slow.Write("gm.missed_tool", chatID, map[string]any{
						"tool":    "update_soul|update_skill|update_memory",
						"reason":  "player facts mentioned in narrative",
						"trigger": t,
					})
				}
				return
			}
		}
	}
	// Missing create_npc: the assistant turn mentions a new
	// NPC by name whose profile does not yet exist on disk.
	// We do a quick check against the worlds/<w>/characters/
	// directory — any uppercase name that is not already a
	// file is a candidate. The threshold is "mentioned in
	// dialogue" (i.e. quoted), not merely listed in npcs.
	if !called["create_npc"] {
		for _, npc := range extractProperNamesFromDialogue(answer) {
			rel := "worlds/" + world + "/characters/" + npc + ".md"
			if !g.fs.Exists(rel) {
				g.log.Warn().
					Str("npc", npc).
					Str("chat", chatID).
					Msg("narrative introduces a new NPC without a profile; create_npc was not called")
				if g.slow != nil {
					_ = g.slow.Write("gm.missed_tool", chatID, map[string]any{
						"tool": "create_npc",
						"npc":  npc,
					})
				}
				return
			}
		}
	}
}

// extractProperNamesFromDialogue returns a slice of
// candidate NPC names that appear in dialogue lines of the
// answer. A "dialogue line" is anything starting with "—".
// We pull the next 1-3 capitalised words off the line — that
// is the pattern a Russian GM uses ("— Ирука повернулся…",
// "— Хокаге-сама, я…").
func extractProperNamesFromDialogue(answer string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, ln := range strings.Split(answer, "\n") {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "—") && !strings.HasPrefix(t, "-") {
			continue
		}
		// Strip the leading dash and split into words.
		body := strings.TrimLeft(t, "—- ")
		// Walk words; collect runs of capitalised tokens.
		var current []string
		for _, w := range strings.Fields(body) {
			// Strip punctuation.
			cleaned := strings.Trim(w, ",.;:!?\"'()«»…")
			if cleaned == "" {
				continue
			}
			// Capitalised iff the first rune is uppercase.
			runes := []rune(cleaned)
			if runes[0] >= 'A' && runes[0] <= 'Z' || runes[0] >= 'А' && runes[0] <= 'Я' {
				current = append(current, cleaned)
			} else {
				if len(current) > 0 {
					name := strings.Join(current, " ")
					if !seen[name] {
						seen[name] = true
						out = append(out, name)
					}
					current = nil
				}
			}
		}
		if len(current) > 0 {
			name := strings.Join(current, " ")
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}

// extractPermanentParty scans the active world's
// state.md for a "## permanent party" section and
// returns the comma-separated names listed under
// it. The list is the cast that travels with the
// player across scene changes; end_scene uses it
// to prune the active roster. A missing or empty
// section returns nil (no prune — the safe default).
//
// State.md format (canonical, set by the operator
// or by the WorldSeed tool):
//
//	## permanent party
//	Какаши, Хината, Ирука
//
// The h5 charprofile refactor moved permanent
// party out of the character files (SOUL.md /
// skill.md) and into the WORLD state — the cast
// is world-scoped, not character-scoped, because
// the same character visits different worlds with
// different retainers. Whitespace around names and
// trailing commas are tolerated. Names are
// returned trimmed but otherwise unchanged — the
// dispatcher passes them straight to state.md's
// active-roster compare.
func extractPermanentParty(stateMD string) []string {
	const marker = "## permanent party"
	idx := strings.Index(stateMD, marker)
	if idx < 0 {
		return nil
	}
	rest := stateMD[idx+len(marker):]
	// Up to the next "## " sibling header or end of body.
	end := strings.Index(rest, "\n## ")
	if end < 0 {
		end = len(rest)
	}
	body := rest[:end]
	// Only the first non-empty line carries the list
	// (subsequent lines are prose, ignored).
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var out []string
		for _, n := range strings.Split(line, ",") {
			if t := strings.TrimSpace(n); t != "" {
				out = append(out, t)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// extractedCommand is one actionable directive recovered
// from the assistant turn. The GM runs these locally as
// if the model had issued a real tool_call (which on
// Ollama Cloud is silently ignored — see the rationale
// in cmd/test-openapi/main.go). Commands are pure data;
// the dispatcher below (executeExtractedCommands) knows
// how to route them to the right usecase.Tool method.
type extractedCommand struct {
	// Kind is one of the wire-form canonical
	// command names. After the h5 refactor the
	// per-character dispatcher is split into
	// update_soul / update_skill / update_memory /
	// update_inventory / remove_inventory_item /
	// set_currency / remove_currency. The legacy
	// "update_character" kind is GONE — the parser
	// below rejects it as unknown so old turns that
	// still write it are surfaced to the slowlog
	// instead of silently dropped.
	Kind string
	Args map[string]string
	// Raw is the original line for the slowlog so an
	// operator can see exactly what the model wrote.
	Raw string
}

// extractContextCommands scans the assistant turn for
// bullet-style tool directives. The model is told in
// prompts/narrative.md to write these as a fallback when
// the provider does not honour tool_choice=required (which
// is every Ollama Cloud model we have probed). Each line
// that matches a known pattern becomes an
// extractedCommand that the GM dispatches in the same
// goroutine after streaming finishes.
//
// Both wire formats are supported:
//
//   - Markdown (legacy / Режим B): the line lives inside
//     the **КОНТЕКСТ И ИЗМЕНЕНИЯ** block, prefixed with
//     "⦁" (or "-" / "•"). We extract the block and then
//     scan its lines.
//
//   - JSON (Режим A): the assistant turn is a JSON object
//     with a `context` field. We parse the field, split on
//     newlines, and treat each line as a markdown-style
//     directive. The JSON path goes through
//     structured.Parse so the field-extraction logic is
//     not duplicated.
//
// Unknown lines are silently dropped — the slowlog
// `context.extracted` event records what we kept so the
// operator can audit the gap.
func extractContextCommands(answer string) []extractedCommand {
	body := answer
	if structured.LooksLikeJSON(answer) {
		n, err := structured.Parse(answer)
		if err == nil {
			body = n.Context
		}
	}
	// Pull the КОНТЕКСТ block if present. Anything outside
	// it is narrative (we do not want to interpret a quoted
	// NPC line as a tool directive). The block ends at
	// **БУДУЩЕЕ or end-of-input.
	block := extractContextBlock(body)
	if block == "" {
		return nil
	}
	var out []extractedCommand
	for _, raw := range strings.Split(block, "\n") {
		line := strings.TrimSpace(raw)
		// Skip empty lines and lines that are clearly
		// not directives (no recognised prefix after
		// the bullet).
		if !looksLikeDirective(line) {
			continue
		}
		if cmd, ok := parseDirectiveLine(line); ok {
			cmd.Raw = line
			out = append(out, cmd)
		}
	}
	return out
}

// extractContextBlock returns the lines between
// "КОНТЕКСТ И ИЗМЕНЕНИЯ" and the next major header
// (БУДУЩЕЕ / ВАЛИДАЦИЯ ПРАВИЛ / end of input). We
// accept several historical phrasings of the header —
// the prompt says "КОНТЕКСТ И ИЗМЕНЕНИЯ" but models
// sometimes shorten it to "**КОНТЕКСТ**" alone, and the
// JSON path emits the field text without any header.
func extractContextBlock(body string) string {
	if body == "" {
		return ""
	}
	lower := strings.ToLower(body)
	start := -1
	for _, marker := range []string{
		"**контекст и изменения**",
		"**контекст**",
		"### контекст",
	} {
		if idx := strings.Index(lower, marker); idx >= 0 {
			start = idx + len(marker)
			break
		}
	}
	if start < 0 {
		// JSON-mode fallback: if the body is one
		// short paragraph with no header, treat the
		// whole body as the context block. Long
		// bodies (>800 chars) probably are not a
		// context field and we return empty to avoid
		// false positives.
		if len(body) <= 800 {
			return body
		}
		return ""
	}
	// Find the end: next major header.
	rest := body[start:]
	for _, end := range []string{
		"**будущее**",
		"**валидация правил**",
		"### будущее",
		"### валидация",
	} {
		if idx := strings.Index(strings.ToLower(rest), end); idx >= 0 {
			rest = rest[:idx]
			break
		}
	}
	return rest
}

// looksLikeDirective is a cheap pre-filter. We accept
// lines that start with a bullet character AND contain
// at least one of the recognised command prefixes
// (case-insensitive). The full parse happens in
// parseDirectiveLine. Bullets are matched as runes
// because ⦁ / • are multi-byte in UTF-8 and indexing
// into a string as bytes would split the codepoint.
func looksLikeDirective(line string) bool {
	if line == "" {
		return false
	}
	runes := []rune(line)
	switch runes[0] {
	case '⦁', '•', '-', '*', '>':
	default:
		return false
	}
	lower := strings.ToLower(line)
	prefixes := []string{
		"update_npc", "update_state", "append_lore",
		"create_npc",
		"update_soul", "update_skill", "update_memory",
		"update_inventory", "remove_inventory_item",
		"set_currency", "remove_currency",
		"lore:", "npc:", "state:",
	}
	for _, p := range prefixes {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// parseDirectiveLine extracts one command from a single
// КОНТЕКСТ line. The grammar is intentionally forgiving:
//
//	⦁ update_npc: Хината — статус: смущена
//	⦁ update_npc: Хината, статус = смущена
//	⦁ lore: День 5: Саске простил Итахи
//	⦁ append_lore: header=День 5, bullet=Саске простил Итахи
//
// We use "—" (em dash) and "," and "=" as the canonical
// separators between name and the rest. Section names
// follow the canonical Russian vocabulary in
// usecase/tools/files/npc.go (temperament, status,
// abilities, etc.) and are matched case-insensitively.
func parseDirectiveLine(line string) (extractedCommand, bool) {
	// Strip the leading bullet. TrimLeftFunc is the
	// rune-safe counterpart to TrimLeft — needed
	// because ⦁ / • are multi-byte UTF-8 codepoints
	// and the cutset would otherwise be interpreted
	//// as bytes.
	stripped := strings.TrimLeftFunc(line, func(r rune) bool {
		switch r {
		case '⦁', '•', '-', '*', '>', ' ', '\'':
			return true
		}
		return false
	})
	lower := strings.ToLower(stripped)

	// npc / lore: short-form prefixes.
	if strings.HasPrefix(lower, "npc:") {
		// "npc: Хината — статус: смущена" is the
		// short form of update_npc. Rewrite to the
		// long form and recurse.
		return parseDirectiveLine("⦁ update_npc: " + strings.TrimSpace(stripped[4:]))
	}
	if strings.HasPrefix(lower, "lore:") {
		body := strings.TrimSpace(stripped[5:])
		// Split on the first em-dash / dash / colon:
		// "header — body" or "header: body".
		header, bullet := splitHeaderBullet(body)
		return extractedCommand{
			Kind: "append_lore",
			Args: map[string]string{"header": header, "bullet": bullet},
		}, true
	}
	if strings.HasPrefix(lower, "state:") {
		body := strings.TrimSpace(stripped[6:])
		// "state: moment=...; npcs=...; events=..."
		args := parseSemicolonPairs(body)
		return extractedCommand{
			Kind: "update_state",
			Args: args,
		}, true
	}

	// Long forms.
	switch {
	case strings.HasPrefix(lower, "update_npc:"):
		body := strings.TrimSpace(stripped[len("update_npc:"):])
		// "Хината — статус: смущена" or
		// "Хината, секция=статус, текст=смущена"
		return parseUpdateNpcLine(body)
	case strings.HasPrefix(lower, "append_lore:"):
		body := strings.TrimSpace(stripped[len("append_lore:"):])
		// append_lore uses comma-separated pairs
		// (not semicolon) because the model often
		// writes "header=День 7, bullet=...". Other
		// directives keep semicolons because their
		// values can contain commas (e.g. event
		// descriptions). The split is on the first
		// "=" of each pair, NOT on commas, so
		// "День 7" with an internal comma would
		// still split correctly.
		args := parseCommaPairs(body)
		if _, ok := args["header"]; !ok {
			// Fallback: treat the whole body as
			// "header — bullet".
			args["header"], args["bullet"] = splitHeaderBullet(body)
		}
		return extractedCommand{Kind: "append_lore", Args: args}, true
	case strings.HasPrefix(lower, "update_state:"):
		body := strings.TrimSpace(stripped[len("update_state:"):])
		return extractedCommand{
			Kind: "update_state",
			Args: parseSemicolonPairs(body),
		}, true
	case strings.HasPrefix(lower, "update_soul:"):
		// "section=Легенда для прикрытия, append=..." —
		// two-key form. The file kind is fixed by
		// the tool name, no `file=` discriminator
		// anymore.
		body := strings.TrimSpace(stripped[len("update_soul:"):])
		return extractedCommand{
			Kind: "update_soul",
			Args: parseCommaPairs(body),
		}, true
	case strings.HasPrefix(lower, "update_skill:"):
		body := strings.TrimSpace(stripped[len("update_skill:"):])
		return extractedCommand{
			Kind: "update_skill",
			Args: parseCommaPairs(body),
		}, true
	case strings.HasPrefix(lower, "update_memory:"):
		body := strings.TrimSpace(stripped[len("update_memory:"):])
		return extractedCommand{
			Kind: "update_memory",
			Args: parseCommaPairs(body),
		}, true
	case strings.HasPrefix(lower, "update_inventory:"):
		// "name=Кунай, type=weapon, equip=true, description=..., special=..."
		body := strings.TrimSpace(stripped[len("update_inventory:"):])
		return extractedCommand{
			Kind: "update_inventory",
			Args: parseCommaPairs(body),
		}, true
	case strings.HasPrefix(lower, "remove_inventory_item:"):
		body := strings.TrimSpace(stripped[len("remove_inventory_item:"):])
		return extractedCommand{
			Kind: "remove_inventory_item",
			Args: parseCommaPairs(body),
		}, true
	case strings.HasPrefix(lower, "set_currency:"):
		// "name=Рё, count=4200"
		body := strings.TrimSpace(stripped[len("set_currency:"):])
		return extractedCommand{
			Kind: "set_currency",
			Args: parseCommaPairs(body),
		}, true
	case strings.HasPrefix(lower, "remove_currency:"):
		body := strings.TrimSpace(stripped[len("remove_currency:"):])
		return extractedCommand{
			Kind: "remove_currency",
			Args: parseCommaPairs(body),
		}, true
	case strings.HasPrefix(lower, "create_npc:"):
		body := strings.TrimSpace(stripped[len("create_npc:"):])
		return extractedCommand{
			Kind: "create_npc",
			Args: parseSemicolonPairs(body),
		}, true
	}
	return extractedCommand{}, false
}

// parseUpdateNpcLine handles "Name — section: text" with
// em-dash, comma, or "section=text" variants.
func parseUpdateNpcLine(body string) (extractedCommand, bool) {
	body = strings.TrimSpace(body)
	if body == "" {
		return extractedCommand{}, false
	}
	// Long form: "Name, section=..., append=..."
	if strings.Contains(body, "=") {
		args := parseSemicolonPairs(body)
		if name, ok := args["npc"]; ok {
			args["npc"] = strings.TrimSpace(name)
		} else {
			// Sometimes the model writes "Name,
			// section=X, append=Y" without
			// explicit "npc=Name". Try to pull
			// the leading word.
			if idx := strings.IndexAny(body, ",;"); idx > 0 {
				args["npc"] = strings.TrimSpace(body[:idx])
			} else {
				args["npc"] = body
			}
		}
		return extractedCommand{Kind: "update_npc", Args: args}, true
	}
	// Short form: "Name — section: text" or
	// "Name — section: text — extra".
	name, rest, ok := splitOnEmDash(body)
	if !ok {
		// "Name" alone is not actionable.
		return extractedCommand{}, false
	}
	section, text, ok := splitOnColon(rest)
	if !ok {
		// "Name — section" without ": text". Treat
		// the rest as section, no text — caller
		// will reject empty append.
		return extractedCommand{
			Kind: "update_npc",
			Args: map[string]string{
				"npc":     strings.TrimSpace(name),
				"section": strings.TrimSpace(rest),
				"append":  "",
			},
		}, true
	}
	return extractedCommand{
		Kind: "update_npc",
		Args: map[string]string{
			"npc":     strings.TrimSpace(name),
			"section": strings.TrimSpace(section),
			"append":  strings.TrimSpace(text),
		},
	}, true
}

// splitHeaderBullet splits "Header — body" or "Header:
// body" on the first em-dash or colon after position 1.
// Returns ("", body) when no separator is present.
func splitHeaderBullet(s string) (string, string) {
	for i, r := range s {
		if r == '—' || r == ':' {
			if r == ':' && i == 0 {
				continue
			}
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
		}
	}
	return "", strings.TrimSpace(s)
}

// splitOnEmDash returns the part before the first "—"
// and the part after. Falls back to comma if no em-dash.
// We work on runes so a multi-byte em-dash does not get
// sliced through the middle (returning garbage bytes
// like 0x80 0x94 to the caller). The returned strings
// are still byte slices of the original — the caller
// only sees the boundary at the right UTF-8 codepoint.
func splitOnEmDash(s string) (string, string, bool) {
	for i, r := range s {
		if r == '—' {
			return s[:i], s[i+len("—"):], true
		}
		if r == ',' {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}

// splitOnColon returns the part before the first colon
// and the part after. Used for "section: text".
func splitOnColon(s string) (string, string, bool) {
	for i, r := range s {
		if r == ':' {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}

// parseSemicolonPairs reads "k1=v1; k2=v2" into a map.
// Keys are lower-cased and trimmed. Empty entries are
// dropped. Quoted values have the quotes stripped.
// parseSemicolonPairs splits `s` on ";" and
// interprets each chunk as a `key=value` pair. Chunks
// without an "=" are treated as a continuation of
// the previous pair's value (joined with the same
// separator) so that a model can write
// "append=киокушинкай, муай-тай" with a comma inside
// the value without breaking the split. Keys are
// lower-cased; surrounding quotes are stripped from
// values.
func parseSemicolonPairs(s string) map[string]string {
	return parseKeyValuePairs(s, ";")
}

// parseCommaPairs is the comma-separated sibling of
// parseSemicolonPairs. Used for append_lore where the
// model writes "header=X, bullet=Y" rather than the
// semicolon form. Same continuation rule as
// parseSemicolonPairs: chunks without "=" join the
// previous value with the same separator, so a
// comma inside the value (e.g.
// "append=рамен, суши") is preserved verbatim.
func parseCommaPairs(s string) map[string]string {
	return parseKeyValuePairs(s, ",")
}

// parseKeyValuePairs is the shared backend for the
// two separators. The "continuation" behaviour is
// what makes this robust against values that happen
// to contain the separator character: a bare chunk
// (no "=") is appended to the most recent value with
// the separator re-inserted, instead of being
// silently dropped. Empty chunks are skipped.
func parseKeyValuePairs(s, sep string) map[string]string {
	out := map[string]string{}
	var lastKey string
	for _, part := range strings.Split(s, sep) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.Index(part, "=")
		if idx < 0 {
			// Bare chunk — append to previous value
			// if we have one. We re-insert the
			// separator (with surrounding spaces)
			// so the original phrasing survives.
			if lastKey == "" {
				continue
			}
			out[lastKey] = out[lastKey] + sep + " " + part
			continue
		}
		key := strings.ToLower(strings.TrimSpace(part[:idx]))
		val := strings.TrimSpace(part[idx+1:])
		val = strings.Trim(val, "\"'«»")
		if key == "" {
			continue
		}
		out[key] = val
		lastKey = key
	}
	return out
}

// parseMixedPairs tries both separators and returns
// whichever yields more recognised keys. Used for
// update_character, where the prompt's recommended
// form is comma-separated ("file=SOUL, section=внешность,
// append=...") but older turns may have used the
// semicolon form ("file=SOUL; section=внешность;
// append=..."). Picking the side with more populated
// keys avoids the trap where a single value contains
// both separators (e.g. an "append" text with a
// comma) and the wrong split eats the value. An
// empty parse result never wins over a non-empty
// one — otherwise a ";"-less body that contains
// only "," would collapse to an empty semicolon
// split (zero keys) and "beat" the comma split
// (three keys) under a naive >= comparison.
func parseMixedPairs(s string) map[string]string {
	semi := parseSemicolonPairs(s)
	comma := parseCommaPairs(s)
	if len(semi) == 0 {
		return comma
	}
	if len(comma) == 0 {
		return semi
	}
	if len(semi) >= len(comma) {
		return semi
	}
	return comma
}

// executeExtractedCommands runs every directive the parser
// recovered. Errors are logged at warn level but do not
// abort the turn — a malformed directive should not
// prevent the user-visible narrative from being delivered.
// The dispatch mirrors gm.dispatchOneTool but is data-
// driven (no llm.ToolCall envelope).
func (g *GM) executeExtractedCommands(ctx context.Context, chatID, world string, cmds []extractedCommand) {
	if len(cmds) == 0 {
		return
	}
	if g.tools == nil {
		g.log.Warn().Int("n", len(cmds)).Msg("context.extracted: no tools wired; skipping")
		return
	}
	var (
		executed int
		failed   int
		byKind   = map[string]int{}
	)
	for _, c := range cmds {
		byKind[c.Kind]++
		var err error
		switch c.Kind {
		case "update_npc":
			err = g.dispatchUpdateNpcDirective(ctx, world, c)
		case "update_state":
			err = g.dispatchUpdateStateDirective(ctx, world, c)
		case "append_lore":
			err = g.dispatchAppendLoreDirective(ctx, world, c)
		case "update_soul":
			err = g.dispatchUpdateSoulDirective(ctx, c)
		case "update_skill":
			err = g.dispatchUpdateSkillDirective(ctx, c)
		case "update_memory":
			err = g.dispatchUpdateMemoryDirective(ctx, c)
		case "update_inventory":
			err = g.dispatchUpdateInventoryDirective(ctx, c)
		case "remove_inventory_item":
			err = g.dispatchRemoveInventoryItemDirective(ctx, c)
		case "set_currency":
			err = g.dispatchSetCurrencyDirective(ctx, c)
		case "remove_currency":
			err = g.dispatchRemoveCurrencyDirective(ctx, c)
		case "create_npc":
			err = g.dispatchCreateNpcDirective(ctx, world, c)
		default:
			g.log.Warn().Str("kind", c.Kind).Str("raw", c.Raw).Msg("context.extracted: unknown kind")
			failed++
			continue
		}
		if err != nil {
			g.log.Warn().Err(err).Str("kind", c.Kind).Str("raw", c.Raw).Msg("context.extracted: dispatch failed")
			failed++
		} else {
			executed++
		}
	}
	if g.slow != nil {
		_ = g.slow.Write("context.extracted", chatID, map[string]any{
			"total":    len(cmds),
			"executed": executed,
			"failed":   failed,
			"by_kind":  byKind,
		})
	}
	g.log.Info().
		Int("total", len(cmds)).
		Int("executed", executed).
		Int("failed", failed).
		Interface("by_kind", byKind).
		Msg("context.extracted: processed directives")
}

// dispatchUpdateNpcDirective runs UpdateNPC from an
// extracted directive. The args map is what
// parseDirectiveLine / parseUpdateNpcLine produced.
func (g *GM) dispatchUpdateNpcDirective(_ context.Context, world string, c extractedCommand) error {
	name := strings.TrimSpace(c.Args["npc"])
	section := strings.TrimSpace(c.Args["section"])
	appendText := c.Args["append"]
	if name == "" {
		return fmt.Errorf("update_npc: npc name required")
	}
	if section == "" {
		return fmt.Errorf("update_npc: section required")
	}
	if strings.TrimSpace(appendText) == "" {
		return fmt.Errorf("update_npc: append text required (no empty updates)")
	}
	return g.tools.UpdateNPC(world, name, section, appendText)
}

// dispatchUpdateStateDirective runs UpdateState from a
// directive. The "npcs" and "events" args are comma-
// separated; "moment" and "in_flight" are scalars.
func (g *GM) dispatchUpdateStateDirective(_ context.Context, world string, c extractedCommand) error {
	moment := strings.TrimSpace(c.Args["moment"])
	if moment == "" {
		return fmt.Errorf("update_state: moment required")
	}
	inFlight := parseBoolArg(c.Args["in_flight"])
	npcs := splitCSV(c.Args["npcs"])
	events := splitCSV(c.Args["events"])
	day := readCurrentDay(g.fs, world)
	return g.tools.UpdateState(StateSnapshot{
		Day: day, InFlight: inFlight, Moment: moment, NPCs: npcs,
		AppendEvents: events,
	})
}

// dispatchAppendLoreDirective writes one deviation
// entry to lore.md. The header is a short title (e.g.
// "День 5"), the bullet is the new fact.
func (g *GM) dispatchAppendLoreDirective(_ context.Context, world string, c extractedCommand) error {
	header := strings.TrimSpace(c.Args["header"])
	bullet := strings.TrimSpace(c.Args["bullet"])
	if header == "" || bullet == "" {
		return fmt.Errorf("append_lore: header and bullet required")
	}
	return g.tools.AppendLore(world, header, bullet)
}

// dispatchUpdateSoulDirective routes a
// КОНТЕКСТ-блок "update_soul: section=X, append=Y"
// to the same AppendSoul path that the native
// tool call uses. The arg map is what
// parseDirectiveLine produced.
func (g *GM) dispatchUpdateSoulDirective(_ context.Context, c extractedCommand) error {
	section := strings.TrimSpace(c.Args["section"])
	appendText := c.Args["append"]
	if section == "" {
		return fmt.Errorf("update_soul: section required")
	}
	if strings.TrimSpace(appendText) == "" {
		return fmt.Errorf("update_soul: append text required (no empty updates)")
	}
	sc, err := g.ss.Start()
	if err != nil {
		return err
	}
	_, err = g.tools.AppendSoul(sc.Character, section, appendText)
	return err
}

// dispatchUpdateSkillDirective is the strict-enum
// sibling of dispatchUpdateSoulDirective. The
// charprofile layer rejects unknown section
// names; the error surfaces here verbatim.
func (g *GM) dispatchUpdateSkillDirective(_ context.Context, c extractedCommand) error {
	section := strings.TrimSpace(c.Args["section"])
	appendText := c.Args["append"]
	if section == "" {
		return fmt.Errorf("update_skill: section required")
	}
	if strings.TrimSpace(appendText) == "" {
		return fmt.Errorf("update_skill: append text required (no empty updates)")
	}
	sc, err := g.ss.Start()
	if err != nil {
		return err
	}
	_, err = g.tools.AppendSkill(sc.Character, section, appendText)
	return err
}

// dispatchUpdateMemoryDirective is the third
// per-file Append* dispatcher. Memory.yaml is
// the strictest enum (4 names).
func (g *GM) dispatchUpdateMemoryDirective(_ context.Context, c extractedCommand) error {
	section := strings.TrimSpace(c.Args["section"])
	appendText := c.Args["append"]
	if section == "" {
		return fmt.Errorf("update_memory: section required")
	}
	if strings.TrimSpace(appendText) == "" {
		return fmt.Errorf("update_memory: append text required (no empty updates)")
	}
	sc, err := g.ss.Start()
	if err != nil {
		return err
	}
	_, err = g.tools.AppendMemorySection(sc.Character, section, appendText)
	return err
}

// dispatchUpdateInventoryDirective is the
// REPLACE-by-name inventory path. The charprofile
// layer does the lookup; the dispatcher unpacks
// the args and forwards. equip is optional —
// false is the safe default for "I just picked
// this up, not wearing it yet".
func (g *GM) dispatchUpdateInventoryDirective(_ context.Context, c extractedCommand) error {
	name := strings.TrimSpace(c.Args["name"])
	typ := strings.TrimSpace(c.Args["type"])
	if name == "" {
		return fmt.Errorf("update_inventory: name required")
	}
	if typ == "" {
		return fmt.Errorf("update_inventory: type required")
	}
	sc, err := g.ss.Start()
	if err != nil {
		return err
	}
	_, err = g.tools.AppendInventoryItem(sc.Character, charprofile.Item{
		Name:        name,
		Description: c.Args["description"],
		Equip:       parseBoolArg(c.Args["equip"]),
		Special:     c.Args["special"],
	})
	return err
}

func (g *GM) dispatchRemoveInventoryItemDirective(_ context.Context, c extractedCommand) error {
	name := strings.TrimSpace(c.Args["name"])
	if name == "" {
		return fmt.Errorf("remove_inventory_item: name required")
	}
	sc, err := g.ss.Start()
	if err != nil {
		return err
	}
	return g.tools.RemoveInventoryItem(sc.Character, name)
}

func (g *GM) dispatchSetCurrencyDirective(_ context.Context, c extractedCommand) error {
	name := strings.TrimSpace(c.Args["name"])
	if name == "" {
		return fmt.Errorf("set_currency: name required")
	}
	count := toInt(c.Args["count"])
	sc, err := g.ss.Start()
	if err != nil {
		return err
	}
	_, err = g.tools.SetCurrency(sc.Character, name, count)
	return err
}

func (g *GM) dispatchRemoveCurrencyDirective(_ context.Context, c extractedCommand) error {
	name := strings.TrimSpace(c.Args["name"])
	if name == "" {
		return fmt.Errorf("remove_currency: name required")
	}
	sc, err := g.ss.Start()
	if err != nil {
		return err
	}
	return g.tools.RemoveCurrency(sc.Character, name)
}

// dispatchCreateNpcDirective is a best-effort fallback
// for when the model wrote create_npc as text but did
// not call it as a tool. We only handle the most
// common shape ("display_name=X; file_slug=Y;
// temperament=Z"); richer payloads (relations,
// nicknames, abilities) are dropped with a warning
// because the real tool path is the rich version.
func (g *GM) dispatchCreateNpcDirective(_ context.Context, world string, c extractedCommand) error {
	spec := NPCProfile{
		DisplayName: strings.TrimSpace(c.Args["display_name"]),
		File:        strings.TrimSpace(c.Args["file_slug"]),
		Temperament: strings.TrimSpace(c.Args["temperament"]),
		Relations:   strings.TrimSpace(c.Args["relations"]),
		Abilities:   []string{strings.TrimSpace(c.Args["abilities"])},
	}
	if spec.DisplayName == "" || spec.File == "" {
		return fmt.Errorf("create_npc: display_name and file_slug required")
	}
	return g.tools.Create(world, spec)
}

// parseBoolArg accepts "true" / "yes" / "1" / "on" as
// truthy; everything else is false.
func parseBoolArg(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "1", "on":
		return true
	}
	return false
}

// splitCSV splits on comma OR semicolon, trims, drops
// empties.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	if len(parts) == 1 {
		parts = strings.Split(s, ";")
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
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

// buildContext loads the current game-data and produces BOTH
// the system prompt (rules + character) and the world-state
// user message (world + NPCs) for the next turn.
//
// disableThinking forwards g.role.DisableThinking into
// BuildSystemPrompt so a /no_think sentinel is prepended when the
// role is configured to skip chain-of-thought.
//
// The system prompt is rules + character (stable for the
// whole conversation); the world-state user message is
// everything world-scoped (changes on end_day / leave /
// reload / compaction). They are sent as two separate
// messages with the world-state one holding the cache-pointe
// on Anthropic.
//
// Snapshotting: the two blocks are cached separately
// (g.systemSnapshot, g.worldSnapshot) but share the same
// sceneKey (world|character|day|in_flight). If any of those
// change, both are rebuilt. invalidateWorldState() drops both,
// forcing a rebuild on the next call (used by end_day, end_scene,
// leave_world, /reload, and compaction).
func (g *GM) buildContext() (systemMsg, worldMsg string, err error) {
	if !g.fs.Exists(storage.InfoFile) {
		// No game state yet — the snapshot is meaningless,
		// just render empty blocks. We still cache them
		// so repeat calls don't redo the lookup.
		g.contextMu.Lock()
		defer g.contextMu.Unlock()
		if g.systemSnapshot == "" {
			g.systemSnapshot = domain.BuildSystemPrompt(g.staticPrompt, domain.CharacterContext{}, g.role.DisableThinking)
			ws, werr := domain.BuildWorldStateMessage(domain.WorldContext{}, domain.CharacterContext{})
			if werr != nil {
				return "", "", werr
			}
			g.worldSnapshot = ws
			g.contextSceneKey = ""
		}
		return g.systemSnapshot, g.worldSnapshot, nil
	}
	sc, err := g.ss.Start()
	if err != nil {
		return "", "", err
	}
	sceneKey := sc.World + "|" + sc.Character + "|"
	// Day and InFlight live in state.md, not in
	// SessionContext. We parse the current state file
	// lazily and include both in the cache key so a
	// day boundary (in_flight flips) invalidates the
	// snapshot automatically.
	if stateBody, _ := g.fs.ReadRaw("worlds/" + sc.World + "/state.md"); stateBody != "" {
		snap := files.ParseStateMD(stateBody)
		sceneKey += strconv.Itoa(snap.Day) + "|" + strconv.FormatBool(snap.InFlight)
	} else {
		sceneKey += "0|false"
	}

	g.contextMu.Lock()
	if g.systemSnapshot != "" && g.worldSnapshot != "" && g.contextSceneKey == sceneKey {
		sys := g.systemSnapshot
		ws := g.worldSnapshot
		g.contextMu.Unlock()
		_ = g.slow.Write("context.snapshot.hit", "", map[string]any{
			"scene_key":   sceneKey,
			"system":      len(sys),
			"world_state": len(ws),
		})
		return sys, ws, nil
	}
	g.contextMu.Unlock()

	charCtx := domain.CharacterContext{
		Character:       sc.Character,
		CharacterSOUL:   safeRead(g.fs, "characters/"+sc.Character+"/SOUL.yaml"),
		CharacterSKILL:  safeRead(g.fs, "characters/"+sc.Character+"/skill.yaml"),
		CharacterMemory: safeRead(g.fs, "characters/"+sc.Character+"/memory.yaml"),
		// CharacterSections is intentionally empty
		// in the YAML era: the section list is
		// already in the file (the data: array).
		// The prompt gets the full body, so the
		// model sees the section names verbatim.
		// A separate enumeration block would just
		// duplicate the YAML keys.
	}
	worldCtx := domain.WorldContext{
		World:         sc.World,
		WorldState:    safeRead(g.fs, "worlds/"+sc.World+"/state.md"),
		WorldCanon:    safeRead(g.fs, "worlds/"+sc.World+"/canon.md"),
		WorldLore:     safeRead(g.fs, "worlds/"+sc.World+"/lore.md"),
		WorldPlan:     safeRead(g.fs, "worlds/"+sc.World+"/plan.md"),
		WorldMemorise: safeRead(g.fs, "worlds/"+sc.World+"/memorise.md"),
		WorldStage:    g.loadWorldStage(sc.World, sc.Character),
		NPCs:          g.loadActiveNPCs(sc.World, sc.State),
	}
	builtSystem := domain.BuildSystemPrompt(g.staticPrompt, charCtx, g.role.DisableThinking)
	builtWorld, err := domain.BuildWorldStateMessage(worldCtx, charCtx)
	if err != nil {
		return "", "", err
	}

	g.contextMu.Lock()
	g.systemSnapshot = builtSystem
	g.worldSnapshot = builtWorld
	g.contextSceneKey = sceneKey
	g.contextBuiltAt = time.Now()
	g.contextMu.Unlock()
	_ = g.slow.Write("context.snapshot.miss", "", map[string]any{
		"scene_key":   sceneKey,
		"system":      len(builtSystem),
		"world_state": len(builtWorld),
	})
	return builtSystem, builtWorld, nil
}

// buildContextPrompt is a thin wrapper kept for tests that
// expect a single string back. It returns ONLY the system
// prompt (Индекс 0). World state now lives in a separate
// user message — see buildContext.
//
// Deprecated: prefer buildContext which returns (system,
// world) so callers can wire the new [system, world, ...]
// wire surface. This wrapper is used by tests that only
// care about the system half.
func (g *GM) buildContextPrompt() (string, error) {
	sys, _, err := g.buildContext()
	return sys, err
}

// invalidateWorldState drops the cached system prompt and
// WorldState user message, forcing the next buildContext to
// re-read from disk. Called on:
//   - end_day (ArchiveDay hook) after appending the protocol
//   - end_scene
//   - leave_world (world switch)
//   - /reload (operator override)
//   - compaction (after appending "## Хроника текущего дня")
//
// Thread-safe.
func (g *GM) invalidateWorldState(reason string) {
	g.contextMu.Lock()
	g.systemSnapshot = ""
	g.worldSnapshot = ""
	g.contextSceneKey = ""
	g.contextMu.Unlock()
	_ = g.slow.Write("context.snapshot.invalidate", "", map[string]any{"reason": reason})
}

// invalidateWorldSnapshot drops ONLY the WorldState user
// message (the rules+character system block stays
// cached). Used by paths that mutate only world-scoped
// data (e.g. the per-NPC auto-maintain at end_day) so
// the next turn rebuilds user[0] with the new world
// body without paying the system-prompt cache miss.
// The full invalidateWorldState is the right call when
// the system prompt itself may have changed (character
// swap, narrative.md edit) — here, neither has.
func (g *GM) invalidateWorldSnapshot(reason string) {
	g.contextMu.Lock()
	g.worldSnapshot = ""
	// Scene key is also dropped so buildContext rebuilds
	// both blocks in lock-step on the next call (this
	// keeps the cache contract uniform — the system
	// block is re-rendered but its content is the same
	// as before, so any external cache key check is a
	// no-op for Anthropic / OpenAI).
	g.contextSceneKey = ""
	g.contextMu.Unlock()
	_ = g.slow.Write("context.world_snapshot.invalidate", "", map[string]any{"reason": reason})
}

// InvalidateWorldState is the public version of
// invalidateWorldState for callers outside this package
// (dispatcher /reload, world.Leave hook, etc.).
func (g *GM) InvalidateWorldState(reason string) {
	g.invalidateWorldState(reason)
}

// sceneKeyOf returns the cache key the current session would
// produce, without actually rebuilding. Exposed for tests.
func (g *GM) sceneKeyOf() (string, error) {
	if !g.fs.Exists(storage.InfoFile) {
		return "", nil
	}
	sc, err := g.ss.Start()
	if err != nil {
		return "", err
	}
	key := sc.World + "|" + sc.Character + "|"
	if stateBody, _ := g.fs.ReadRaw("worlds/" + sc.World + "/state.md"); stateBody != "" {
		snap := files.ParseStateMD(stateBody)
		key += strconv.Itoa(snap.Day) + "|" + strconv.FormatBool(snap.InFlight)
	} else {
		key += "0|false"
	}
	return key, nil
}

// shouldUseInPlaceCompaction reports whether this GM
// should prefer the in-place compaction path over the
// legacy drop-only compaction. We use the new path when
// the summarizer is wired AND the in-place prompt is
// loaded (otherwise the call would no-op and we silently
// lose the dropped turns' events).
func (g *GM) shouldUseInPlaceCompaction() bool {
	if g.summarizer == nil || !g.summarizer.IsConfigured() {
		return false
	}
	if g.summarizer.compactionInPlacePrompt == "" {
		return false
	}
	return true
}

// extractChronicleSection returns the body of the
// "## Хроника текущего дня" section in state.md, or
// "" if absent. The body is everything between the
// section header and the next "## " sibling header
// (or EOF).
func extractChronicleSection(stateMD string) string {
	if stateMD == "" {
		return ""
	}
	start := strings.Index(stateMD, "## Хроника текущего дня")
	if start < 0 {
		return ""
	}
	after := stateMD[start+len("## Хроника текущего дня"):]
	for i := 0; i < len(after); {
		if i+3 <= len(after) && after[i:i+3] == "## " {
			return strings.TrimRight(after[:i], "\n")
		}
		next := strings.IndexByte(after[i:], '\n')
		if next < 0 {
			return strings.TrimRight(after, "\n")
		}
		i += next + 1
	}
	return strings.TrimRight(after, "\n")
}

// appendChronicleEntry writes a "[События текущего дня
// Д<N>] ..." line to the "## Хроника текущего дня"
// section in state.md. If the section does not exist
// it is created. The line is APPENDED (chronological).
// "## Протокол прошедших дней" is a separate section —
// they do not mix.
func (g *GM) appendChronicleEntry(world string, day int, body []byte) error {
	if world == "" || len(body) == 0 {
		return nil
	}
	rel := "worlds/" + world + "/state.md"
	cur, _ := g.fs.ReadRaw(rel)
	sectionMarker := "## Хроника текущего дня"
	idx := strings.Index(cur, sectionMarker)
	entry := strings.TrimSpace(string(body)) + "\n"
	if idx < 0 {
		// Create the section at the end of the file.
		if cur != "" && !strings.HasSuffix(cur, "\n") {
			cur += "\n"
		}
		cur += "\n" + sectionMarker + "\n" + entry
	} else {
		// Find the end of the section. The section runs
		// until the next "## " header (or end of file).
		// We insert the new entry right after the
		// section header line, so the most recent
		// chronicle is at the top of the section
		// (closer to the model when it reads the file).
		afterHeader := cur[idx+len(sectionMarker):]
		nl := strings.Index(afterHeader, "\n")
		if nl < 0 {
			// Section header is the last line.
			cur += "\n" + entry
		} else {
			insertAt := idx + len(sectionMarker) + nl + 1
			cur = cur[:insertAt] + entry + cur[insertAt:]
		}
	}
	return g.fs.WriteRawAtomic(rel, cur)
}

// EndOfDay compresses the closing day into a
// 200-400 word narrative protocol and appends it to
// "## Протокол прошедших дней" in state.md. The protocol
// is consulted by the model through g.protocolWindowDays
// (default 2) — the oldest day is then evicted to
// memorise.md to keep the section bounded.
//
// Sources of context for the protocol:
//   - "## Хроника текущего дня" (if in-place compaction
//     ran during the day)
//   - state.md body (current "moment", active NPCs)
//   - the day's summary that ArchiveDay just wrote to
//     memorise.md (always present — ArchiveDay's
//     contract requires summary != "")
//
// Note: this is a "summary of summaries" — if the day
// had no compaction, the protocol is built mostly from
// the player's short summary in memorise.md. Days with
// heavy activity and no compaction will produce
// short protocols; that is a known cost of running
// without compaction. The narrative still captures
// the arc because the player wrote a summary.
func (g *GM) EndOfDay(ctx context.Context, world string, day int) error {
	if world == "" {
		return nil
	}
	if g.summarizer == nil || !g.summarizer.IsConfigured() {
		g.log.Warn().Str("world", world).Int("day", day).Msg("end-of-day: no summarizer wired; skipping protocol append")
		return nil
	}
	if g.summarizer.endOfDayPrompt == "" {
		g.log.Warn().Str("world", world).Int("day", day).Msg("end-of-day: end_of_day prompt not wired; skipping")
		return nil
	}
	stateMD, _ := g.fs.ReadRaw("worlds/" + world + "/state.md")
	memoriseMD, _ := g.fs.ReadRaw("worlds/" + world + "/memorise.md")
	// Build a context-only message set: extract the
	// current "## Хроника текущего дня" lines (if any)
	// and the player's summary from memorise.md. We
	// pass these as the user message so the model
	// reads them as one block, not as a conversation
	// turn. Using a synthetic single user message
	// keeps the call cheap and consistent.
	var ctxBuf strings.Builder
	if chronicle := extractChronicleSection(stateMD); chronicle != "" {
		ctxBuf.WriteString("# Хроника текущего дня (со сжатиями)\n")
		ctxBuf.WriteString(chronicle)
		ctxBuf.WriteString("\n\n")
	}
	if memoriseMD != "" {
		// Find the last "д<day>: " line in memorise.md.
		dayStr := fmt.Sprintf("%05d", day)
		marker := "д" + dayStr + ":"
		idx := strings.Index(memoriseMD, marker)
		if idx >= 0 {
			nl := strings.Index(memoriseMD[idx:], "\n")
			if nl >= 0 {
				ctxBuf.WriteString("# Краткий summary игрока в memorise.md (этот день)\n")
				ctxBuf.WriteString(memoriseMD[idx : idx+nl])
				ctxBuf.WriteString("\n")
			}
		}
	}
	dayMessages := []llm.Message{{Role: "user", Content: ctxBuf.String()}}
	res, err := g.summarizer.SummarizeEndOfDay(ctx, world, day, dayMessages, stateMD)
	if err != nil {
		return err
	}
	if len(res.Body) == 0 {
		g.log.Info().Str("world", world).Int("day", day).Msg("end-of-day: summarizer returned empty; skipping")
		return nil
	}
	if err := g.appendProtocolEntry(world, res.Body); err != nil {
		return err
	}
	// Window enforcement: if the section grew past the
	// limits, evict the oldest day to memorise.md.
	if err := g.enforceProtocolWindow(world); err != nil {
		g.log.Warn().Err(err).Str("world", world).Msg("end-of-day: enforce window failed (non-fatal)")
	}
	// Apply pending stage transition. The model calls
	// update_stage during the day; the transition lives in
	// stage.md as staging.next until end_day applies it.
	// We do this BEFORE MaintainNPCs/MaintainCharacterMemory
	// so the new stage is visible to the model on the next
	// turn's WorldState rebuild. No LLM call — the file is
	// rewritten in-place. Failure here is non-fatal: the
	// pending stage survives to the next end_day attempt.
	if err := g.tools.ApplyPendingStage(world); err != nil {
		g.log.Warn().Err(err).Str("world", world).Msg("end-of-day: apply_pending_stage failed; continuing")
	} else {
		g.invalidateWorldSnapshot("end_day_stage_transition")
	}
	// Auto-maintain NPC profiles that overflowed their
	// personal_memory list during the day. Synchronous
	// call — the player is reading the day's summary, a
	// 30-60s pause is acceptable. Per-NPC failures are
	// isolated (one bad profile does not block the
	// rest), see Memory.maintainOne. If any NPC was
	// rewritten, the world snapshot must be invalidated
	// so the next turn rebuilds the user[0] block with
	// the compacted profiles. The system prompt
	// (rules+character) is unaffected and stays cached.
	if touched, err := g.tools.MaintainNPCs(world); err != nil {
		g.log.Warn().Err(err).Str("world", world).Msg("end-of-day: maintain_npc hook failed; continuing")
	} else if len(touched) > 0 {
		g.invalidateWorldSnapshot("end_day_maintain_npc")
		g.log.Info().
			Str("world", world).
			Strs("touched", touched).
			Int("day", day).
			Msg("end-of-day: auto-maintained NPC profiles")
	}
	// Auto-maintain the ACTIVE character's
	// memory.yaml. Runs AFTER MaintainNPCs so the
	// LLM call budget is spent on the more
	// frequent compaction (NPC) first — character
	// memory is per-day, NPC is per-NPC, so NPC
	// wins on volume. Both hooks are best-effort:
	// the daily protocol already landed, a
	// 30-second LLM call failure does not roll
	// back the day.
	//
	// Resolves the active character from the
	// session start (the EndOfDay path is reached
	// from the end_day tool, where the player
	// just typed "end day N" with a character
	// already in the active session). When no
	// character is set (a brand-new world before
	// /launch), the hook is a no-op.
	if g.ss != nil {
		if sc, err := g.ss.Start(); err == nil && sc.Character != "" {
			if rewritten, err := g.tools.MaintainCharacterMemory(ctx, world, sc.Character); err != nil {
				g.log.Warn().Err(err).Str("world", world).Str("character", sc.Character).Msg("end-of-day: maintain_character_memory hook failed; continuing")
			} else if rewritten {
				g.invalidateWorldSnapshot("end_day_maintain_memory")
				g.log.Info().
					Str("world", world).
					Str("character", sc.Character).
					Int("day", day).
					Msg("end-of-day: defragmented character memory")
			}
		}
	}
	g.log.Info().Str("world", world).Int("day", day).Int("chars", res.OutputChars).Msg("end-of-day: protocol appended")
	return nil
}

// appendProtocolEntry appends a single #### Д<N> line
// (with the 200-400 word narrative) under
// "## Протокол прошедших дней" in state.md. The
// section is created if absent. The day number is
// parsed out of the body's leading marker so we do
// not double-write headers.
func (g *GM) appendProtocolEntry(world string, body []byte) error {
	rel := "worlds/" + world + "/state.md"
	cur, _ := g.fs.ReadRaw(rel)
	sectionMarker := "## Протокол прошедших дней"
	idx := strings.Index(cur, sectionMarker)
	entry := strings.TrimSpace(string(body)) + "\n"
	if idx < 0 {
		if cur != "" && !strings.HasSuffix(cur, "\n") {
			cur += "\n"
		}
		cur += "\n" + sectionMarker + "\n" + entry
	} else {
		// Insert right after the section header.
		after := cur[idx+len(sectionMarker):]
		nl := strings.Index(after, "\n")
		if nl < 0 {
			cur += "\n" + entry
		} else {
			insertAt := idx + len(sectionMarker) + nl + 1
			cur = cur[:insertAt] + entry + cur[insertAt:]
		}
	}
	return g.fs.WriteRawAtomic(rel, cur)
}

// protocolSectionBounds returns the start (offset of
// "## Протокол прошедших дней") and the end (the next
// "## " header or EOF) of the protocol section in
// state.md. If absent returns (-1, -1).
func protocolSectionBounds(body string) (start, end int) {
	start = strings.Index(body, "## Протокол прошедших дней")
	if start < 0 {
		return -1, -1
	}
	after := body[start+len("## Протокол прошедших дней"):]
	// Find next "## " at column 0 (a sibling header,
	// not part of the section body which uses "#### Д<NN>").
	for i := 0; i < len(after); {
		if i+3 <= len(after) && after[i:i+3] == "## " {
			return start, start + len("## Протокол прошедших дней") + i
		}
		next := strings.IndexByte(after[i:], '\n')
		if next < 0 {
			return start, len(body)
		}
		i += next + 1
	}
	return start, len(body)
}

// enforceProtocolWindow applies g.protocolWindowDays
// (count of day entries) and g.protocolMaxChars (size
// of the entire section) limits. The oldest day is
// moved into memorise.md (appended as a "д<NNNNN>: "
// line). The new section is the truncated remaining
// entries.
func (g *GM) enforceProtocolWindow(world string) error {
	rel := "worlds/" + world + "/state.md"
	cur, _ := g.fs.ReadRaw(rel)
	start, end := protocolSectionBounds(cur)
	if start < 0 {
		return nil
	}
	section := cur[start:end]
	lines := strings.Split(strings.TrimRight(section, "\n"), "\n")
	// First line is the header; the rest are the
	// day entries ("#### Д<N>\n<body>").
	if len(lines) <= 1 {
		return nil
	}
	header := lines[0]
	bodyLines := lines[1:]

	// Parse entries: each starts with "#### Д<NNNNN>".
	type entry struct {
		day    int
		header string
		body   string
	}
	var entries []entry
	var current *entry
	for _, l := range bodyLines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "#### Д") {
			rest := strings.TrimPrefix(t, "#### Д")
			colon := strings.Index(rest, ":")
			if colon < 0 {
				continue
			}
			n, err := strconv.Atoi(strings.TrimSpace(rest[:colon]))
			if err != nil {
				continue
			}
			current = &entry{day: n, header: t}
			entries = append(entries, *current)
		} else if current != nil {
			entries[len(entries)-1].body += l + "\n"
		}
	}

	// Decide what to evict: by count, then by chars.
	// The window is strict — we always trim to it,
	// even if a single entry is huge, because
	// tolerating one big entry defeats the point of
	// the limit. We then re-add it on the next day
	// if the user wants.
	for len(entries) > g.protocolWindowDays || (end-start) > g.protocolMaxChars {
		if len(entries) == 0 {
			break
		}
		oldest := entries[0]
		entries = entries[1:]
		// Move to memorise.md.
		if err := g.evictProtocolToMemorise(world, oldest); err != nil {
			return err
		}
		g.log.Info().Str("world", world).Int("day", oldest.day).Msg("end-of-day: protocol entry evicted to memorise")
	}

	// Reassemble the section.
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	for _, e := range entries {
		b.WriteString(e.header)
		b.WriteString("\n")
		if e.body != "" {
			b.WriteString(e.body)
			if !strings.HasSuffix(e.body, "\n") {
				b.WriteString("\n")
			}
		}
	}
	newSection := b.String()
	newBody := cur[:start] + newSection + cur[end:]
	return g.fs.WriteRawAtomic(rel, newBody)
}

// evictProtocolToMemorise appends the evicted day to
// memorise.md as "д<NNNNN>: <narrative>" so the
// 30-day LLM-compression path (Memory.MemoriseCompressWindow)
// can later condense it.
func (g *GM) evictProtocolToMemorise(world string, oldest struct {
	day    int
	header string
	body   string
}) error {
	rel := "worlds/" + world + "/memorise.md"
	cur, _ := g.fs.ReadRaw(rel)
	if cur != "" && !strings.HasSuffix(cur, "\n") {
		cur += "\n"
	}
	dayStr := fmt.Sprintf("%05d", oldest.day)
	narrative := strings.TrimSpace(oldest.body)
	cur += "д" + dayStr + ": " + narrative + "\n"
	return g.fs.WriteRawAtomic(rel, cur)
}

func safeRead(fs *storage.FileStore, rel string) string {
	s, _ := fs.ReadRaw(rel)
	return s
}

// loadWorldStage loads the active stage for the world and renders it
// into the WorldState user message. Returns an empty string when
// staging is disabled (sandbox) or no staging.yaml is configured.
//
// The character's display name is read from SOUL.yaml when available
// so the `$(name)` placeholder in stage descriptions expands to the
// human name rather than the directory slug. Fallback to the slug.
func (g *GM) loadWorldStage(world, characterSlug string) string {
	s, err := staging.Load(g.fs, world)
	if err != nil {
		g.log.Warn().Err(err).Str("world", world).Msg("load_world_stage failed; rendering without stage")
		return ""
	}
	if !s.Enabled {
		return ""
	}
	displayName := characterSlug
	if characterSlug != "" {
		if body, _ := g.fs.ReadRaw("characters/" + characterSlug + "/SOUL.yaml"); body != "" {
			var soul struct {
				Name string `yaml:"name"`
			}
			if err := yaml.Unmarshal([]byte(body), &soul); err == nil {
				if t := strings.TrimSpace(soul.Name); t != "" {
					displayName = t
				}
			}
		}
	}
	return s.Render(displayName)
}

// loadActiveNPCs reads the world state, parses the
// active NPC roster, and loads each profile at an LOD
// that depends on the cast size.
//
// Roster resolution: the canonical modern format
// (BuildStateMarkdown) emits "NPC: <comma list>" on a
// single line. The legacy format emitted
// "Активные NPC прямо сейчас: <list>". We accept
// either, preferring the modern parser (state.go's
// ParseStateMD handles both) so a hand-edited state.md
// or a pre-migration world keeps working.
//
// LOD policy (the cache budget is the constraint):
//
//	≤ 4 NPCs   → all Full (BuildMarkdown)
//	5 – 9      → first 4 Full, rest Compact
//	≥ 10      → first 3 Full, next 5 Compact, rest OneLine
//
// The tiers are chosen so the worst-case world block
// stays under roughly 7k tokens even with 20 active
// NPCs (3 Full ~ 4.5k + 5 Compact ~ 1.5k + 12 OneLine
// ~ 1k). The model always sees the names; only the
// depth of detail per NPC varies. The operator can
// still call search_npc for a deeper view of any
// background NPC.
func (g *GM) loadActiveNPCs(world, state string) []domain.NPCSnapshot {
	// First try the modern parser (state.md produced by
	// BuildStateMarkdown). It reads "NPC: ..." and
	// returns the roster on parsed.NPCs.
	if parsed := files.ParseStateMD(state); len(parsed.NPCs) > 0 {
		return g.loadRosterAtLOD(world, parsed.NPCs)
	}
	// Fallback: legacy "Активные NPC прямо сейчас:"
	// line, used by hand-edited or pre-migration files.
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
	return g.loadRosterAtLOD(world, names)
}

// loadRosterAtLOD walks a roster (already split into
// names) and renders each NPC at the policy-determined
// LOD. Pure helper — split out so the modern and
// legacy roster parsers share the same downstream
// pagination.
func (g *GM) loadRosterAtLOD(world string, names []string) []domain.NPCSnapshot {
	var out []domain.NPCSnapshot
	for i, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		lod := lodForIndex(i)
		body, err := g.tools.LoadLOD(world, name, lod)
		if err != nil {
			g.log.Warn().Err(err).Str("npc", name).Str("lod", lodName(lod)).Msg("skip npc load")
			continue
		}
		out = append(out, domain.NPCSnapshot{DisplayName: name, Profile: body})
	}
	return out
}

// lodForIndex maps a position in the active roster to
// an LOD tier. The 0-indexed cutoffs (3/8) match the
// 3/5/4 tier sizes — see loadActiveNPCs's comment.
func lodForIndex(i int) tools.NPCLOD {
	switch {
	case i < 3:
		return tools.LODFull
	case i < 8:
		return tools.LODCompact
	default:
		return tools.LODOneLine
	}
}

// lodName is the wire-friendly string for a level —
// used in slowlog and log lines, never on the model
// wire (the model gets the body, not the LOD tag).
func lodName(lod tools.NPCLOD) string {
	switch lod {
	case tools.LODFull:
		return "full"
	case tools.LODCompact:
		return "compact"
	case tools.LODOneLine:
		return "one_line"
	default:
		return "unknown"
	}
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
			// The errText alone ("npc profile not
			// found; call create_npc first") does not
			// name which NPC failed. The argument
			// map is decoded here (a second time —
			// dispatchOneTool already unmarshalled it)
			// so the slowlog + zerolog line tells the
			// operator at a glance which NPC, which
			// section, and which arg was rejected.
			// We swallow the decode error silently
			// because the first decode succeeded
			// and we are only reading; if the
			// arguments are genuinely malformed the
			// previous log line already covers it.
			args := map[string]any{}
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			ev := g.log.Warn().
				Str("tool", tc.Function.Name).
				Str("err", errText)
			if name, ok := args["npc"].(string); ok && name != "" {
				ev = ev.Str("npc", name)
			}
			if name, ok := args["display_name"].(string); ok && name != "" {
				ev = ev.Str("display_name", name)
			}
			if name, ok := args["file"].(string); ok && name != "" {
				ev = ev.Str("file", name)
			}
			if section, ok := args["section"].(string); ok && section != "" {
				ev = ev.Str("section", section)
			}
			ev.Msg("tool error")
		}
	}
	return out
}

// dispatchOneTool is the tool-to-usecase bridge. The argument JSON
// is decoded into the matching usecase type and the result is
// rendered as a short JSON-ish text the model can read.
func (g *GM) dispatchOneTool(ctx context.Context, tc llm.ToolCall) (string, string) {
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
		world := currentWorldName(g.fs)
		// BEFORE ArchiveDay updates state.md to day+1, produce
		// the "## Протокол прошедших дней" entry from whatever
		// context survived (Хроника if compaction ran, state.md
		// body, and the memorise.md summary).
		if err := g.EndOfDay(ctx, world, day); err != nil {
			g.log.Warn().Err(err).Str("world", world).Int("day", day).Msg("end_day: protocol append failed; continuing with archive")
		}
		if err := g.tools.ArchiveDay(ctx, world, day, summary); err != nil {
			return "", err.Error()
		}
		return okJSON("archived"), ""
	case "end_scene":
		// end_scene closes the current scene without
		// closing the day. The player has moved to a
		// new location / sub-plot. We:
		//   1. prune the active roster to the
		//      permanent_party subset (which the player
		//      may pass as a list of names — for now we
		//      accept it as a comma-separated string in
		//      `permanent_party` or read it from
		//      characters/<active>/SKILL.md as a
		//      `## permanent_party` section);
		//   2. reset the per-chat conversation so the
		//      next turn starts with a clean dialogue;
		//   3. drop the world snapshot so the next turn
		//      rebuilds user[0] with the pruned roster.
		// Step (1) is the only file change; (2) and (3)
		// are pure in-memory.
		if g.tools == nil {
			return "", "end_scene: tool not wired"
		}
		world := currentWorldName(g.fs)
		// Resolve permanent_party. Two sources are
		// accepted (in order of precedence):
		//   1. explicit tool arg: permanent_party (comma-
		//      separated list of display names).
		//   2. implicit: a "## permanent_party" section in
		//      characters/<active>/SKILL.md. This lets the
		//      operator pin a default cast without the
		//      model having to repeat it on every call.
		var pp []string
		if raw := toString(args["permanent_party"]); raw != "" {
			for _, p := range strings.Split(raw, ",") {
				if t := strings.TrimSpace(p); t != "" {
					pp = append(pp, t)
				}
			}
		} else if g.ss != nil {
			sc, err := g.ss.Start()
			if err == nil && sc.World != "" {
				// h5 refactor: permanent party is
				// world-scoped, not character-scoped.
				// Read it from the active world's
				// state.md, not from the character
				// files.
				if state, _ := g.fs.ReadRaw("worlds/" + sc.World + "/state.md"); state != "" {
					pp = extractPermanentParty(state)
				}
			}
		}
		res, err := g.tools.EndScene(world, pp)
		if err != nil {
			return "", err.Error()
		}
		// Reset the per-chat conversation so the next
		// turn rebuilds a clean dialogue. The chatID is
		// not in scope here (dispatchOneTool is
		// tool-call-level, not turn-level), so we reset
		// ALL conversations. This is acceptable: end_scene
		// is a rare, player-driven event, and a stale
		// conversation in a different chat is harmless.
		conversations.Range(func(k, _ any) bool {
			conversations.Delete(k)
			return true
		})
		// Drop the world snapshot so the next turn
		// rebuilds user[0] with the pruned roster.
		g.invalidateWorldSnapshot("end_scene")
		return okJSON(map[string]any{
			"status":          "scene_closed",
			"kept_npcs":       res.KeptNPCs,
			"pruned_npcs_len": res.PrunedNPCsLen,
			"hint":            "next turn rebuilds the scene around the kept roster",
		}), ""
	case "maintain_lore":
		rewritten, err := g.tools.MaintainLore(ctx, currentWorldName(g.fs))
		if err != nil {
			return "", err.Error()
		}
		return okJSON(map[string]any{"compacted": rewritten}), ""
	case "update_state":
		moment := toString(args["moment"])
		inFlight := toBool(args["in_flight"])
		npcs := toStringSlice(args["npcs"])
		events := toStringSlice(args["events"])
		if moment == "" {
			return "", "update_state requires moment"
		}
		day := readCurrentDay(g.fs, currentWorldName(g.fs))
		if err := g.tools.UpdateState(StateSnapshot{
			Day: day, InFlight: inFlight, Moment: moment, NPCs: npcs,
			AppendEvents: events,
		}); err != nil {
			return "", err.Error()
		}
		// Emit a `tool.update_state` slowlog event from
		// the GM layer too. The state.go side already
		// emits one with the on-disk delta (npcs_added /
		// npcs_removed / events_added). This GM-level
		// emission adds the LLM-side intent (the raw
		// args the model passed) so an operator can
		// correlate an `update_state` tool call with
		// the prompt that produced it. The two entries
		// are intentionally distinct — one tagged with
		// `dispatch: state` (filesystem delta), the other
		// with `dispatch: gm` (LLM intent).
		if g.slow != nil {
			_ = g.slow.Write("tool.update_state", "", map[string]any{
				"day":      day,
				"moment":   moment,
				"npcs":     npcs,
				"events":   events,
				"dispatch": "gm",
			})
		}
		// ToolResult is intentionally SHORT. We do NOT
		// rebuild the WorldState snapshot here (it would
		// bust the prompt cache every turn). The model
		// reads the delta in this ToolResult and writes
		// its narrative; the snapshot only rebuilds on
		// end_day/leave_world/reload/compaction.
		return okJSON(map[string]any{
			"status":   "recorded",
			"delta":    "локация/момент обновлены: " + moment,
			"npcs_now": npcs,
			"cache":    "stable",
		}), ""
	case "rotate_plan":
		events := toStringSlice(args["events"])
		if err := g.tools.RotatePlan(currentWorldName(g.fs), events); err != nil {
			return "", err.Error()
		}
		return okJSON("plan rotated"), ""
	case "create_npc":
		spec := NPCProfile{
			DisplayName: toString(args["display_name"]),
			File:        toString(args["file_slug"]),
			Temperament: toString(args["temperament"]),
			Relations:   toString(args["relations"]),
			Abilities:   toStringSlice(args["abilities"]),
			Nicknames:   toStringSlice(args["nicknames"]),
		}
		if err := g.tools.Create(currentWorldName(g.fs), spec); err != nil {
			return "", err.Error()
		}
		// Short ToolResult. The newly created NPC's
		// profile is on disk but is NOT yet loaded into
		// the next turn's WorldState (the snapshot stays
		// the same — adding it requires `update_state`
		// with the new npc in the list, which itself does
		// not invalidate the cache either). The model must
		// follow up with update_state if the NPC is in the
		// current scene.
		return okJSON(map[string]any{
			"status":       "created",
			"display_name": spec.DisplayName,
			"file":         spec.File,
			"cache":        "stable",
			"in_scene_now": false,
			"hint":         "NPC profile is on disk; add to scene with update_state if needed",
		}), ""
	case "leave_world":
		to := toString(args["to_world"])
		skip := toString(args["skip_note"])
		sc, err := g.ss.Start()
		if err != nil {
			return "", err.Error()
		}
		if _, err := g.tools.Leave(sc.World, to, skip, sc.Character); err != nil {
			return "", err.Error()
		}
		g.ResetConversation(sc.Character)
		return okJSON("world switched"), ""
	case "update_soul":
		// h5: 4 per-file Append* tools replace the legacy
		// single update_character(file=...) dispatcher.
		// The section name is free-form on SOUL.yaml.
		if g.tools == nil {
			return "", "update_soul: tool not wired"
		}
		section := toString(args["section"])
		appendText := toString(args["append"])
		if section == "" {
			return "", "update_soul: section required"
		}
		if strings.TrimSpace(appendText) == "" {
			return "", "update_soul: append text required (no empty updates)"
		}
		sc, err := g.ss.Start()
		if err != nil {
			return "", err.Error()
		}
		_, err = g.tools.AppendSoul(sc.Character, section, appendText)
		if err != nil {
			return "", err.Error()
		}
		return okJSON(map[string]any{
			"status":  "appended",
			"file":    "SOUL.yaml",
			"section": section,
			"cache":   "stable",
		}), ""
	case "update_skill":
		// skill.yaml is a fixed-enum file. The dispatcher
		// itself rejects unknown section names; the error
		// surfaces back to the model so it can recover
		// (try a different canonical name).
		if g.tools == nil {
			return "", "update_skill: tool not wired"
		}
		section := toString(args["section"])
		appendText := toString(args["append"])
		if section == "" {
			return "", "update_skill: section required"
		}
		if strings.TrimSpace(appendText) == "" {
			return "", "update_skill: append text required"
		}
		sc, err := g.ss.Start()
		if err != nil {
			return "", err.Error()
		}
		_, err = g.tools.AppendSkill(sc.Character, section, appendText)
		if err != nil {
			return "", err.Error()
		}
		return okJSON(map[string]any{
			"status":  "appended",
			"file":    "skill.yaml",
			"section": section,
			"cache":   "stable",
		}), ""
	case "update_memory":
		// memory.yaml sections are STRICT (4 names). The
		// charprofile layer rejects unknowns.
		if g.tools == nil {
			return "", "update_memory: tool not wired"
		}
		section := toString(args["section"])
		appendText := toString(args["append"])
		if section == "" {
			return "", "update_memory: section required"
		}
		if strings.TrimSpace(appendText) == "" {
			return "", "update_memory: append text required"
		}
		sc, err := g.ss.Start()
		if err != nil {
			return "", err.Error()
		}
		_, err = g.tools.AppendMemorySection(sc.Character, section, appendText)
		if err != nil {
			return "", err.Error()
		}
		return okJSON(map[string]any{
			"status":  "appended",
			"file":    "memory.yaml",
			"section": section,
			"cache":   "stable",
		}), ""
	case "update_inventory":
		// REPLACE-by-name semantics. The charprofile
		// layer does the lookup; the dispatcher just
		// unpacks the args. equip defaults to false
		// when omitted (the optional Boolean helper
		// returns false on a missing key).
		if g.tools == nil {
			return "", "update_inventory: tool not wired"
		}
		name := strings.TrimSpace(toString(args["name"]))
		if name == "" {
			return "", "update_inventory: name required"
		}
		typ := toString(args["type"])
		if typ == "" {
			return "", "update_inventory: type required"
		}
		sc, err := g.ss.Start()
		if err != nil {
			return "", err.Error()
		}
		_, err = g.tools.AppendInventoryItem(sc.Character, charprofile.Item{
			Name:        name,
			Description: toString(args["description"]),
			Equip:       toBool(args["equip"]),
			Special:     toString(args["special"]),
		})
		if err != nil {
			return "", err.Error()
		}
		return okJSON(map[string]any{
			"status": "recorded",
			"name":   name,
			"type":   typ,
			"cache":  "stable",
		}), ""
	case "remove_inventory_item":
		if g.tools == nil {
			return "", "remove_inventory_item: tool not wired"
		}
		name := strings.TrimSpace(toString(args["name"]))
		if name == "" {
			return "", "remove_inventory_item: name required"
		}
		sc, err := g.ss.Start()
		if err != nil {
			return "", err.Error()
		}
		if err := g.tools.RemoveInventoryItem(sc.Character, name); err != nil {
			return "", err.Error()
		}
		return okJSON(map[string]any{
			"status": "removed",
			"name":   name,
			"cache":  "stable",
		}), ""
	case "set_currency":
		if g.tools == nil {
			return "", "set_currency: tool not wired"
		}
		name := strings.TrimSpace(toString(args["name"]))
		if name == "" {
			return "", "set_currency: name required"
		}
		count := toInt(args["count"])
		sc, err := g.ss.Start()
		if err != nil {
			return "", err.Error()
		}
		_, err = g.tools.SetCurrency(sc.Character, name, count)
		if err != nil {
			return "", err.Error()
		}
		return okJSON(map[string]any{
			"status": "recorded",
			"name":   name,
			"count":  count,
			"cache":  "stable",
		}), ""
	case "remove_currency":
		if g.tools == nil {
			return "", "remove_currency: tool not wired"
		}
		name := strings.TrimSpace(toString(args["name"]))
		if name == "" {
			return "", "remove_currency: name required"
		}
		sc, err := g.ss.Start()
		if err != nil {
			return "", err.Error()
		}
		if err := g.tools.RemoveCurrency(sc.Character, name); err != nil {
			return "", err.Error()
		}
		return okJSON(map[string]any{
			"status": "removed",
			"name":   name,
			"cache":  "stable",
		}), ""
	case "update_npc":
		if g.tools == nil {
			return "", "update_npc: tool not wired"
		}
		npcName := toString(args["npc"])
		section := toString(args["section"])
		appendText := toString(args["append"])
		if npcName == "" {
			return "", "update_npc: npc name required"
		}
		if section == "" {
			return "", "update_npc: section required"
		}
		if strings.TrimSpace(appendText) == "" {
			return "", "update_npc: append text required (no empty updates)"
		}
		if err := g.tools.UpdateNPC(currentWorldName(g.fs), npcName, section, appendText); err != nil {
			return "", err.Error()
		}
		// Short ToolResult. NPC profile updated on disk;
		// snapshot stays the same.
		return okJSON(map[string]any{
			"status":  "appended",
			"npc":     npcName,
			"section": section,
			"cache":   "stable",
		}), ""
	case "search_npc":
		// search_npc returns a compact description (not the
		// full YAML) so the messages cache stays tight.
		// Rate-limit (in-memory): at most one search per
		// query string per 5 turns — prevents the model
		// from re-asking the same NPC over and over.
		if g.tools == nil {
			return "", "search_npc: tool not wired"
		}
		query := strings.TrimSpace(toString(args["query"]))
		if query == "" {
			return "", "search_npc: query required"
		}
		if rate, ok := g.npcSearchRate[query]; ok && g.turnCounter-rate < g.rateWindow {
			return "", "search_npc: rate-limited (same query recently)"
		}
		world := currentWorldName(g.fs)
		res, err := g.tools.SearchNPC(world, query)
		if err != nil {
			// Compact error so the messages cache stays
			// small and the model can recover (try a
			// different query, or call create_npc).
			return "", "search_npc: " + err.Error()
		}
		g.npcSearchRate[query] = g.turnCounter
		return okJSON(map[string]any{
			"status":         "found",
			"display_name":   res.DisplayName,
			"slug":           res.Slug,
			"temperament":    res.Temperament,
			"current_status": res.CurrentStatus,
			"source":         res.Source,
			"cache":          "stable",
			"hint":           "if you need this NPC in the current scene, follow up with update_state",
		}), ""
	}
	return "", "unknown tool: " + tc.Function.Name
}

// --- helpers ---

// allToolCallsBroken reports whether every entry in calls
// has an empty Function.Name — the signature of a stream
// that emitted `delta.tool_calls` JSON fragments but never
// assembled them into a complete call. minimax-m3:cloud
// (Ollama Cloud) does this when its reasoning block runs
// past the response budget: the model decides to call a
// tool, the stream cuts off before the head lands, and the
// caller (us) ends up with one or more headless tool-call
// entries. They would be useless to dispatch (every one
// becomes "unknown tool:" in dispatchOneTool's default
// branch), so we treat the whole round as "empty content"
// and let the existing retry path take over.
func allToolCallsBroken(calls []llm.ToolCall) bool {
	if len(calls) == 0 {
		return false
	}
	for _, tc := range calls {
		if tc.Function.Name != "" {
			return false
		}
	}
	return true
}

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

// retryEmptyOnce re-issues the LLM request that just
// produced 0 content (and 0 surviving tool calls).
// Two cases motivate this:
//
//  1. The cloud-Ollama deployment returning 4 chunks
//     of `delta.content: ""` and then `finish_reason:
//     stop` — recovers on the second attempt.
//  2. minimax-m3:cloud emitting a native `tool_calls`
//     round whose arguments are not valid JSON (Ollama
//     double-wrap / truncation). The driver drops the
//     broken calls, the GM is left with an empty
//     content, and re-issuing the SAME prompt yields
//     the same broken calls. We nudge the model by
//     appending a synthetic user message asking it to
//     skip tool calls and write the directives inline
//     in the КОНТЕКСТ block instead — the parser picks
//     them up the same way.
//
// Returned: the assistant text (may still be empty),
// the token usage, the raw SSE trace (so the slowlog
// can diagnose a second empty round), and the stream
// error if any.
func (g *GM) retryEmptyOnce(ctx context.Context, messages []llm.Message, timeoutSec int, nudgeToolCallsOff bool) (string, TokenUsage, []string, error) {
	var (
		buf          strings.Builder
		usageFromAPI llm.Usage
		gotUsage     bool
		compChars    int
		finishReason string
		rawTrace     []string
	)
	totals := TokenUsage{}
	if g.tracking == "" || g.tracking == "off" {
		totals.Source = "off"
	}
	outMsgs := messages
	if nudgeToolCallsOff {
		// Append (do not replace) a synthetic user
		// turn that asks the model to skip native
		// tool calls on this retry. We add a
		// distinct marker in the prompt so the
		// model treats it as a one-shot hint rather
		// than a system rule. The parser does not
		// see this — it never touches a messages
		// slice, only the rendered content.
		hint := "[system note] Предыдущий вызов tool_calls не прошёл парсинг аргументов " +
			"на стороне драйвера. На этой попытке НЕ используй native tool_calls — " +
			"верни только текстовый ответ: 4-блочный markdown (Режим B) или JSON " +
			"с полями narration/context/future/validation (Режим A). Если нужно " +
			"обновить character-файлы — пиши маркеры в поле `context` или в блок " +
			"**КОНТЕКСТ И ИЗМЕНЕНИЯ**: ⦁ update_soul: section=..., append=... " +
			"(или update_skill / update_memory — по файлу, к которому относится факт)."
		outMsgs = append(outMsgs, llm.Message{Role: "user", Content: hint})
	}
	streamErr := g.llm.Stream(ctx, llm.ChatRequest{
		Model:          g.role.Model,
		Messages:       outMsgs,
		Tools:          g.toolSpecs,
		Temperature:    g.role.Temperature,
		MaxTokens:      g.role.MaxTokens,
		TimeoutSeconds: timeoutSec,
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
		return "", totals, rawTrace, streamErr
	}
	_ = compChars
	_ = finishReason
	if gotUsage {
		totals = TokenUsage{
			PromptTokens:     usageFromAPI.PromptTokens,
			CompletionTokens: usageFromAPI.CompletionTokens,
			TotalTokens:      usageFromAPI.TotalTokens,
			Source:           "usage",
		}
	} else if g.tracking == "estimate" {
		totals = TokenUsage{
			PromptTokens:     llm.EstimateMessages(messages),
			CompletionTokens: llm.EstimateTokens(buf.String()),
			TotalTokens:      llm.EstimateMessages(messages) + llm.EstimateTokens(buf.String()),
			Source:           "estimate",
		}
	}
	return buf.String(), totals, rawTrace, nil
}

// formatMessagesForLog renders a messages slice into a list of
// compact {role, content_len, content, tool_calls?} entries for
// the slowlog "request_messages" field. The full content is
// preserved (truncation loses the very information we need on a
// format miss — was the system prompt / history cut by the
// context window?). This payload is logged only on the
// failing path; healthy rounds emit a 500-char preview instead.
//
// The output is intentionally a []map[string]any (not a
// []llm.Message) so the slowlog JSON is decoupled from the
// internal type — operators reading it in jq see exactly the
// shape that left the GM, no Go struct tags.
func formatMessagesForLog(messages []llm.Message) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		entry := map[string]any{
			"role":        m.Role,
			"content_len": len(m.Content),
		}
		if m.Content != "" {
			entry["content"] = m.Content
		}
		if len(m.ToolCalls) > 0 {
			calls := make([]map[string]any, 0, len(m.ToolCalls))
			for _, c := range m.ToolCalls {
				calls = append(calls, map[string]any{
					"name": c.Function.Name,
					"args": c.Function.Arguments,
				})
			}
			entry["tool_calls"] = calls
		}
		out = append(out, entry)
	}
	return out
}

// formatToolsForLog renders a []llm.ToolSchema into a compact
// {name, description_len} slice for the slowlog "request_tools"
// field. We omit the JSON parameter schema (it is large,
// static, and identical to the embedded copy in the
// narrative.md prompt) — what the operator needs to confirm
// on a format miss is "was the tool list non-empty?" and
// "were the create_npc / update_npc schemas present?".
func formatToolsForLog(specs []llm.ToolSchema) []map[string]any {
	out := make([]map[string]any, 0, len(specs))
	for _, s := range specs {
		out = append(out, map[string]any{
			"name": s.Name,
		})
	}
	return out
}

func toolNames(specs []llm.ToolSchema) []string {
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	return names
}
