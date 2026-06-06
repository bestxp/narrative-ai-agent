package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"narrative/internal/adapter/llm"
	"narrative/internal/domain"
	"narrative/internal/slowlog"
)

// Summarizer compresses old conversation turns into a short
// fact log via a cheap secondary LLM role. The bot calls it
// during compaction preflight (when the narrative history grows
// past context_window * compaction_threshold): the dropped
// turns are passed in, the role returns a 200-400 token
// chronological summary that the GM then appends to state.md
// under "## История (сжато)" so the next system prompt has
// the facts even after the original turns leave conversations[].
//
// Three configurations are supported:
//
//   1. Dedicated summary role (e.g. deepseek-v4-flash). Cheap,
//      fast, isolated from narrative. RECOMMENDED.
//   2. Fallback: the narrative role itself is used with
//      lowered max_tokens and temperature so a single-model
//      deployment still gets a written fact log. The cost is
//      one extra round-trip to the same model per compaction;
//      latency is comparable to the dedicated role.
//   3. Disabled: no summary at all — drop-only. The OnCompaction
//      notice still fires, but state.md gets no history section.
type Summarizer struct {
	llm    LLMClient
	role   llm.RoleConfig
	prompt string
	slow   *slowlog.Logger
	log    zerolog.Logger
	// fallbackMode is true when this summarizer shares the
	// narrative model. We use the same role config but with
	// a small MaxTokens override to keep the response tight.
	fallbackMode bool
}

// NewSummarizer builds a Summarizer for the given role. The
// role's SystemPromptPath is read at construction so a missing
// prompt is a startup-time error, not a runtime surprise.
func NewSummarizer(llmCli LLMClient, role llm.RoleConfig, systemPrompt string, slow *slowlog.Logger, log zerolog.Logger) *Summarizer {
	return &Summarizer{
		llm:    llmCli,
		role:   role,
		prompt: systemPrompt,
		slow:   slow,
		log:    log.With().Str("component", "summarizer").Logger(),
	}
}

// NewFallbackSummarizer wires the narrative role itself as a
// summary role. Used when no dedicated summary role is
// configured. The MaxTokens is clamped to 500 and the
// temperature to 0.2 — narrative temperatures are typically
// 0.7+ which produce noisy summaries. The system prompt is
// the same prompts/summary.md (or whatever the operator
// pointed at via the fallback path).
func NewFallbackSummarizer(llmCli LLMClient, narrative llm.RoleConfig, systemPrompt string, slow *slowlog.Logger, log zerolog.Logger) *Summarizer {
	role := narrative
	if role.MaxTokens > 500 {
		role.MaxTokens = 500
	}
	if role.Temperature == 0 || role.Temperature > 0.4 {
		role.Temperature = 0.2
	}
	return &Summarizer{
		llm:          llmCli,
		role:         role,
		prompt:       systemPrompt,
		slow:         slow,
		log:          log.With().Str("component", "summarizer").Logger(),
		fallbackMode: true,
	}
}

// IsFallback reports whether this summarizer uses the
// narrative model (true) or a dedicated summary role (false).
// The operator sees this in slowlog to disambiguate the
// source of the summary in the audit trail.
func (s *Summarizer) IsFallback() bool {
	return s != nil && s.fallbackMode
}

// IsConfigured reports whether the summarizer is wired to a
// real LLM. The GM checks this before calling Summarize.
func (s *Summarizer) IsConfigured() bool {
	return s != nil && s.llm != nil
}

// SummaryResult is what the summarizer returns. Text is the
// compressed text the caller should write to state.md; Tokens
// is the count of the *input* turns so the operator can see
// how much the summary role compressed. Source is "summary" if
// the role responded, "skipped" if no role was wired.
type SummaryResult struct {
	Text   string
	Tokens int
	Source string
}

// SummarizeOldTurns compresses the provided conversation
// messages into a 200-400 token chronological summary. The
// caller passes the turns that are about to be dropped from
// conversations[] (NOT the kept ones). The summarizer renders
// the messages to a flat text buffer, ships it to the summary
// role, and returns the assistant's reply.
//
// On any error the function returns a zero SummaryResult and
// the err — the caller (GM) logs and falls back to drop-only.
func (s *Summarizer) SummarizeOldTurns(ctx context.Context, messages []llm.Message) (SummaryResult, error) {
	if !s.IsConfigured() {
		return SummaryResult{Source: "skipped"}, nil
	}
	if len(messages) == 0 {
		return SummaryResult{Source: "skipped"}, nil
	}
	userText := renderTurnsForSummary(messages)
	tokens := EstimateConversationTokens(messages, 0)
	req := llm.ChatRequest{
		Model: s.role.Model,
		Messages: []llm.Message{
			{Role: "system", Content: s.prompt},
			{Role: "user", Content: userText},
		},
		Temperature: s.role.Temperature,
		MaxTokens:   s.role.MaxTokens,
	}
	var buf strings.Builder
	streamErr := s.llm.Stream(ctx, req, func(ch llm.Chunk) error {
		if ch.Done || ch.Content == "" {
			return nil
		}
		buf.WriteString(ch.Content)
		return nil
	})
	if streamErr != nil {
		return SummaryResult{}, fmt.Errorf("summarizer: stream: %w", streamErr)
	}
	out := strings.TrimSpace(buf.String())
	if out == "" {
		return SummaryResult{}, fmt.Errorf("summarizer: empty response from %s", s.role.Model)
	}
	res := SummaryResult{
		Text:   out,
		Tokens: tokens,
		Source: source(s),
	}
	s.log.Info().
		Str("model", s.role.Model).
		Bool("fallback", s.fallbackMode).
		Int("input_tokens", tokens).
		Int("output_chars", len(out)).
		Msg("summary generated")
	if s.slow != nil {
		_ = s.slow.Write("summary.generated", "", map[string]any{
			"model":        s.role.Model,
			"fallback":     s.fallbackMode,
			"input_tokens": tokens,
			"output_chars": len(out),
		})
	}
	return res, nil
}

// source returns "summary" for dedicated roles and
// "summary-fallback" for narrative-mode roles. The flag is
// visible in slowlog so the operator can audit which model
// was used without grepping config.
func source(s *Summarizer) string {
	if s.fallbackMode {
		return "summary-fallback"
	}
	return "summary"
}

// renderTurnsForSummary flattens a slice of chat-completion
// messages into a single user-role text buffer that the
// summary role can consume. The format is intentionally
// minimal — the summary role is told in its system prompt
// what to look for, the body is just chronological feed.
func renderTurnsForSummary(msgs []llm.Message) string {
	var b strings.Builder
	b.WriteString("# История диалога (хронологически)\n\n")
	for i, m := range msgs {
		switch m.Role {
		case "user":
			fmt.Fprintf(&b, "[Игрок]: %s\n", strings.TrimSpace(m.Content))
		case "assistant":
			body := strings.TrimSpace(m.Content)
			if body == "" && len(m.ToolCalls) > 0 {
				names := make([]string, 0, len(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					names = append(names, tc.Function.Name)
				}
				body = "(вызвал tools: " + strings.Join(names, ",") + ")"
			}
			fmt.Fprintf(&b, "[GM]: %s\n", body)
		case "tool":
			fmt.Fprintf(&b, "  [→ %s]: %s\n", m.Name, strings.TrimSpace(m.Content))
		}
		if i < len(msgs)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// AppendHistoryToState appends a compaction summary as a new
// section to the active world's state.md. The section is
// timestamped and includes a marker so the operator can grep
// the file for "сжато" and find every compaction.
//
// The function reads the existing state, parses it via
// parseStateMD, and re-renders through BuildStateMarkdown so
// the format stays consistent. The history block lives at
// the end of the file (after the Хронология дня) and is
// append-only — we never rewrite old history sections.
func (m *Maintenance) AppendHistoryToState(world, summary string, at time.Time) error {
	if world == "" {
		return errors.New("maintenance: empty world")
	}
	if summary == "" {
		return nil
	}
	rel := "worlds/" + world + "/state.md"
	cur, _ := m.fs.ReadRaw(rel)
	parsed := parseStateMD(cur)
	parsed.World = world
	// History is stored under the existing "Events" slice with
	// a [history] prefix so the layout stays compatible with
	// BuildStateMarkdown. BuildStateMarkdown renders the
	// "Хронология дня" section from Events; we tag the
	// summary line with [history] so a reader can tell it
	// apart from in-day events.
	marker := fmt.Sprintf("[history сжато %s]", at.UTC().Format("2006-01-02 15:04"))
	parsed.Events = append(parsed.Events, marker+"\n"+summary)
	body := domain.BuildStateMarkdown(parsed)
	return m.fs.WriteRawAtomic(rel, body)
}
