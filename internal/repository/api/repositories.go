package api

import (
	"github.com/bestxp/narrative-ai-agent/internal/repository/yaml"
	"github.com/bestxp/narrative-ai-agent/internal/storage"
)

// Repositories is the bundle every Toolset receives
// via DI. Each field is an interface so tests can mock
// individual repositories without standing up the
// whole filesystem.
//
// The bundle itself is the API the rest of the project
// uses — domain code (usecase/tools/files/*.go) imports
// api.Repositories, never the concrete YAML
// implementations. This is what makes a SQL/noSQL
// backend a drop-in replacement: write a SqlRepository
// for each domain, then construct a sql.Repositories
// with the same field types.
type Repositories struct {
	Info        InfoRepository
	WorldState  WorldStateRepository
	Plan        PlanRepository
	Lore        LoreRepository
	Canon       CanonRepository
	Chronicle   ChronicleRepository
	Soul        SoulRepository
	Skill       SkillRepository
	Memory      CharacterMemoryRepository
	Inventory   InventoryRepository
	NPCProfile  NPCProfileRepository
	NPCRegistry NPCRegistryRepository
	Staging     StagingRepository
}

// NewYamlRepositories constructs the canonical
// YAML-backed bundle. The same store is shared across
// every repository; reads and writes go through the
// same atomicity guarantee.
//
// main.go is the only caller. Tests build their own
// Repositories with mocks.
func NewYamlRepositories(store storage.Storage) *Repositories {
	return &Repositories{
		Info:        yaml.NewInfoYaml(store),
		WorldState:  yaml.NewWorldStateYaml(store),
		Plan:        yaml.NewPlanYaml(store),
		Lore:        yaml.NewLoreYaml(store),
		Canon:       yaml.NewCanonYaml(store),
		Chronicle:   yaml.NewChronicleYaml(store),
		Soul:        yaml.NewSoulYaml(store),
		Skill:       yaml.NewSkillYaml(store),
		Memory:      yaml.NewCharacterMemoryYaml(store),
		Inventory:   yaml.NewInventoryYaml(store),
		NPCProfile:  yaml.NewNPCProfileYaml(store),
		NPCRegistry: yaml.NewNPCRegistryYaml(store),
		Staging:     yaml.NewStagingYaml(store),
	}
}

// Compile-time guarantees that every YAML repository
// implements its corresponding interface. These vars
// live in the parent package (not in yaml/) so the
// yaml/ package does not need to import the parent —
// that would create a cycle (parent imports yaml/ for
// the constructors; yaml/ would import parent for the
// interface declarations).
//
// If you add a new repository interface, add the
// corresponding assertion here. The build breaks if
// the YAML implementation drifts.
var (
	_ InfoRepository            = (*yaml.InfoYaml)(nil)
	_ WorldStateRepository      = (*yaml.WorldStateYaml)(nil)
	_ PlanRepository            = (*yaml.PlanYaml)(nil)
	_ LoreRepository            = (*yaml.LoreYaml)(nil)
	_ CanonRepository           = (*yaml.CanonYaml)(nil)
	_ ChronicleRepository       = (*yaml.ChronicleYaml)(nil)
	_ SoulRepository            = (*yaml.SoulYaml)(nil)
	_ SkillRepository           = (*yaml.SkillYaml)(nil)
	_ CharacterMemoryRepository = (*yaml.CharacterMemoryYaml)(nil)
	_ InventoryRepository       = (*yaml.InventoryYaml)(nil)
	_ NPCProfileRepository      = (*yaml.NPCProfileYaml)(nil)
	_ NPCRegistryRepository     = (*yaml.NPCRegistryYaml)(nil)
	_ StagingRepository         = (*yaml.StagingYaml)(nil)
)
