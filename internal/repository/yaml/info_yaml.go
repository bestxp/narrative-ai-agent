package yaml

import (
	"fmt"
	"strings"

	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/storage"
)

// infoYamlKey is the canonical storage key for the
// bot registry. Lives at the storage root, not under
// any world/character subdirectory.
const infoYamlKey = "info.yaml"

// InfoYaml is the YAML-backed implementation of
// repository.InfoRepository. The key is the relative
// path "info.yaml" — the YamlStorage backend resolves
// it against its root.
type InfoYaml struct {
	store storage.Storage
}

// NewInfoYaml constructs an InfoYaml on top of store.
// The store's Root() does not need to exist yet — the
// first Save creates the directory.
func NewInfoYaml(store storage.Storage) *InfoYaml {
	return &InfoYaml{store: store}
}

// Load returns the registry or domain.Info{} if no
// info.yaml has been written yet.
func (r *InfoYaml) Load() (domain.Info, error) {
	body, err := r.store.Read(infoYamlKey)
	if err != nil {
		return domain.Info{}, fmt.Errorf("info_load: Read failed: %w", err)
	}

	bodyStr := string(body)
	if strings.TrimSpace(bodyStr) == "" {
		return domain.Info{}, nil
	}

	info, err := domain.ParseInfo(bodyStr)
	if err != nil {
		return domain.Info{}, fmt.Errorf("info_load: parse: %w", err)
	}

	return info, nil
}

// Save persists info as YAML.
func (r *InfoYaml) Save(info domain.Info) error {
	body, err := info.MarshalInfo()
	if err != nil {
		return fmt.Errorf("save: MarshalInfo failed: %w", err)
	}

	if err := r.store.Write(infoYamlKey, []byte(body)); err != nil {
		return fmt.Errorf("save: write: %w", err)
	}

	return nil
}

// Compile-time guard: InfoYaml implements
// repository.InfoRepository. Lives at the bottom
// of the file (after the methods) so the dependency
// on the interface is local.
