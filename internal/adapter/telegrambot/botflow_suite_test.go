package telegrambot

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"narrative/internal/adapter/storage"
	"narrative/internal/config"
)

// BotFlowSuite walks the entire Telegram handler surface through the
// Dispatcher. Each subtest gets a fresh git repo + filesystem so they
// can be run in any order or in parallel.
type BotFlowSuite struct {
	suite.Suite
}

func (s *BotFlowSuite) freshEnv(t *testing.T) *Dispatcher {
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
	dataDir := filepath.Join(dir, "game-data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	fs, err := storage.NewFileStore(dataDir)
	require.NoError(t, err)
	cfg := &config.Config{
		Telegram:  config.TelegramConfig{Token: "x"},
		Access:    config.AccessConfig{AllowedUserIDs: []int{1}},
		Paths:     config.PathsConfig{DataRoot: dataDir, GitWorkdir: dir},
		Git:       config.GitConfig{Remote: "origin", Branch: "master", CommitAuthor: "Bot", CommitEmail: "b@b"},
		Narrative: config.NarrativeConfig{WordLimit: 350, Language: "ru"},
	}
	return NewDispatcher(cfg, fs, nil)
}

func (s *BotFlowSuite) Test_Launch() {
	d := s.freshEnv(s.T())
	rep, err := d.Handle(&Context{Command: "launch", Args: []string{"Маркус", "naruto"}})
	require.NoError(s.T(), err)
	assert.Contains(s.T(), rep, "Создано")
}

func (s *BotFlowSuite) Test_StartAfterLaunch() {
	d := s.freshEnv(s.T())
	_, _ = d.Handle(&Context{Command: "launch", Args: []string{"m", "naruto"}})
	rep, err := d.Handle(&Context{Command: "start"})
	require.NoError(s.T(), err)
	assert.Contains(s.T(), rep, "Мир: naruto")
}

func (s *BotFlowSuite) Test_EndDayAndMemorise() {
	d, fs := s.freshEnvWithFS(s.T())
	_, _ = d.Handle(&Context{Command: "launch", Args: []string{"m", "naruto"}})
	_, err := d.Handle(&Context{Command: "endday", Args: []string{"3", "событие"}})
	require.NoError(s.T(), err)
	mem, _ := fs.ReadRaw("worlds/naruto/memorise.md")
	assert.Contains(s.T(), mem, "д00003: событие")
}

func (s *BotFlowSuite) Test_LeaveCreatesWorld() {
	d, fs := s.freshEnvWithFS(s.T())
	_, _ = d.Handle(&Context{Command: "launch", Args: []string{"m", "naruto"}})
	rep, err := d.Handle(&Context{Command: "leave", Args: []string{"bleach"}})
	require.NoError(s.T(), err)
	assert.Contains(s.T(), rep, "Активный мир: bleach")
	assert.True(s.T(), fs.Exists("worlds/bleach/state.md"))
}

func (s *BotFlowSuite) Test_ReturnAdvancesDay() {
	d, fs := s.freshEnvWithFS(s.T())
	_, _ = d.Handle(&Context{Command: "launch", Args: []string{"m", "naruto"}})
	_, _ = d.Handle(&Context{Command: "leave", Args: []string{"bleach"}})
	_, err := d.Handle(&Context{Command: "return", Args: []string{"naruto", "4"}})
	require.NoError(s.T(), err)
	state, _ := fs.ReadRaw("worlds/naruto/state.md")
	assert.Contains(s.T(), state, "День 5")
}

func (s *BotFlowSuite) Test_MaintenanceFindsNothing() {
	d := s.freshEnv(s.T())
	_, _ = d.Handle(&Context{Command: "launch", Args: []string{"m", "naruto"}})
	rep, err := d.Handle(&Context{Command: "maintenance"})
	require.NoError(s.T(), err)
	assert.Contains(s.T(), rep, "не требуется")
}

func (s *BotFlowSuite) freshEnvWithFS(t *testing.T) (*Dispatcher, *storage.FileStore) {
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
	dataDir := filepath.Join(dir, "game-data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	fs, err := storage.NewFileStore(dataDir)
	require.NoError(t, err)
	cfg := &config.Config{
		Telegram:  config.TelegramConfig{Token: "x"},
		Access:    config.AccessConfig{AllowedUserIDs: []int{1}},
		Paths:     config.PathsConfig{DataRoot: dataDir, GitWorkdir: dir},
		Git:       config.GitConfig{Remote: "origin", Branch: "master", CommitAuthor: "Bot", CommitEmail: "b@b"},
		Narrative: config.NarrativeConfig{WordLimit: 350, Language: "ru"},
	}
	return NewDispatcher(cfg, fs, nil), fs
}

func TestBotFlowSuite(t *testing.T) {
	suite.Run(t, new(BotFlowSuite))
}
