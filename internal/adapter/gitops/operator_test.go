package gitops

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	cmds := [][]string{
		{"init", "--initial-branch=master"},
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@test.local"},
	}
	for _, c := range cmds {
		cmd := exec.Command("git", c...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%v: %s", c, out)
	}
	return dir
}

func newBufLogger() (zerolog.Logger, *strings.Builder) {
	var buf strings.Builder
	return zerolog.New(&buf), &buf
}

func TestIsRepo_True(t *testing.T) {
	dir := initRepo(t)
	assert.True(t, IsRepo(dir))
}

func TestIsRepo_False(t *testing.T) {
	dir := t.TempDir()
	assert.False(t, IsRepo(dir))
}

func TestCommitAll_NothingToCommitIsNoop(t *testing.T) {
	dir := initRepo(t)
	op := New(dir, "origin", "master", "Bot", "bot@x")
	assert.NoError(t, op.CommitAll("noop"))
}

func TestCommitAll_CommitsNewFile(t *testing.T) {
	dir := initRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.md"), []byte("x"), 0o644))
	op := New(dir, "origin", "master", "Bot", "bot@x")
	require.NoError(t, op.CommitAll("test commit"))
	out, err := op.Status()
	require.NoError(t, err)
	assert.Empty(t, out, "expected clean tree")
}

func TestStatus_PorcelainOutput(t *testing.T) {
	dir := initRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.md"), []byte("y"), 0o644))
	op := New(dir, "origin", "master", "Bot", "bot@x")
	out, err := op.Status()
	require.NoError(t, err)
	assert.Contains(t, out, "a.md")
}

func TestCommitAll_LogsInfo(t *testing.T) {
	dir := initRepo(t)
	log, buf := newBufLogger()
	op := NewWithLogger(dir, "origin", "master", "Bot", "bot@x", log)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.md"), []byte("x"), 0o644))
	require.NoError(t, op.CommitAll("logged commit"))
	assert.Contains(t, buf.String(), "git commit")
}

func TestCommitAll_LogsDebugOnNoop(t *testing.T) {
	dir := initRepo(t)
	log, buf := newBufLogger()
	op := NewWithLogger(dir, "origin", "master", "Bot", "bot@x", log)
	require.NoError(t, op.CommitAll("noop"))
	assert.Contains(t, buf.String(), "nothing to commit")
}

func TestRun_LogsFailure(t *testing.T) {
	dir := initRepo(t)
	log, buf := newBufLogger()
	op := NewWithLogger(dir, "origin", "master", "Bot", "bot@x", log)
	_, _ = op.run("nonexistent-subcommand-xyz")
	assert.Contains(t, buf.String(), "git cmd failed")
}
