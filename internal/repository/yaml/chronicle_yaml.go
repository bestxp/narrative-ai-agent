package yaml

import (
	"strings"

	"fmt"
	"github.com/bestxp/narrative-ai-agent/internal/chronicle"
	"github.com/bestxp/narrative-ai-agent/internal/storage"
)

// ChronicleYaml is the YAML-backed implementation of
// ChronicleRepository. The on-disk format is YAML with
// two top-level arrays (periods + days) defined by
// chronicle.Chronicle.
type ChronicleYaml struct {
	store storage.Storage
}

// NewChronicleYaml constructs the chronicle repository.
func NewChronicleYaml(store storage.Storage) *ChronicleYaml {
	return &ChronicleYaml{store: store}
}

// Load returns the chronicle for world. A missing
// file is equivalent to an empty chronicle — the
// repository returns Chronicle{Periods: [], Days: {}}
// so callers can iterate without nil checks.
//
// Parse errors are returned as-is; callers (usecase
// layer) decide whether to log-and-continue or surface.
func (r *ChronicleYaml) Load(world string) (chronicle.Chronicle, error) {
	body, err := r.store.Read(chronicleKey(world))
	if err != nil {
		return chronicle.Chronicle{}, fmt.Errorf("chronicle_load: Read failed: %w", err)
	}
	if strings.TrimSpace(string(body)) == "" {
		return chronicle.Chronicle{
			Periods: []chronicle.Period{},
			Days:    map[int]string{},
		}, nil
	}
	c, err := chronicle.Load(string(body))
	if err != nil {
		return chronicle.Chronicle{}, fmt.Errorf("wrap: %w", err)
	}
	// chronicle.Load may return nil Periods / nil Days
	// when the YAML had empty sections; normalise so
	// the caller never has to nil-check.
	if c.Periods == nil {
		c.Periods = []chronicle.Period{}
	}
	if c.Days == nil {
		c.Days = map[int]string{}
	}
	return c, nil
}

// Save persists the chronicle atomically.
func (r *ChronicleYaml) Save(world string, c chronicle.Chronicle) error {
	body, err := c.Save()
	if err != nil {
		return fmt.Errorf("save: Save failed: %w", err)
	}
	return r.store.Write(chronicleKey(world), []byte(body))
}

// Compile-time guard.

// chronicleKey returns the storage key for chronicle.yaml.
func chronicleKey(world string) string {
	return "worlds/" + world + "/chronicle.yaml"
}

// its corresponding repository.XxxRepository. The
// matching assertion lives in repository/contracts.go
// (which can import yaml/, but yaml/ cannot import
// the parent package — that would cycle).
