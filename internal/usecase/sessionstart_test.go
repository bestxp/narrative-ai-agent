package usecase

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/chronicle"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
)

func TestSessionStart_NoInfo(t *testing.T) {
	t.Parallel()
	fs, _ := storage.NewFileStore(t.TempDir())
	s := NewSessionStart(fs)
	_, err := s.Start()
	assert.ErrorIs(t, err, ErrNoActiveSession)
}

func TestSessionStart_BootstrapsMissingRegistry(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile, ""))
	s := NewSessionStart(fs)
	require.NoError(t, s.ensureRegistry())
	body, _ := fs.ReadRaw(storage.InfoFile)
	assert.NotEmpty(t, body, "empty registry should be replaced with placeholder")
}

// writeChronicleDays is a small helper for tests: it
// writes a YAML chronicle with the given per-day
// entries. Used by the checkSync tests below.
func writeChronicleDays(t *testing.T, fs *storage.FileStore, world string, days map[int]string) {
	t.Helper()
	c := chronicle.Chronicle{Periods: []chronicle.Period{}, Days: map[int]string{}}
	for d, txt := range days {
		c.AppendDay(d, txt)
	}
	body, err := c.Save()
	require.NoError(t, err)
	require.NoError(t, fs.WriteRawAtomic(fs.WorldChronicle(world), body))
}

func TestSessionStart_OK(t *testing.T) {
	t.Parallel()
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile, domain.BuildInfo("markus", "naruto", nil, nil)))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md", "День 3 (в процессе).\nЧто-то происходит.\n"))
	writeChronicleDays(t, fs, "naruto", map[int]string{1: "a", 2: "b"})
	s := NewSessionStart(fs)
	ctx, err := s.Start()
	require.NoError(t, err)
	assert.Equal(t, "naruto", ctx.World)
	assert.Equal(t, "markus", ctx.Character)
	assert.True(t, ctx.SyncStateAhead, "state=3, chronicle last day=2 → state ahead")
}

func TestSessionStart_DetectsStateAhead(t *testing.T) {
	t.Parallel()
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile, domain.BuildInfo("markus", "naruto", nil, nil)))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md", "День 5 (в процессе).\n"))
	writeChronicleDays(t, fs, "naruto", map[int]string{1: "a", 2: "b"})
	s := NewSessionStart(fs)
	ctx, _ := s.Start()
	assert.True(t, ctx.SyncStateAhead)
}

func TestSessionStart_DetectsChronicleAhead(t *testing.T) {
	t.Parallel()
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile, domain.BuildInfo("markus", "naruto", nil, nil)))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md", "День 1 (в процессе).\n"))
	writeChronicleDays(t, fs, "naruto", map[int]string{1: "a", 5: "b"})
	s := NewSessionStart(fs)
	ctx, _ := s.Start()
	assert.True(t, ctx.SyncChronicleAhead)
}

func TestSessionStart_PopulatesStateString(t *testing.T) {
	t.Parallel()
	fs, _ := storage.NewFileStore(t.TempDir())
	seedWorld(t, fs, "naruto")
	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile, domain.BuildInfo("markus", "naruto", nil, nil)))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md", "День 7 (в процессе).\nNPC: Какаши\n"))
	s := NewSessionStart(fs)
	ctx, _ := s.Start()
	assert.Contains(t, ctx.State, "Какаши")
}
