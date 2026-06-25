package files

import (
	"context"
	"errors"
	"fmt"

	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/staging"
	"github.com/rs/zerolog"
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
func (s *StageTool) UpdateStage(_ context.Context, world, nextID string) (bool, error) {
	if world == "" {
		return false, errors.New("stage: world is empty")
	}
	if nextID == "" {
		return false, errors.New("stage: next_id is empty")
	}

	rt, snap, err := s.loadRuntime(world)
	if err != nil {
		return false, fmt.Errorf("stage: load runtime: %w", err)
	}

	ok, err := s.repos.Staging.UpdateStage(world, nextID, rt)
	if err != nil {
		return false, fmt.Errorf("stage: update: %w", err)
	}

	if !ok {
		return false, nil
	}

	if err := s.saveStage(world, snap, rt); err != nil {
		return false, fmt.Errorf("stage: persist: %w", err)
	}

	s.log.Info().
		Str("world", world).
		Str("next", nextID).
		Msg("update_stage: pending transition recorded")

	return true, nil
}

// AdvanceTimeline moves the timeline cursor forward.
func (s *StageTool) AdvanceTimeline(_ context.Context, world string) (bool, error) {
	if world == "" {
		return false, errors.New("stage: world is empty")
	}

	rt, snap, err := s.loadRuntime(world)
	if err != nil {
		return false, fmt.Errorf("stage: load runtime: %w", err)
	}

	ok, err := s.repos.Staging.AdvanceTimeline(world, rt)
	if err != nil {
		return false, fmt.Errorf("stage: advance: %w", err)
	}

	if !ok {
		return false, nil
	}

	if err := s.saveStage(world, snap, rt); err != nil {
		return false, fmt.Errorf("stage: persist: %w", err)
	}

	s.log.Info().
		Str("world", world).
		Msg("advance_timeline: cursor moved")

	return true, nil
}

// ApplyPendingStage activates a scheduled transition.
func (s *StageTool) ApplyPendingStage(world string) error {
	if world == "" {
		return errors.New("stage: world is empty")
	}

	rt, snap, err := s.loadRuntime(world)
	if err != nil {
		return fmt.Errorf("stage: load runtime: %w", err)
	}

	if rt.Next == "" {
		return nil
	}

	newRT, err := s.repos.Staging.ApplyPendingStage(world, rt)
	if err != nil {
		return fmt.Errorf("stage: apply: %w", err)
	}

	if err := s.saveStage(world, snap, newRT); err != nil {
		return fmt.Errorf("stage: persist: %w", err)
	}

	s.log.Info().
		Str("world", world).
		Str("current", newRT.Current).
		Msg("apply_pending_stage: transition activated")
	return nil
}

// loadRuntime pulls the stage slice from state.yaml
// before any staging call. planning/0001: stage.md
// was merged into state.yaml; the staging package
// works on in-memory state but needs the runtime
// pointer (current / timeline_index / next) from
// somewhere. The single source of truth is
// state.yaml.
func (s *StageTool) loadRuntime(world string) (staging.StageRuntime, domain.StateSnapshot, error) {
	snap, err := s.repos.WorldState.Load(world)
	if err != nil {
		return staging.StageRuntime{}, domain.StateSnapshot{}, fmt.Errorf("stage: load runtime: %w", err)
	}

	return staging.StageRuntime{
		Current:       snap.Stage.Current,
		TimelineIndex: snap.Stage.TimelineIndex,
		Next:          snap.Stage.Next,
	}, snap, nil
}

// saveStage projects the staging graph back into
// state.yaml. Single writer — keeps the runtime
// slice and the on-disk file in lockstep.
func (s *StageTool) saveStage(world string, snap domain.StateSnapshot, rt staging.StageRuntime) error {
	snap.Stage = domain.StageState{
		Current:       rt.Current,
		TimelineIndex: rt.TimelineIndex,
		Next:          rt.Next,
	}

	if err := s.repos.WorldState.Save(world, snap); err != nil {
		return fmt.Errorf("stage: persist: %w", err)
	}

	return nil
}
