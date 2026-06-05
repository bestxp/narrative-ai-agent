package usecase

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"narrative/internal/adapter/storage"
	"narrative/internal/domain"
)

func TestSessionStart_NoInfo(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	s := NewSessionStart(fs)
	_, err := s.Start()
	assert.ErrorIs(t, err, ErrNoActiveSession)
}

func TestSessionStart_OK(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	require.NoError(t, fs.WriteRawAtomic("info.md", domain.BuildInfo("markus", "naruto", nil, nil)))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md", "День 3 (в процессе).\nЧто-то происходит.\n"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/memorise.md", "д00001: a\nд00002: b\n"))
	s := NewSessionStart(fs)
	ctx, err := s.Start()
	require.NoError(t, err)
	assert.Equal(t, "naruto", ctx.World)
	assert.Equal(t, "markus", ctx.Character)
	assert.True(t, ctx.SyncStateAhead, "state=3, mem=2 → state ahead")
}

func TestSessionStart_DetectsStateAhead(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	require.NoError(t, fs.WriteRawAtomic("info.md", domain.BuildInfo("markus", "naruto", nil, nil)))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md", "День 5 (в процессе).\n"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/memorise.md", "д00001: a\nд00002: b\n"))
	s := NewSessionStart(fs)
	ctx, _ := s.Start()
	assert.True(t, ctx.SyncStateAhead)
}

func TestSessionStart_DetectsMemoriseAhead(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	require.NoError(t, fs.WriteRawAtomic("info.md", domain.BuildInfo("markus", "naruto", nil, nil)))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md", "День 1 (в процессе).\n"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/memorise.md", "д00001: a\nд00005: b\n"))
	s := NewSessionStart(fs)
	ctx, _ := s.Start()
	assert.True(t, ctx.SyncMemoriseAhead)
}

func TestSessionStart_AnchorsCounted(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	require.NoError(t, fs.WriteRawAtomic("info.md", domain.BuildInfo("markus", "naruto", nil, nil)))
	s := NewSessionStart(fs)
	ctx, _ := s.Start()
	assert.GreaterOrEqual(t, len(ctx.UnreadAnchors), 3)
}

func TestSessionStart_PopulatesStateString(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	require.NoError(t, fs.WriteRawAtomic("info.md", domain.BuildInfo("markus", "naruto", nil, nil)))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md", "День 7 (в процессе).\nNPC: Какаши\n"))
	s := NewSessionStart(fs)
	ctx, _ := s.Start()
	assert.Contains(t, ctx.State, "Какаши")
}
