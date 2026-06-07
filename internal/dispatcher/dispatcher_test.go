package dispatcher

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"narrative/internal/adapter/storage"
	"narrative/internal/config"
	"narrative/internal/messaging"
	"narrative/internal/slowlog"
	"narrative/internal/usecase"
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
		Messaging: config.MessagingConfig{
			Telegram: config.TelegramConfig{
				Token:          "x",
				PollingTimeout: 30,
				ParseMode:      "",
				AllowedUserIDs: []int{1, 2},
			},
		},
		Paths:     config.PathsConfig{DataRoot: workdir, GitWorkdir: workdir},
		Git:       config.GitConfig{Remote: "origin", Branch: "master", CommitAuthor: "Bot", CommitEmail: "b@b"},
		Narrative: config.NarrativeConfig{WordLimit: 350, Language: "ru"},
	}
}

func setup(t *testing.T) (*Dispatcher, *storage.FileStore) {
	t.Helper()
	workdir := initRepo(t)
	dataDir := filepath.Join(workdir, "game-data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	fs, err := storage.NewFileStore(dataDir)
	require.NoError(t, err)
	tools := usecase.NewFileToolset(fs, zerolog.Nop(), slowlog.Discard())
	d := New(newCfg(t, workdir), fs, nil, tools, slowlog.Discard(), zerolog.Nop())
	return d, fs
}

func TestDispatcher_CommandsHasAllEntries(t *testing.T) {
	d, _ := setup(t)
	cmds := d.Commands()
	assert.GreaterOrEqual(t, len(cmds), 8, "expected at least 8 commands")
	names := make(map[string]bool, len(cmds))
	for _, c := range cmds {
		assert.NotEmpty(t, c.Command)
		assert.NotEmpty(t, c.Description)
		assert.False(t, names[c.Command], "duplicate command name: %s", c.Command)
		names[c.Command] = true
	}
	for _, want := range []string{"start", "status", "me", "launch", "endday", "save", "help"} {
		assert.True(t, names[want], "missing %q in Commands()", want)
	}
}

func TestDispatcher_LaunchAndStart(t *testing.T) {
	d, fs := setup(t)
	rep, err := d.Handle(context.Background(), messaging.IncomingMessage{
		Command: "launch", Args: []string{"Маркус", "naruto", "канон"},
	})
	require.NoError(t, err)
	assert.Contains(t, rep, "Создано")
	assert.True(t, fs.Exists("characters/markus"))

	rep, err = d.Handle(context.Background(), messaging.IncomingMessage{Command: "start"})
	require.NoError(t, err)
	assert.Contains(t, rep, "Мир: naruto")
}

func TestDispatcher_EndDay(t *testing.T) {
	d, fs := setup(t)
	_, _ = d.Handle(context.Background(), messaging.IncomingMessage{Command: "launch", Args: []string{"m", "naruto"}})
	rep, err := d.Handle(context.Background(), messaging.IncomingMessage{Command: "endday", Args: []string{"5", "бой"}})
	require.NoError(t, err)
	assert.Contains(t, rep, "День 5")
	mem, _ := fs.ReadRaw("worlds/naruto/memorise.md")
	assert.Contains(t, mem, "д00005: бой")
}

func TestDispatcher_LeaveAndReturn(t *testing.T) {
	d, fs := setup(t)
	_, _ = d.Handle(context.Background(), messaging.IncomingMessage{Command: "launch", Args: []string{"m", "naruto"}})
	rep, err := d.Handle(context.Background(), messaging.IncomingMessage{Command: "leave", Args: []string{"bleach"}})
	require.NoError(t, err)
	assert.Contains(t, rep, "Активный мир: bleach")
	assert.True(t, fs.Exists("worlds/bleach/state.md"))

	_, err = d.Handle(context.Background(), messaging.IncomingMessage{Command: "return", Args: []string{"naruto", "3"}})
	require.NoError(t, err)
	state, _ := fs.ReadRaw("worlds/naruto/state.md")
	assert.Contains(t, state, "День 4")
}

func TestDispatcher_FreeformValidates(t *testing.T) {
	d, _ := setup(t)
	rep, err := d.Handle(context.Background(), messaging.IncomingMessage{Text: "ты усмехнулся"})
	require.NoError(t, err)
	assert.Contains(t, rep, "**ВАЛИДАЦИЯ ПРАВИЛ**")
	assert.Contains(t, rep, "ты усмехнулся")
}

func TestDispatcher_UnknownCommandIsSilent(t *testing.T) {
	d, _ := setup(t)
	rep, err := d.Handle(context.Background(), messaging.IncomingMessage{Command: "no-such"})
	require.NoError(t, err)
	assert.Empty(t, rep)
}

func TestDispatcher_Help(t *testing.T) {
	d, _ := setup(t)
	rep, _ := d.Handle(context.Background(), messaging.IncomingMessage{Command: "help"})
	assert.Contains(t, rep, "/launch")
}
