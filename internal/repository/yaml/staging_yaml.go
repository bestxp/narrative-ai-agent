package yaml

import (
	"fmt"
	"strings"

	"github.com/bestxp/narrative-ai-agent/internal/staging"
	"github.com/bestxp/narrative-ai-agent/internal/storage"
)

// stagingAdapter wraps storage.Storage into the
// staging.FileStore interface that the staging package
// expects (string-based ReadRaw/WriteRawAtomic/Exists).
// The adapter is constructed once per StagingYaml and
// lives as long as the repository — no per-call
// allocation.
//
// Why this layer exists: the staging package was
// written before the storage interface was introduced
// and takes its own narrow FileStore. The repository
// layer translates our generic 5-operation Storage
// into the staging package's string-based view so we
// don't have to fork the staging package.
type stagingAdapter struct {
	store storage.Storage
}

// ReadRaw satisfies staging.FileStore.
func (a *stagingAdapter) ReadRaw(rel string) (string, error) {
	body, err := a.store.Read(rel)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// WriteRawAtomic satisfies staging.FileStore. Empty
// bodies pass through as "" (consistent with the
// previous FileStore behaviour — staging treats
// empty as a disabled world).
func (a *stagingAdapter) WriteRawAtomic(rel, body string) error {
	if strings.TrimSpace(body) == "" {
		return nil
	}
	return a.store.Write(rel, []byte(body))
}

// Exists satisfies staging.FileStore.
func (a *stagingAdapter) Exists(rel string) bool {
	ok, _ := a.store.Exists(rel)
	return ok
}

// Compile-time guarantee.
var _ staging.FileStore = (*stagingAdapter)(nil)

// StagingYaml is the YAML-backed implementation of
// StagingRepository. The staging package does the
// heavy lifting (graph validation, transition rules);
// the repository wraps it under the StagingRepository
// interface and routes I/O through the stagingAdapter.
type StagingYaml struct {
	adapter *stagingAdapter
}

// NewStagingYaml constructs the staging repository.
func NewStagingYaml(store storage.Storage) *StagingYaml {
	return &StagingYaml{adapter: &stagingAdapter{store: store}}
}

// Load delegates to staging.Load. planning/0001:
// stage.md no longer exists — the runtime slice of
// the graph (current / timeline_index / next) lives
// in state.yaml. Callers must pass the parsed
// runtime state (typically loaded from WorldState
// by the caller) so the in-memory staging graph
// starts from the same place the on-disk snapshot
// shows.
func (r *StagingYaml) Load(world string, runtime staging.StageRuntime) (*staging.Staging, error) {
	s, err := staging.Load(r.adapter, world, runtime)
	if err != nil {
		return nil, fmt.Errorf("staging: load: %w", err)
	}

	return s, nil
}

// UpdateStage sets the pending next stage. Returns
// true when the file changed (false if nextID is
// already the current pending value or unknown).
// In-memory mutation only — caller is responsible
// for persisting via WorldState.Save (planning/0001:
// no more stage.md writes).
func (r *StagingYaml) UpdateStage(world, nextID string, runtime staging.StageRuntime) (bool, error) {
	s, err := r.Load(world, runtime)
	if err != nil {
		return false, err
	}
	before := s.Next
	if err := s.UpdateStage(r.adapter, world, nextID); err != nil {
		return false, err
	}
	return s.Next != before, nil
}

// AdvanceTimeline moves the cursor within the active
// stage's timeline. In-memory mutation only.
func (r *StagingYaml) AdvanceTimeline(world string, runtime staging.StageRuntime) (bool, error) {
	s, err := r.Load(world, runtime)
	if err != nil {
		return false, err
	}
	before := s.TimelineIndex
	if err := s.AdvanceTimeline(r.adapter, world); err != nil {
		return false, err
	}
	return s.TimelineIndex != before, nil
}

// ApplyPendingStage moves Next into Current.
// In-memory mutation only — caller persists via
// WorldState.Save. Returns the post-apply runtime
// slice so the caller doesn't need to re-load the
// graph just to capture the new Current.
func (r *StagingYaml) ApplyPendingStage(world string, runtime staging.StageRuntime) (staging.StageRuntime, error) {
	s, err := r.Load(world, runtime)
	if err != nil {
		return staging.StageRuntime{}, err
	}
	return s.ApplyPending(r.adapter, world)
}

// Compile-time guard.

// its corresponding repository.XxxRepository. The
// matching assertion lives in repository/contracts.go
// (which can import yaml/, but yaml/ cannot import
// the parent package — that would cycle).
