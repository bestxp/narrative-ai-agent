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
		cmd := exec.CommandContext(t.Context(), "git", c...)
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
	t.Parallel()
	dir := initRepo(t)
	assert.True(t, IsRepo(dir))
}

func TestIsRepo_False(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	assert.False(t, IsRepo(dir))
}

func TestCommitAll_NothingToCommitIsNoop(t *testing.T) {
	t.Parallel()
	dir := initRepo(t)
	op := New(dir, "origin", "master", "Bot", "bot@x")
	res, err := op.CommitAll("noop")
	require.NoError(t, err)
	assert.True(t, res.Empty)
}

func TestCommitAll_CommitsNewFile(t *testing.T) {
	t.Parallel()
	dir := initRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.md"), []byte("x"), 0o600))
	op := New(dir, "origin", "master", "Bot", "bot@x")
	res, err := op.CommitAll("test commit")
	require.NoError(t, err)
	assert.False(t, res.Empty)
	assert.NotEmpty(t, res.Hash)
	assert.Contains(t, res.FilesChanged, "a.md")
	out, err := op.Status()
	require.NoError(t, err)
	assert.Empty(t, out, "expected clean tree")
}

func TestStatus_PorcelainOutput(t *testing.T) {
	t.Parallel()
	dir := initRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.md"), []byte("y"), 0o600))
	op := New(dir, "origin", "master", "Bot", "bot@x")
	out, err := op.Status()
	require.NoError(t, err)
	assert.Contains(t, out, "a.md")
}

func TestCommitAll_LogsInfo(t *testing.T) {
	t.Parallel()
	dir := initRepo(t)
	log, buf := newBufLogger()
	op := NewWithLogger(dir, "origin", "master", "Bot", "bot@x", false, log)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.md"), []byte("x"), 0o600))
	_, err := op.CommitAll("logged commit")
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "git commit")
}

func TestCommitAll_LogsDebugOnNoop(t *testing.T) {
	t.Parallel()
	dir := initRepo(t)
	log, buf := newBufLogger()
	op := NewWithLogger(dir, "origin", "master", "Bot", "bot@x", false, log)
	_, err := op.CommitAll("noop")
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "nothing to commit")
}

func TestRun_LogsFailure(t *testing.T) {
	t.Parallel()
	dir := initRepo(t)
	log, buf := newBufLogger()
	op := NewWithLogger(dir, "origin", "master", "Bot", "bot@x", false, log)
	_, _ = op.run("nonexistent-subcommand-xyz")

	assert.Contains(t, buf.String(), "git cmd failed")
}

func TestSyncRebase_RemoteDisabledReturnsError(t *testing.T) {
	t.Parallel()
	dir := initRepo(t)
	op := New(dir, "origin", "master", "Bot", "bot@x")
	op.remoteDisabled = true
	err := op.SyncRebase()
	assert.ErrorIs(t, err, ErrRemoteDisabled)
}

func TestSyncRebase_RemoteDisabled_DoesNotTouchNetwork(t *testing.T) {
	t.Parallel()
	// If SyncRebase were to call `git pull` while remoteDisabled
	// is true, the test would try to talk to a non-existent remote
	// and fail with a different error. We assert that no network
	// command runs by ensuring the error is exactly ErrRemoteDisabled.
	dir := initRepo(t)
	log, buf := newBufLogger()
	op := NewWithLogger(dir, "origin", "master", "Bot", "bot@x", true, log)
	err := op.SyncRebase()
	require.ErrorIs(t, err, ErrRemoteDisabled)
	assert.Contains(t, buf.String(), "git push skipped")
}

func TestRemoteDisabled_DefaultFalse(t *testing.T) {
	t.Parallel()
	dir := initRepo(t)
	op := New(dir, "origin", "master", "Bot", "bot@x")
	assert.False(t, op.RemoteDisabled())
}

func TestRemoteDisabled_ReflectsConfig(t *testing.T) {
	t.Parallel()
	dir := initRepo(t)
	op := NewWithLogger(dir, "origin", "master", "Bot", "bot@x", true, zerolog.Nop())
	assert.True(t, op.RemoteDisabled())
}
