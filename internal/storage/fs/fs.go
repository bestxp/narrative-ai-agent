// Package fs implements the Storage interface for a
// filesystem backend (YAML and markdown files on disk).
//
// The implementation is a thin wrapper over os.ReadFile,
// os.WriteFile, and filepath. Path-helpers like
// WorldChronicle or CharacterSOUL are GONE — those
// concerns moved into repositories under
// internal/repository/*_yaml.go. YamlStorage only
// knows about Read/Write/Exists/ListChildren/EnsureDir
// of opaque keys.
package fs

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
)

// YamlStorage is the filesystem implementation of
// storage.Storage. All operations are relative to
// Root; keys are slash-separated paths ("worlds/naruto/
// chronicle.yaml"). Atomicity is provided by temp-file
// + rename on Write (the rename is atomic at the OS
// level on POSIX filesystems).
type YamlStorage struct {
	root string
}

// New constructs a YamlStorage rooted at the given
// absolute directory. The directory is created if it
// does not exist.
func New(root string) (*YamlStorage, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("fs: abs root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("fs: mkdir root: %w", err)
	}
	return &YamlStorage{root: abs}, nil
}

// Root returns the absolute filesystem path this
// storage is rooted at. Repositories use it to build
// concrete file paths from logical keys.
func (s *YamlStorage) Root() string { return s.root }

// Join builds an absolute path from key. Exported for
// repositories that need to stat / list a concrete
// directory under the storage root.
func (s *YamlStorage) Join(key string) string {
	return filepath.Join(s.root, key)
}

// Dir returns the parent directory of key, used by
// EnsureDir.
func (s *YamlStorage) dirOf(key string) string {
	return filepath.Dir(s.Join(key))
}

// Read returns the bytes at key. Returns (nil, nil)
// for a missing key — the "empty file" case is normal
// in domain code. Returns the read error otherwise.
func (s *YamlStorage) Read(key string) ([]byte, error) {
	b, err := os.ReadFile(s.Join(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read: ReadFile failed: %w", err)
	}
	return b, nil
}

// Write persists data at key atomically (temp file +
// rename). Creates parent directories as needed.
// Strips any "  12| " index pollution the input may
// contain (defensive: editor / viewer side-effects).
func (s *YamlStorage) Write(key string, data []byte) error {
	clean := stripIndexPollutionBytes(data)
	if err := os.MkdirAll(s.dirOf(key), 0o755); err != nil {
		return fmt.Errorf("write: MkdirAll failed: %w", err)
	}
	target := s.Join(key)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, clean, 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return os.Rename(tmp, target)
}

// Exists reports whether key is present on disk.
func (s *YamlStorage) Exists(key string) (bool, error) {
	_, err := os.Stat(s.Join(key))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// ListChildren returns the names (NOT full keys) of
// files / subdirectories directly under dir. Missing
// dir is treated as empty (returns nil, nil).
func (s *YamlStorage) ListChildren(dir string) ([]string, error) {
	entries, err := os.ReadDir(s.Join(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list_children: ReadDir failed: %w", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out, nil
}

// EnsureDir makes the parent directory of key exist.
// For filesystem this is os.MkdirAll. Calling on an
// already-existing dir is a no-op.
func (s *YamlStorage) EnsureDir(key string) error {
	if err := os.MkdirAll(s.dirOf(key), 0o755); err != nil {
		return fmt.Errorf("ensure_dir: %w", err)
	}
	return nil
}

// Compile-time guarantee *YamlStorage implements Storage.
var _ interface {
	Read(key string) ([]byte, error)
	Write(key string, data []byte) error
	Exists(key string) (bool, error)
	ListChildren(dir string) ([]string, error)
	EnsureDir(key string) error
} = (*YamlStorage)(nil)

// stripIndexPollutionBytes removes lines like "  12| foo"
// that some editor tools inject into file reads. This is
// the bytes variant of the legacy stripIndexPollution
// (string-based) used by the previous FileStore
// implementation. The behaviour is identical — only the
// input/output type changed.
var indexLineRe = regexp.MustCompile(`^\s*\d+\|\s?`)

func stripIndexPollutionBytes(s []byte) []byte {
	var buf bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(s))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		buf.Write(indexLineRe.ReplaceAll(line, nil))
		buf.WriteByte('\n')
	}
	out := buf.Bytes()
	// Trim a single trailing newline we may have added to
	// a file that did not originally end with one — but
	// only if the result is otherwise identical to the
	// input after stripping.
	if len(out) > 0 && out[len(out)-1] == '\n' && (len(s) == 0 || s[len(s)-1] != '\n') {
		out = out[:len(out)-1]
	}
	return out
}

// Detect for testing whether a key would be a
// directory listing (ends in /). Repositories use this
// to decide whether to call ListChildren or Read. Kept
// as a helper rather than an interface method because
// the backend's directory-vs-file distinction is
// filesystem-specific; SQL/S3 backends do not have
// this concept at the Storage layer.
func IsDirKey(key string) bool {
	return strings.HasSuffix(key, "/")
}

// Compile-time guard: io.Reader is unused at the
// interface level but the package imports io for
// ErrUnexpectedEOF reuse; this avoids the "imported and
// not used" error if ErrNotFound is ever removed.
var _ = io.EOF
