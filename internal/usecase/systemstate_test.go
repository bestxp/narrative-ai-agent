package usecase

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
)

func TestSystemState_LoadMissingReturnsZero(t *testing.T) {
	t.Parallel()
	fs, _ := storage.NewFileStore(t.TempDir())
	ss := NewSystemState(fs, discardLogger(), slowlog.Discard())
	state, err := ss.Load()
	require.NoError(t, err)
	assert.Equal(t, domain.SystemState{}, state)
}

func TestSystemState_LoadEmptyReturnsZero(t *testing.T) {
	t.Parallel()
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.WriteRawAtomic(domain.SystemStateFile, ""))
	ss := NewSystemState(fs, discardLogger(), slowlog.Discard())
	state, err := ss.Load()
	require.NoError(t, err)
	assert.Equal(t, domain.SystemState{}, state)
}

func TestSystemState_LoadBadYAMLErrors(t *testing.T) {
	t.Parallel()
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.WriteRawAtomic(domain.SystemStateFile, "session: : :"))
	ss := NewSystemState(fs, discardLogger(), slowlog.Discard())
	_, err := ss.Load()
	assert.Error(t, err)
}

func TestSystemState_AppendCompaction_RoundTrip(t *testing.T) {
	t.Parallel()
	fs, _ := storage.NewFileStore(t.TempDir())
	ss := NewSystemState(fs, discardLogger(), slowlog.Discard())
	now := time.Now().UTC().Truncate(time.Second)
	_, err := ss.AppendCompaction(domain.CompactionEvent{
		At:           now,
		Trigger:      "context_window*0.7",
		Role:         "narrative",
		BeforeTokens: 22000,
		AfterTokens:  5500,
		DroppedTurns: 23,
		KeptRecent:   5,
	})
	require.NoError(t, err)
	state, err := ss.Load()
	require.NoError(t, err)
	assert.Equal(t, 1, state.Compaction.TotalCompactions)
	require.Len(t, state.Compaction.History, 1)
	assert.Equal(t, 22000, state.Compaction.History[0].BeforeTokens)
	assert.Equal(t, 23, state.Compaction.History[0].DroppedTurns)
}

func TestSystemState_AppendCompaction_Multiple(t *testing.T) {
	t.Parallel()
	fs, _ := storage.NewFileStore(t.TempDir())
	ss := NewSystemState(fs, discardLogger(), slowlog.Discard())
	for i := 0; i < 3; i++ {
		_, err := ss.AppendCompaction(domain.CompactionEvent{
			At:           time.Unix(int64(i), 0).UTC(),
			BeforeTokens: 22000,
			AfterTokens:  5500,
		})
		require.NoError(t, err)
	}
	state, _ := ss.Load()
	assert.Equal(t, 3, state.Compaction.TotalCompactions)
	assert.Len(t, state.Compaction.History, 3)
}

func TestSystemState_RecordAutosave_EmptyHashIsNoop(t *testing.T) {
	t.Parallel()
	fs, _ := storage.NewFileStore(t.TempDir())
	ss := NewSystemState(fs, discardLogger(), slowlog.Discard())
	_, err := ss.RecordAutosave("", time.Now())
	require.NoError(t, err)
	state, _ := ss.Load()
	assert.Equal(t, 0, state.Autosave.TotalSaves)
	assert.Empty(t, state.Autosave.LastHash)
}

func TestSystemState_RecordAutosave_HashUpdates(t *testing.T) {
	t.Parallel()
	fs, _ := storage.NewFileStore(t.TempDir())
	ss := NewSystemState(fs, discardLogger(), slowlog.Discard())
	now := time.Now().UTC()
	_, err := ss.RecordAutosave("abc1234", now)
	require.NoError(t, err)
	state, _ := ss.Load()
	assert.Equal(t, "abc1234", state.Autosave.LastHash)
	assert.Equal(t, 1, state.Autosave.TotalSaves)
	assert.True(t, state.Autosave.LastSaveAt.Equal(now))
}

func TestSystemState_TouchSession_BumpsCounters(t *testing.T) {
	t.Parallel()
	fs, _ := storage.NewFileStore(t.TempDir())
	ss := NewSystemState(fs, discardLogger(), slowlog.Discard())
	state, _ := ss.Load()
	now := time.Now().UTC()
	ss.TouchSession(&state, true, now)
	ss.TouchSession(&state, false, now)
	ss.TouchSession(&state, true, now)
	assert.Equal(t, 3, state.Session.TurnCount)
	assert.Equal(t, 2, state.Session.FreeformTurnCount)
}

func TestSystemState_SetSessionContext_OneShot(t *testing.T) {
	t.Parallel()
	fs, _ := storage.NewFileStore(t.TempDir())
	ss := NewSystemState(fs, discardLogger(), slowlog.Discard())
	state, _ := ss.Load()
	now := time.Now().UTC()
	ss.SetSessionContext(&state, "markus", "naruto", "167898078", now)
	assert.Equal(t, "markus", state.Session.Character)
	assert.Equal(t, "naruto", state.Session.World)
	assert.Equal(t, "167898078", state.Session.ChatID)
	assert.True(t, state.Session.StartedAt.Equal(now))
}

func TestSystemState_SlowlogOnCompaction(t *testing.T) {
	t.Parallel()
	fs, _ := storage.NewFileStore(t.TempDir())
	dir := t.TempDir()
	sl, err := slowlog.File(dir + "/slow.log")
	require.NoError(t, err)
	ss := NewSystemState(fs, discardLogger(), sl)
	_, err = ss.AppendCompaction(domain.CompactionEvent{
		At:           time.Now().UTC(),
		Trigger:      "context_window*0.7",
		BeforeTokens: 22000,
		AfterTokens:  5500,
		DroppedTurns: 23,
	})
	require.NoError(t, err)
	body, _ := readWhole(dir + "/slow.log")
	assert.Contains(t, string(body), `"compaction"`)
	assert.Contains(t, string(body), `"dropped_turns":23`)
}
