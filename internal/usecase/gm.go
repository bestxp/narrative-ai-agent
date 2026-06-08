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

	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/structured"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools/files"
)

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
const maxToolRounds = 5

func (g *GM) Reply(ctx context.Context, chatID, userText string, cb Callbacks) (TokenUsage, error) {
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
		if len(fullContent) > 0 && !g.isFormatCompliant(fullContent) {
			fields["content_full"] = fullContent
			fields["missing_headers"] = g.missingFormatHeaders(fullContent)
			// On a format miss we also log the request we sent
			// (system prompt + history + tool specs) so an
			// operator can see whether the model was actually
			// told the 4-block contract on this round. The
			// payload is large (10-30 KB), so we keep it gated
			// on the failing path. We render each message
			// as {role, content, tool_calls?} so the slowlog
			// entry is readable in jq.
			fields["request_messages"] = formatMessagesForLog(messages)
			fields["request_tools"] = formatToolsForLog(g.toolSpecs)
		}
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
					// Persist ONLY the fixed chunk as the
					// assistant turn. The truncated
					// original is omitted from history —
					// otherwise the model sees half a
					// sentence + a [формат восстановлен]
					// marker on its next turn and tries
					// to "continue" the broken text. The
					// recovered text already satisfies
					// the 4-block contract, so it is
					// self-contained as a training
					// example.
					conv.mu.Lock()
					if len(conv.messages) > 0 {
						last := conv.messages[len(conv.messages)-1]
						if last.Role == "assistant" {
							last.Content = fixed
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
		// Post-tool guard: if the model returned a narrative that
		// mentions facts about the player (age, name, skills) or
		// new NPC names but did NOT call update_character /
		// create_npc, the player's persistent files are now
		// out-of-date. We log a warning so the operator can
		// see the gap in slow.log, and inject a one-shot hint
		// onto the next user turn via the conv history so the
		// model gets a chance to self-correct.
		g.missedToolGuard(ctx, chatID, toolCalls, assistantBuf.String(), currentWorldName(g.fs))
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
	// Missing update_character: the assistant turn mentions
	// the player's name, age, or a clear self-disclosure
	// ("мне N лет", "меня зовут", "я умею"). These are the
	// triggers the prompt's checklist calls out, so the
	// absence of the tool is a real signal.
	if !called["update_character"] {
		triggers := []string{
			"мне ", "лет", "меня зовут", "я умею", "мой навык", "моя цель",
			"я родился", "я из ", "моё прошлое", "в моём мире", "в моей школе",
		}
		for _, t := range triggers {
			if strings.Contains(lower, t) {
				g.log.Warn().
					Str("trigger", t).
					Str("chat", chatID).
					Msg("narrative mentions player facts but update_character was not called")
				if g.slow != nil {
					_ = g.slow.Write("gm.missed_tool", chatID, map[string]any{
						"tool":   "update_character",
						"reason": "player facts mentioned in narrative",
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

// extractedCommand is one actionable directive recovered
// from the assistant turn. The GM runs these locally as
// if the model had issued a real tool_call (which on
// Ollama Cloud is silently ignored — see the rationale
// in cmd/test-openapi/main.go). Commands are pure data;
// the dispatcher below (executeExtractedCommands) knows
// how to route them to the right usecase.Tool method.
type extractedCommand struct {
	Kind string // "update_npc" | "update_state" | "append_lore" | "update_character" | "create_npc"
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
		"update_character", "create_npc",
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
	case strings.HasPrefix(lower, "update_character:"):
		body := strings.TrimSpace(stripped[len("update_character:"):])
		// "file=SOUL, section=внешность, append=..." — the
		// comma form is what the prompt recommends
		// (short, one fact per directive, three keys).
		// We also accept the semicolon form for
		// robustness, since older turns may have used
		// it. parseMixedPairs tries ";" first, then
		// "," — whichever gives us all three keys wins.
		args := parseMixedPairs(body)
		return extractedCommand{
			Kind: "update_character",
			Args: args,
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
		case "update_character":
			err = g.dispatchUpdateCharacterDirective(ctx, c)
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

// dispatchUpdateCharacterDirective is symmetric with
// update_npc but writes to the active character's
// SKILL/SOUL/memory.md files.
func (g *GM) dispatchUpdateCharacterDirective(_ context.Context, c extractedCommand) error {
	file := strings.TrimSpace(c.Args["file"])
	section := strings.TrimSpace(c.Args["section"])
	appendText := c.Args["append"]
	if file == "" || section == "" || strings.TrimSpace(appendText) == "" {
		return fmt.Errorf("update_character: file, section, append required")
	}
	sc, err := g.ss.Start()
	if err != nil {
		return err
	}
	return g.tools.Append(sc.Character, file, section, appendText)
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
		Abilities:   strings.TrimSpace(c.Args["abilities"]),
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
		// Inject the operator's actual section
		// vocabulary into the prompt so the model
		// picks an existing `## <name>` rather
		// than inventing one. Without this block
		// the model guesses ("внешность",
		// "ресурсы") and Append happily creates a
		// new header next to the existing
		// "Истинная сущность" / "Философия" one.
		CharacterSections: files.FormatSectionList(
			safeRead(g.fs, "characters/"+sc.Character+"/SOUL.md"),
			safeRead(g.fs, "characters/"+sc.Character+"/SKILL.md"),
			safeRead(g.fs, "characters/"+sc.Character+"/memory.md"),
		),
		WorldCanon:        safeRead(g.fs, "worlds/"+sc.World+"/canon.md"),
		WorldState:        safeRead(g.fs, "worlds/"+sc.World+"/state.md"),
		WorldLore:         safeRead(g.fs, "worlds/"+sc.World+"/lore.md"),
		WorldPlan:         safeRead(g.fs, "worlds/"+sc.World+"/plan.md"),
		WorldMemorise:     safeRead(g.fs, "worlds/"+sc.World+"/memorise.md"),
		NPCs:              g.loadActiveNPCs(sc.World, sc.State),
	}
	return domain.BuildSystemPrompt(g.staticPrompt, ctx, g.role.DisableThinking), nil
}

func safeRead(fs *storage.FileStore, rel string) string {
	s, _ := fs.ReadRaw(rel)
	return s
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
		body, err := g.tools.Load(world, name)
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
		if err := g.tools.ArchiveDay(ctx, currentWorldName(g.fs), day, summary); err != nil {
			return "", err.Error()
		}
		return okJSON("archived"), ""
	case "run_maintenance":
		// Alias for the renamed maintain_npcs tool.
		// The legacy name is kept so existing prompts
		// that call run_maintenance still work; the
		// canonical name in narrative.md is
		// maintain_npcs (new behaviour: LLM-driven
		// compaction, was naive strip).
		touched, err := g.tools.MaintainNPCs(currentWorldName(g.fs))
		if err != nil {
			return "", err.Error()
		}
		return okJSON(map[string]any{"compacted": touched}), ""
	case "maintain_npcs":
		touched, err := g.tools.MaintainNPCs(currentWorldName(g.fs))
		if err != nil {
			return "", err.Error()
		}
		return okJSON(map[string]any{"compacted": touched}), ""
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
		return okJSON("state updated"), ""
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
			Abilities:   toString(args["abilities"]),
			Nicknames:   toStringSlice(args["nicknames"]),
		}
		if err := g.tools.Create(currentWorldName(g.fs), spec); err != nil {
			return "", err.Error()
		}
		return okJSON("npc created"), ""
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
	case "update_character":
		if g.tools == nil {
		return "", "update_character: tool not wired"
	}
		file := toString(args["file"])
		section := toString(args["section"])
		appendText := toString(args["append"])
		sc, err := g.ss.Start()
		if err != nil {
			return "", err.Error()
		}
		if err := g.tools.Append(sc.Character, file, section, appendText); err != nil {
			return "", err.Error()
		}
		return okJSON(map[string]any{
			"file":    file,
			"section": section,
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
		return okJSON(map[string]any{
			"npc":     npcName,
			"section": section,
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

// isFormatCompliant accepts EITHER of two shapes:
//
//  1. JSON mode (preferred). The assistant turn is a
//     JSON object with four fields (narration, context,
//     future, validation) and no extraneous keys. The
//     provider with structured_output=json_object
//     guarantees this shape; the bot still validates
//     because providers sometimes ignore strict mode.
//
//  2. Legacy markdown mode. The assistant turn contains
//     all four "**диалоги и действия**" / "КОНТЕКСТ" /
//     "БУДУЩЕЕ" / "ВАЛИДАЦИЯ ПРАВИЛ" headers. The legacy
//     driver and providers that ignore json_object take
//     this path.
//
// The function does NOT log missing fields — call
// missingFormatHeaders for that. The distinction matters
// because for JSON we want to surface a different
// diagnostic (e.g. "model produced JSON with a missing
// `future` field") than for markdown.
func (g *GM) isFormatCompliant(text string) bool {
	if text == "" {
		return false
	}
	if structured.LooksLikeJSON(text) {
		n, err := structured.Parse(text)
		if err != nil {
			return false
		}
		return len(n.MissingFields()) == 0
	}
	for _, h := range requiredFormatHeaders {
		if !strings.Contains(text, h) {
			return false
		}
	}
	return true
}

// missingFormatHeaders reports the contract elements
// that are absent in the assistant turn. For JSON the
// list is the empty field names; for markdown the list
// is the bold headers. The format re-prompt path uses
// the returned strings verbatim, so the wording must
// be unambiguous to the model — see repromptForFormat
// for the prompt template.
func (g *GM) missingFormatHeaders(text string) []string {
	if text == "" {
		return requiredFormatHeaders
	}
	if structured.LooksLikeJSON(text) {
		n, err := structured.Parse(text)
		if err != nil {
			return requiredFormatHeaders
		}
		return n.MissingFields()
	}
	var missing []string
	for _, h := range requiredFormatHeaders {
		if !strings.Contains(text, h) {
			missing = append(missing, h)
		}
	}
	return missing
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
			"**КОНТЕКСТ И ИЗМЕНЕНИЯ**: ⦁ update_character: file=SOUL, section=..., " +
			"append=..."
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
			"**КРАТКО**: нарратив ≤ 150 слов, каждый служебный блок — 1-2 строки (иначе ответ оборвётся по лимиту токенов). "+
			"**Не продолжай** предыдущий оборванный текст — начни с нового абзаца. "+
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
		Tools:       g.toolSpecs,
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
					"name":      c.Function.Name,
					"args":      c.Function.Arguments,
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
