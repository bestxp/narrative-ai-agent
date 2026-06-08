package usecase

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
)

func TestSessionStart_NoInfo(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	s := NewSessionStart(fs)
	_, err := s.Start()
	assert.ErrorIs(t, err, ErrNoActiveSession)
}

func TestSessionStart_BootstrapsMissingRegistry(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	s := NewSessionStart(fs)
	require.NoError(t, s.ensureRegistry())
	assert.True(t, fs.Exists(storage.InfoFile))
	body, _ := fs.ReadRaw(storage.InfoFile)
	assert.NotEmpty(t, body)
	// Without an active world set the bot still cannot start a session.
	_, err := s.Start()
	assert.ErrorIs(t, err, ErrNoActiveSession)
}

func TestSessionStart_BootstrapsEmptyRegistry(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile, ""))
	s := NewSessionStart(fs)
	require.NoError(t, s.ensureRegistry())
	body, _ := fs.ReadRaw(storage.InfoFile)
	assert.NotEmpty(t, body, "empty registry should be replaced with placeholder")
}

func TestSessionStart_OK(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile, domain.BuildInfo("markus", "naruto", nil, nil)))
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
	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile, domain.BuildInfo("markus", "naruto", nil, nil)))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md", "День 5 (в процессе).\n"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/memorise.md", "д00001: a\nд00002: b\n"))
	s := NewSessionStart(fs)
	ctx, _ := s.Start()
	assert.True(t, ctx.SyncStateAhead)
}

func TestSessionStart_DetectsMemoriseAhead(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile, domain.BuildInfo("markus", "naruto", nil, nil)))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md", "День 1 (в процессе).\n"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/memorise.md", "д00001: a\nд00005: b\n"))
	s := NewSessionStart(fs)
	ctx, _ := s.Start()
	assert.True(t, ctx.SyncMemoriseAhead)
}

func TestSessionStart_PopulatesStateString(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile, domain.BuildInfo("markus", "naruto", nil, nil)))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md", "День 7 (в процессе).\nNPC: Какаши\n"))
	s := NewSessionStart(fs)
	ctx, _ := s.Start()
	assert.Contains(t, ctx.State, "Какаши")
}
