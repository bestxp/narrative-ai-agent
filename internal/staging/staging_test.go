package staging_test

import (
	"strings"
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/staging"
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
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if s.Enabled {
		t.Fatalf("sandbox: expected Enabled=false, got true")
	}
}

func TestLoad_DisabledExplicit(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = "enabled: false\ninit: []\nstages: []\n"

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if s.Enabled {
		t.Fatalf("expected Enabled=false")
	}
}

func TestLoad_BrokenYAML(t *testing.T) {
	t.Parallel()

	fs := newMemFS()

	fs.files["worlds/naruto/staging.yaml"] = "enabled: true\nstages: : :\n"
	if _, err := staging.Load(fs, "naruto", staging.StageRuntime{}); err == nil {
		t.Fatalf("expected parse error")
	}
}

func TestLoad_EnabledInitialisesFromInit(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !s.Enabled {
		t.Fatalf("expected Enabled=true")
	}

	if s.Current.ID != "beginning" {
		t.Fatalf("expected current=beginning, got %q", s.Current.ID)
	}

	if s.TimelineIndex != 0 {
		t.Fatalf("expected timeline_index=0, got %d", s.TimelineIndex)
	}

	if s.Next != "" {
		t.Fatalf("expected empty next, got %q", s.Next)
	}
}

func TestLoad_ReusesStageState(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	// planning/0001: stage.md no longer exists; the
	// runtime slice is passed in by the caller.
	s, err := staging.Load(fs, "naruto", staging.StageRuntime{Current: "accepted"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if s.Current.ID != "accepted" {
		t.Fatalf("expected current=accepted, got %q", s.Current.ID)
	}
}

func TestLoad_RepairsUnknownNext(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{Current: "beginning", Next: "ghost"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if s.Next != "" {
		t.Fatalf("expected next cleared, got %q", s.Next)
	}
}

func TestLoad_RepairsTimelineIndex(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{Current: "beginning", TimelineIndex: 99})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if s.TimelineIndex != 0 {
		t.Fatalf("expected timeline_index=0, got %d", s.TimelineIndex)
	}
}

func TestUpdateStage_Valid(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := s.UpdateStage(fs, "naruto", "accepted"); err != nil {
		t.Fatalf("UpdateStage: %v", err)
	}

	if s.Next != "accepted" {
		t.Fatalf("expected next=accepted, got %q", s.Next)
	}
	// planning/0001: staging.UpdateStage is in-memory
	// only. Persistence goes through
	// WorldState.Save(world, snap{Stage: Runtime()}).
	// Here we just assert the in-memory mutation.
	if s.Runtime().Next != "accepted" {
		t.Fatalf("expected Runtime().Next=accepted, got %q", s.Runtime().Next)
	}
}

func TestUpdateStage_InvalidTransition(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := s.UpdateStage(fs, "naruto", "nowhere"); err == nil {
		t.Fatalf("expected error for invalid transition")
	}
}

func TestUpdateStage_Idempotent(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := s.UpdateStage(fs, "naruto", "accepted"); err != nil {
		t.Fatalf("UpdateStage #1: %v", err)
	}
	// Second call with same id is no-op, no error.
	if err := s.UpdateStage(fs, "naruto", "accepted"); err != nil {
		t.Fatalf("UpdateStage #2: %v", err)
	}

	if s.Next != "accepted" {
		t.Fatalf("expected next=accepted, got %q", s.Next)
	}
}

func TestAdvanceTimeline(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// beginning has 2 timeline points, advance once.
	if err := s.AdvanceTimeline(fs, "naruto"); err != nil {
		t.Fatalf("AdvanceTimeline: %v", err)
	}

	if s.TimelineIndex != 1 {
		t.Fatalf("expected timeline_index=1, got %d", s.TimelineIndex)
	}
	// Second advance should fail (already at last point).
	if err := s.AdvanceTimeline(fs, "naruto"); err == nil {
		t.Fatalf("expected error at end of timeline")
	}
}

func TestApplyPending(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := s.UpdateStage(fs, "naruto", "accepted"); err != nil {
		t.Fatalf("UpdateStage: %v", err)
	}

	if _, err := s.ApplyPending(fs, "naruto"); err != nil {
		t.Fatalf("ApplyPending: %v", err)
	}

	if s.Current.ID != "accepted" {
		t.Fatalf("expected current=accepted, got %q", s.Current.ID)
	}

	if s.Next != "" {
		t.Fatalf("expected next cleared, got %q", s.Next)
	}

	if s.TimelineIndex != 0 {
		t.Fatalf("expected timeline_index=0, got %d", s.TimelineIndex)
	}
}

func TestApplyPending_NoopIfEmpty(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if _, err := s.ApplyPending(fs, "naruto"); err != nil {
		t.Fatalf("ApplyPending: %v", err)
	}

	if s.Current.ID != "beginning" {
		t.Fatalf("expected current unchanged=beginning, got %q", s.Current.ID)
	}
}

func TestRender_Basic(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, err := staging.Load(fs, "naruto", staging.StageRuntime{Current: "beginning", TimelineIndex: 1})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	out := s.Render("Маркус")
	if !strings.Contains(out, "Сюжетная стадия") {
		t.Fatalf("missing header in render: %q", out)
	}

	if !strings.Contains(out, "Маркус") {
		t.Fatalf("expected character name in render: %q", out)
	}

	if !strings.Contains(out, "[>]") {
		t.Fatalf("expected [>] current marker: %q", out)
	}

	if !strings.Contains(out, "[X]") {
		t.Fatalf("expected [X] done marker: %q", out)
	}

	if !strings.Contains(out, "→ accepted") {
		t.Fatalf("expected transition listed: %q", out)
	}
}

func TestRender_SandboxEmpty(t *testing.T) {
	t.Parallel()

	fs := newMemFS()

	s, _ := staging.Load(fs, "naruto", staging.StageRuntime{})
	if s.Render("Маркус") != "" {
		t.Fatalf("sandbox render should be empty")
	}
}

func TestRender_PendingShown(t *testing.T) {
	t.Parallel()

	fs := newMemFS()
	fs.files["worlds/naruto/staging.yaml"] = validYAML

	s, _ := staging.Load(fs, "naruto", staging.StageRuntime{})
	_ = s.UpdateStage(fs, "naruto", "accepted")

	out := s.Render("")
	if !strings.Contains(out, "(завершается) → accepted") {
		t.Fatalf("expected pending marker: %q", out)
	}
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
		t.Fatalf("expected duplicate-id validation error")
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
		t.Fatalf("expected mixed-days validation error")
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
		t.Fatalf("expected unknown-transition validation error")
	}
}
