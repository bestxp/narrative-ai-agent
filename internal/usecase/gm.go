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
	"narrative/internal/structured"
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
			// produced no content. Re-prompting on an empty
			// string would force us to send "[system note]
			// missing: 4 blocks" with no original to fix, and
			// the model would either repeat nothing or invent
			// text that ignores our blocks.
			//
			// We DO try up to g.role.MaxEmptyRetries automatic
			// retries of the same request: cloud Ollama
			// models (and o1 family) occasionally return a
			// `finish_reason: stop` with 0 content on the
			// first round and a normal reply on the second.
			// Some flaky providers need two restarts to
			// recover. The retry is identical (same
			// messages, same temperature) — it just gives
			// the model another chance. MaxEmptyRetries is
			// the hard cap: if all retries return empty we
			// surface the placeholder so the operator can
			// diagnose via slowlog. The default of 2
			// is set in config/config.go's Validate; the
			// operator can raise it for particularly flaky
			// cloud providers via config.yaml.
			if len(assistantBuf.String()) == 0 {
				// MaxEmptyRetries=0 explicitly disables
				// auto-retry — the operator gets the polite
				// placeholder immediately. Any positive
				// value N yields exactly N retries.
				if g.role.MaxEmptyRetries <= 0 {
					if cb.OnDelta != nil {
						_ = cb.OnDelta("⚠️ модель не вернула ответ — попробуй ещё раз")
					}
					g.log.Warn().
						Int("round", round).
						Str("finish", finishReason).
						Msg("llm returned empty content; auto-retry disabled")
					return totals, nil
				}
				for attempt := 1; attempt <= g.role.MaxEmptyRetries; attempt++ {
					retryText, retryUsage, retryRawTrace, retryErr := g.retryEmptyOnce(ctx, messages, g.role.EmptyRetryTimeoutSeconds)
					totals.PromptTokens += retryUsage.PromptTokens
					totals.CompletionTokens += retryUsage.CompletionTokens
					totals.TotalTokens += retryUsage.TotalTokens
					if retryUsage.Source != "" && (totals.Source == "" || totals.Source == "off") {
						totals.Source = retryUsage.Source
					}
					if retryErr != nil {
						g.log.Warn().Err(retryErr).Int("attempt", attempt).Msg("empty-content retry failed; falling through to placeholder")
						break
					}
					if retryText != "" {
						// Success on retry — splice the
						// recovered text into the visible
						// buffer with a marker so the
						// player sees what happened.
						if cb.OnDelta != nil {
							_ = cb.OnDelta("\n\n[ответ восстановлен после повтора]\n\n" + retryText)
						}
						conv.mu.Lock()
						if len(conv.messages) > 0 {
							last := conv.messages[len(conv.messages)-1]
							if last.Role == "assistant" {
								last.Content = "[первый ответ был пустым]\n\n" + retryText
								conv.messages[len(conv.messages)-1] = last
							}
						}
						conv.mu.Unlock()
						g.log.Info().Int("attempt", attempt).Int("retry_chars", len(retryText)).Msg("empty-content retry succeeded")
						return totals, nil
					}
					// Retry also empty — log the raw trace
					// for diagnosis. Continue the loop to
					// try the next attempt.
					if g.slow != nil {
						_ = g.slow.Write("llm.empty_retry", chatID, map[string]any{
							"round":     round,
							"attempt":   attempt,
							"raw_trace": retryRawTrace,
						})
					}
					g.log.Warn().
						Int("round", round).
						Int("attempt", attempt).
						Msg("empty-content retry also returned 0; trying again")
				}
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
		if err := g.tools.ArchiveDay(currentWorldName(g.fs), day, summary); err != nil {
			return "", err.Error()
		}
		return okJSON("archived"), ""
	case "run_maintenance":
		touched, err := g.tools.CompactNPCs(currentWorldName(g.fs))
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

// retryEmptyOnce re-issues the exact same LLM request that
// just produced 0 content. The two cloud-Ollama cases that
// motivated this — the minimax-m3:cloud deployment returning
// 4 chunks of `delta.content: ""` and then `finish_reason:
// stop`, and the o1 family occasionally returning an empty
// stop frame after a long thinking phase — both recover on
// the second attempt. The retry is byte-for-byte identical
// to the original (same messages, same temperature, same
// tools); the only thing that changes is the model gets
// another chance to emit visible text.
//
// Returned: the assistant text (may still be empty), the
// token usage, the raw SSE trace (so the slowlog can
// diagnose a second empty round), and the stream error if
// any.
func (g *GM) retryEmptyOnce(ctx context.Context, messages []llm.Message, timeoutSec int) (string, TokenUsage, []string, error) {
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
	streamErr := g.llm.Stream(ctx, llm.ChatRequest{
		Model:          g.role.Model,
		Messages:       messages,
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
