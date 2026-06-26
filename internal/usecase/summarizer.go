package usecase

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
	"github.com/bestxp/narrative-ai-agent/internal/chronicle"
	"github.com/bestxp/narrative-ai-agent/internal/npcprofile"
	"github.com/bestxp/narrative-ai-agent/internal/prompts"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools/files"
	"github.com/rs/zerolog"
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
//  1. Dedicated summary role (e.g. deepseek-v4-flash). Cheap,
//     fast, isolated from narrative. RECOMMENDED.
//  2. Fallback: the narrative role itself is used with
//     lowered max_tokens and temperature so a single-model
//     deployment still gets a written fact log. The cost is
//     one extra round-trip to the same model per compaction;
//     latency is comparable to the dedicated role.
//  3. Disabled: no summary at all — drop-only. The OnCompaction
//     notice still fires, but state.md gets no history section.
type Summarizer struct {
	llm    LLMClient
	role   llm.RoleConfig
	prompt string
	// compactionInPlacePrompt is used by SummarizeInPlace.
	// When empty, SummarizeInPlace no-ops and returns an
	// empty body. Wired in main.go via
	// SetCompactionInPlacePrompt or NewSummarizerWith (we
	// keep the basic NewSummarizer constructor for backward
	// compat with the migrate tests).
	compactionInPlacePrompt string
	// endOfDayPrompt is used by SummarizeEndOfDay. Same
	// nil-safety as compactionInPlace.
	endOfDayPrompt string
	// characterMemoryPrompt is used by
	// SummarizeCharacterMemory. Loaded from
	// prompts/character_memory_maintain.md. When
	// empty, SummarizeCharacterMemory no-ops (the
	// caller still runs the size check; the LLM
	// call is the only thing skipped). Wired in
	// main.go via SetCharacterMemoryPrompt.
	characterMemoryPrompt string
	// chronicleSummaryPrompt is the system prompt for
	// SummarizeChronicle, loaded from
	// prompts/chronicle_summary.md.tmpl. When empty,
	// SummarizeChronicle falls back to s.prompt (the base
	// summary.md prompt) for backward compat. Wired in
	// main.go via SetChronicleSummaryPrompt.
	chronicleSummaryPrompt string
	slow                   *slowlog.Logger
	log                    zerolog.Logger
	// fallbackMode is true when this summarizer shares the
	// narrative model. We use the same role config but with
	// a small MaxTokens override to keep the response tight.
	fallbackMode bool
	// compaction is the config-derived compaction knobs
	// (InPlaceSummaryWordsMin/Max, EndOfDay...,
	// OldTurns..., LoreTargetLines..., Memorise...).
	// Used by RenderSummarizerUser so the user-message
	// templates can reference {{ .Compaction.* }} for
	// the soft targets ("150-300 слов", "200-400 токенов",
	// etc.) instead of hard-coded magic numbers. Wired
	// in main.go via SetCompactionConfig.
	compaction prompts.CompactionData
}

// NewSummarizer builds a Summarizer for the given role. The
// role's SystemPromptPath is read at construction so a missing
// prompt is a startup-time error, not a runtime surprise.
func NewSummarizer(
	llmCli LLMClient,
	role llm.RoleConfig,
	systemPrompt string,
	slow *slowlog.Logger,
	log zerolog.Logger,
) *Summarizer {
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
func NewFallbackSummarizer(
	llmCli LLMClient,
	narrative llm.RoleConfig,
	systemPrompt string,
	slow *slowlog.Logger,
	log zerolog.Logger,
) *Summarizer {
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
	if s == nil || !s.IsConfigured() {
		return SummaryResult{Source: "skipped"}, nil
	}

	if len(messages) == 0 {
		return SummaryResult{Source: "skipped"}, nil
	}

	userText, err := s.renderSummary("summarizer_old_turns_user.md.tmpl", prompts.NewOldTurnsSummaryData(projectMessages(messages)))
	if err != nil {
		return SummaryResult{}, fmt.Errorf("summarizer: render old-turns user: %w", err)
	}

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
	Body        []byte
	Compressed  bool
	BeforeCount int
	AfterCount  int
	OutputChars int
}

// SummarizeNPC asks the LLM to compact a single NPC
// profile. The system prompt (passed in as `systemPrompt`,
// typically loaded from internal/prompts/npc_summary.md)
// tells the model the rules: keep base sections, dedup
// relations, prune abilities, squeeze personal_memory to
// 20-30 critical facts. The world name and the
// chronicle tail give the model context (which days
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
func (s *Summarizer) SummarizeNPC(
	ctx context.Context,
	displayName, world string,
	yamlBody, chronicleTail []byte,
) (NPCSummaryResult, error) {
	res := NPCSummaryResult{Body: yamlBody, Compressed: false}
	if !s.IsConfigured() {
		return res, nil
	}

	if len(yamlBody) == 0 {
		return res, nil
	}

	data := prompts.NewNPCSummaryData(
		world,
		displayName,
		projectNPCProfile(yamlBody),
		projectChronicle(parseChronicleBytes(chronicleTail)),
	)

	userText, err := s.renderSummary("summarizer_npc_user.md.tmpl", data)
	if err != nil {
		return res, fmt.Errorf("summarizer: render npc user: %w", err)
	}

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
			{Role: "user", Content: userText},
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

// projectMessages converts []llm.Message into the
// template-friendly []prompts.MessageData. This is data
// projection only — all formatting ([Игрок]/[GM]/[→ tool]
// labels, trimming, separator newlines) lives in the
// summarizer_*_user.md.tmpl templates via {{ range }} and
// the `trim`/`join` FuncMap helpers. The Go side never
// touches LLM-facing text.
func projectMessages(msgs []llm.Message) []prompts.MessageData {
	out := make([]prompts.MessageData, 0, len(msgs))
	for _, m := range msgs {
		var toolCalls []string
		if len(m.ToolCalls) > 0 {
			toolCalls = make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				toolCalls = append(toolCalls, tc.Function.Name)
			}
		}

		out = append(out, prompts.MessageData{
			Role:      m.Role,
			Content:   m.Content,
			Name:      m.Name,
			ToolCalls: toolCalls,
		})
	}

	return out
}

// projectChronicle converts chronicle.Chronicle into the
// template-friendly *prompts.ChronicleData. Data-only
// projection: no formatting, no markdown. Returns nil for
// a nil input so the template's {{ if .Chronicle }} guard
// skips the block.
func projectChronicle(c *chronicle.Chronicle) *prompts.ChronicleData {
	if c == nil {
		return nil
	}

	out := &prompts.ChronicleData{
		Periods: make([]prompts.ChroniclePeriodData, len(c.Periods)),
	}
	for i, p := range c.Periods {
		out.Periods[i] = prompts.ChroniclePeriodData{
			From:   p.From,
			To:     p.To,
			Memory: p.Memory,
		}
	}

	for n, txt := range c.Days {
		out.Days = append(out.Days, prompts.ChronicleDayData{Number: n, Text: txt})
	}

	sort.Slice(out.Days, func(i, j int) bool { return out.Days[i].Number < out.Days[j].Number })

	return out
}

// parseChronicleBytes parses a raw YAML chronicle body
// into *chronicle.Chronicle. Returns nil on empty input or
// parse error — the caller's template guard skips the
// block. Data-only.
func parseChronicleBytes(body []byte) *chronicle.Chronicle {
	if len(body) == 0 {
		return nil
	}

	c, err := chronicle.Load(string(body))
	if err != nil {
		return nil
	}

	return &c
}

// projectNPCProfile parses a YAML body into
// npcprofile.Profile and projects it into the
// template-friendly *prompts.NPCProfileData. Data-only:
// the template renders the YAML/markdown from the struct.
// Returns nil if the body fails to parse — the caller
// should log and skip in that case.
func projectNPCProfile(yamlBody []byte) *prompts.NPCProfileData {
	if len(yamlBody) == 0 {
		return nil
	}

	p, err := npcprofile.Load(string(yamlBody))
	if err != nil {
		return nil
	}

	rows := make([]prompts.NPCRelationRow, 0, len(p.RelationsNPCs))
	for _, r := range p.RelationsNPCs {
		rows = append(rows, prompts.NPCRelationRow{
			Target: strings.TrimSpace(r.Target),
			Note:   strings.TrimSpace(r.Note),
		})
	}

	return prompts.NewNPCProfileDataFromFields(
		strings.TrimSpace(p.DisplayName),
		strings.TrimSpace(p.Temperament),
		strings.TrimSpace(p.RelationsGG),
		rows,
		p.Abilities,
		p.PersonalMemory,
		p.CriticalKnowledge,
		p.Nicknames,
		strings.TrimSpace(p.CurrentStatus),
		strings.TrimSpace(p.LastUpdate),
	)
}

// projectState converts a raw state.yaml body into the
// template-friendly *prompts.StateData via the
// canonical full-state parser. Data-only: the
// template renders the YAML from the struct. Returns
// nil if parsing yields nothing useful.
func projectState(stateBody string) *prompts.StateData {
	if strings.TrimSpace(stateBody) == "" {
		return nil
	}

	snap := files.ParseStateYAMLFull(stateBody)

	return prompts.NewStateData(
		snap.World, snap.Day, snap.InFlight,
		snap.Daytime, snap.Location, snap.Moment, snap.Current,
		prompts.StageStateData{
			Current:       snap.Stage.Current,
			TimelineIndex: snap.Stage.TimelineIndex,
			Next:          snap.Stage.Next,
		},
		snap.NPCs, snap.Events,
	)
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
// chronologically. The world name and the chronicle
// tail + state.md give the model the long-term context.
//
// Best-effort: invalid markdown or a returned body
// that is not smaller than the input leaves the file
// untouched. The caller (MaintainLore) decides what
// counts as "valid" — for lore this is just a non-empty
// body with at least one "## " section.
func (s *Summarizer) SummarizeLore(
	ctx context.Context,
	world string,
	loreBody, chronicleTail, stateMD []byte,
) (LoreSummaryResult, error) {
	res := LoreSummaryResult{Body: loreBody, Compressed: false}
	if !s.IsConfigured() {
		return res, nil
	}

	if len(loreBody) == 0 {
		return res, nil
	}

	data := prompts.NewLoreSummaryData(
		world,
		string(loreBody),
		projectChronicle(parseChronicleBytes(chronicleTail)),
		projectState(string(stateMD)),
	)

	userText, err := s.renderSummary("summarizer_lore_user.md.tmpl", data)
	if err != nil {
		return res, fmt.Errorf("summarizer: render lore user: %w", err)
	}

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
			{Role: "user", Content: userText},
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
	res.Compressed = true

	return res, nil
}

// CharacterMemorySummaryResult is what
// SummarizeCharacterMemory returns. Body is the
// new memory.yaml in `data: [{section, values}]`
// shape. Compressed is true when the body shrank
// (a real defragmentation happened, not a no-op
// echo). BeforeBytes / AfterBytes / BeforeValues /
// AfterValues feed the slowlog line.
type CharacterMemorySummaryResult struct {
	Body         []byte
	Compressed   bool
	BeforeBytes  int
	AfterBytes   int
	OutputChars  int
	BeforeValues int
	AfterValues  int
}

// SummarizeCharacterMemory asks the LLM to
// defragment the active character's memory.yaml:
// dedupe, drop redundant day-of references, and
// refile legacy free-form sections ("## Действия
// дня 1", "## Видения Кагуи", "## Эмоции", "##
// Эволюция", …) into the 4 canonical sections
// ("Яркие моменты", "Факты о мире", "Обещания и
// цели", "Важные люди"). The system prompt
// (loaded from prompts/character_memory_maintain.md)
// is the source of truth for what counts as
// "legacy" and how to refile.
//
// The world name (for canon context), the display
// name (not the dir slug), the current YAML body,
// and the world's chronicle tail are passed to
// the model. The model emits a NEW YAML body in
// the canonical `data: [{section, values}]` shape.
//
// Best-effort: empty, equal-sized, larger, or
// unparseable output leaves the on-disk file
// untouched (the caller writes nothing). The
// caller (MaintainCharacterMemory) is the only
// gatekeeper — we just produce the body.
func (s *Summarizer) SummarizeCharacterMemory(
	ctx context.Context,
	world, character string,
	memoryBody, chronicleTail []byte,
) (CharacterMemorySummaryResult, error) {
	res := CharacterMemorySummaryResult{Body: memoryBody, Compressed: false, BeforeBytes: len(memoryBody)}
	if !s.IsConfigured() {
		return res, nil
	}

	if s.characterMemoryPrompt == "" {
		return res, errors.New("summarizer: character_memory prompt not wired")
	}

	if len(memoryBody) == 0 {
		return res, nil
	}

	data := prompts.NewCharacterMemorySummaryData(
		world,
		character,
		string(memoryBody),
		projectChronicle(parseChronicleBytes(chronicleTail)),
	)

	userText, err := s.renderSummary("summarizer_charmem_user.md.tmpl", data)
	if err != nil {
		return res, fmt.Errorf("summarizer: render charmem user: %w", err)
	}

	role := s.role
	if role.MaxTokens < 4000 {
		role.MaxTokens = 4000
	}

	if role.Temperature == 0 || role.Temperature > 0.4 {
		role.Temperature = 0.2
	}

	// Use the dedicated chronicle prompt when wired,
	// otherwise fall back to the base summary prompt.
	chronicleSys := s.chronicleSummaryPrompt
	if chronicleSys == "" {
		chronicleSys = s.prompt
	}

	req := llm.ChatRequest{
		Model: role.Model,
		Messages: []llm.Message{
			{Role: "system", Content: chronicleSys},
			{Role: "user", Content: userText},
		},
		Temperature: role.Temperature,
		MaxTokens:   role.MaxTokens,
	}

	streamRaw, streamErr := s.streamToString(ctx, req)
	if streamErr != nil {
		return res, fmt.Errorf("summarizer: character_memory stream: %w", streamErr)
	}

	out := strings.TrimSpace(streamRaw)
	if out == "" {
		return res, nil
	}

	cleaned := stripYAMLFence(out)
	res.Body = []byte(cleaned)
	res.OutputChars = len(cleaned)
	res.AfterBytes = len(cleaned)
	res.Compressed = len(cleaned) < len(memoryBody)

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

// ChronicleSummaryResult is what SummarizeChronicle returns.
// Body is the compressed text the caller should store as a
// new Period's Memory field. Compressed is true when the
// body is non-empty and shorter than the input window
// (i.e. a real shrink happened, not a no-op echo).
type ChronicleSummaryResult struct {
	Body        []byte
	Compressed  bool
	OutputChars int
	InputDays   int
}

// SummarizeChronicle asks the LLM to compact a window of
// N consecutive day entries (default N=30, larger on
// timeskips) into a single Period memory: "<10..N
// sentences of distilled essence>". The system prompt
// (loaded from internal/prompts/chronicle_summary.md)
// sets the rules: chronological, dedupe repetitions,
// preserve canon-relevant facts (NPC introductions,
// death, player actions that change the world).
//
// The summarizer receives the WHOLE current chronicle
// YAML as context — earlier, already-compressed periods
// are passed through so the model can keep the new
// summary consistent with what has already been said
// (and dedupe arcs that span the window boundary, like a
// 15-day training run that started inside the previous
// window).
//
// Best-effort: the model may return an empty body (the
// window is too thin to compress — e.g. only 3 real days
// of activity in a 30-day calendar window). The caller
// treats that as "no compression happened" and leaves
// the file untouched.
func (s *Summarizer) SummarizeChronicle(
	ctx context.Context,
	world string,
	startDay, endDay int,
	fullChronicle string,
) (ChronicleSummaryResult, error) {
	res := ChronicleSummaryResult{Body: nil, Compressed: false, InputDays: endDay - startDay + 1}
	if !s.IsConfigured() {
		return res, nil
	}

	if endDay < startDay {
		return res, fmt.Errorf("summarizer: chronicle window invalid: start=%d end=%d", startDay, endDay)
	}

	userText, err := s.renderSummary(
		"summarizer_chronicle_user.md.tmpl",
		prompts.NewChronicleSummaryData(
			world, startDay, endDay,
			projectChronicle(parseChronicleBytes([]byte(fullChronicle))),
		),
	)
	if err != nil {
		return res, fmt.Errorf("summarizer: render chronicle user: %w", err)
	}

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
			{Role: "user", Content: userText},
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
		return res, fmt.Errorf("summarizer: chronicle stream: %w", streamErr)
	}

	out := strings.TrimSpace(buf.String())
	if out == "" {
		return res, nil
	}

	cleaned := stripMarkdownFence(out)
	res.Body = []byte(cleaned)
	res.OutputChars = len(cleaned)
	res.Compressed = true

	return res, nil
}

// InPlaceSummaryResult is what SummarizeInPlace returns.
// Body is the compressed text the caller should append to
// index:1 as "## Хроника текущего дня (Д<N>)". It is
// expected to START with "[События текущего дня Д<N>]"
// (the prompt enforces this). Compressed is true when
// a real shrink happened.
type InPlaceSummaryResult struct {
	Body        []byte
	Compressed  bool
	OutputChars int
	Day         int
}

// SetCompactionInPlacePrompt wires the prompt loaded
// from prompts/compaction_in_place.md. Called once
// at construction time from main.go.
func (s *Summarizer) SetCompactionInPlacePrompt(p string) {
	s.compactionInPlacePrompt = p
}

// SetEndOfDayPrompt wires the prompt loaded from
// prompts/end_of_day.md. Called once at construction
// time from main.go.
func (s *Summarizer) SetEndOfDayPrompt(p string) {
	s.endOfDayPrompt = p
}

// SetCharacterMemoryPrompt wires the prompt loaded
// from prompts/character_memory_maintain.md. Called
// once at construction time from main.go. The
// SummarizeCharacterMemory call is the only consumer
// of this prompt; nil-safety matches the other
// prompt setters (empty = no-op, caller skips the
// LLM call but still records the size check).
func (s *Summarizer) SetCharacterMemoryPrompt(p string) {
	s.characterMemoryPrompt = p
}

// SetChronicleSummaryPrompt wires the chronicle-window
// summarizer system prompt (chronicle_summary.md.tmpl).
// Called once at construction time from main.go. When not
// wired, SummarizeChronicle falls back to the base
// summary prompt (s.prompt) — this preserves backward
// compat for tests that do not load the new template.
func (s *Summarizer) SetChronicleSummaryPrompt(p string) {
	s.chronicleSummaryPrompt = p
}

// SetCompactionConfig wires the config-derived compaction
// knobs (InPlaceSummaryWordsMin/Max,
// EndOfDaySummaryWordsMin/Max, OldTurnsSummaryTokensMin/Max,
// LoreTargetLinesMin/Max, LoreSectionTargetMin/Max,
// MemoriseSentenceMin/Max, plus the existing limits) so the
// summarizer user-message templates can reference
// {{ .Compaction.* }} instead of hard-coded magic numbers.
// Called once at construction time from main.go after
// NewSummarizer / NewFallbackSummarizer.
func (s *Summarizer) SetCompactionConfig(c prompts.CompactionData) {
	s.compaction = c
}

// SummarizeInPlace compresses the current in-memory
// conversation of day N into a 150-300 word narrative
// that will be appended to index:1 as "## Хроника
// текущего дня". This is the in-place compaction path,
// triggered when messages[2:] grows past
// g.compaction.Threshold * context_window.
//
// The summarizer does NOT mark the day as closed
// (the compaction_in_place.md prompt is explicit about
// this). On end_day the same conversation is
// re-compressed differently — see SummarizeEndOfDay.
// summarizer dispatch is intentionally straight-line; helper extraction would just shuffle the same lines.
//
//nolint:funlen // summarizer dispatch is intentionally straight-line; helper extraction would just shuffle the same lines
func (s *Summarizer) SummarizeInPlace(
	ctx context.Context,
	world string,
	day int,
	messages []llm.Message,
) (InPlaceSummaryResult, error) {
	res := InPlaceSummaryResult{Day: day}
	if !s.IsConfigured() {
		return res, nil
	}

	if s.compactionInPlacePrompt == "" {
		return res, errors.New("summarizer: in-place compaction prompt not wired")
	}

	if len(messages) == 0 {
		return res, nil
	}

	userText, err := s.renderSummary(
		"summarizer_inplace_user.md.tmpl",
		prompts.NewInPlaceSummaryData(world, day, projectMessages(messages)),
	)
	if err != nil {
		return res, fmt.Errorf("summarizer: render in-place user: %w", err)
	}

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
			{Role: "system", Content: s.compactionInPlacePrompt},
			{Role: "user", Content: userText},
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
		return res, fmt.Errorf("summarizer: in-place stream: %w", streamErr)
	}

	out := strings.TrimSpace(buf.String())
	if out == "" {
		return res, nil
	}

	cleaned := stripMarkdownFence(out)
	res.Body = []byte(cleaned)
	res.OutputChars = len(cleaned)
	res.Compressed = true

	return res, nil
}

// EndOfDaySummaryResult is what SummarizeEndOfDay returns.
// Body is a 200-400 word narrative intended for
// "## Протокол прошедших дней" in index:1. It should
// start with "[События прошедшего дня Д<N>]".
type EndOfDaySummaryResult struct {
	Body        []byte
	Compressed  bool
	OutputChars int
	Day         int
}

// SummarizeEndOfDay produces the full narrative protocol
// for a closing day. Called from GM.EndOfDay.
// The body is appended to "## Протокол прошедших дней"
// in WorldState and (when the window overflows) eventually
// moved to chronicle.
//
// The end_of_day.md prompt is explicit: this is a
// CLOSING day. Tomorrow is a new day. Verbs are in
// past tense. Quotations are short (1 sentence max).
// Format is a free-form narrative (NOT a numbered list
// — that was the v1 mistake).
func (s *Summarizer) SummarizeEndOfDay(
	ctx context.Context,
	world string,
	day int,
	messages []llm.Message,
	stateMD string,
) (EndOfDaySummaryResult, error) {
	res := EndOfDaySummaryResult{Day: day}
	if !s.IsConfigured() {
		return res, nil
	}

	if s.endOfDayPrompt == "" {
		return res, errors.New("summarizer: end-of-day prompt not wired")
	}

	if len(messages) == 0 {
		return res, nil
	}

	userText, err := s.renderSummary(
		"summarizer_eod_user.md.tmpl",
		prompts.NewEndOfDaySummaryData(world, day, projectMessages(messages), projectState(stateMD)),
	)
	if err != nil {
		return res, fmt.Errorf("summarizer: render eod user: %w", err)
	}

	role := s.role
	if role.MaxTokens < 3000 {
		role.MaxTokens = 3000
	}

	if role.Temperature == 0 || role.Temperature > 0.4 {
		role.Temperature = 0.2
	}

	req := llm.ChatRequest{
		Model: role.Model,
		Messages: []llm.Message{
			{Role: "system", Content: s.endOfDayPrompt},
			{Role: "user", Content: userText},
		},
		Temperature: role.Temperature,
		MaxTokens:   role.MaxTokens,
	}

	streamRaw, streamErr := s.streamToString(ctx, req)
	if streamErr != nil {
		return res, fmt.Errorf("summarizer: end-of-day stream: %w", streamErr)
	}

	out := strings.TrimSpace(streamRaw)
	if out == "" {
		return res, nil
	}

	cleaned := stripMarkdownFence(out)
	res.Body = []byte(cleaned)
	res.OutputChars = len(cleaned)
	res.Compressed = true

	return res, nil
}

// renderSummary renders a summarizer user-message template
// with the Summarizer's compaction config wired in, so the
// template can reference {{ .Compaction.* }} for the soft
// targets. Shorthand for
// `prompts.RenderSummarizerUser` + config injection.
func (s *Summarizer) renderSummary(name string, sum *prompts.SummarizerData) (string, error) {
	out, err := prompts.Render(name, prompts.PromptData{
		Compaction: s.compaction,
		Summarizer: sum,
	})
	if err != nil {
		return "", fmt.Errorf("render_summary %s: %w", name, err)
	}

	return out, nil
}

// streamToString runs the LLM stream and concatenates content
// chunks into a single string. Done chunks and empty chunks
// are skipped; the caller decides how to handle empty
// output. Used by every Summarize* method to keep the
// streaming boilerplate out of each prompt-specific path.
func (s *Summarizer) streamToString(ctx context.Context, req llm.ChatRequest) (string, error) {
	var buf strings.Builder

	streamErr := s.llm.Stream(ctx, req, func(ch llm.Chunk) error {
		if ch.Done || ch.Content == "" {
			return nil
		}

		buf.WriteString(ch.Content)

		return nil
	})
	if streamErr != nil {
		return "", fmt.Errorf("summarizer.streamToString: %w", streamErr)
	}

	return buf.String(), nil
}
