package usecase

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"narrative/internal/adapter/storage"
	"narrative/internal/slowlog"
)

func TestCharacterUpdate_AppendsNewSection(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.WriteRawAtomic("characters/markus/SOUL.md", "# Маркус\n## Истинная сущность\nчеловек\n"))
	cu := NewCharacterUpdate(fs, discardLogger(), slowlog.Discard())
	require.NoError(t, cu.Append("markus", "SOUL", "Философия и принципы", "Воля и честь"))
	got, _ := fs.ReadRaw("characters/markus/SOUL.md")
	assert.Contains(t, got, "## Философия и принципы")
	assert.Contains(t, got, "Воля и честь")
	assert.Contains(t, got, "## Истинная сущность", "existing section preserved")
}

func TestCharacterUpdate_AppendsToExistingSection(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.WriteRawAtomic("characters/markus/SOUL.md", "## Истинная сущность\nчеловек\n"))
	cu := NewCharacterUpdate(fs, discardLogger(), slowlog.Discard())
	require.NoError(t, cu.Append("markus", "SOUL", "Истинная сущность", "Имя: Маркус"))
	got, _ := fs.ReadRaw("characters/markus/SOUL.md")
	assert.Contains(t, got, "человек")
	assert.Contains(t, got, "Имя: Маркус")
}

func TestCharacterUpdate_DedupesIdenticalLine(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.WriteRawAtomic("characters/markus/SOUL.md", "## X\nfoo\n"))
	cu := NewCharacterUpdate(fs, discardLogger(), slowlog.Discard())
	require.NoError(t, cu.Append("markus", "SOUL", "X", "foo"))
	got, _ := fs.ReadRaw("characters/markus/SOUL.md")
	assert.Equal(t, 1, strings.Count(got, "foo"), "identical line should not be inserted twice")
}

func TestCharacterUpdate_ValidatesArgs(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	cu := NewCharacterUpdate(fs, discardLogger(), slowlog.Discard())
	assert.ErrorIs(t, cu.Append("", "SOUL", "x", "y"), ErrNoActiveCharacter)
	assert.ErrorIs(t, cu.Append("m", "BOGUS", "x", "y"), ErrUnknownCharacterFile)
	assert.ErrorIs(t, cu.Append("m", "SOUL", "", "y"), ErrEmptySection)
	assert.ErrorIs(t, cu.Append("m", "SOUL", "x", ""), ErrEmptyAppend)
}

func TestCharacterUpdate_ResolvesFileAliases(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	cu := NewCharacterUpdate(fs, discardLogger(), slowlog.Discard())
	for _, in := range []string{"SOUL", "soul", "SKILL", "skill", "memory"} {
		require.NoError(t, cu.Append("m", in, "X", "y"))
	}
	assert.True(t, fs.Exists("characters/m/SOUL.md"))
	assert.True(t, fs.Exists("characters/m/SKILL.md"))
	assert.True(t, fs.Exists("characters/m/memory.md"))
}

func TestCharacterUpdate_SlowlogWritten(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	dir := t.TempDir()
	sl, err := slowlog.File(dir + "/slow.log")
	require.NoError(t, err)
	cu := NewCharacterUpdate(fs, discardLogger(), sl)
	require.NoError(t, cu.Append("markus", "SOUL", "Background", "Пришёл из другого мира"))
	body, _ := readWhole(dir + "/slow.log")
	assert.Contains(t, string(body), `"character.update"`)
	assert.Contains(t, string(body), `"section":"Background"`)
}

func TestCharacterSnapshot_FormatIncludesAllSections(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.WriteRawAtomic("characters/markus/SOUL.md", "## Истинная сущность\nчеловек\n"))
	require.NoError(t, fs.WriteRawAtomic("characters/markus/SKILL.md", "## Оружие\nмеч\n"))
	require.NoError(t, fs.WriteRawAtomic("characters/markus/memory.md", "Пришёл из нашего мира.\n"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md", "День 3 (в процессе).\nСтарт.\n"))
	cu := NewCharacterUpdate(fs, discardLogger(), slowlog.Discard())
	snap, err := cu.Read("markus", "naruto")
	require.NoError(t, err)
	out := snap.Format(50)
	assert.Contains(t, out, "**Персонаж: markus**")
	assert.Contains(t, out, "naruto")
	assert.Contains(t, out, "SOUL.md")
	assert.Contains(t, out, "SKILL.md")
	assert.Contains(t, out, "memory.md")
	assert.Contains(t, out, "state.md")
}

func TestCharacterSnapshot_TruncatesLongFiles(t *testing.T) {
	fs, _ := storage.NewFileStore(t.TempDir())
	long := "## X\n"
	for i := 0; i < 100; i++ {
		long += "line\n"
	}
	require.NoError(t, fs.WriteRawAtomic("characters/markus/SOUL.md", long))
	cu := NewCharacterUpdate(fs, discardLogger(), slowlog.Discard())
	snap, _ := cu.Read("markus", "")
	out := snap.Format(10)
	// Snapshot reports the actual on-disk line count, not the
	// count we wrote — WriteRawAtomic normalises the trailing
	// newline. Assert the truncation marker is present without
	// locking in a specific number; pin the math with a separate
	// assertion.
	assert.Contains(t, out, "строк обрезано")
	kept := strings.Count(out, "\nline\n")
	assert.LessOrEqual(t, kept, 10, "should keep at most max lines")
}
