package api

import (
	"context"

	"github.com/bestxp/narrative-ai-agent/internal/domain"
)

// WorldStateRepository owns the world's "here and now"
// snapshot (state.md). The snapshot is markdown with a
// fixed block layout: WorldState type from domain
// captures the parsed shape.
//
// Methods take a world slug and return the parsed
// snapshot. The raw markdown (with `### headers` and
// `## sections`) is the storage format; the YAML/JSON
// backend would store it as a single row.
type WorldStateRepository interface {
	Load(world string) (domain.StateSnapshot, error)
	Save(world string, s domain.StateSnapshot) error
	// AppendEvent is the read-modify-write helper for the
	// "хронология дня" log. Each ArchiveDay pass clears
	// the events; each UpdateState appends one. The
	// repository takes the responsibility of atomicity
	// (transaction for SQL, temp+rename for YAML).
	AppendEvent(world, event string) error
	// EnsureExists writes a minimal placeholder if the
	// world has no state file yet. Used by /launch.
	EnsureExists(world string, day int, inFlight bool) error
}

// LoreRepository owns the world's deviations log
// (lore.md). Markdown format with `## header` + `- bullet`
// sections.
type LoreRepository interface {
	Load(world string) (string, error)
	Save(world, body string) error
	// AppendEntry adds a new `## header\n- bullet` block
	// to the end of lore. Used by append_lore tool.
	AppendEntry(world, header, bullet string) error
}

// CanonRepository owns canon.md (operator-owned, the
// bot only reads it). Write is intentionally NOT in
// the interface — canon is operator territory. Load
// returns "" when the file does not exist.
type CanonRepository interface {
	Load(world string) (string, error)
}

// PlanRepository owns plan.md (3-5 upcoming events).
type PlanRepository interface {
	Load(world string) (string, error)
	Save(world, body string) error
	// ReplaceEvents atomically rewrites the events list.
	// Used by rotate_plan. Takes context so the caller
	// can pass a deadline.
	ReplaceEvents(ctx context.Context, world string, events []string) error
}
