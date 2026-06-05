package usecase

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rs/zerolog"

	"narrative/internal/adapter/storage"
	"narrative/internal/domain"
)

// NPCManager handles "create on first appearance" and "info isolation"
// checks. It is intentionally narrow — knowledge isolation is a
// decision the calling GM makes, but the manager exposes a helper
// (FilterKnowledge) that trims what an NPC is allowed to know.
type NPCManager struct {
	fs  *storage.FileStore
	log zerolog.Logger
}

func NewNPCManager(fs *storage.FileStore) *NPCManager {
	return NewNPCManagerWithLogger(fs, zerolog.Nop())
}

func NewNPCManagerWithLogger(fs *storage.FileStore, log zerolog.Logger) *NPCManager {
	return &NPCManager{fs: fs, log: log.With().Str("component", "npc_manager").Logger()}
}

type NPCProfile struct {
	DisplayName string
	File        string // latin
	Nicknames   []string
	Temperament string
	Relations   string
	Abilities   string
}

var ErrNPCExists = errors.New("npc file already exists")

func (n *NPCManager) Create(world string, p NPCProfile) error {
	name, err := domain.SanitizeName(p.File)
	if err != nil {
		return err
	}
	rel := "worlds/" + world + "/characters/" + name + ".md"
	if n.fs.Exists(rel) {
		return ErrNPCExists
	}
	if err := n.fs.EnsureDir("worlds/" + world + "/characters"); err != nil {
		return err
	}
	body := "# " + strings.TrimSpace(p.DisplayName) + "\n" +
		"## Темперамент\n" + strings.TrimSpace(p.Temperament) + "\n\n" +
		"## Отношения с ГГ\n" + strings.TrimSpace(p.Relations) + "\n\n" +
		"## Способности\n" + strings.TrimSpace(p.Abilities) + "\n"
	if err := n.fs.WriteRawAtomic(rel, body); err != nil {
		return err
	}
	if err := n.appendRegistry(world, p.DisplayName, name, p.Nicknames); err != nil {
		return err
	}
	n.log.Info().Str("world", world).Str("npc", name).Msg("npc_created")
	return nil
}

func (n *NPCManager) appendRegistry(world, display, file string, nicks []string) error {
	rel := "worlds/" + world + "/characters.md"
	cur, _ := n.fs.ReadRaw(rel)
	if cur != "" && !strings.HasSuffix(cur, "\n") {
		cur += "\n"
	}
	nickStr := strings.Join(nicks, ", ")
	cur += "| " + display + " | characters/" + file + " | " + nickStr + " |\n"
	return n.fs.WriteRawAtomic(rel, cur)
}

// FilterKnowledge strips any line from `candidate` whose marker is NOT
// present in `allowed`. The marker convention is `<!NPC:marker!>` at
// line end. This is the runtime helper for "info isolation" — the GM
// writes a candidate reply with all knowledge it COULD say, and the
// manager drops anything the NPC has not earned.
func FilterKnowledge(candidate, allowed string) string {
	if candidate == "" {
		return ""
	}
	var b strings.Builder
	for _, line := range strings.Split(candidate, "\n") {
		markers := extractMarkers(line)
		if len(markers) == 0 {
			b.WriteString(line)
			b.WriteByte('\n')
			continue
		}
		ok := true
		for _, mk := range markers {
			if !strings.Contains(allowed, mk) {
				ok = false
				break
			}
		}
		if ok {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func extractMarkers(line string) []string {
	var out []string
	low := line
	for {
		start := strings.Index(low, "<!NPC:")
		if start < 0 {
			break
		}
		rest := low[start+len("<!NPC:"):]
		end := strings.Index(rest, "!>")
		if end < 0 {
			break
		}
		out = append(out, "<!NPC:"+rest[:end]+"!>")
		low = rest[end+2:]
	}
	return out
}

// Load returns the NPC's file contents (empty if missing). Callers
// should only invoke this at the start of a scene that features the
// NPC — per skill rule #4.
func (n *NPCManager) Load(world, npc string) (string, error) {
	name, err := domain.SanitizeName(npc)
	if err != nil {
		return "", fmt.Errorf("npc name: %w", err)
	}
	return n.fs.ReadRaw("worlds/" + world + "/characters/" + name + ".md")
}
