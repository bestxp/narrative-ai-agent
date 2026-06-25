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
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

// minimalValidYAML is the smallest config that passes Validate():
// messaging.telegram configured, access list non-empty, llm.narrative
// present. Other sections use defaults.
const minimalValidYAML = `
messaging:
  telegram:
    token: "ABC:XYZ"
    allowed_user_ids: [1]
llm:
  roles:
    narrative:
      model: "qwen2.5:7b-instruct"
`

func TestLoad_OK(t *testing.T) {
	t.Parallel()
	body := `
messaging:
  telegram:
    token: "ABC:XYZ"
    polling_timeout: 45
    parse_mode: "MarkdownV2"
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
llm:
  default_timeout_seconds: 90
  roles:
    narrative:
      model: "qwen2.5:7b-instruct"
      temperature: 0.4
`
	cfg, err := Load(writeTempConfig(t, body))
	require.NoError(t, err)
	assert.Equal(t, "ABC:XYZ", cfg.Messaging.Telegram.Token)
	assert.True(t, cfg.TelegramIsAllowed(111))
	assert.False(t, cfg.TelegramIsAllowed(999))
	assert.Equal(t, 200, cfg.Narrative.WordLimit)
	assert.Equal(t, 90, cfg.LLM.DefaultTimeoutSeconds)
}

func TestLoad_RejectsPlaceholderToken(t *testing.T) {
	t.Parallel()
	body := `
messaging:
  telegram:
    token: "REPLACE_WITH_BOTFATHER_TOKEN"
    allowed_user_ids: [1]
llm:
  roles:
    narrative: { model: "x" }
`
	_, err := Load(writeTempConfig(t, body))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "messaging.telegram.token")
}

func TestLoad_RejectsEmptyAllowed(t *testing.T) {
	t.Parallel()
	body := `
messaging:
  telegram:
    token: "ABC:XYZ"
    allowed_user_ids: []
llm:
  roles:
    narrative: { model: "x" }
`
	_, err := Load(writeTempConfig(t, body))
	require.Error(t, err)
}

func TestLoad_RejectsMissingToken(t *testing.T) {
	t.Parallel()
	// Token completely absent — also rejected (the bot cannot talk
	// to anyone if the transport is not configured).
	body := `
messaging:
  telegram:
    allowed_user_ids: [1]
llm:
  roles:
    narrative: { model: "x" }
`
	_, err := Load(writeTempConfig(t, body))
	require.Error(t, err)
}

func TestLoad_AppliesDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeTempConfig(t, minimalValidYAML))
	require.NoError(t, err)
	assert.NotEmpty(t, cfg.Paths.DataRoot)
	assert.Equal(t, "origin", cfg.Git.Remote)
	assert.Equal(t, 150, cfg.Narrative.WordLimit)
	assert.Equal(t, 120, cfg.LLM.DefaultTimeoutSeconds)
	role, ok := cfg.Role(NarrativeRole)
	require.True(t, ok)
	assert.Equal(t, "qwen2.5:7b-instruct", role.Model)
	assert.Equal(t, "http://localhost:11434/v1", role.APIURL)
	assert.InDelta(t, 0.8, role.Temperature, 1e-9)
	assert.Equal(t, 2500, role.MaxTokens)
	assert.Equal(t, 120, role.RequestTimeoutSeconds)
	// system_prompt_path is empty by default — the embed.FS
	// copy in internal/prompts is the fallback.
	assert.Empty(t, role.SystemPromptPath)
}

func TestLoad_MultipleRoles(t *testing.T) {
	t.Parallel()
	body := `
messaging:
  telegram:
    token: "ABC"
    allowed_user_ids: [1]
llm:
  default_timeout_seconds: 60
  roles:
    narrative:
      model: "qwen2.5:7b-instruct"
      temperature: 0.8
    summary:
      model: "qwen2.5:1.5b-instruct"
      temperature: 0.2
      max_tokens: 400
      system_prompt_path: "prompts/summary.md"
`
	cfg, err := Load(writeTempConfig(t, body))
	require.NoError(t, err)
	assert.Len(t, cfg.LLM.Roles, 2)
	narr, ok := cfg.Role("narrative")
	require.True(t, ok)
	assert.InDelta(t, 0.8, narr.Temperature, 1e-9)
	sum, ok := cfg.Role("summary")
	require.True(t, ok)
	assert.InDelta(t, 0.2, sum.Temperature, 1e-9)
	assert.Equal(t, 400, sum.MaxTokens)
	// Roles without explicit timeout fall back to default_timeout_seconds.
	assert.Equal(t, 60, sum.RequestTimeoutSeconds)
}

func TestLoad_RoleFallsBackToDefaults(t *testing.T) {
	t.Parallel()
	// Role exists but no fields filled in — defaults kick in.
	body := `
messaging:
  telegram:
    token: "ABC"
    allowed_user_ids: [1]
llm:
  roles:
    narrative:
      model: "x"
`
	cfg, err := Load(writeTempConfig(t, body))
	require.NoError(t, err)
	role, ok := cfg.Role(NarrativeRole)
	require.True(t, ok)
	assert.InDelta(t, 0.8, role.Temperature, 1e-9)
	assert.Equal(t, 2500, role.MaxTokens)
	// system_prompt_path is empty by default — main.go will fall
	// back to the embed.FS copy in internal/prompts/narrative.md.
	assert.Empty(t, role.SystemPromptPath)
}

func TestRole_UnknownReturnsFalse(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeTempConfig(t, minimalValidYAML))
	require.NoError(t, err)
	_, ok := cfg.Role("does-not-exist")
	assert.False(t, ok)
}

func TestRole_EmptyModelReturnsFalse(t *testing.T) {
	t.Parallel()
	// Role key present but model empty — should be treated as
	// "not configured" so callers can default to a healthy fallback.
	body := `
messaging:
  telegram:
    token: "ABC"
    allowed_user_ids: [1]
llm:
  roles:
    narrative: { model: "" }
`
	cfg, err := Load(writeTempConfig(t, body))
	// Validate() rejects empty model: the bot refuses to start with
	// a half-configured narrative role.
	require.Error(t, err)
	_ = cfg
}

func TestMustRole_PanicsOnMissing(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeTempConfig(t, minimalValidYAML))
	require.NoError(t, err)
	assert.Panics(t, func() { cfg.MustRole("ghost") })
}

func TestMustRole_ReturnsConfigured(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeTempConfig(t, minimalValidYAML))
	require.NoError(t, err)
	r := cfg.MustRole(NarrativeRole)
	assert.Equal(t, "qwen2.5:7b-instruct", r.Model)
}

func TestTelegramIsAllowed_EmptyList(t *testing.T) {
	t.Parallel()
	cfg := &Config{}
	assert.False(t, cfg.TelegramIsAllowed(0))
	assert.False(t, cfg.TelegramIsAllowed(42))
}

func TestLoad_RemoteDisabled_DefaultsFalse(t *testing.T) {
	t.Parallel()
	// Absent `remote_disabled` key → push is enabled.
	cfg, err := Load(writeTempConfig(t, minimalValidYAML))
	require.NoError(t, err)
	assert.False(t, cfg.Git.RemoteDisabled)
}

func TestLoad_RemoteDisabled_TrueHonoured(t *testing.T) {
	t.Parallel()
	body := `
messaging:
  telegram:
    token: "ABC"
    allowed_user_ids: [1]
git:
  remote_disabled: true
llm:
  roles:
    narrative: { model: "x" }
`
	cfg, err := Load(writeTempConfig(t, body))
	require.NoError(t, err)
	assert.True(t, cfg.Git.RemoteDisabled)
}
