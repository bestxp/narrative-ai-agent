package files

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/staging"
)

// StageTool implements UpdateStage, AdvanceTimeline and
// ApplyPendingStage against the world's staging.yaml + stage.md
// pair. It is a thin layer over the staging package — all
// validation and repair logic lives there.
type StageTool struct {
	fs  staging.FileStore
	log zerolog.Logger
}

func newStage(fs staging.FileStore, log zerolog.Logger) *StageTool {
	return &StageTool{fs: fs, log: log.With().Str("component", "stage").Logger()}
}

// UpdateStage writes the pending stage transition to stage.md.
// Returns true when the file was rewritten. The model can
// re-call with the same next_id — it is idempotent.
func (s *StageTool) UpdateStage(ctx context.Context, world, nextID string) (bool, error) {
	if world == "" {
		return false, fmt.Errorf("stage: world is empty")
	}
	if nextID == "" {
		return false, fmt.Errorf("stage: next_id is empty")
	}
	st, err := staging.Load(s.fs, world)
	if err != nil {
		return false, fmt.Errorf("stage: load: %w", err)
	}
	if !st.Enabled {
		return false, nil
	}
	if err := st.UpdateStage(s.fs, world, nextID); err != nil {
		return false, fmt.Errorf("stage: update: %w", err)
	}
	s.log.Info().
		Str("world", world).
		Str("next", nextID).
		Msg("update_stage: pending transition recorded")
	return true, nil
}

// AdvanceTimeline moves the timeline cursor forward by one.
// Returns true on a successful advance, false when already at
// the last point or when staging is disabled.
func (s *StageTool) AdvanceTimeline(ctx context.Context, world string) (bool, error) {
	if world == "" {
		return false, fmt.Errorf("stage: world is empty")
	}
	st, err := staging.Load(s.fs, world)
	if err != nil {
		return false, fmt.Errorf("stage: load: %w", err)
	}
	if !st.Enabled {
		return false, nil
	}
	if err := st.AdvanceTimeline(s.fs, world); err != nil {
		return false, fmt.Errorf("stage: advance: %w", err)
	}
	s.log.Info().
		Str("world", world).
		Int("timeline_index", st.TimelineIndex).
		Msg("advance_timeline: cursor moved")
	return true, nil
}

// ApplyPendingStage activates a previously-scheduled transition.
// No-op when nothing is pending.
func (s *StageTool) ApplyPendingStage(world string) error {
	if world == "" {
		return fmt.Errorf("stage: world is empty")
	}
	st, err := staging.Load(s.fs, world)
	if err != nil {
		return fmt.Errorf("stage: load: %w", err)
	}
	if !st.Enabled {
		return nil
	}
	if st.Next == "" {
		return nil
	}
	if err := st.ApplyPending(s.fs, world); err != nil {
		return fmt.Errorf("stage: apply: %w", err)
	}
	s.log.Info().
		Str("world", world).
		Str("current", st.Current.ID).
		Msg("apply_pending_stage: transition activated")
	return nil
}
