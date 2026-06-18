package api

import "github.com/bestxp/narrative-ai-agent/internal/staging"

// StagingRepository owns the world's staged story graph
// (staging.yaml + stage.md). The two files together
// represent the current stage and the pending transition.
type StagingRepository interface {
	Load(world string) (*staging.Staging, error)
	// UpdateStage schedules a new pending transition. The
	// transition is not active until end_day applies it.
	UpdateStage(world, nextID string) (bool, error)
	// AdvanceTimeline moves the cursor within the active
	// stage's timeline.
	AdvanceTimeline(world string) (bool, error)
	// ApplyPendingStage activates a previously-scheduled
	// stage transition. No-op when nothing is pending.
	ApplyPendingStage(world string) error
}
