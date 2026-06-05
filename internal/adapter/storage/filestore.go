package storage

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rs/zerolog"
)

// FileStore is a tiny abstraction over the on-disk game-data tree.
// It is the ONLY place that touches files directly; usecases operate on
// the methods exposed here.
type FileStore struct {
	root string
	log zerolog.Logger
}

// NewFileStore is the production constructor. Pass logging.Discard()
// for the logger to silence per-write events (e.g. in tests).
func NewFileStore(root string) (*FileStore, error) {
	return NewFileStoreWithLogger(root, zerolog.Nop())
}

func NewFileStoreWithLogger(root string, log zerolog.Logger) (*FileStore, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("abs root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir root: %w", err)
	}
	return &FileStore{root: abs, log: log.With().Str("component", "filestore").Logger()}, nil
}

func (f *FileStore) Root() string { return f.root }

// InfoFile is the canonical on-disk name of the multiverse
// registry. It used to be "info.md" with a hybrid YAML+markdown
// payload, but anchors were duplicated against the system prompt
// and the markdown tail had nothing the LLM would consume. Pure
// YAML keeps the storage layer aligned with the rest of the bot's
// configuration (config.yaml, plan.md is still markdown because
// it's a freeform human checklist for the GM).
const InfoFile = "info.yaml"

func (f *FileStore) InfoYAMLPath() string             { return filepath.Join(f.root, InfoFile) }
func (f *FileStore) CharacterDir(name string) string  { return filepath.Join(f.root, "characters", name) }
func (f *FileStore) WorldDir(name string) string       { return filepath.Join(f.root, "worlds", name) }
func (f *FileStore) WorldState(name string) string     { return f.WorldDir(name) + string(filepath.Separator) + "state.md" }
func (f *FileStore) WorldPlan(name string) string      { return f.WorldDir(name) + string(filepath.Separator) + "plan.md" }
func (f *FileStore) WorldMemorise(name string) string  { return f.WorldDir(name) + string(filepath.Separator) + "memorise.md" }
func (f *FileStore) WorldLore(name string) string      { return f.WorldDir(name) + string(filepath.Separator) + "lore.md" }
func (f *FileStore) WorldCanon(name string) string     { return f.WorldDir(name) + string(filepath.Separator) + "canon.md" }
func (f *FileStore) WorldNPCsDir(name string) string   { return f.WorldDir(name) + string(filepath.Separator) + "characters" }
func (f *FileStore) WorldNPCRegistry(name string) string {
	return f.WorldDir(name) + string(filepath.Separator) + "characters.md"
}
func (f *FileStore) WorldNPCFile(world, npc string) string {
	return f.WorldNPCsDir(world) + string(filepath.Separator) + npc + ".md"
}
func (f *FileStore) CharacterSOUL(name string) string  { return f.CharacterDir(name) + string(filepath.Separator) + "SOUL.md" }
func (f *FileStore) CharacterSKILL(name string) string { return f.CharacterDir(name) + string(filepath.Separator) + "SKILL.md" }
func (f *FileStore) CharacterMemory(name string) string {
	return f.CharacterDir(name) + string(filepath.Separator) + "memory.md"
}

// Exists reports whether path (relative to root) is present.
func (f *FileStore) Exists(rel string) bool {
	_, err := os.Stat(filepath.Join(f.root, rel))
	return err == nil
}

// ReadRaw returns the raw bytes of rel path. Empty string if missing.
func (f *FileStore) ReadRaw(rel string) (string, error) {
	p := filepath.Join(f.root, rel)
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// WriteRaw is the ONLY way usecases may overwrite a file. It is the safe
// counterpart of `read_file → write_file` in the skill and is guaranteed
// to strip any line-number prefix pollution before persisting.
func (f *FileStore) WriteRaw(rel, content string) error {
	clean := stripIndexPollution(content)
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f.log.Debug().Str("path", rel).Int("bytes", len(clean)).Msg("write_raw")
	return os.WriteFile(p, []byte(clean), 0o644)
}

// WriteRawAtomic writes via temp file + rename to avoid torn writes.
func (f *FileStore) WriteRawAtomic(rel, content string) error {
	clean := stripIndexPollution(content)
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(clean), 0o644); err != nil {
		return err
	}
	f.log.Debug().Str("path", rel).Int("bytes", len(clean)).Msg("write_atomic")
	return os.Rename(tmp, p)
}

// Patch replaces old with new exactly once. Returns ErrPatchNotFound
// if old is missing or matches more than once.
var ErrPatchNotFound = errors.New("patch: old string not found")
var ErrPatchAmbiguous = errors.New("patch: old string matched multiple times")

func (f *FileStore) Patch(rel, oldStr, newStr string) error {
	current, err := f.ReadRaw(rel)
	if err != nil {
		return err
	}
	count := strings.Count(current, oldStr)
	switch {
	case count == 0:
		f.log.Warn().Str("path", rel).Msg("patch: old string not found")
		return ErrPatchNotFound
	case count > 1:
		f.log.Warn().Str("path", rel).Int("matches", count).Msg("patch: ambiguous")
		return ErrPatchAmbiguous
	}
	next := strings.Replace(current, oldStr, newStr, 1)
	return f.WriteRawAtomic(rel, next)
}

// AppendIfMissing appends `line` to rel if it is not already present.
func (f *FileStore) AppendIfMissing(rel, line string) (bool, error) {
	current, err := f.ReadRaw(rel)
	if err != nil {
		return false, err
	}
	if strings.Contains(current, line) {
		return false, nil
	}
	if current != "" && !strings.HasSuffix(current, "\n") {
		current += "\n"
	}
	return true, f.WriteRawAtomic(rel, current+line+"\n")
}

// CountLines returns line count of rel, -1 if file missing.
func (f *FileStore) CountLines(rel string) int {
	current, err := f.ReadRaw(rel)
	if err != nil || current == "" {
		return -1
	}
	n := strings.Count(current, "\n")
	if !strings.HasSuffix(current, "\n") {
		n++
	}
	return n
}

// ListChildren returns file/folder names (not full paths) directly under rel.
func (f *FileStore) ListChildren(rel string) ([]string, error) {
	p := filepath.Join(f.root, rel)
	entries, err := os.ReadDir(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out, nil
}

// EnsureDir makes rel a directory tree.
func (f *FileStore) EnsureDir(rel string) error {
	return os.MkdirAll(filepath.Join(f.root, rel), 0o755)
}

// --- index pollution guard ---

// indexLineRe matches lines like "  12| foo" produced by some editor
// read tools. We strip them defensively on every write.
var indexLineRe = regexp.MustCompile(`^\s*\d+\|\s?`)

func stripIndexPollution(s string) string {
	var buf bytes.Buffer
	scanner := bufio.NewScanner(strings.NewReader(s))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		buf.WriteString(indexLineRe.ReplaceAllString(line, ""))
		buf.WriteByte('\n')
	}
	// Trim a single trailing newline we may have added to a file that did
	// not originally end with one — but only if the result is otherwise
	// identical to the input after stripping.
	out := buf.String()
	if !strings.HasSuffix(s, "\n") && strings.HasSuffix(out, "\n") {
		out = strings.TrimRight(out, "\n")
	}
	return out
}

// PipeCat streams a file to the writer (used in tests and CLI debugging).
func (f *FileStore) PipeCat(rel string, w io.Writer) error {
	fp, err := os.Open(filepath.Join(f.root, rel))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer fp.Close()
	_, err = io.Copy(w, fp)
	return err
}
