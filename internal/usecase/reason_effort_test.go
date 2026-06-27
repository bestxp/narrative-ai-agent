package usecase

import (
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
	"github.com/stretchr/testify/assert"
)

// TestReasonEffort covers the three cases of the helper
// used by GM.Reply → ChatRequest.ReasoningEffort:
//  1. role.ReasoningEffort wins when the operator pinned a
//     level (even if DisableThinking is also true).
//  2. role.DisableThinking defaults to "none" when no
//     level is set, so providers that recognise the
//     reasoning_effort key (Ollama Cloud, xAI Grok,
//     OpenRouter) skip the chain-of-thought trace.
//  3. Empty when neither flag is set — drivers fall back
//     to their role-level fields in that case.
func TestReasonEffort(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		role llm.RoleConfig
		want string
	}{
		{
			name: "explicit level wins over disable_thinking",
			role: llm.RoleConfig{DisableThinking: true, ReasoningEffort: "low"},
			want: "low",
		},
		{
			name: "disable_thinking defaults to none",
			role: llm.RoleConfig{DisableThinking: true},
			want: "none",
		},
		{
			name: "neither set returns empty (driver falls back)",
			role: llm.RoleConfig{},
			want: "",
		},
		{
			name: "level without disable_thinking passes through",
			role: llm.RoleConfig{ReasoningEffort: "medium"},
			want: "medium",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, reasonEffort(tc.role))
		})
	}
}
