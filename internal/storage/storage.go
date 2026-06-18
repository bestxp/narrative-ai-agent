// Package storage defines the Storage interface — the
// thin abstraction every backend (filesystem, SQL, S3,
// git-lfs) implements. Repositories in internal/repository
// accept a Storage and hide the backend from domain code.
//
// The interface is intentionally minimal (5 operations).
// Domain-specific operations (append, dedup, atomic
// read-modify-write) live in the repository layer, not
// here. A SQL backend that has no concept of "list
// children" can no-op Lister / DirectoryMaker; the
// repositories that need them simply won't be wired.
package storage

import "io"

// ErrNotFound is returned by Reader.Read when the key
// does not exist. Callers should treat (nil, nil) as a
// successful "empty" read for the key, NOT a "file
// missing" error; ErrNotFound is reserved for "you asked
// for something that should be there but isn't" (e.g.
// info.yaml after a Launch).
//
// FileSystem backend maps "no entry" → (nil, nil) so
// domain code can treat the world as "empty chronicle"
// out of the box. Repositories that need to distinguish
// missing-vs-empty use ExistenceProbe.Exists.
var ErrNotFound = io.ErrUnexpectedEOF

// Reader returns the bytes stored under key.
//
// The key is opaque from the caller's perspective: for
// filesystem backends it is a relative path
// ("worlds/naruto/chronicle.yaml"), for S3 it is an
// object key, for SQL it is an opaque identifier the
// backend parses internally.
//
// Returns (nil, nil) when the key does not exist — the
// "empty file" case is normal in domain code
// (a brand-new world has no chronicle yet).
// Returns (nil, ErrNotFound) when the caller requires
// the key to exist (rare; mostly in repository internals).
type Reader interface {
	Read(key string) ([]byte, error)
}

// Writer persists data under key. The semantics of
// "atomic" are backend-specific:
//
//   - filesystem: temp file + rename (current behaviour
//     of WriteRawAtomic, only-fsync-at-the-OS-level);
//   - SQL: single UPDATE inside an implicit transaction;
//   - S3: PUT with If-Match (conditional write).
//
// Callers MUST treat Write as atomic with respect to a
// concurrent Read of the same key. There is no
// "read-modify-write" guarantee from the interface —
// that lives in the repository layer.
type Writer interface {
	Write(key string, data []byte) error
}

// ExistenceProbe reports whether key exists. Cheaper
// than Read for the "do I have a file for this?" use
// case in the seedWorld skeleton.
type ExistenceProbe interface {
	Exists(key string) (bool, error)
}

// Lister returns the names (NOT full keys) of the
// immediate children of dir. The ordering is
// backend-specific; callers that need a stable order
// sort the result themselves.
//
// For backends without a directory concept (S3 flat
// namespace, KV stores), this method may return
// (nil, nil) — repositories that need listing must
// implement it at the repository level instead.
type Lister interface {
	ListChildren(dir string) ([]string, error)
}

// DirectoryMaker ensures the parent directories for
// key exist. For filesystem backends this is
// os.MkdirAll(filepath.Dir(key)); for SQL/S3 it is a
// no-op (always returns nil).
//
// Repositories that need a directory to exist before
// the first Write call this in their constructor or
// before a Save (the implementation may call it once
// per Save, the FS backend short-circuits if the dir
// already exists).
type DirectoryMaker interface {
	EnsureDir(key string) error
}

// Storage is the composite interface every backend
// satisfies. SQL/noSQL backends may stub Lister or
// DirectoryMaker with no-ops (returning nil); the
// repositories that depend on those methods simply
// won't be wired with that backend.
//
// Storage does NOT define domain semantics. There is
// no ReadNPCProfile or WriteLoreEntry method — those
// belong to repositories. Storage answers only
// "what does it mean to read/write/exist a key on
// this backend?".
type Storage interface {
	Reader
	Writer
	ExistenceProbe
	Lister
	DirectoryMaker
}
