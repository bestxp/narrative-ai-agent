package files

import (
	"context"
	"fmt"
	"strings"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/charprofile"
	"github.com/bestxp/narrative-ai-agent/internal/chronicle"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/limits"
	"github.com/bestxp/narrative-ai-agent/internal/npcprofile"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
)

// Memory is the repository-backed implementation of
// tools.MemoryTool: NPC condensation (LLM-driven),
// lore compaction, character memory defragmentation,
// and chronicle window compression.
//
// All persistent reads and writes go through the
// *api.Repositories bundle. The LLM hooks
// (summarizer, loreSummarizer, chronicleSummarizer,
// characterMemorySummarizer) are injected at
// construction time and called by the methods that
// actually need LLM assistance (MaintainNPCs,
// MaintainLore, ChronicleCompressWindow,
// MaintainCharacterMemory). Methods that only touch
// local file data (AppendLore, AppendMemory) do NOT
// take an LLM dependency.
type Memory struct {
	repos *api.Repositories
	log   zerolog.Logger
	// LLM hooks. nil disables the corresponding LLM
	// path; the method logs and skips in that case.
	// Wired in cmd/bot/main.go via the Summarizer
	// adapter. Tests pass nil.
	summarizer                tools.NPCSummarizer
	loreSummarizer            tools.LoreSummarizer
	chronicleSummarizer       tools.ChronicleSummarizer
	characterMemorySummarizer tools.CharacterMemorySummarizer
}

func newMemory(log zerolog.Logger, summarizer tools.NPCSummarizer, loreSummarizer tools.LoreSummarizer, chronicleSummarizer tools.ChronicleSummarizer, characterMemorySummarizer tools.CharacterMemorySummarizer, repos *api.Repositories) *Memory {
	return &Memory{
		repos:                     repos,
		log:                       log.With().Str("component", "memory").Logger(),
		summarizer:                summarizer,
		loreSummarizer:            loreSummarizer,
		chronicleSummarizer:       chronicleSummarizer,
		characterMemorySummarizer: characterMemorySummarizer,
	}
}

// AppendLore adds a new `## header\n- bullet` block to
// the world's lore file via repos.Lore.
func (m *Memory) AppendLore(world, header, bullet string) error {
	if err := m.repos.Lore.AppendEntry(world, header, bullet); err != nil {
		return err
	}
	m.log.Info().Str("world", world).Str("header", header).Msg("append_lore")
	return nil
}

// AppendMemory is a no-op here — the legacy per-character
// memory.md journal was retired in the h5 refactor.
// Kept for interface compatibility; calls
// CharacterMemoryRepository.Save with the current state
// unchanged (a no-op write). Operators with active legacy
// callers should migrate to update_character instead.
func (m *Memory) AppendMemory(character, line string) error {
	// Best-effort: read the existing memory, no-op. This
	// keeps the interface stable without resurrecting the
	// old markdown journal.
	_, _ = m.repos.Memory.Load(character)
	m.log.Warn().Str("character", character).Str("line", line).Msg("append_memory is a no-op post-h5 refactor; use update_character")
	return nil
}

// MaintainLore compacts lore.md via the LLM summarizer
// when it grows past LoreMaintainThreshold lines.
// Reads + writes go through repos.Lore.
func (m *Memory) MaintainLore(ctx context.Context, world string) (bool, error) {
	body, err := m.repos.Lore.Load(world)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(body) == "" {
		return false, nil
	}
	beforeLines := lineCount(body)
	if beforeLines <= tools.LoreMaintainThreshold {
		m.log.Debug().
			Str("world", world).
			Int("lines", beforeLines).
			Int("threshold", tools.LoreMaintainThreshold).
			Msg("lore_maintain: under threshold; skipping")
		return false, nil
	}
	if m.loreSummarizer == nil {
		m.log.Warn().
			Str("world", world).
			Int("lines", beforeLines).
			Msg("lore_maintain: no summarizer wired; skipping")
		return false, nil
	}
	newBody, err := m.loreSummarizer.SummarizeLore(ctx, world, []byte(body), nil, nil)
	if err != nil {
		return false, err
	}
	if len(newBody) == 0 || len(newBody) >= len(body) {
		m.log.Info().
			Str("world", world).
			Int("before_lines", beforeLines).
			Int("output_chars", len(newBody)).
			Msg("lore_maintain: summarizer returned equal/empty body; skipping")
		return false, nil
	}
	if !strings.Contains(string(newBody), "## ") {
		m.log.Warn().
			Str("world", world).
			Int("output_chars", len(newBody)).
			Msg("lore_maintain: summarizer returned body with no sections; skipping")
		return false, nil
	}
	if err := m.repos.Lore.Save(world, string(newBody)); err != nil {
		return false, err
	}
	afterLines := lineCount(string(newBody))
	m.log.Info().
		Str("world", world).
		Int("before_lines", beforeLines).
		Int("after_lines", afterLines).
		Int("output_chars", len(newBody)).
		Msg("lore_maintain: compacted")
	return true, nil
}

// chronicleCompressAfterArchive is the post-write hook
// called by State.ArchiveChronicleDay. Decides WHICH
// windows to collapse and delegates each one to
// ChronicleCompressWindow.
//
// Window rule (Window=limits.MemoriseWindowDays=30):
// the hook is called when the just-archived day
// closes a window (day % Window == 0) or for any
// wider timeskip. Wider timeskips are handled by
// ChronicleCompressWindow itself.
func (m *Memory) chronicleCompressAfterArchive(ctx context.Context, world string, dayJustArchived int) error {
	if m.chronicleSummarizer == nil {
		m.log.Warn().
			Str("world", world).
			Int("day", dayJustArchived).
			Msg("chronicle_compress: no summarizer wired; skipping")
		return nil
	}
	c, err := m.repos.Chronicle.Load(world)
	if err != nil {
		m.log.Warn().Err(err).Str("world", world).Msg("chronicle_compress: parse failed; skipping")
		return nil
	}
	if len(c.Days) == 0 {
		return nil
	}
	lastEnd, hasPeriod := c.LastPeriodEnd()
	startFrom := 1
	if hasPeriod {
		startFrom = lastEnd + 1
	}
	endAt := dayJustArchived
	if endAt < startFrom {
		return nil
	}
	window := chronicle.WindowSize
	for wEnd := startFrom + window - 1; wEnd <= endAt; wEnd += window {
		wStart := wEnd - window + 1
		if wStart < startFrom {
			wStart = startFrom
		}
		ok, err := m.ChronicleCompressWindow(ctx, world, wStart, wEnd)
		if err != nil {
			return err
		}
		if !ok {
			m.log.Info().
				Str("world", world).
				Int("start", wStart).
				Int("end", wEnd).
				Msg("chronicle_compress: window skipped (too thin or no-op)")
		}
	}
	return nil
}

// ChronicleCompressWindow collapses a closed
// [startDay..endDay] window into a single Period entry.
// The summarizer receives the whole chronicle YAML as
// context so it can dedupe arcs that span the window
// boundary. See research_repository_pattern.md for the
// compression-rule rationale.
func (m *Memory) ChronicleCompressWindow(ctx context.Context, world string, startDay, endDay int) (bool, error) {
	if startDay > endDay {
		return false, nil
	}
	if m.chronicleSummarizer == nil {
		m.log.Warn().
			Str("world", world).
			Int("start", startDay).
			Int("end", endDay).
			Msg("chronicle_compress: no summarizer wired; skipping")
		return false, nil
	}
	c, err := m.repos.Chronicle.Load(world)
	if err != nil {
		return false, err
	}
	lastEnd, hasPeriod := c.LastPeriodEnd()
	from := startDay
	if hasPeriod && startDay <= lastEnd {
		from = lastEnd + 1
	}
	to := endDay
	if to < from {
		return false, nil
	}
	var inWindow []chronicle.DayEntry
	for _, e := range c.SortedDays() {
		if e.Number >= from && e.Number <= to {
			inWindow = append(inWindow, e)
		}
	}
	if len(inWindow) < 2 {
		m.log.Info().
			Str("world", world).
			Int("start", from).
			Int("end", to).
			Int("present", len(inWindow)).
			Msg("chronicle_compress: window too thin; skipping")
		return false, nil
	}
	actualStart := inWindow[0].Number
	actualEnd := inWindow[len(inWindow)-1].Number
	body, err := m.repos.Chronicle.Load(world)
	if err != nil {
		return false, err
	}
	rawBytes, err := yamlOrBytes(body)
	if err != nil {
		return false, err
	}
	summarizerBody, err := m.chronicleSummarizer.SummarizeChronicle(ctx, world, actualStart, actualEnd, string(rawBytes))
	if err != nil {
		return false, err
	}
	if len(summarizerBody) == 0 {
		return false, nil
	}
	memory := strings.TrimSpace(string(summarizerBody))
	if memory == "" {
		return false, nil
	}
	if err := c.CompressWindow(actualStart, actualEnd, memory); err != nil {
		return false, err
	}
	if err := m.repos.Chronicle.Save(world, c); err != nil {
		return false, err
	}
	m.log.Info().
		Str("world", world).
		Int("start", actualStart).
		Int("end", actualEnd).
		Int("present_days", len(inWindow)).
		Int("output_chars", len(memory)).
		Msg("chronicle_compress: compacted")
	return true, nil
}

// yamlOrBytes is a small helper that returns the
// canonical string form of a chronicle. The
// ChronicleYaml.Load already returns a Chronicle, so
// callers that need the on-disk YAML form for the
// LLM context re-load via the underlying storage.
// This helper exists so the call site is readable.
func yamlOrBytes(c chronicle.Chronicle) ([]byte, error) {
	body, err := c.Save()
	if err != nil {
		return nil, err
	}
	return []byte(body), nil
}

// lineCount is a small helper for the lore maintainer.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// MaintainNPCs walks the world's NPC files and asks
// the LLM-driven summarizer to compact any profile
// whose personal_memory has grown past npcprofile's
// NPCPersonalMemoryLimit. Reads + writes go through
// repos.NPCProfile.
func (m *Memory) MaintainNPCs(world string) ([]string, error) {
	// MaintainNPCs operates on per-NPC files under
	// worlds/<w>/characters/. The repository layer
	// exposes a Load/Save per slug but not a List of
	// slugs — we read the directory listing through
	// the underlying Storage. For tests this is a
	// YAML storage; in production it's the same.
	// We delegate to a helper that walks the dir.
	slugs, err := m.listNPCSlugs(world)
	if err != nil {
		return nil, err
	}
	var touched []string
	for _, slug := range slugs {
		ok, err := m.maintainOne(world, slug)
		if err != nil {
			m.log.Warn().Err(err).Str("slug", slug).Msg("maintain_npcs: per-NPC error; continuing")
			continue
		}
		if ok {
			touched = append(touched, slug)
		}
	}
	return touched, nil
}

// listNPCSlugs returns the per-NPC filenames (without
// .yaml) under worlds/<w>/characters/. The list is
// the directory entries; the repository Load() per
// slug handles the rest.
func (m *Memory) listNPCSlugs(world string) ([]string, error) {
	// The repository pattern separates "read a key"
	// from "list children". The yaml ChronicleYaml
	// has both via its storage. For listing we use
	// the underlying storage through a small seam.
	return m.repos.NPCProfile.ListSlugs(world)
}

// maintainOne runs the LLM summarizer on a single
// NPC profile whose personal_memory is over the
// threshold. Returns true if the file changed.
func (m *Memory) maintainOne(world, slug string) (bool, error) {
	profile, err := m.repos.NPCProfile.Load(world, slug)
	if err != nil {
		return false, err
	}
	if len(profile.PersonalMemory) < npcprofile.NPCPersonalMemoryLimit {
		return false, nil
	}
	if m.summarizer == nil {
		m.log.Warn().
			Str("world", world).
			Str("slug", slug).
			Msg("maintain_npcs: no summarizer wired; skipping")
		return false, nil
	}
	body, err := profile.Save()
	if err != nil {
		return false, err
	}
	newBody, err := m.summarizer.SummarizeNPC(context.Background(), profile.DisplayName, world, []byte(body), nil)
	if err != nil {
		return false, err
	}
	if len(newBody) == 0 || len(newBody) >= len(body) {
		m.log.Info().
			Str("world", world).
			Str("slug", slug).
			Int("before", len(profile.PersonalMemory)).
			Int("output_chars", len(newBody)).
			Msg("maintain_npcs: summarizer returned equal/empty body; skipping")
		return false, nil
	}
	newProfile, err := npcprofile.Load(string(newBody))
	if err != nil {
		return false, fmt.Errorf("maintain_npcs: LLM response not parseable YAML: %w", err)
	}
	// Verify the shrink is real.
	if len(newProfile.PersonalMemory) >= len(profile.PersonalMemory) {
		m.log.Warn().
			Str("world", world).
			Str("slug", slug).
			Int("before", len(profile.PersonalMemory)).
			Int("after", len(newProfile.PersonalMemory)).
			Msg("maintain_npcs: no shrinkage; skipping")
		return false, nil
	}
	if err := m.repos.NPCProfile.Save(world, slug, newProfile); err != nil {
		return false, err
	}
	m.log.Info().
		Str("world", world).
		Str("slug", slug).
		Str("npc", profile.DisplayName).
		Int("before", len(profile.PersonalMemory)).
		Int("after", len(newProfile.PersonalMemory)).
		Int("output_chars", len(newBody)).
		Msg("maintain_npcs: compacted")
	return true, nil
}

// MaintainCharacterMemory defragments the active
// character's memory.yaml when it grows past the
// threshold. Reads + writes go through
// repos.CharacterMemory.
func (m *Memory) MaintainCharacterMemory(ctx context.Context, world, character string) (bool, error) {
	if m.characterMemorySummarizer == nil {
		m.log.Warn().
			Str("world", world).
			Str("character", character).
			Msg("character_memory_maintain: no summarizer wired; skipping")
		return false, nil
	}
	mem, err := m.repos.Memory.Load(character)
	if err != nil {
		return false, err
	}
	body, err := mem.Save()
	if err != nil {
		return false, err
	}
	if len(body) < tools.CharacterMemoryMaintainBytes {
		m.log.Debug().
			Str("world", world).
			Str("character", character).
			Int("bytes", len(body)).
			Int("threshold", tools.CharacterMemoryMaintainBytes).
			Msg("character_memory_maintain: under threshold; skipping")
		return false, nil
	}
	chronicleRaw, _ := m.repos.Chronicle.Load(world)
	chronicleBody, _ := yamlMarshalChronicle(chronicleRaw)
	newBody, err := m.characterMemorySummarizer.SummarizeCharacterMemory(ctx, world, character, []byte(body), []byte(chronicleBody))
	if err != nil {
		return false, err
	}
	if len(newBody) == 0 || len(newBody) >= len(body) {
		m.log.Info().
			Str("world", world).
			Str("character", character).
			Int("before", len(body)).
			Int("output_chars", len(newBody)).
			Msg("character_memory_maintain: summarizer returned equal/empty body; skipping")
		return false, nil
	}
	newMem, err := charprofile.LoadMemory(string(newBody))
	if err != nil {
		return false, fmt.Errorf("character_memory_maintain: LLM response not parseable YAML: %w", err)
	}
	if err := m.repos.Memory.Save(character, newMem); err != nil {
		return false, err
	}
	m.log.Info().
		Str("world", world).
		Str("character", character).
		Int("before", len(body)).
		Int("after", len(newBody)).
		Msg("character_memory_maintain: compacted")
	return true, nil
}

// yamlMarshalChronicle serialises a Chronicle to its
// on-disk YAML form. Used as context for the LLM
// summarizer.
func yamlMarshalChronicle(c chronicle.Chronicle) (string, error) {
	return c.Save()
}

// Reference imports that are part of the public
// interface but used only in edge-case paths above.
var _ = domain.StateSnapshot{}
var _ = limits.MemoriseWindowDays
