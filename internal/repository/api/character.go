package api

import "github.com/bestxp/narrative-ai-agent/internal/charprofile"

// SoulRepository owns the character's soul (SOUL.yaml).
type SoulRepository interface {
	Load(character string) (charprofile.Soul, error)
	Save(character string, s charprofile.Soul) error
	// AppendSection adds a new free-form bullet under
	// the named section. The current on-disk schema is
	// a `data: [{section, values}]` array; this method
	// appends one item. The model calls append_soul with
	// section + value; the repository delegates to
	// charprofile.Append for the actual mutation.
	AppendSection(character, section, value string) (bool, error)
}

// SkillRepository owns skill.yaml.
type SkillRepository interface {
	Load(character string) (charprofile.Skill, error)
	Save(character string, s charprofile.Skill) error
	AppendSection(character, section, value string) (bool, error)
}

// CharacterMemoryRepository owns memory.yaml (4-section
// enum: "Яркие моменты" / "Факты о мире" / "Обещания и
// цели" / "Важные люди").
type CharacterMemoryRepository interface {
	Load(character string) (charprofile.Memory, error)
	Save(character string, m charprofile.Memory) error
	AppendSection(character, section, value string) (bool, error)
}

// InventoryRepository owns inventory.yaml (currency +
// items arrays).
type InventoryRepository interface {
	Load(character string) (charprofile.Inventory, error)
	Save(character string, inv charprofile.Inventory) error
	// Item ops (REPLACE-on-name).
	AppendItem(character string, item charprofile.Item) (bool, error)
	RemoveItem(character, name string) error
	// Currency ops.
	SetCurrency(character, name string, count int) (bool, error)
	RemoveCurrency(character, name string) error
}
