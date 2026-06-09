package files

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
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
	// memoriseSummarizer is the LLM-driven 30-day
	// memorise.md compaction hook. Called from
	// MemoriseCompressWindow whenever a window closes
	// (default: day % 30 == 0, or any wider timeskip).
	memoriseSummarizer tools.MemoriseSummarizer
}

func newMemory(fs *storage.FileStore, log zerolog.Logger, summarizer tools.NPCSummarizer, loreSummarizer tools.LoreSummarizer, memoriseSummarizer tools.MemoriseSummarizer) *Memory {
	return &Memory{
		fs:                 fs,
		log:                log.With().Str("component", "memory").Logger(),
		summarizer:         summarizer,
		loreSummarizer:     loreSummarizer,
		memoriseSummarizer: memoriseSummarizer,
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
	memoriseFull, _ := m.fs.ReadRaw("worlds/" + world + "/memorise.md")
	stateMD, _ := m.fs.ReadRaw("worlds/" + world + "/state.md")

	newBody, err := m.loreSummarizer.SummarizeLore(ctx, world, []byte(raw), []byte(memoriseFull), []byte(stateMD))
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

// memoriseCompressAfterArchive is the post-write hook
// called by State.ArchiveDay. It decides WHICH windows
// to collapse and delegates each one to
// MemoriseCompressWindow. The trigger rules:
//
//   1. If the just-archived day is the last unfilled slot
//      of a window (i.e. a day that brings the file's
//      last single-day entry to a multiple of Window),
//      collapse that window.
//   2. If the just-archived day is a TIMESKIP past one
//      or more whole windows (e.g. last day was д00010
//      and we just archived д00090), collapse every full
//      window in between (д00001-д00030, д00031-д00060,
//      д00061-д00090). This is the "timeskip" case the
//      operator triggered via /leave with a skip_note.
//
// The function never errors out the ArchiveDay call: a
// failed compression just logs a warning and leaves the
// file untouched. The next ArchiveDay re-evaluates.
func (m *Memory) memoriseCompressAfterArchive(ctx context.Context, world string, dayJustArchived int) error {
	if m.memoriseSummarizer == nil {
		m.log.Warn().
			Str("world", world).
			Int("day", dayJustArchived).
			Msg("memorise_compress: no summarizer wired; skipping")
		return nil
	}
	rel := "worlds/" + world + "/memorise.md"
	raw, err := m.fs.ReadRaw(rel)
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil
	}
	entries, err := domain.ParseDays(raw)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	lastSummaryEnd, _, hasSummary := lastMemoriseSummaryLine(raw)
	startFrom := 1
	if hasSummary {
		startFrom = lastSummaryEnd + 1
	}
	endAt := dayJustArchived
	if endAt < startFrom {
		return nil
	}
	// Collect full windows of size Window=30 (default)
	// that fit in [startFrom..endAt]. A window is
	// "full" when both endpoints are within the range.
	// We do NOT compress partial windows — the operator
	// sees the un-archived days as a flat log and the
	// next ArchiveDay will close them.
	const window = 30
	for wEnd := startFrom + window - 1; wEnd <= endAt; wEnd += window {
		wStart := wEnd - window + 1
		if wStart < startFrom {
			wStart = startFrom
		}
		ok, err := m.MemoriseCompressWindow(ctx, world, wStart, wEnd)
		if err != nil {
			return err
		}
		if !ok {
			// Window too thin or summarizer
			// refused — log and continue with the
			// next window. We do not abort the
			// whole cascade: an underfilled
			// early window is not a reason to
			// leave later windows open.
			m.log.Info().
				Str("world", world).
				Int("start", wStart).
				Int("end", wEnd).
				Msg("memorise_compress: window skipped (too thin or no-op)")
		}
	}
	return nil
}

// MemoriseCompressWindow collapses a closed [startDay..endDay]
// window of memorise.md entries into a single distilled line
// "д<start>-д<end>: <10..N sentences of essence>". The
// summarizer receives the WHOLE current memorise.md as
// context so it can dedupe arcs that span the window
// boundary (a 15-day training run that started inside the
// previous window, for example) and stay consistent with
// earlier, already-compressed lines.
//
// The caller is expected to invoke this whenever a window
// closes (the default rule is "day % Window == 0", where
// Window is configurable; for timeskips the caller may
// collapse several windows in one go). This function itself
// collapses exactly ONE window — multi-window collapse is
// the caller's loop.
//
// Returns true if the file was rewritten, false if the
// summarizer returned an empty body (window too thin) or
// a malformed prefix (caller should log and leave the file
// alone). Errors are surfaced for the operator; partial
// writes are not — the rewrite is atomic via WriteRawAtomic.
func (m *Memory) MemoriseCompressWindow(ctx context.Context, world string, startDay, endDay int) (bool, error) {
	if startDay > endDay {
		return false, nil
	}
	rel := "worlds/" + world + "/memorise.md"
	raw, err := m.fs.ReadRaw(rel)
	if err != nil || strings.TrimSpace(raw) == "" {
		return false, nil
	}
	if m.memoriseSummarizer == nil {
		m.log.Warn().
			Str("world", world).
			Int("start", startDay).
			Int("end", endDay).
			Msg("memorise_compress: no summarizer wired; skipping")
		return false, nil
	}

	// Take the last compressed-window end so we know which
	// entries to replace. Window boundaries are inclusive:
	// the line "д00001-д00030: ..." owns slots 1..30.
	// Anything BEFORE the last existing summary line is
	// already finalised and we MUST NOT touch it.
	lastSummaryEnd, _, hasSummary := lastMemoriseSummaryLine(raw)
	from := startDay
	if hasSummary && startDay <= lastSummaryEnd {
		from = lastSummaryEnd + 1
	}
	to := endDay
	if to < from {
		return false, nil
	}

	// Parse entries and collect exactly those whose Number
	// is in [from..to]. The window may have gaps (e.g. days
	// 22 and 25 are missing); we still compress the days
	// that are present.
	entries, err := domain.ParseDays(raw)
	if err != nil {
		return false, err
	}
	var inWindow []domain.DayEntry
	for _, e := range entries {
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
			Msg("memorise_compress: window too thin; skipping")
		return false, nil
	}
	actualStart := inWindow[0].Number
	actualEnd := inWindow[len(inWindow)-1].Number

	body, err := m.memoriseSummarizer.SummarizeMemorise(ctx, world, actualStart, actualEnd, raw)
	if err != nil {
		return false, err
	}
	if len(body) == 0 {
		return false, nil
	}
	cleaned := strings.TrimSpace(string(body))
	// Validate prefix: "д<5digits>-д<5digits>: ". The
	// caller (ArchiveDay → MemoriseCompressWindow) trusts
	// the model output line; a wrong prefix means the LLM
	// drifted and we should not corrupt the file.
	startStr := fmt.Sprintf("д%05d", actualStart)
	endStr := fmt.Sprintf("д%05d", actualEnd)
	prefix := startStr + "-" + endStr + ": "
	if !strings.HasPrefix(cleaned, prefix) {
		m.log.Warn().
			Str("world", world).
			Int("start", actualStart).
			Int("end", actualEnd).
			Str("output", truncateForLog(cleaned, 120)).
			Msg("memorise_compress: bad prefix; skipping")
		return false, nil
	}
	if strings.ContainsAny(cleaned, "\n\r") {
		// The contract is one logical line. Newlines
		// break the day-line parser on next read.
		cleaned = strings.ReplaceAll(cleaned, "\n", " ")
		cleaned = strings.ReplaceAll(cleaned, "\r", " ")
		cleaned = strings.Join(strings.Fields(cleaned), " ")
	}

	// Rewrite: keep the file head (everything up to and
	// including the last summary line), drop entries
	// [actualStart..actualEnd], append the new summary
	// line. The head is computed from the last summary
	// end we found above — we re-scan to be exact.
	newBody, err := rewriteMemoriseAfterCompression(raw, actualStart, actualEnd, cleaned)
	if err != nil {
		return false, err
	}
	if newBody == raw {
		return false, nil
	}
	if err := m.fs.WriteRawAtomic(rel, newBody); err != nil {
		return false, err
	}
	m.log.Info().
		Str("world", world).
		Int("start", actualStart).
		Int("end", actualEnd).
		Int("present_days", len(inWindow)).
		Int("output_chars", len(cleaned)).
		Msg("memorise_compress: compacted")
	return true, nil
}

// lastMemoriseSummaryLine finds the most recent
// "д<start>-д<end>: ..." line in memorise.md and returns
// (end, start, true). If no summary line is present (file
// is a flat log of single days) it returns (0, 0, false).
func lastMemoriseSummaryLine(body string) (end, start int, ok bool) {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "д") {
			continue
		}
		idx := strings.Index(line, "-")
		if idx < 0 {
			continue
		}
		left := strings.TrimPrefix(line[:idx], "д")
		right := strings.TrimPrefix(line[idx+1:], "д")
		colon := strings.Index(right, ":")
		if colon < 0 {
			continue
		}
		s, err1 := strconv.Atoi(strings.TrimSpace(left))
		e, err2 := strconv.Atoi(strings.TrimSpace(right[:colon]))
		if err1 != nil || err2 != nil {
			continue
		}
		if s >= e {
			continue
		}
		return e, s, true
	}
	return 0, 0, false
}

// rewriteMemoriseAfterCompression builds the new file body
// by walking each line: keep non-day lines as-is, keep
// out-of-window day lines (whose Number is outside
// [start..end]), and drop in-window day lines. The new
// summary line is written exactly once, at the position
// of the FIRST dropped day line. The head (everything
// before the first in-window entry) is preserved
// unchanged, so earlier summary lines stay anchored.
func rewriteMemoriseAfterCompression(body string, start, end int, newLine string) (string, error) {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	var out strings.Builder
	wroteSummary := false
	for _, line := range lines {
		n, isDay, _ := parseDayLineNumber(line)
		if !isDay {
			out.WriteString(line)
			out.WriteString("\n")
			continue
		}
		if n >= start && n <= end {
			if !wroteSummary {
				out.WriteString(newLine)
				out.WriteString("\n")
				wroteSummary = true
			}
			continue
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	return strings.TrimRight(out.String(), "\n") + "\n", nil
}

// parseDayLineNumber returns the day Number of a line if
// it matches the "дNNNNN: ..." shape, plus a bool. The
// summary line "д00001-д00030: ..." does NOT match (it has
// a "-" before the colon) — the caller handles those
// separately via lastMemoriseSummaryLine.
func parseDayLineNumber(line string) (n int, ok bool, err error) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "д") {
		return 0, false, nil
	}
	rest := strings.TrimPrefix(t, "д")
	colon := strings.Index(rest, ":")
	if colon < 0 {
		return 0, false, nil
	}
	head := rest[:colon]
	// A summary line's left side stops at "-", e.g.
	// "00001-д00030: ..." — we want to bail on those.
	if strings.Contains(head, "-") {
		return 0, false, nil
	}
	num, perr := strconv.Atoi(strings.TrimSpace(head))
	if perr != nil {
		return 0, false, perr
	}
	return num, true, nil
}

// truncateForLog keeps the slowlog readable when the model
// emits a wall of text without the expected prefix.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// lineCount is a small helper for the lore maintainer.
// Empty / whitespace-only lines are counted the same
// way editors do (every "\n" plus a final 1 for the
// last line). The number is approximate — what matters
// is that the threshold is a real file-length check,
// not a character count.
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
	memoriseFull, _ := m.fs.ReadRaw("worlds/" + world + "/memorise.md")

	newBody, err := m.summarizer.SummarizeNPC(ctx, displayName, world, []byte(body), []byte(memoriseFull))
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
	if err := m.fs.WriteRawAtomic(rel, finalBody); err != nil {
		return displayName, false, err
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

// looksLikeLegacyNPC returns true when the body looks
// like the old markdown NPC file (starts with "# "
// followed by a name). Used by MaintainNPCs to decide
// whether to invoke MigrateFromMarkdown.
func looksLikeLegacyNPC(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "# ") && !strings.HasPrefix(t, "#!")
}
