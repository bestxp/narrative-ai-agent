package files

import (
	"context"
	"fmt"
	"strings"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/charprofile"
	"github.com/bestxp/narrative-ai-agent/internal/chronicle"
	"github.com/bestxp/narrative-ai-agent/internal/npcprofile"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
)

// Memory is the file-backed implementation of tools.MemoryTool:
// NPC condensation (LLM-driven) + lore.md + character memory.md.
type Memory struct {
	fs  *storage.FileStore
	log zerolog.Logger
	// summarizer is the LLM-driven NPC compaction hook.
	// nil disables MaintainNPCs — the tool will log a
	// warning and skip rather than calling the LLM.
	// Wired at construction in NewFileToolset from
	// cmd/bot/main.go; the production code passes a
	// *usecase.Summarizer (which already implements
	// tools.NPCSummarizer) and the tests pass a stub.
	summarizer tools.NPCSummarizer
	// loreSummarizer is the LLM-driven lore.md compaction
	// hook. Same nil semantics as summarizer. May be a
	// different implementation in tests; in production
	// it is the same *usecase.Summarizer wrapped via
	// a second adapter in cmd/bot/main.go.
	loreSummarizer tools.LoreSummarizer
	// chronicleSummarizer is the LLM-driven
	// chronicle.yaml compaction hook. Called from
	// ChronicleCompressWindow whenever a window closes
	// (default: day % Window == 0, or any wider timeskip).
	chronicleSummarizer tools.ChronicleSummarizer
	// characterMemorySummarizer is the LLM-driven
	// memory.yaml compaction hook for the active
	// character. Called from MaintainCharacterMemory
	// during the end-of-day pass (and from the
	// /maintenance operator path). Same nil
	// semantics as the other summarizers.
	characterMemorySummarizer tools.CharacterMemorySummarizer
}

func newMemory(fs *storage.FileStore, log zerolog.Logger, summarizer tools.NPCSummarizer, loreSummarizer tools.LoreSummarizer, chronicleSummarizer tools.ChronicleSummarizer, characterMemorySummarizer tools.CharacterMemorySummarizer) *Memory {
	return &Memory{
		fs:                        fs,
		log:                       log.With().Str("component", "memory").Logger(),
		summarizer:                summarizer,
		loreSummarizer:            loreSummarizer,
		chronicleSummarizer:       chronicleSummarizer,
		characterMemorySummarizer: characterMemorySummarizer,
	}
}

// AppendLore appends a new deviation entry to lore.md.
func (m *Memory) AppendLore(world, header, bullet string) error {
	rel := "worlds/" + world + "/lore.md"
	cur, _ := m.fs.ReadRaw(rel)
	if cur != "" && !strings.HasSuffix(cur, "\n") {
		cur += "\n"
	}
	cur += "\n## " + header + "\n- " + bullet + "\n"
	return m.fs.WriteRawAtomic(rel, cur)
}

// AppendMemory appends a single first-person line to the active
// character's memory.md.
func (m *Memory) AppendMemory(character, line string) error {
	rel := "characters/" + character + "/memory.md"
	_, err := m.fs.AppendIfMissing(rel, "- "+strings.TrimSpace(line))
	return err
}

// MaintainLore asks the LLM-driven summarizer to
// compact the active world's lore.md when it grows past
// tools.LoreMaintainThreshold (500 lines by default).
// Returns true when the file was rewritten.
//
// Unlike MaintainNPCs (which is per-NPC and the threshold
// is checked per file), lore.md is a single file per
// world, so the flow is simpler: read, count lines,
// if over threshold call summarizer, validate the
// returned body is non-empty and shorter, write.
//
// Per the lore_summary prompt rules, the summarizer
// preserves canon deviations, NPC death events, and
// first NPC appearances — so a successful compression
// keeps the narrative consistent.
func (m *Memory) MaintainLore(ctx context.Context, world string) (bool, error) {
	rel := "worlds/" + world + "/lore.md"
	raw, err := m.fs.ReadRaw(rel)
	if err != nil || strings.TrimSpace(raw) == "" {
		return false, nil
	}
	beforeLines := lineCount(raw)
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
	chronicleFull, _ := m.fs.ReadRaw(m.fs.WorldChronicle(world))
	stateMD, _ := m.fs.ReadRaw("worlds/" + world + "/state.md")

	newBody, err := m.loreSummarizer.SummarizeLore(ctx, world, []byte(raw), []byte(chronicleFull), []byte(stateMD))
	if err != nil {
		return false, err
	}
	if len(newBody) == 0 || len(newBody) >= len(raw) {
		m.log.Info().
			Str("world", world).
			Int("before_lines", beforeLines).
			Int("output_chars", len(newBody)).
			Msg("lore_maintain: summarizer returned equal/empty body; skipping")
		return false, nil
	}
	// Sanity: the result must still look like lore
	// (at least one "## " header). If the LLM stripped
	// every section we are in trouble and would lose
	// the narrative. Bail.
	if !strings.Contains(string(newBody), "## ") {
		m.log.Warn().
			Str("world", world).
			Int("output_chars", len(newBody)).
			Msg("lore_maintain: summarizer returned body with no sections; skipping")
		return false, nil
	}
	if err := m.fs.WriteRawAtomic(rel, string(newBody)); err != nil {
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
// called by State.ArchiveChronicleDay. It decides WHICH
// windows to collapse and delegates each one to
// ChronicleCompressWindow. The trigger rules:
//
//  1. If the just-archived day is the last unfilled slot
//     of a window (i.e. a day that brings the chronicle's
//     last raw day to a multiple of Window), collapse that
//     window.
//  2. If the just-archived day is a TIMESKIP past one or
//     more whole windows (e.g. last day was д00010 and
//     we just archived д00090), collapse every full window
//     in between (д00001-д00030, д00031-д00060,
//     д00061-д00090). This is the "timeskip" case the
//     operator triggered via /leave with a skip_note.
//
// The function never errors out the ArchiveChronicleDay
// call: a failed compression just logs a warning and
// leaves the file untouched. The next ArchiveChronicleDay
// re-evaluates.
func (m *Memory) chronicleCompressAfterArchive(ctx context.Context, world string, dayJustArchived int) error {
	if m.chronicleSummarizer == nil {
		m.log.Warn().
			Str("world", world).
			Int("day", dayJustArchived).
			Msg("chronicle_compress: no summarizer wired; skipping")
		return nil
	}
	rel := m.fs.WorldChronicle(world)
	raw, err := m.fs.ReadRaw(rel)
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil
	}
	c, err := chronicle.Load(raw)
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
	// Collect full windows of size Window=WindowSize (30)
	// that fit in [startFrom..endAt]. A window is "full"
	// when both endpoints are within the range. We do NOT
	// compress partial windows — the operator sees the
	// un-archived days as a flat log and the next
	// ArchiveChronicleDay will close them.
	const window = chronicle.WindowSize
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
			// Window too thin or summarizer
			// refused — log and continue with the
			// next window. We do not abort the
			// whole cascade: an underfilled early
			// window is not a reason to leave later
			// windows open.
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
// [startDay..endDay] window of chronicle.yaml raw-day
// entries into a single Period entry with the summarizer's
// distilled memory text. The summarizer receives the WHOLE
// current chronicle YAML as context so it can dedupe arcs
// that span the window boundary (a 15-day training run that
// started inside the previous window, for example) and stay
// consistent with earlier, already-compressed periods.
//
// The caller is expected to invoke this whenever a window
// closes (the default rule is "day % Window == 0", where
// Window is configurable; for timeskips the caller may
// collapse several windows in one go). This function itself
// collapses exactly ONE window — multi-window collapse is
// the caller's loop.
//
// Returns true if the file was rewritten, false if the
// summarizer returned an empty body (window too thin).
// Errors are surfaced for the operator; partial writes are
// not — the rewrite is atomic via WriteRawAtomic.
func (m *Memory) ChronicleCompressWindow(ctx context.Context, world string, startDay, endDay int) (bool, error) {
	if startDay > endDay {
		return false, nil
	}
	rel := m.fs.WorldChronicle(world)
	raw, err := m.fs.ReadRaw(rel)
	if err != nil || strings.TrimSpace(raw) == "" {
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

	c, err := chronicle.Load(raw)
	if err != nil {
		return false, err
	}

	// Take the last compressed-window end so we know
	// which days are already finalised and MUST NOT be
	// touched. Window boundaries are inclusive: a Period
	// (from=a, to=b) owns slots a..b. Anything BEFORE
	// the last existing period's end is finalised.
	lastEnd, hasPeriod := c.LastPeriodEnd()
	from := startDay
	if hasPeriod && startDay <= lastEnd {
		from = lastEnd + 1
	}
	to := endDay
	if to < from {
		return false, nil
	}

	// Collect exactly those raw days whose Number is
	// in [from..to]. The window may have gaps (e.g.
	// days 22 and 25 are missing); we still compress
	// the days that are present.
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

	body, err := m.chronicleSummarizer.SummarizeChronicle(ctx, world, actualStart, actualEnd, raw)
	if err != nil {
		return false, err
	}
	if len(body) == 0 {
		return false, nil
	}
	memory := strings.TrimSpace(string(body))
	if memory == "" {
		return false, nil
	}
	if err := c.CompressWindow(actualStart, actualEnd, memory); err != nil {
		// Includes "window start inside closed
		// period" — caller logged the structural
		// issue; we surface it for the operator's
		// slowlog trail.
		return false, err
	}
	newBody, err := c.Save()
	if err != nil {
		return false, err
	}
	if err := m.fs.WriteRawAtomic(rel, newBody); err != nil {
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

// lineCount is a small helper for the lore maintainer.
// Empty / whitespace-only lines are counted the same way
// editors do (every "\n" plus a final 1 for the last line).
// The number is approximate — what matters is that the
// threshold is a real file-length check, not a character
// count.
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

// truncateForLog keeps the slowlog readable when the
// summarizer emits a wall of text without the expected
// shape (e.g. an NPC summary that forgot the personal
// memory prefix).
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// MaintainNPCs walks the world's NPC files and asks the
// LLM-driven summarizer to compact any profile whose
// personal_memory list has grown past
// npcprofile.NPCPersonalMemoryLimit (40). Returns the
// list of NPC display names that were actually rewritten.
//
// Per-NPC flow:
//   - Read the file as raw text (YAML in the canonical
//     path, legacy markdown otherwise).
//   - Parse via npcprofile.Load with a fallback to
//     MigrateFromMarkdown. If the file cannot be parsed
//     either way, log a warning and skip.
//   - If personal_memory is at or below the threshold,
//     skip (the model would have nothing to do).
//   - Call summarizer.SummarizeNPC. The result is a
//     new YAML body; we parse it to count personal_memory
//     items and verify the shrink is real (we never
//     replace the file with a longer or unchanged
//     version).
//   - Write back via WriteRawAtomic.
//
// Per-NPC errors are isolated: a bad profile on disk
// does not prevent the next NPC from being compacted.
// The full error list is in slowlog (kind
// "npc.maintain") for the operator to inspect.
func (m *Memory) MaintainNPCs(world string) ([]string, error) {
	dir := "worlds/" + world + "/characters"
	files, err := m.fs.ListChildren(dir)
	if err != nil {
		return nil, err
	}
	var touched []string
	var skipped []string
	var failed []string
	for _, f := range files {
		if !strings.HasSuffix(f, ".yaml") {
			continue
		}
		rel := dir + "/" + f
		slug := strings.TrimSuffix(f, ".yaml")
		name, rewritten, err := m.maintainOne(context.Background(), world, slug, rel)
		switch {
		case err != nil:
			m.log.Warn().Str("world", world).Str("npc", slug).Err(err).Msg("npc.maintain: failed")
			failed = append(failed, slug)
		case rewritten:
			touched = append(touched, name)
		default:
			skipped = append(skipped, name)
		}
	}
	m.log.Info().
		Str("world", world).
		Strs("compacted", touched).
		Strs("skipped", skipped).
		Strs("failed", failed).
		Msg("npc_maintain")
	return touched, nil
}

// maintainOne handles a single NPC file. Returns the
// display name (so the caller can log it), whether the
// file was rewritten, and any error. The function is
// pure: read, summarise, write, return — no global
// state.
func (m *Memory) maintainOne(ctx context.Context, world, slug, rel string) (string, bool, error) {
	raw, err := m.fs.ReadRaw(rel)
	if err != nil || strings.TrimSpace(raw) == "" {
		return slug, false, nil
	}
	// Try YAML first; fall back to the legacy markdown
	// migrator if the file is the old format. We do
	// NOT want the migrator to run on every maintenance
	// call (it would lose the unsummarised-but-yaml
	// form on each save), so we only migrate when the
	// YAML parse truly fails.
	profile, yamlErr := npcprofile.Load(raw)
	if yamlErr != nil {
		if !looksLikeLegacyNPC(raw) {
			return slug, false, yamlErr
		}
		var migErr error
		profile, migErr = npcprofile.MigrateFromMarkdown(raw, slug)
		if migErr != nil {
			return slug, false, migErr
		}
	}
	displayName := profile.DisplayName
	if displayName == "" {
		displayName = slug
	}
	beforeCount := len(profile.PersonalMemory)
	if beforeCount <= npcprofile.NPCPersonalMemoryLimit {
		return displayName, false, nil
	}
	if m.summarizer == nil {
		m.log.Warn().
			Str("world", world).
			Str("npc", displayName).
			Int("personal_memory", beforeCount).
			Msg("npc_maintain: no summarizer wired; skipping")
		return displayName, false, nil
	}
	// Build the per-NPC YAML body we hand to the
	// summarizer. We re-serialise the parsed profile
	// (not the on-disk raw) so the model sees a
	// canonical shape regardless of how the file was
	// stored.
	body, err := profile.Save()
	if err != nil {
		return displayName, false, err
	}
	chronicleFull, _ := m.fs.ReadRaw(m.fs.WorldChronicle(world))

	newBody, err := m.summarizer.SummarizeNPC(ctx, displayName, world, []byte(body), []byte(chronicleFull))
	if err != nil {
		return displayName, false, err
	}
	if len(newBody) == 0 || len(newBody) >= len(body) {
		m.log.Info().
			Str("npc", displayName).
			Int("before", beforeCount).
			Int("output_chars", len(newBody)).
			Msg("npc_maintain: summarizer returned equal/empty body; skipping")
		return displayName, false, nil
	}
	// Validate the new body parses. If it does not, we
	// keep the original on disk and log a warning.
	newProfile, parseErr := npcprofile.Load(string(newBody))
	if parseErr != nil {
		m.log.Warn().
			Str("npc", displayName).
			Err(parseErr).
			Int("output_chars", len(newBody)).
			Msg("npc_maintain: summarizer returned invalid YAML; skipping")
		return displayName, false, nil
	}
	// Sanity: the new personal_memory must be SHORTER
	// (we never replace with a larger one). If the
	// model hallucinated more facts, drop the write.
	if len(newProfile.PersonalMemory) >= beforeCount {
		m.log.Warn().
			Str("npc", displayName).
			Int("before", beforeCount).
			Int("after", len(newProfile.PersonalMemory)).
			Msg("npc_maintain: summarizer did not shrink personal_memory; skipping")
		return displayName, false, nil
	}
	finalBody, err := newProfile.Save()
	if err != nil {
		return displayName, false, err
	}
	// Backup the current file BEFORE rewriting. If
	// anything goes wrong between WriteRawAtomic and the
	// post-write validation, the .bak contains the
	// pre-rewrite bytes — operator can `mv` it back.
	bakRel := rel + ".bak"
	if oldBody, err := m.fs.ReadRaw(rel); err == nil && oldBody != "" {
		if err := m.fs.WriteRawAtomic(bakRel, oldBody); err != nil {
			m.log.Warn().
				Str("npc", displayName).
				Err(err).
				Msg("npc_maintain: backup write failed; proceeding anyway")
		}
	}
	if err := m.fs.WriteRawAtomic(rel, finalBody); err != nil {
		return displayName, false, err
	}
	// Post-write sanity: re-read the just-written file
	// and confirm it parses. A disk-level error between
	// WriteRawAtomic and the next read (e.g. fs unmount)
	// would leave us with a corrupted profile on disk
	// that fails to load on the next turn. The .bak
	// above is the recovery path.
	if reread, rerr := m.fs.ReadRaw(rel); rerr != nil || reread != finalBody {
		m.log.Warn().
			Str("npc", displayName).
			Str("read", truncateForLog(reread, 80)).
			Str("expected", truncateForLog(string(finalBody), 80)).
			Msg("npc_maintain: post-write read mismatch; .bak preserves the original")
	}
	m.log.Info().
		Str("world", world).
		Str("npc", displayName).
		Int("before", beforeCount).
		Int("after", len(newProfile.PersonalMemory)).
		Int("output_chars", len(finalBody)).
		Msg("npc_maintain: compacted")
	return displayName, true, nil
}

// MaintainCharacterMemory is the end-of-day (and
// /maintenance) hook for the active character's
// memory.yaml. It runs AFTER the NPC maintenance
// pass — the player is reading the day's summary,
// the slowlog already has the per-NPC events, a
// 30-60s pause for one extra LLM call is
// acceptable.
//
// Threshold: tools.CharacterMemoryMaintainBytes
// (4KB). Below the threshold the file is left
// alone — the model has nothing to do, and the
// cost of an LLM call is not worth a marginal
// shrink. Above the threshold the summarizer is
// asked to defragment, dedup, and refile the
// memory into the 4 canonical sections.
//
// Safety net (mirrors MaintainNPCs):
//
//   - result must parse as charprofile.Memory
//   - result must be strictly shorter than the
//     input (the LLM never grows a maintenance
//     pass — the only valid direction is shrink)
//   - result must contain at least one section
//     (no full wipe)
//   - the pre-rewrite body is preserved as
//     `<rel>.bak` so the operator can `mv` it
//     back on a regression
//
// Returns true when the file was rewritten.
func (m *Memory) MaintainCharacterMemory(ctx context.Context, world, character string) (bool, error) {
	if strings.TrimSpace(character) == "" {
		return false, nil
	}
	rel := "characters/" + character + "/memory.yaml"
	raw, err := m.fs.ReadRaw(rel)
	if err != nil || strings.TrimSpace(raw) == "" {
		return false, nil
	}
	beforeBytes := len(raw)
	if beforeBytes <= tools.CharacterMemoryMaintainBytes {
		m.log.Debug().
			Str("world", world).
			Str("character", character).
			Int("bytes", beforeBytes).
			Int("threshold", tools.CharacterMemoryMaintainBytes).
			Msg("character_memory_maintain: under threshold; skipping")
		return false, nil
	}
	if m.characterMemorySummarizer == nil {
		m.log.Warn().
			Str("world", world).
			Str("character", character).
			Int("bytes", beforeBytes).
			Msg("character_memory_maintain: no summarizer wired; skipping")
		return false, nil
	}
	// Resolve display name from SOUL.yaml when
	// available — the LLM prompt expects the
	// human name, not the directory slug. SOUL
	// may be missing in edge cases (operator
	// pre-migration) — fall back to the slug.
	displayName := character
	if soulBody, err := m.fs.ReadRaw("characters/" + character + "/SOUL.yaml"); err == nil {
		var s charprofile.Soul
		if yamlErr := yaml.Unmarshal([]byte(soulBody), &s); yamlErr == nil {
			if t := strings.TrimSpace(s.Name); t != "" {
				displayName = t
			}
		}
	}
	chronicleFull, _ := m.fs.ReadRaw(m.fs.WorldChronicle(world))

	newBody, err := m.characterMemorySummarizer.SummarizeCharacterMemory(ctx, world, displayName, []byte(raw), []byte(chronicleFull))
	if err != nil {
		return false, fmt.Errorf("character_memory_maintain: summarizer call: %w", err)
	}
	if len(newBody) == 0 {
		m.log.Info().
			Str("character", character).
			Int("bytes", beforeBytes).
			Msg("character_memory_maintain: summarizer returned empty body; skipping")
		return false, nil
	}
	if len(newBody) >= beforeBytes {
		m.log.Info().
			Str("character", character).
			Int("before", beforeBytes).
			Int("output_chars", len(newBody)).
			Msg("character_memory_maintain: summarizer returned equal/larger body; skipping")
		return false, nil
	}
	// Validate the new body parses as Memory. A
	// malformed reply from the LLM must NOT corrupt
	// the on-disk file — bail before WriteRawAtomic.
	var newMem charprofile.Memory
	if yamlErr := yaml.Unmarshal(newBody, &newMem); yamlErr != nil {
		m.log.Warn().
			Str("character", character).
			Err(yamlErr).
			Int("output_chars", len(newBody)).
			Msg("character_memory_maintain: summarizer returned invalid YAML; skipping")
		return false, nil
	}
	if len(newMem.Data) == 0 {
		m.log.Warn().
			Str("character", character).
			Int("output_chars", len(newBody)).
			Msg("character_memory_maintain: summarizer returned body with no sections; skipping")
		return false, nil
	}
	// Re-serialise through the typed model so the
	// on-disk file is canonical (alphabetical
	// sections, stable field order). The LLM's
	// free-form output is normalised here.
	finalBody, err := newMem.Save()
	if err != nil {
		return false, fmt.Errorf("character_memory_maintain: re-serialise: %w", err)
	}
	// Snapshot the pre-rewrite body so a
	// post-write corruption can be reversed.
	bakRel := rel + ".bak"
	if err := m.fs.WriteRawAtomic(bakRel, raw); err != nil {
		m.log.Warn().
			Str("character", character).
			Err(err).
			Msg("character_memory_maintain: backup write failed; proceeding anyway")
	}
	if err := m.fs.WriteRawAtomic(rel, finalBody); err != nil {
		return false, err
	}
	// Post-write sanity: re-read and confirm
	// round-trip is stable. A torn write would
	// leave us with a non-parseable file on the
	// next turn; the .bak above is the recovery
	// path.
	if reread, rerr := m.fs.ReadRaw(rel); rerr != nil || reread != finalBody {
		m.log.Warn().
			Str("character", character).
			Str("read", truncateForLog(reread, 80)).
			Str("expected", truncateForLog(string(finalBody), 80)).
			Msg("character_memory_maintain: post-write read mismatch; .bak preserves the original")
	}
	// Count the values across all sections for the
	// slowlog line — operators want to see "30
	// values → 18 values" at a glance.
	beforeValues := countValuesInMemoryYAML(raw)
	afterValues := countValuesInMemoryYAML(finalBody)
	m.log.Info().
		Str("world", world).
		Str("character", character).
		Str("display_name", displayName).
		Int("before_bytes", beforeBytes).
		Int("after_bytes", len(finalBody)).
		Int("before_values", beforeValues).
		Int("after_values", afterValues).
		Int("sections", len(newMem.Data)).
		Msg("character_memory_maintain: compacted")
	return true, nil
}

// countValuesInMemoryYAML is a best-effort
// counter that walks the YAML body and sums
// the `values:` lists. Used purely for the
// slowlog "30 → 18 values" line — it does not
// need to be 100% accurate (we are not making
// decisions on it), so we just count lines
// starting with "- " under any `values:` key.
// Cheap and dependency-free.
func countValuesInMemoryYAML(body string) int {
	count := 0
	lines := strings.Split(body, "\n")
	inValues := false
	for _, l := range lines {
		trimmed := strings.TrimRight(l, " \t")
		switch {
		case strings.HasSuffix(trimmed, "values:"):
			inValues = true
		case strings.HasPrefix(trimmed, "    - ") || strings.HasPrefix(trimmed, "  - "):
			if inValues {
				count++
			}
		case strings.HasPrefix(trimmed, "- section:") || strings.HasPrefix(trimmed, "data:"):
			inValues = false
		case strings.HasPrefix(trimmed, "name:") || strings.HasPrefix(trimmed, "soul:"):
			// top-level SOUL fields, not in data block
			inValues = false
		}
	}
	return count
}

// looksLikeLegacyNPC returns true when the body looks
// like the old markdown NPC file (starts with "# "
// followed by a name). Used by MaintainNPCs to decide
// whether to invoke MigrateFromMarkdown.
func looksLikeLegacyNPC(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "# ") && !strings.HasPrefix(t, "#!")
}
