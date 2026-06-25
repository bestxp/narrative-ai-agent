package api

import "github.com/bestxp/narrative-ai-agent/internal/staging"

// StagingRepository owns the world's staged story
// graph (staging.yaml). planning/0001: stage.md was
// merged into state.yaml. The runtime slice
// (current / timeline_index / next) is read from
// state.yaml by the caller and passed in here — the
// staging package doesn't read state.yaml directly to
// avoid an import cycle (staging ↔ yaml-repository).
type StagingRepository interface {
	Load(world string, runtime staging.StageRuntime) (*staging.Staging, error)
	// UpdateStage schedules a new pending transition. The
	// transition is not active until end_day applies it.
	// In-memory mutation — caller must persist via
	// WorldState.Save.
	UpdateStage(world, nextID string, runtime staging.StageRuntime) (bool, error)
	// AdvanceTimeline moves the cursor within the active
	// stage's timeline. In-memory mutation — caller must
	// persist via WorldState.Save.
	AdvanceTimeline(world string, runtime staging.StageRuntime) (bool, error)
	// ApplyPendingStage activates a previously-scheduled
	// stage transition. No-op when nothing is pending.
	// In-memory mutation — caller must persist via
	// WorldState.Save. Returns the post-apply runtime
	// slice so the caller doesn't need to re-load the
	// graph to capture the new Current.
	ApplyPendingStage(world string, runtime staging.StageRuntime) (staging.StageRuntime, error)
}
