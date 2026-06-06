package usecase

import (
	"fmt"
	"time"

	"narrative/internal/adapter/llm"
	"narrative/internal/domain"
)

// EstimateConversationTokens is a coarse approximation of the
// input tokens for a chat-completion request, used by the
// compaction preflight. It sums the character counts of every
// message and divides by 4 — same rule-of-thumb the
// llm.EstimateTokens helper uses, but applied to a whole
// conversation. Accurate enough to detect "we are about to
// cross 70% of the context window"; not accurate enough to
// bill against.
func EstimateConversationTokens(messages []llm.Message, systemPromptChars int) int {
	total := systemPromptChars
	for _, m := range messages {
		total += len(m.Content)
		for _, tc := range m.ToolCalls {
			total += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
		if m.Name != "" {
			total += len(m.Name)
		}
	}
	return (total + 3) / 4
}

// NeedsCompaction reports whether the current input would
// exceed the role's configured threshold. The threshold is
// expressed as a fraction of ContextWindow (e.g. 0.7). A zero
// ContextWindow disables compaction entirely — the bot will
// then issue requests of any size and trust the provider's
// hard limit to reject what it cannot serve.
func NeedsCompaction(messages []llm.Message, systemPromptChars, contextWindow int, threshold float64) bool {
	if contextWindow <= 0 || threshold <= 0 {
		return false
	}
	tokens := EstimateConversationTokens(messages, systemPromptChars)
	return tokens >= int(float64(contextWindow)*threshold)
}

// CompactionResult is the public summary the bot sends to
// the player (via OnCompaction callback) and writes to
// system_state.md.compaction.history. Before/After is
// measured with the same estimator so the numbers are
// self-consistent.
type CompactionResult struct {
	BeforeTokens int
	AfterTokens  int
	DroppedTurns int
	KeptRecent   int
}

// CompactConversations trims the oldest turns from messages
// so that the kept prefix does not exceed keepRecent. A
// "turn" is one (user, assistant, ...) group. We trim from
// the front of the slice; the caller passes a freshly-snapshotted
// history.
//
// The returned messages slice is safe to assign back to the
// GM's conversation state. The CompactionResult is for the
// caller to feed into OnCompaction + system_state.md.
func CompactConversations(messages []llm.Message, keepRecent int) ([]llm.Message, CompactionResult) {
	if keepRecent < 0 {
		keepRecent = 0
	}
	before := EstimateConversationTokens(messages, 0)
	if len(messages) <= keepRecent {
		return messages, CompactionResult{
			BeforeTokens: before,
			AfterTokens:  before,
			DroppedTurns: 0,
			KeptRecent:   keepRecent,
		}
	}
	dropped := len(messages) - keepRecent
	kept := messages[len(messages)-keepRecent:]
	after := EstimateConversationTokens(kept, 0)
	return kept, CompactionResult{
		BeforeTokens: before,
		AfterTokens:  after,
		DroppedTurns: dropped,
		KeptRecent:   keepRecent,
	}
}

// NewCompactionEvent is a tiny builder for the
// domain.CompactionEvent the writer (usecase/systemstate)
// expects. Pulled out here so the GM does not import domain
// at the call site.
func NewCompactionEvent(role string, before, after, dropped, keptRecent int, now time.Time) domain.CompactionEvent {
	trigger := "context_window*0.7"
	return domain.CompactionEvent{
		At:            now.UTC(),
		Trigger:       trigger,
		Role:          role,
		BeforeTokens:  before,
		AfterTokens:   after,
		DroppedTurns:  dropped,
		KeptRecent:    keptRecent,
	}
}

// DescribeCompaction renders the player-facing notification
// text. Short or verbose per the config flag.
func DescribeCompaction(r CompactionResult, role string, verbose bool) string {
	if r.DroppedTurns == 0 {
		return "" // nothing to say when no trimming happened
	}
	if !verbose {
		return fmt.Sprintf("🔄 компактирую историю (%dk → %dk tok, −%d ходов)",
			r.BeforeTokens/1000, r.AfterTokens/1000, r.DroppedTurns)
	}
	out := "🔄 компактирую историю\n"
	out += fmt.Sprintf("   было:  %d tok\n", r.BeforeTokens)
	out += fmt.Sprintf("   стало: %d tok\n", r.AfterTokens)
	out += fmt.Sprintf("   дропнуто: %d ходов\n", r.DroppedTurns)
	out += fmt.Sprintf("   осталось: %d свежих\n", r.KeptRecent)
	if role != "" {
		out += fmt.Sprintf("   роль: %s\n", role)
	}
	return out
}
