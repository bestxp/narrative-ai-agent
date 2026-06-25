package yaml

import (
	"errors"
	"strings"

	"fmt"
	"github.com/bestxp/narrative-ai-agent/internal/charprofile"
	"github.com/bestxp/narrative-ai-agent/internal/storage"
)

// soulKey returns the storage key for a character's
// SOUL.yaml file.
func soulKey(character string) string {
	return "characters/" + character + "/SOUL.yaml"
}

// skillKey returns the storage key for a character's
// skill.yaml file.
func skillKey(character string) string {
	return "characters/" + character + "/skill.yaml"
}

// characterMemoryKey returns the storage key for a
// character's memory.yaml file.
func characterMemoryKey(character string) string {
	return "characters/" + character + "/memory.yaml"
}

// inventoryKey returns the storage key for a
// character's inventory.yaml file.
func inventoryKey(character string) string {
	return "characters/" + character + "/inventory.yaml"
}

// --- SOUL ---

// SoulYaml is the YAML-backed SoulRepository.
type SoulYaml struct {
	store storage.Storage
}

// NewSoulYaml constructs the soul repository.
func NewSoulYaml(store storage.Storage) *SoulYaml {
	return &SoulYaml{store: store}
}

// Load returns the parsed Soul. Empty body returns the
// zero Soul (a brand-new character has no sections yet).
func (r *SoulYaml) Load(character string) (charprofile.Soul, error) {
	body, err := r.store.Read(soulKey(character))
	if err != nil {
		return charprofile.Soul{}, fmt.Errorf("load: Read failed: %w", err)
	}
	if strings.TrimSpace(string(body)) == "" {
		return charprofile.Soul{}, nil
	}
	s, err := charprofile.LoadSoul(string(body))
	if err != nil {
		return charprofile.Soul{}, fmt.Errorf("load: LoadSoul failed: %w", err)
	}
	return s, nil
}

// Save persists the Soul as YAML.
func (r *SoulYaml) Save(character string, s charprofile.Soul) error {
	body, err := s.Save()
	if err != nil {
		return fmt.Errorf("save: Save failed: %w", err)
	}
	return r.store.Write(soulKey(character), []byte(body))
}

// AppendSection adds a new free-form bullet to the
// named section. Section-name validation (must be on
// the file's enum for Skill/Memory, free-form for Soul)
// lives in charprofile.Append; the repository just
// delegates.
//
// Returns true if the section was actually modified
// (a duplicate bullet is a no-op).
func (r *SoulYaml) AppendSection(character, section, value string) (bool, error) {
	s, err := r.Load(character)
	if err != nil {
		return false, err
	}
	if !s.Append(section, value) {
		return false, nil
	}
	if err := r.Save(character, s); err != nil {
		return false, err
	}
	return true, nil
}

// --- SKILL ---

// SkillYaml is the YAML-backed SkillRepository.
type SkillYaml struct {
	store storage.Storage
}

// NewSkillYaml constructs the skill repository.
func NewSkillYaml(store storage.Storage) *SkillYaml {
	return &SkillYaml{store: store}
}

// Load returns the parsed Skill.
func (r *SkillYaml) Load(character string) (charprofile.Skill, error) {
	body, err := r.store.Read(skillKey(character))
	if err != nil {
		return charprofile.Skill{}, fmt.Errorf("load: Read failed: %w", err)
	}
	if strings.TrimSpace(string(body)) == "" {
		return charprofile.Skill{}, nil
	}
	s, err := charprofile.LoadSkill(string(body))
	if err != nil {
		return charprofile.Skill{}, fmt.Errorf("load: LoadSkill failed: %w", err)
	}
	return s, nil
}

// Save persists the Skill as YAML.
func (r *SkillYaml) Save(character string, s charprofile.Skill) error {
	body, err := s.Save()
	if err != nil {
		return fmt.Errorf("save: Save failed: %w", err)
	}
	return r.store.Write(skillKey(character), []byte(body))
}

// AppendSection appends to a skill section.
func (r *SkillYaml) AppendSection(character, section, value string) (bool, error) {
	s, err := r.Load(character)
	if err != nil {
		return false, err
	}
	if !s.Append(section, value) {
		return false, nil
	}
	if err := r.Save(character, s); err != nil {
		return false, err
	}
	return true, nil
}

// --- CHARACTER MEMORY ---

// CharacterMemoryYaml is the YAML-backed
// CharacterMemoryRepository.
type CharacterMemoryYaml struct {
	store storage.Storage
}

// NewCharacterMemoryYaml constructs the memory
// repository.
func NewCharacterMemoryYaml(store storage.Storage) *CharacterMemoryYaml {
	return &CharacterMemoryYaml{store: store}
}

// Load returns the parsed Memory.
func (r *CharacterMemoryYaml) Load(character string) (charprofile.Memory, error) {
	body, err := r.store.Read(characterMemoryKey(character))
	if err != nil {
		return charprofile.Memory{}, fmt.Errorf("load: Read failed: %w", err)
	}
	if strings.TrimSpace(string(body)) == "" {
		return charprofile.Memory{}, nil
	}
	m, err := charprofile.LoadMemory(string(body))
	if err != nil {
		return charprofile.Memory{}, fmt.Errorf("load: LoadMemory failed: %w", err)
	}
	return m, nil
}

// Save persists the Memory as YAML.
func (r *CharacterMemoryYaml) Save(character string, m charprofile.Memory) error {
	body, err := m.Save()
	if err != nil {
		return fmt.Errorf("save: Save failed: %w", err)
	}
	return r.store.Write(characterMemoryKey(character), []byte(body))
}

// AppendSection appends to a memory section.
func (r *CharacterMemoryYaml) AppendSection(character, section, value string) (bool, error) {
	m, err := r.Load(character)
	if err != nil {
		return false, err
	}
	if !m.Append(section, value) {
		return false, nil
	}
	if err := r.Save(character, m); err != nil {
		return false, err
	}
	return true, nil
}

// --- INVENTORY ---

// InventoryYaml is the YAML-backed InventoryRepository.
type InventoryYaml struct {
	store storage.Storage
}

// NewInventoryYaml constructs the inventory repository.
func NewInventoryYaml(store storage.Storage) *InventoryYaml {
	return &InventoryYaml{store: store}
}

// Load returns the parsed Inventory.
func (r *InventoryYaml) Load(character string) (charprofile.Inventory, error) {
	body, err := r.store.Read(inventoryKey(character))
	if err != nil {
		return charprofile.Inventory{}, fmt.Errorf("inventory_load: Read failed: %w", err)
	}
	if strings.TrimSpace(string(body)) == "" {
		return charprofile.Inventory{}, nil
	}
	inv, err := charprofile.LoadInventory(string(body))
	if err != nil {
		return charprofile.Inventory{}, fmt.Errorf("load: LoadInventory failed: %w", err)
	}
	return inv, nil
}

// Save persists the Inventory as YAML.
func (r *InventoryYaml) Save(character string, inv charprofile.Inventory) error {
	body, err := inv.Save()
	if err != nil {
		return fmt.Errorf("save: Save failed: %w", err)
	}
	return r.store.Write(inventoryKey(character), []byte(body))
}

// AppendItem REPLACE-on-name. Returns true if the
// inventory changed. A duplicate item (same name)
// is treated as a no-op (consistent with the
// previous AppendInventoryItem behaviour).
func (r *InventoryYaml) AppendItem(character string, item charprofile.Item) (bool, error) {
	inv, err := r.Load(character)
	if err != nil {
		return false, err
	}
	if !inv.AppendItem(item) {
		return false, nil
	}
	if err := r.Save(character, inv); err != nil {
		return false, err
	}
	return true, nil
}

// RemoveItem deletes an item by name. A missing item
// is a silent no-op (consistent with the previous
// `RemoveInventoryItem` behaviour).
func (r *InventoryYaml) RemoveItem(character, name string) error {
	inv, err := r.Load(character)
	if err != nil {
		return err
	}
	if err := inv.RemoveItem(name); err != nil {
		// charprofile.ErrItemNotFound is a no-op at the
		// repository level — the dispatcher treats
		// "remove a non-existent item" the same as
		// "remove it successfully".
		if errors.Is(err, charprofile.ErrItemNotFound) {
			return nil
		}
		return err
	}
	return r.Save(character, inv)
}

// SetCurrency upserts a currency entry.
func (r *InventoryYaml) SetCurrency(character, name string, count int) (bool, error) {
	inv, err := r.Load(character)
	if err != nil {
		return false, err
	}
	if !inv.SetCurrency(name, count) {
		return false, nil
	}
	if err := r.Save(character, inv); err != nil {
		return false, err
	}
	return true, nil
}

// RemoveCurrency deletes a currency entry. A missing
// currency is a silent no-op (mirrors RemoveItem).
func (r *InventoryYaml) RemoveCurrency(character, name string) error {
	inv, err := r.Load(character)
	if err != nil {
		return err
	}
	if err := inv.RemoveCurrency(name); err != nil {
		if errors.Is(err, charprofile.ErrItemNotFound) {
			return nil
		}
		return err
	}
	return r.Save(character, inv)
}

// its corresponding repository.XxxRepository. The
// matching assertion lives in repository/contracts.go
// (which can import yaml/, but yaml/ cannot import
// the parent package — that would cycle).
