package api

import "github.com/bestxp/narrative-ai-agent/internal/chronicle"

// ChronicleRepository owns the world's day-by-day log +
// LLM-compressed windows (chronicle.yaml).
//
// The YAML format has two top-level arrays — periods
// (LLM-compressed windows) and days (raw per-day log
// for the open window). Repository.Load returns both;
// Chronicle.AppendDay / CompressWindow mutate in memory;
// Save persists the result.
//
// Repository does NOT call the LLM. The compression
// hook stays in usecase/tools/files/memory.go (it needs
// the summariser interface and a context); the
// repository only persists the result.
type ChronicleRepository interface {
	Load(world string) (chronicle.Chronicle, error)
	Save(world string, c chronicle.Chronicle) error
}
