package yaml

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/bestxp/narrative-ai-agent/internal/storage"
)

// planKey returns the storage key for plan.md.
func planKey(world string) string {
	return "worlds/" + world + "/plan.md"
}

// PlanYaml is the YAML-backed (actually markdown)
// implementation of PlanRepository. plan.md is a
// 3-5 event bullet list edited by rotate_plan.
type PlanYaml struct {
	store storage.Storage
}

// NewPlanYaml constructs the plan repository.
func NewPlanYaml(store storage.Storage) *PlanYaml {
	return &PlanYaml{store: store}
}

// Load returns the raw plan.md body. Returns "" for a
// missing file (a brand-new world has no plan).
func (r *PlanYaml) Load(world string) (string, error) {
	body, err := r.store.Read(planKey(world))
	if err != nil {
		return "", fmt.Errorf("plan_load: Read failed: %w", err)
	}

	return string(body), nil
}

// Save persists the plan body.
func (r *PlanYaml) Save(world, body string) error {
	if err := r.store.Write(planKey(world), []byte(body)); err != nil {
		return fmt.Errorf("save: %w", err)
	}

	return nil
}

// ReplaceEvents rewrites plan.md with the given
// 3-5 events. The format is `## План\n- event 1\n- ...`.
// The current implementation rejects any count outside
// 3..5 — same constraint as the previous PlanRangeError
// path. Returns nil for an empty events slice (used to
// clear the plan during /leave).
func (r *PlanYaml) ReplaceEvents(_ context.Context, world string, events []string) error {
	if len(events) > 0 && (len(events) < 3 || len(events) > 5) {
		return fmt.Errorf("plan: must contain 3-5 events, got %d", len(events))
	}

	var b strings.Builder
	b.WriteString("## План\n")

	for _, e := range events {
		b.WriteString("- ")
		b.WriteString(strings.TrimSpace(e))
		b.WriteString("\n")
	}

	return r.Save(world, b.String())
}

// loreKey returns the storage key for lore.md.
func loreKey(world string) string {
	return "worlds/" + world + "/lore.md"
}

// LoreYaml is the markdown-backed implementation of
// LoreRepository. lore.md is a list of `## header\n- bullet`
// blocks accumulated over the campaign.
type LoreYaml struct {
	store storage.Storage
}

// NewLoreYaml constructs the lore repository.
func NewLoreYaml(store storage.Storage) *LoreYaml {
	return &LoreYaml{store: store}
}

// Load returns the raw lore.md body.
func (r *LoreYaml) Load(world string) (string, error) {
	body, err := r.store.Read(loreKey(world))
	if err != nil {
		return "", fmt.Errorf("lore_load: Read failed: %w", err)
	}

	return string(body), nil
}

// Save persists the lore body.
func (r *LoreYaml) Save(world, body string) error {
	if err := r.store.Write(loreKey(world), []byte(body)); err != nil {
		return fmt.Errorf("save: %w", err)
	}

	return nil
}

// AppendEntry adds a new `## header\n- bullet` block to
// the end of lore.md. Empty header / bullet is rejected.
func (r *LoreYaml) AppendEntry(world, header, bullet string) error {
	header = strings.TrimSpace(header)

	bullet = strings.TrimSpace(bullet)
	if header == "" || bullet == "" {
		return errors.New("lore: empty header or bullet")
	}

	current, err := r.Load(world)
	if err != nil {
		return err
	}

	if current != "" && !strings.HasSuffix(current, "\n") {
		current += "\n"
	}

	next := current + "\n## " + header + "\n- " + bullet + "\n"

	return r.Save(world, next)
}

// canonKey returns the storage key for canon.md.
func canonKey(world string) string {
	return "worlds/" + world + "/canon.md"
}

// CanonYaml is the read-only markdown-backed
// implementation of CanonRepository. The bot never
// writes canon — only the operator does.
type CanonYaml struct {
	store storage.Storage
}

// NewCanonYaml constructs the canon repository.
func NewCanonYaml(store storage.Storage) *CanonYaml {
	return &CanonYaml{store: store}
}

// Load returns the canon body or "" if missing.
func (r *CanonYaml) Load(world string) (string, error) {
	body, err := r.store.Read(canonKey(world))
	if err != nil {
		return "", fmt.Errorf("canon_load: Read failed: %w", err)
	}

	return string(body), nil
}

// planEventRe validates a single bullet line.
// Empty lines and comments are ignored on parse; on
// Save we emit only `- <text>`.
var planEventRe = regexp.MustCompile(`^\s*-\s+(.+?)\s*$`)

// ParsePlanEvents extracts bullet lines from a plan
// body. Used by tests + the /inspect command.
func ParsePlanEvents(body string) []string {
	var out []string

	for line := range strings.SplitSeq(body, "\n") {
		if m := planEventRe.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}

	return out
}

// its corresponding repository.XxxRepository. The
// matching assertion lives in repository/contracts.go
// (which can import yaml/, but yaml/ cannot import
// the parent package — that would cycle).
