package storage_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage/storagetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) *storage.FileStore {
	t.Helper()
	dir := t.TempDir()
	fs, err := storage.NewFileStore(dir)
	require.NoError(t, err)

	return fs
}

func TestWriteRaw_StripsIndexPollution(t *testing.T) {
	t.Parallel()
	fs := newTestStore(t)
	polluted := "1| День 1.\n2| NPC говорит.\n3| Конец.\n"
	require.NoError(t, fs.WriteRaw("worlds/naruto/state.md", polluted))
	got, _ := fs.ReadRaw("worlds/naruto/state.md")
	assert.Equal(t, "День 1.\nNPC говорит.\nКонец.\n", got)
}

func TestPatch_OK(t *testing.T) {
	t.Parallel()
	fs := newTestStore(t)
	require.NoError(t, fs.WriteRawAtomic("a.md", "alpha\nbeta\ngamma\n"))
	require.NoError(t, fs.Patch("a.md", "beta", "BETA"))
	got, _ := fs.ReadRaw("a.md")
	assert.Equal(t, "alpha\nBETA\ngamma\n", got)
}

func TestPatch_NotFound(t *testing.T) {
	t.Parallel()
	fs := newTestStore(t)
	require.NoError(t, fs.WriteRawAtomic("a.md", "x"))
	err := fs.Patch("a.md", "y", "z")
	assert.ErrorIs(t, err, storage.ErrPatchNotFound)
}

func TestPatch_Ambiguous(t *testing.T) {
	t.Parallel()
	fs := newTestStore(t)
	require.NoError(t, fs.WriteRawAtomic("a.md", "x\nx\n"))
	err := fs.Patch("a.md", "x", "y")
	assert.ErrorIs(t, err, storage.ErrPatchAmbiguous)
}

func TestAppendIfMissing(t *testing.T) {
	t.Parallel()
	fs := newTestStore(t)
	added, err := fs.AppendIfMissing("a.md", "д00001: x")
	require.NoError(t, err)
	assert.True(t, added, "first append should add")

	added, err = fs.AppendIfMissing("a.md", "д00001: x")
	require.NoError(t, err)
	assert.False(t, added, "second append should be no-op")
}

func TestReadRaw_Missing(t *testing.T) {
	t.Parallel()
	fs := newTestStore(t)
	got, err := fs.ReadRaw("missing.md")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestCountLines(t *testing.T) {
	t.Parallel()
	fs := newTestStore(t)
	require.NoError(t, fs.WriteRawAtomic("a.md", "1\n2\n3\n"))
	assert.Equal(t, 3, fs.CountLines("a.md"))
	assert.Equal(t, -1, fs.CountLines("missing"))
}

func TestListChildren(t *testing.T) {
	t.Parallel()
	fs := newTestStore(t)
	require.NoError(t, fs.EnsureDir("worlds/naruto/characters"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md", "x"))
	kids, err := fs.ListChildren("worlds/naruto")
	require.NoError(t, err)
	assert.Len(t, kids, 2)
}

func TestWriteRaw_CreatesParentDirs(t *testing.T) {
	t.Parallel()
	fs := newTestStore(t)
	require.NoError(t, fs.WriteRaw("deep/nested/file.md", "hi"))
	_, err := os.Stat(filepath.Join(fs.Root(), "deep", "nested", "file.md"))
	require.NoError(t, err)
}

func TestStripPollution_HandlesClean(t *testing.T) {
	t.Parallel()
	fs := newTestStore(t)
	body := "line1\nline2\n"
	require.NoError(t, fs.WriteRaw("a.md", body))
	got, _ := fs.ReadRaw("a.md")
	assert.Contains(t, got, "line1", "clean input mangled: %q", got)
}

func TestWriteRaw_LogsAtDebugLevel(t *testing.T) {
	t.Parallel()

	var buf strings.Builder

	dir := t.TempDir()
	fsI, err := storage.NewFileStoreWithLogger(dir, storagetest.NewBufLogger(&buf, "debug"))
	require.NoError(t, err)

	fs := fsI
	require.NoError(t, fs.WriteRaw("a.md", "x"))
	assert.Contains(t, buf.String(), "write_raw")
}

func TestPatch_LogsWarningOnMissing(t *testing.T) {
	t.Parallel()

	var buf strings.Builder

	dir := t.TempDir()
	fsI, err := storage.NewFileStoreWithLogger(dir, storagetest.NewBufLogger(&buf, "debug"))
	require.NoError(t, err)

	fs := fsI
	require.NoError(t, fs.WriteRawAtomic("a.md", "x"))
	err = fs.Patch("a.md", "missing", "y")
	require.Error(t, err)
	assert.Contains(t, buf.String(), "not found")
}
