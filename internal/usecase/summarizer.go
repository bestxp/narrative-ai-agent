package usecase

import (
	"context"
	"fmt"
	"strings"

	"github.com/rs/zerolog"

	"narrative/internal/adapter/llm"
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

// NPCSummaryResult is what SummarizeNPC returns. Body is
// the new YAML profile the caller writes back to disk;
// Compressed is true when the body shrank (false means
// the summarizer decided the profile was already tight
// and returned the input unchanged). BeforeCount and
// AfterCount are personal_memory lengths so the operator
// can see "40 → 28 facts" at a glance in slowlog.
type NPCSummaryResult struct {
	Body         []byte
	Compressed   bool
	BeforeCount  int
	AfterCount   int
	OutputChars  int
}

// SummarizeNPC asks the LLM to compact a single NPC
// profile. The system prompt (passed in as `systemPrompt`,
// typically loaded from internal/prompts/npc_summary.md)
// tells the model the rules: keep base sections, dedup
// relations, prune abilities, squeeze personal_memory to
// 20-30 critical facts. The world name and the
// memorise.md tail give the model context (which days
// were key, which NPCs are still active).
//
// The summarizer is best-effort. If the LLM returns the
// same content it received (no compression), or returns
// invalid YAML, or returns fewer facts than the input had
// (the model went too far), the caller (MaintainNPCs)
// leaves the file untouched and logs a warning.
//
// This method is safe to call from any goroutine — the
// underlying LLMClient.Stream is safe under concurrent
// use on the same role (the GM serialises per-chat turns
// with chatMu; the maintenance tool is called serially
// per round by the dispatcher).
func (s *Summarizer) SummarizeNPC(ctx context.Context, displayName, world string, yamlBody, memoriseTail []byte) (NPCSummaryResult, error) {
	res := NPCSummaryResult{Body: yamlBody, Compressed: false}
	if !s.IsConfigured() {
		return res, nil
	}
	if len(yamlBody) == 0 {
		return res, nil
	}

	// Build the user prompt. The memorise.md tail
	// (last 20 days) gives the model the long-term
	// context it needs to decide which facts are
	// "key" vs "everyday". Without that context the
	// model treats every fact as critical.
	var userBuf strings.Builder
	userBuf.WriteString("# World: ")
	userBuf.WriteString(world)
	userBuf.WriteString("\n\n# NPC: ")
	userBuf.WriteString(displayName)
	userBuf.WriteString("\n\n# Current profile (YAML)\n```yaml\n")
	userBuf.Write(yamlBody)
	userBuf.WriteString("\n```\n")
	if len(memoriseTail) > 0 {
		userBuf.WriteString("\n# Хвост memorise.md (последние дни, контекст)\n```\n")
		userBuf.Write(memoriseTail)
		userBuf.WriteString("\n```\n")
	}
	userBuf.WriteString("\nВерни сжатый YAML. Только YAML, без обёрток и комментариев.")

	// Reuse the role config but bump MaxTokens — the
	// compression output can be up to the input size
	// (we never enlarge, but the model is verbose
	// about YAML).
	role := s.role
	if role.MaxTokens < 2000 {
		role.MaxTokens = 2000
	}
	if role.Temperature == 0 || role.Temperature > 0.4 {
		role.Temperature = 0.2
	}

	req := llm.ChatRequest{
		Model: role.Model,
		Messages: []llm.Message{
			{Role: "system", Content: s.prompt},
			{Role: "user", Content: userBuf.String()},
		},
		Temperature: role.Temperature,
		MaxTokens:   role.MaxTokens,
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
		return res, fmt.Errorf("summarizer: npc stream: %w", streamErr)
	}
	out := strings.TrimSpace(buf.String())
	if out == "" {
		return res, fmt.Errorf("summarizer: empty response for npc %q", displayName)
	}

	// Strip a leading/trailing ```yaml fence the
	// model sometimes emits despite being told not
	// to. We do NOT trust anything between a fence
	// pair: even a "thinking" preamble is dropped
	// because the parser only knows the YAML shape.
	cleaned := stripYAMLFence(out)
	res.Body = []byte(cleaned)
	res.OutputChars = len(cleaned)
	res.Compressed = len(cleaned) < len(yamlBody)

	// Sanity check: try to parse. If invalid, the
	// caller will log a warning and skip the write.
	// We do not return an error here because the
	// caller may want to keep the original file and
	// just log the failure (the round-trip is
	// idempotent — the file stays as it was).
	return res, nil
}

// stripYAMLFence removes a leading ```yaml (or ```) and
// the matching trailing ``` from the model response. The
// summarizer system prompt forbids fences but a fraction
// of models emit them anyway; we keep the parse path
// permissive.
func stripYAMLFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if idx := strings.Index(s, "\n"); idx > 0 {
		s = s[idx+1:]
	}
	if idx := strings.LastIndex(s, "```"); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
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

// LoreSummaryResult is what SummarizeLore returns. Body
// is the new markdown the caller writes back; Compressed
// is true when the body shrank; BeforeLines / AfterLines
// are file lengths so the operator can see "500 → 230
// lines" in slowlog.
type LoreSummaryResult struct {
	Body        []byte
	Compressed  bool
	BeforeLines int
	AfterLines  int
	OutputChars int
}

// SummarizeLore asks the LLM to compact a world's
// lore.md. The system prompt (passed in as `systemPrompt`,
// loaded from internal/prompts/lore_summary.md) tells
// the model the rules: keep canon deviations, death,
// first NPC appearances; trim routine events; preserve
// chronologically. The world name and the memorise.md
// tail + state.md give the model the long-term context.
//
// Best-effort: invalid markdown or a returned body
// that is not smaller than the input leaves the file
// untouched. The caller (MaintainLore) decides what
// counts as "valid" — for lore this is just a non-empty
// body with at least one "## " section.
func (s *Summarizer) SummarizeLore(ctx context.Context, world string, loreBody, memoriseTail, stateMD []byte) (LoreSummaryResult, error) {
	res := LoreSummaryResult{Body: loreBody, Compressed: false}
	if !s.IsConfigured() {
		return res, nil
	}
	if len(loreBody) == 0 {
		return res, nil
	}

	var userBuf strings.Builder
	userBuf.WriteString("# World: ")
	userBuf.WriteString(world)
	userBuf.WriteString("\n\n# Current lore.md\n```markdown\n")
	userBuf.Write(loreBody)
	userBuf.WriteString("\n```\n")
	if len(memoriseTail) > 0 {
		userBuf.WriteString("\n# Хвост memorise.md (последние 20 дней)\n```\n")
		userBuf.Write(memoriseTail)
		userBuf.WriteString("\n```\n")
	}
	if len(stateMD) > 0 {
		userBuf.WriteString("\n# state.md (текущий момент)\n```\n")
		userBuf.Write(stateMD)
		userBuf.WriteString("\n```\n")
	}
	userBuf.WriteString("\nВерни сжатый lore.md. Только markdown, без обёрток.")

	role := s.role
	if role.MaxTokens < 4000 {
		role.MaxTokens = 4000
	}
	if role.Temperature == 0 || role.Temperature > 0.4 {
		role.Temperature = 0.2
	}

	req := llm.ChatRequest{
		Model: role.Model,
		Messages: []llm.Message{
			{Role: "system", Content: s.prompt},
			{Role: "user", Content: userBuf.String()},
		},
		Temperature: role.Temperature,
		MaxTokens:   role.MaxTokens,
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
		return res, fmt.Errorf("summarizer: lore stream: %w", streamErr)
	}
	out := strings.TrimSpace(buf.String())
	if out == "" {
		return res, fmt.Errorf("summarizer: empty response for lore of %q", world)
	}
	cleaned := stripMarkdownFence(out)
	res.Body = []byte(cleaned)
	res.OutputChars = len(cleaned)
	res.Compressed = len(cleaned) < len(loreBody)
	return res, nil
}

// stripMarkdownFence removes a leading ```markdown (or
// ```) and the matching trailing ``` from a model
// response. The lore summarizer prompt forbids fences
// but a fraction of models emit them anyway; we keep
// the parse path permissive.
func stripMarkdownFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if idx := strings.Index(s, "\n"); idx > 0 {
		s = s[idx+1:]
	}
	if idx := strings.LastIndex(s, "```"); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

