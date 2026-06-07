package files

import (
	"strings"

	"github.com/rs/zerolog"

	"narrative/internal/adapter/storage"
)

// Memory is the file-backed implementation of tools.MemoryTool:
// NPC condensation + lore.md + character memory.md.
type Memory struct {
	fs  *storage.FileStore
	log zerolog.Logger
}

func newMemory(fs *storage.FileStore, log zerolog.Logger) *Memory {
	return &Memory{fs: fs, log: log.With().Str("component", "memory").Logger()}
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

// CompactNPCs walks characters/*.md and condenses any file longer than
// the configured threshold. The actual condensing is delegated to
// CompactNPCBody (see npc.go in this package).
func (m *Memory) CompactNPCs(world string) ([]string, error) {
	dir := "worlds/" + world + "/characters"
	files, err := m.fs.ListChildren(dir)
	if err != nil {
		return nil, err
	}
	var touched []string
	for _, f := range files {
		if !strings.HasSuffix(f, ".md") {
			continue
		}
		rel := dir + "/" + f
		if m.fs.CountLines(rel) <= NPCCompactLineThreshold {
			continue
		}
		body, _ := m.fs.ReadRaw(rel)
		condensed := CompactNPCBody(body)
		if err := m.fs.WriteRawAtomic(rel, condensed); err != nil {
			return touched, err
		}
		touched = append(touched, strings.TrimSuffix(f, ".md"))
	}
	m.log.Info().Str("world", world).Strs("npcs", touched).Msg("compact_npcs")
	return touched, nil
}
