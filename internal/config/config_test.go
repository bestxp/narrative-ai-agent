package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	return p
}

func TestLoad_OK(t *testing.T) {
	body := `
telegram:
  token: "ABC:XYZ"
access:
  allowed_user_ids: [111, 222]
paths:
  data_root: "data"
  git_workdir: "."
git:
  remote: "origin"
  branch: "master"
narrative:
  word_limit: 200
  language: "ru"
`
	cfg, err := Load(writeTempConfig(t, body))
	require.NoError(t, err)
	assert.Equal(t, "ABC:XYZ", cfg.Telegram.Token)
	assert.True(t, cfg.IsAllowed(111))
	assert.False(t, cfg.IsAllowed(999))
	assert.Equal(t, 200, cfg.Narrative.WordLimit)
}

func TestLoad_RejectsPlaceholderToken(t *testing.T) {
	body := `
telegram:
  token: "REPLACE_WITH_BOTFATHER_TOKEN"
access:
  allowed_user_ids: [1]
`
	_, err := Load(writeTempConfig(t, body))
	assert.Error(t, err)
}

func TestLoad_RejectsEmptyAllowed(t *testing.T) {
	body := `
telegram:
  token: "ABC:XYZ"
access:
  allowed_user_ids: []
`
	_, err := Load(writeTempConfig(t, body))
	assert.Error(t, err)
}

func TestLoad_AppliesDefaults(t *testing.T) {
	body := `
telegram:
  token: "ABC:XYZ"
access:
  allowed_user_ids: [1]
`
	cfg, err := Load(writeTempConfig(t, body))
	require.NoError(t, err)
	assert.NotEmpty(t, cfg.Paths.DataRoot)
	assert.Equal(t, "origin", cfg.Git.Remote)
	assert.Equal(t, 350, cfg.Narrative.WordLimit)
}
