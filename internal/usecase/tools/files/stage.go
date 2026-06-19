package files

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
)

// StageTool implements UpdateStage, AdvanceTimeline and
// ApplyPendingStage via the StagingRepository. It is a
// thin layer over repos.Staging — all validation and
// repair logic lives in the staging package (via the
// repository's YAML implementation).
type StageTool struct {
	repos *api.Repositories
	log   zerolog.Logger
}

func newStage(log zerolog.Logger, repos *api.Repositories) *StageTool {
	return &StageTool{repos: repos, log: log.With().Str("component", "stage").Logger()}
}

// UpdateStage writes the pending stage transition.
func (s *StageTool) UpdateStage(ctx context.Context, world, nextID string) (bool, error) {
	if world == "" {
		return false, fmt.Errorf("stage: world is empty")
	}
	if nextID == "" {
		return false, fmt.Errorf("stage: next_id is empty")
	}
	ok, err := s.repos.Staging.UpdateStage(world, nextID)
	if err != nil {
		return false, fmt.Errorf("stage: update: %w", err)
	}
	if ok {
		s.log.Info().
			Str("world", world).
			Str("next", nextID).
			Msg("update_stage: pending transition recorded")
	}
	return ok, nil
}

// AdvanceTimeline moves the timeline cursor forward.
func (s *StageTool) AdvanceTimeline(ctx context.Context, world string) (bool, error) {
	if world == "" {
		return false, fmt.Errorf("stage: world is empty")
	}
	ok, err := s.repos.Staging.AdvanceTimeline(world)
	if err != nil {
		return false, fmt.Errorf("stage: advance: %w", err)
	}
	if ok {
		s.log.Info().
			Str("world", world).
			Msg("advance_timeline: cursor moved")
	}
	return ok, nil
}

// ApplyPendingStage activates a scheduled transition.
func (s *StageTool) ApplyPendingStage(world string) error {
	if world == "" {
		return fmt.Errorf("stage: world is empty")
	}
	if err := s.repos.Staging.ApplyPendingStage(world); err != nil {
		return fmt.Errorf("stage: apply: %w", err)
	}
	s.log.Info().
		Str("world", world).
		Msg("apply_pending_stage: transition activated")
	return nil
}
