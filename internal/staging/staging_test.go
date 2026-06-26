package staging_test

import (
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/staging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memFS is a tiny in-memory FileStore for tests.
type memFS struct {
	files map[string]string
}

func newMemFS() *memFS {
	return &memFS{files: map[string]string{}}
}

func (m *memFS) ReadRaw(rel string) (string, error) {
	return m.files[rel], nil
}

func (m *memFS) WriteRawAtomic(rel, body string) error {
	m.files[rel] = body

	return nil
}

func (m *memFS) Exists(rel string) bool {
	_, ok := m.files[rel]

	return ok
}

const validYAML = `enabled: true
init:
  - beginning
stages:
  - id: beginning
    name: Появление в мире
    description: |
      Герой $(name) появляется в мире.
    timeline:
      - days: 1
        info: Появление
        description: Герой материализуется.
      - days: 2-4
        info: Допрос
        description: Допрос дознавателей.
    next:
      - id: accepted
        requirements:
          - Герой доказал невиновность
  - id: accepted
    name: Принятие
    description: Герой принят.
    timeline:
      - days: 5-7
        info: Адаптация
        description: Знакомство с городом.
    next:
      - id: beginning
        requirements:
          - Откат
`

func TestLoad_Sandbox(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	// No staging.yaml at all.
	s, err := staging.Load(fs, "naruto", staging.StageRuntime{})
	require.NoError(t, err, "Load")

	require.False(t, s.Enabled, "sandbox: expected Enabled=false, got true")
}

func TestLoad_DisabledExplicit(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = "enabled: false\ninit: []\nstages: []\n"

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{})
	require.NoError(t, err, "Load")

	require.False(t, s.Enabled, "expected Enabled=false")
}

func TestLoad_BrokenYAML(t *testing.T) {
	t.Parallel()

	fs := newMemFS()

	fs.files["worlds/naruto/staging.yaml"] = "enabled: true\nstages: : :\n"
	if _, err := staging.Load(fs, "naruto", staging.StageRuntime{}); err == nil {
		require.Fail(t, "expected parse error")
	}
}

func TestLoad_EnabledInitialisesFromInit(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{})
	require.NoError(t, err, "Load")

	require.True(t, s.Enabled, "expected Enabled=true")
	require.Equal(t, "beginning", s.Current.ID)
	require.Equal(t, 0, s.TimelineIndex)
	require.Empty(t, s.Next)
}

func TestLoad_ReusesStageState(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	// planning/0001: stage.md no longer exists; the
	// runtime slice is passed in by the caller.
	s, err := staging.Load(fs, "naruto", staging.StageRuntime{Current: "accepted"})
	require.NoError(t, err, "Load")

	require.Equal(t, "accepted", s.Current.ID)
}

func TestLoad_RepairsUnknownNext(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{Current: "beginning", Next: "ghost"})
	require.NoError(t, err, "Load")

	require.Empty(t, s.Next)
}

func TestLoad_RepairsTimelineIndex(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{Current: "beginning", TimelineIndex: 99})
	require.NoError(t, err, "Load")

	require.Equal(t, 0, s.TimelineIndex)
}

func TestUpdateStage_Valid(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{})
	require.NoError(t, err, "Load")

	require.NoError(t, s.UpdateStage(fs, "naruto", "accepted"), "UpdateStage")

	require.Equal(t, "accepted", s.Next)
	require.Equal(t, "accepted", s.Runtime().Next)
}

func TestUpdateStage_InvalidTransition(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{})
	require.NoError(t, err, "Load")

	require.Error(t, s.UpdateStage(fs, "naruto", "nowhere"), "expected error for invalid transition")
}

func TestUpdateStage_Idempotent(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{})
	require.NoError(t, err, "Load")

	require.NoError(t, s.UpdateStage(fs, "naruto", "accepted"), "UpdateStage #1")
	require.NoError(t, s.UpdateStage(fs, "naruto", "accepted"), "UpdateStage #2")

	require.Equal(t, "accepted", s.Next)
}

func TestAdvanceTimeline(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{})
	require.NoError(t, err, "Load")

	require.NoError(t, s.AdvanceTimeline(fs, "naruto"), "AdvanceTimeline")
	require.Equal(t, 1, s.TimelineIndex)
	require.Error(t, s.AdvanceTimeline(fs, "naruto"), "expected error at end of timeline")
}

func TestApplyPending(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{})
	require.NoError(t, err, "Load")

	require.NoError(t, s.UpdateStage(fs, "naruto", "accepted"), "UpdateStage")
	_, err = s.ApplyPending(fs, "naruto")
	require.NoError(t, err, "ApplyPending")

	require.Equal(t, "accepted", s.Current.ID)
	require.Empty(t, s.Next)
	require.Equal(t, 0, s.TimelineIndex)
}

func TestApplyPending_NoopIfEmpty(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{})
	require.NoError(t, err, "Load")

	_, err = s.ApplyPending(fs, "naruto")
	require.NoError(t, err, "ApplyPending")

	require.Equal(t, "beginning", s.Current.ID)
}

func TestRender_Basic(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{Current: "beginning", TimelineIndex: 1})
	require.NoError(t, err, "Load")

	out := s.Render("Маркус")
	assert.Contains(t, out, "Сюжетная стадия", "render must include header")
	assert.Contains(t, out, "Маркус", "render must include character name")
	assert.Contains(t, out, "[>]", "render must include current marker")
	assert.Contains(t, out, "[X]", "render must include done marker")
	assert.Contains(t, out, "→ accepted", "render must list transition")
}

func TestRender_SandboxEmpty(t *testing.T) {
	t.Parallel()

	fs := newMemFS()

	s, _ := staging.Load(fs, "naruto", staging.StageRuntime{})
	if s.Render("Маркус") != "" {
		require.Fail(t, "sandbox render should be empty")
	}
}

func TestRender_PendingShown(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, _ := staging.Load(fs, "naruto", staging.StageRuntime{})
	_ = s.UpdateStage(fs, "naruto", "accepted")

	out := s.Render("")
	assert.Contains(t, out, "(завершается) → accepted", "render must include pending marker")
}

func TestValidation_DuplicateID(t *testing.T) {
	t.Parallel()

	bad := `enabled: true
init: [a]
stages:
  - id: a
    name: A
    description: A
    timeline: []
    next:
      - id: a
        requirements: [r]
  - id: a
    name: A2
    description: A2
    timeline: []
    next:
      - id: a
        requirements: [r]
`
	fs := newMemFS()

	fs.files["worlds/naruto/staging.yaml"] = bad
	if _, err := staging.Load(fs, "naruto", staging.StageRuntime{}); err == nil {
		require.Fail(t, "expected duplicate-id validation error")
	}
}

func TestValidation_MixedDays(t *testing.T) {
	t.Parallel()

	bad := `enabled: true
init: [a]
stages:
  - id: a
    name: A
    description: A
    timeline:
      - days: 1
        info: x
        description: y
      - info: z
        description: w
    next:
      - id: a
        requirements: [r]
`
	fs := newMemFS()

	fs.files["worlds/naruto/staging.yaml"] = bad
	if _, err := staging.Load(fs, "naruto", staging.StageRuntime{}); err == nil {
		require.Fail(t, "expected mixed-days validation error")
	}
}

func TestValidation_UnknownTransition(t *testing.T) {
	t.Parallel()

	bad := `enabled: true
init: [a]
stages:
  - id: a
    name: A
    description: A
    timeline: []
    next:
      - id: ghost
        requirements: [r]
`
	fs := newMemFS()

	fs.files["worlds/naruto/staging.yaml"] = bad
	if _, err := staging.Load(fs, "naruto", staging.StageRuntime{}); err == nil {
		require.Fail(t, "expected unknown-transition validation error")
	}
}
