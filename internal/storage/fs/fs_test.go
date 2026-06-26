package fs_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/storage/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_CreatesRootDir(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "data")

	s, err := fs.New(dir)
	require.NoError(t, err, "New")

	require.NotEmpty(t, s.Root(), "Root() must not be empty")

	info, err := os.Stat(dir)
	require.NoError(t, err, "root not created")

	require.True(t, info.IsDir(), "root must be a directory")
}

func TestReadWrite_RoundTrip(t *testing.T) {
	t.Parallel()
	s, _ := fs.New(t.TempDir())

	want := []byte("hello world")
	require.NoError(t, s.Write("a/b.txt", want), "Write")

	got, err := s.Read("a/b.txt")
	require.NoError(t, err, "Read")

	assert.Equal(t, string(want), string(got), "Read returned different bytes than written")
}

func TestRead_Missing(t *testing.T) {
	t.Parallel()
	s, _ := fs.New(t.TempDir())

	got, err := s.Read("does/not/exist.txt")
	require.NoError(t, err, "missing Read should not error")
	assert.Empty(t, got, "missing Read should return nil bytes")
}

func TestExists(t *testing.T) {
	t.Parallel()
	s, _ := fs.New(t.TempDir())

	ok, err := s.Exists("nonexistent")
	require.NoError(t, err, "Exists (missing)")
	require.False(t, ok, "Exists returned true for missing key")

	require.NoError(t, s.Write("here.txt", []byte("x")), "Write")

	ok, err = s.Exists("here.txt")
	require.NoError(t, err, "Exists (present)")
	require.True(t, ok, "Exists returned false for present key")
}

func TestWrite_Overwrites(t *testing.T) {
	t.Parallel()

	s, _ := fs.New(t.TempDir())
	require.NoError(t, s.Write("f.txt", []byte("first")), "first write")
	require.NoError(t, s.Write("f.txt", []byte("second")), "second write")

	got, _ := s.Read("f.txt")
	assert.Equal(t, "second", string(got), "expected overwrite")
}

func TestWrite_AtomicViaTmpRename(t *testing.T) {
	t.Parallel()

	s, _ := fs.New(t.TempDir())
	require.NoError(t, s.Write("a.txt", []byte("v1")), "Write")
	// During a successful Write there must not be a
	// leftover .tmp file.
	_, err := os.Stat(filepath.Join(s.Root(), "a.txt.tmp"))
	assert.ErrorIs(t, err, os.ErrNotExist, "expected no .tmp file after Write")
}

func TestListChildren(t *testing.T) {
	t.Parallel()

	s, _ := fs.New(t.TempDir())
	require.NoError(t, s.Write("d/a.txt", []byte("x")), "write a.txt")
	require.NoError(t, s.Write("d/b.txt", []byte("y")), "write b.txt")
	require.NoError(t, s.Write("d/sub/c.txt", []byte("z")), "write sub/c.txt")

	got, err := s.ListChildren("d")
	require.NoError(t, err, "ListChildren")

	want := map[string]bool{"a.txt": false, "b.txt": false, "sub": true}
	require.Len(t, got, len(want), "expected %d entries, got %d (%v)", len(want), len(got), got)

	for _, name := range got {
		isDir, ok := want[name]
		assert.True(t, ok, "unexpected entry: %q", name)

		full := filepath.Join(s.Root(), "d", name)

		info, err := os.Stat(full)
		require.NoError(t, err, "stat %q", name)

		assert.Equal(t, isDir, info.IsDir(), "entry %q: isDir", name)
	}
}

func TestListChildren_MissingDir(t *testing.T) {
	t.Parallel()
	s, _ := fs.New(t.TempDir())

	got, err := s.ListChildren("does/not/exist")
	require.NoError(t, err, "missing dir ListChildren should not error")
	assert.Empty(t, got, "missing dir should return nil entries")
}

func TestEnsureDir_Idempotent(t *testing.T) {
	t.Parallel()

	s, _ := fs.New(t.TempDir())
	require.NoError(t, s.EnsureDir("a/b/c.txt"), "EnsureDir")
	require.NoError(t, s.EnsureDir("a/b/c.txt"), "EnsureDir second call")
	require.NoError(t, s.Write("a/b/c.txt", []byte("hi")), "Write after EnsureDir")
}

func TestStripIndexPollutionBytes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "line1\nline2\n", "line1\nline2\n"},
		{"polluted", "  12| line1\n  13| line2\n", "line1\nline2\n"},
		{"no trailing newline", "  12| a", "a"},
		{"blank line stays", "  12| a\n\n  14| b\n", "a\n\nb\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			got := string(fs.StripIndexPollutionBytes([]byte(c.in)))
			assert.Equal(t, c.want, got, "StripIndexPollutionBytes(%q)", c.in)
		})
	}
}

func TestJoin(t *testing.T) {
	t.Parallel()
	s, _ := fs.New(t.TempDir())
	got := s.Join("worlds/naruto/chronicle.yaml")

	want := filepath.Join(s.Root(), "worlds/naruto/chronicle.yaml")
	assert.Equal(t, want, got, "Join")
}

func TestIsDirKey(t *testing.T) {
	t.Parallel()

	assert.True(t, fs.IsDirKey("worlds/naruto/"), "trailing slash should be a dir key")
	assert.False(t, fs.IsDirKey("worlds/naruto/chronicle.yaml"), "non-trailing slash should not be a dir key")
}

func TestRoot_Absolute(t *testing.T) {
	t.Parallel()

	s, _ := fs.New(t.TempDir())
	assert.True(t, strings.HasPrefix(s.Root(), "/"), "Root() should be absolute, got %q", s.Root())
}
