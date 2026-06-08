package files

import (
	"context"
	"strings"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
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
}

func newMemory(fs *storage.FileStore, log zerolog.Logger, summarizer tools.NPCSummarizer, loreSummarizer tools.LoreSummarizer) *Memory {
	return &Memory{
		fs:             fs,
		log:            log.With().Str("component", "memory").Logger(),
		summarizer:     summarizer,
		loreSummarizer: loreSummarizer,
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
	memoriseTail, _ := m.fs.ReadRaw("worlds/" + world + "/memorise.md")
	memoriseTail = tailMemorise(memoriseTail, 20)
	stateMD, _ := m.fs.ReadRaw("worlds/" + world + "/state.md")

	newBody, err := m.loreSummarizer.SummarizeLore(ctx, world, []byte(raw), []byte(memoriseTail), []byte(stateMD))
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
	memoriseTail, _ := m.fs.ReadRaw("worlds/" + world + "/memorise.md")
	// Trim the memorise tail to its last ~20 days so
	// the prompt stays small.
	memoriseTail = tailMemorise(memoriseTail, 20)

	newBody, err := m.summarizer.SummarizeNPC(ctx, displayName, world, []byte(body), []byte(memoriseTail))
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

// tailMemorise is a small inlined copy of the helper in
// gm.go: it returns the last `n` "## День" sections
// from a memorise.md body. We duplicate rather than
// import to keep the file pkg independent of usecase.
func tailMemorise(body string, n int) string {
	if body == "" || n <= 0 {
		return ""
	}
	idx := []int{}
	for i := 0; i < len(body); i++ {
		if i+7 <= len(body) && body[i:i+7] == "## День" {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return body
	}
	if len(idx) <= n {
		return body
	}
	return body[idx[len(idx)-n]:]
}
