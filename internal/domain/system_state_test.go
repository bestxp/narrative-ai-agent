package domain_test

import (
	"testing"
	"time"

	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystemState_RoundTrip(t *testing.T) {
	t.Parallel()

	in := domain.SystemState{
		Session: domain.SessionState{
			StartedAt:         time.Date(2026, 6, 5, 22, 0, 0, 0, time.UTC),
			LastActive:        time.Date(2026, 6, 6, 14, 23, 0, 0, time.UTC),
			TurnCount:         50,
			FreeformTurnCount: 47,
			ChatID:            "167898078",
			Character:         "markus",
			World:             "naruto",
		},
		Compaction: domain.CompactionLog{
			TotalCompactions: 2,
			LastCompactionAt: time.Date(2026, 6, 6, 13, 45, 0, 0, time.UTC),
			History: []domain.CompactionEvent{
				{
					At:           time.Date(2026, 6, 6, 12, 30, 0, 0, time.UTC),
					Trigger:      "context_window*0.7",
					Role:         "narrative",
					BeforeTokens: 22000,
					AfterTokens:  5500,
					DroppedTurns: 23,
					KeptRecent:   5,
				},
			},
		},
		Autosave: domain.AutosaveState{
			LastHash:   "abc1234",
			LastSaveAt: time.Date(2026, 6, 6, 13, 30, 0, 0, time.UTC),
			TotalSaves: 12,
		},
	}
	body, err := in.MarshalSystemState()
	require.NoError(t, err)
	out, err := domain.ParseSystemState(body)
	require.NoError(t, err)
	assert.Equal(t, in.Session.TurnCount, out.Session.TurnCount)
	assert.Equal(t, in.Session.Character, out.Session.Character)
	assert.Equal(t, in.Compaction.TotalCompactions, out.Compaction.TotalCompactions)
	assert.Equal(t, in.Compaction.History[0].BeforeTokens, out.Compaction.History[0].BeforeTokens)
	assert.Equal(t, in.Autosave.LastHash, out.Autosave.LastHash)
}

func TestParseSystemState_EmptyErrors(t *testing.T) {
	t.Parallel()

	_, err := domain.ParseSystemState("")
	assert.Error(t, err)
}

func TestParseSystemState_BadYAMLErrors(t *testing.T) {
	t.Parallel()

	_, err := domain.ParseSystemState("session: : :")
	assert.Error(t, err)
}

func TestCompactionLog_AppendEvictsOldest(t *testing.T) {
	t.Parallel()

	c := &domain.CompactionLog{}
	for i := range 5 {
		c.AppendCompactionEvent(domain.CompactionEvent{At: time.Unix(int64(i), 0), KeptRecent: i}, 3)
	}

	assert.Len(t, c.History, 3, "history should be capped at 3")
	assert.Equal(t, 5, c.TotalCompactions, "total counts all appends")
	assert.Equal(t, 2, c.History[0].KeptRecent, "oldest two should be evicted")
	assert.Equal(t, 4, c.History[2].KeptRecent, "newest survives")
}

func TestCompactionLog_AppendHonoursMaxEntries1(t *testing.T) {
	t.Parallel()

	c := &domain.CompactionLog{}
	c.AppendCompactionEvent(domain.CompactionEvent{KeptRecent: 1}, 1)
	c.AppendCompactionEvent(domain.CompactionEvent{KeptRecent: 2}, 1)
	c.AppendCompactionEvent(domain.CompactionEvent{KeptRecent: 3}, 1)
	assert.Len(t, c.History, 1)
	assert.Equal(t, 3, c.History[0].KeptRecent)
}

func TestCompactionLog_AppendHonoursZeroMax(t *testing.T) {
	t.Parallel()

	c := &domain.CompactionLog{}
	c.AppendCompactionEvent(domain.CompactionEvent{KeptRecent: 1}, 0)
	assert.Len(t, c.History, 1, "maxEntries < 1 is clamped to 1")
}

func TestDefaultSystemState_IsZero(t *testing.T) {
	t.Parallel()
	assert.Equal(t, domain.SystemState{}, domain.DefaultSystemState())
}

func TestSystemStateFile_Constant(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "system_state.md", domain.SystemStateFile)
}
