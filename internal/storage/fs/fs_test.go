package fs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNew_CreatesRootDir(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.Root() == "" {
		t.Fatal("Root() empty")
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("root not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("root is not a directory")
	}
}

func TestReadWrite_RoundTrip(t *testing.T) {
	t.Parallel()
	s, _ := New(t.TempDir())
	want := []byte("hello world")
	if err := s.Write("a/b.txt", want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := s.Read("a/b.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRead_Missing(t *testing.T) {
	t.Parallel()
	s, _ := New(t.TempDir())
	got, err := s.Read("does/not/exist.txt")
	if err != nil {
		t.Fatalf("missing Read should not error: %v", err)
	}
	if got != nil {
		t.Errorf("missing Read should return nil bytes, got %q", got)
	}
}

func TestExists(t *testing.T) {
	t.Parallel()
	s, _ := New(t.TempDir())
	ok, err := s.Exists("nonexistent")
	if err != nil {
		t.Fatalf("Exists (missing): %v", err)
	}
	if ok {
		t.Error("Exists returned true for missing key")
	}
	if err := s.Write("here.txt", []byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	ok, err = s.Exists("here.txt")
	if err != nil {
		t.Fatalf("Exists (present): %v", err)
	}
	if !ok {
		t.Error("Exists returned false for present key")
	}
}

func TestWrite_Overwrites(t *testing.T) {
	t.Parallel()
	s, _ := New(t.TempDir())
	if err := s.Write("f.txt", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := s.Write("f.txt", []byte("second")); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Read("f.txt")
	if string(got) != "second" {
		t.Errorf("expected overwrite, got %q", got)
	}
}

func TestWrite_AtomicViaTmpRename(t *testing.T) {
	t.Parallel()
	s, _ := New(t.TempDir())
	if err := s.Write("a.txt", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	// During a successful Write there must not be a
	// leftover .tmp file.
	if _, err := os.Stat(filepath.Join(s.Root(), "a.txt.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected no .tmp file after Write, got err=%v", err)
	}
}

func TestListChildren(t *testing.T) {
	t.Parallel()
	s, _ := New(t.TempDir())
	if err := s.Write("d/a.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := s.Write("d/b.txt", []byte("y")); err != nil {
		t.Fatal(err)
	}
	if err := s.Write("d/sub/c.txt", []byte("z")); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListChildren("d")
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	want := map[string]bool{"a.txt": false, "b.txt": false, "sub": true}
	if len(got) != len(want) {
		t.Errorf("expected %d entries, got %d (%v)", len(want), len(got), got)
	}
	for _, name := range got {
		isDir, ok := want[name]
		if !ok {
			t.Errorf("unexpected entry: %q", name)
			continue
		}
		full := filepath.Join(s.Root(), "d", name)
		info, err := os.Stat(full)
		if err != nil {
			t.Errorf("stat %q: %v", name, err)
			continue
		}
		if info.IsDir() != isDir {
			t.Errorf("entry %q: expected isDir=%v, got %v", name, isDir, info.IsDir())
		}
	}
}

func TestListChildren_MissingDir(t *testing.T) {
	t.Parallel()
	s, _ := New(t.TempDir())
	got, err := s.ListChildren("does/not/exist")
	if err != nil {
		t.Fatalf("missing dir ListChildren should not error: %v", err)
	}
	if got != nil {
		t.Errorf("missing dir should return nil entries, got %v", got)
	}
}

func TestEnsureDir_Idempotent(t *testing.T) {
	t.Parallel()
	s, _ := New(t.TempDir())
	if err := s.EnsureDir("a/b/c.txt"); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if err := s.EnsureDir("a/b/c.txt"); err != nil {
		t.Fatalf("EnsureDir second call: %v", err)
	}
	if err := s.Write("a/b/c.txt", []byte("hi")); err != nil {
		t.Fatalf("Write after EnsureDir: %v", err)
	}
}

func TestStripIndexPollutionBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "line1\nline2\n", "line1\nline2\n"},
		{"polluted", "  12| line1\n  13| line2\n", "line1\nline2\n"},
		{"no trailing newline", "  12| a", "a"},
		{"blank line stays", "  12| a\n\n  14| b\n", "a\n\nb\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := string(stripIndexPollutionBytes([]byte(c.in)))
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestJoin(t *testing.T) {
	t.Parallel()
	s, _ := New(t.TempDir())
	got := s.Join("worlds/naruto/chronicle.yaml")
	want := filepath.Join(s.Root(), "worlds/naruto/chronicle.yaml")
	if got != want {
		t.Errorf("Join: got %q, want %q", got, want)
	}
}

func TestIsDirKey(t *testing.T) {
	t.Parallel()
	if IsDirKey("worlds/naruto/") != true {
		t.Error("trailing slash should be a dir key")
	}
	if IsDirKey("worlds/naruto/chronicle.yaml") != false {
		t.Error("non-trailing slash should not be a dir key")
	}
}

func TestRoot_Absolute(t *testing.T) {
	t.Parallel()
	s, _ := New(t.TempDir())
	if !strings.HasPrefix(s.Root(), "/") {
		t.Errorf("Root() should be absolute, got %q", s.Root())
	}
}
