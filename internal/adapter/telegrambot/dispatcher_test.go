package telegrambot

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"narrative/internal/adapter/storage"
	"narrative/internal/config"
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

func newCfg(t *testing.T, workdir string) *config.Config {
	return &config.Config{
		Telegram:  config.TelegramConfig{Token: "x", ParseMode: ""},
		Access:    config.AccessConfig{AllowedUserIDs: []int{1}},
		Paths:     config.PathsConfig{DataRoot: workdir, GitWorkdir: workdir},
		Git:       config.GitConfig{Remote: "origin", Branch: "master", CommitAuthor: "Bot", CommitEmail: "b@b"},
		Narrative: config.NarrativeConfig{WordLimit: 350, Language: "ru"},
	}
}

func setup(t *testing.T) (*Dispatcher, *storage.FileStore, string) {
	t.Helper()
	workdir := initRepo(t)
	dataDir := filepath.Join(workdir, "game-data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	fs, err := storage.NewFileStore(dataDir)
	require.NoError(t, err)
	cfg := newCfg(t, workdir)
	d := NewDispatcher(cfg, fs, nil)
	return d, fs, workdir
}

func TestDispatcher_LaunchAndStart(t *testing.T) {
	d, fs, _ := setup(t)
	rep, err := d.Handle(&Context{Command: "launch", Args: []string{"Маркус", "naruto", "канон"}})
	require.NoError(t, err)
	assert.Contains(t, rep, "Создано")
	assert.True(t, fs.Exists("characters/markus"))
	rep, err = d.Handle(&Context{Command: "start"})
	require.NoError(t, err)
	assert.Contains(t, rep, "Мир: naruto")
}

func TestDispatcher_StatusBeforeLaunch(t *testing.T) {
	d, _, _ := setup(t)
	rep, _ := d.Handle(&Context{Command: "status"})
	assert.Contains(t, rep, "Нет активной")
}

func TestDispatcher_EndDay(t *testing.T) {
	d, fs, _ := setup(t)
	_, _ = d.Handle(&Context{Command: "launch", Args: []string{"m", "naruto"}})
	rep, err := d.Handle(&Context{Command: "endday", Args: []string{"5", "первый", "бой"}})
	require.NoError(t, err)
	assert.Contains(t, rep, "День 5")
	mem, _ := fs.ReadRaw("worlds/naruto/memorise.md")
	assert.Contains(t, mem, "д00005: первый бой")
}

func TestDispatcher_Leave(t *testing.T) {
	d, fs, _ := setup(t)
	_, _ = d.Handle(&Context{Command: "launch", Args: []string{"m", "naruto"}})
	rep, err := d.Handle(&Context{Command: "leave", Args: []string{"bleach", "мгновение"}})
	require.NoError(t, err)
	assert.Contains(t, rep, "Активный мир: bleach")
	assert.True(t, fs.Exists("worlds/bleach/state.md"))
}

func TestDispatcher_Return(t *testing.T) {
	d, fs, _ := setup(t)
	_, _ = d.Handle(&Context{Command: "launch", Args: []string{"m", "naruto"}})
	_, _ = d.Handle(&Context{Command: "leave", Args: []string{"bleach"}})
	_, err := d.Handle(&Context{Command: "return", Args: []string{"naruto", "3"}})
	require.NoError(t, err)
	state, _ := fs.ReadRaw("worlds/naruto/state.md")
	assert.True(t, strings.HasPrefix(state, "День 4 (в процессе)."))
}

func TestDispatcher_Help(t *testing.T) {
	d, _, _ := setup(t)
	rep, _ := d.Handle(&Context{Command: "help"})
	assert.Contains(t, rep, "/launch")
}

func TestDispatcher_FreeformValidates(t *testing.T) {
	d, _, _ := setup(t)
	rep, err := d.Handle(&Context{RawText: "ты усмехнулся, потом подумал."})
	require.NoError(t, err)
	assert.Contains(t, rep, "**ВАЛИДАЦИЯ ПРАВИЛ**")
	assert.Contains(t, rep, "ты усмехнулся")
}

func TestDispatcher_LaunchRejects(t *testing.T) {
	d, _, _ := setup(t)
	rep, _ := d.Handle(&Context{Command: "launch"})
	assert.Contains(t, rep, "Использование")
}
