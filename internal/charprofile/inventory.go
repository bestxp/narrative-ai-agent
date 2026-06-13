package charprofile

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Inventory is the inventory.yaml payload — the GG's
// wallet and pockets. The shape is intentionally
// different from the other three files: items are a
// flat array of {name, description, equip, special}
// records, not a `data: [{section, values}]` list.
// The dispatch is the same (per-file Append tool)
// but the data model is its own type.
//
// Two top-level arrays:
//
//	currency: [{name, count}, ...]   — money
//	items:    [{name, description, equip, special}, ...]
//
// The character name is NOT here. SOUL.yaml is
// the canonical place for the display name; this
// file's identity is the directory it lives in
// (characters/<dir>/inventory.yaml).
type Inventory struct {
	// Currency is a flat list of {name, count}
	// records. The count is REPLACE-only — the
	// model is expected to read the current value
	// (returned in /me) and submit the new absolute
	// number, not a delta.
	Currency []Currency `yaml:"currency,omitempty"`
	// Items is a flat list of items. The name
	// field is the primary key.
	Items []Item `yaml:"items,omitempty"`
}

// Currency is a single money line. Count is
// non-negative; the loader clamps to [0, max] but
// the model is expected to send sane values.
type Currency struct {
	Name  string `yaml:"name"`
	Count int    `yaml:"count"`
}

// Item is a single inventory entry. Name is the
// primary key; description, equip and special are
// plain text/booleans.
//
// equip is the "currently worn / held" flag and
// is REPLACED on every update (REPLACE semantics
// for both true and false — the model can also
// unequip by sending equip: false). It defaults
// to false on a fresh item.
//
// special is free-form text. "нет" if no
// properties. Same Russian convention as the
// sample in planning/char_format.md.
//
// description is a 1-4 sentence prose description.
// The bot does not enforce the length — the prompt
// is the right place to nudge the model to keep it
// short.
type Item struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	Equip       bool   `yaml:"equip,omitempty"`
	Special     string `yaml:"special,omitempty"`
}

// ErrItemNotFound is returned by RemoveItem when
// the item name is not in the inventory. Distinct
// from ErrNotFound (whole-file) so callers can
// surface "remove_inventory_item: <name> not found".
var ErrItemNotFound = errors.New("charprofile: item not found")

// LoadInventory reads inventory.yaml. Empty
// body -> ErrNotFound.
func LoadInventory(body string) (Inventory, error) {
	var inv Inventory
	if strings.TrimSpace(body) == "" {
		return inv, ErrNotFound
	}
	if err := yaml.Unmarshal([]byte(body), &inv); err != nil {
		return inv, fmt.Errorf("charprofile: inventory: yaml.Unmarshal: %w", err)
	}
	return inv, nil
}

// Save serialises the inventory back to YAML.
func (inv Inventory) Save() (string, error) {
	out, err := yaml.Marshal(inv)
	if err != nil {
		return "", fmt.Errorf("charprofile: inventory: yaml.Marshal: %w", err)
	}
	return string(out), nil
}

// AppendItem adds or REPLACES an item by name.
// The match is exact-string after TrimSpace.
// Returns true if the file changed (a new item
// or a replaced existing one).
//
// Quantity is encoded in the name itself
// ("Кунай x3" or one items[] entry per unit) —
// the model picks the form. We do not enforce
// either. The bot just stores what it is told.
func (inv *Inventory) AppendItem(item Item) bool {
	name := strings.TrimSpace(item.Name)
	if name == "" {
		return false
	}
	item.Name = name
	for i := range inv.Items {
		if inv.Items[i].Name == name {
			// REPLACE: keep the identity, refresh
			// the attributes.
			changed := inv.Items[i].Description != item.Description ||
				inv.Items[i].Equip != item.Equip ||
				inv.Items[i].Special != item.Special
			inv.Items[i] = item
			return changed
		}
	}
	inv.Items = append(inv.Items, item)
	return true
}

// RemoveItem deletes an item by name. Returns
// ErrItemNotFound if the name is not present —
// the slowlog / ToolResult surfaces the name
// back to the model so it can recover.
func (inv *Inventory) RemoveItem(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrItemNotFound
	}
	for i := range inv.Items {
		if inv.Items[i].Name == name {
			inv.Items = append(inv.Items[:i], inv.Items[i+1:]...)
			return nil
		}
	}
	return ErrItemNotFound
}

// SetCurrency REPLACES the count of a currency
// line. Returns true if the line existed and was
// updated (or was just added).
//
// The model sends absolute values, not deltas
// ("count: 4200", not "count: -100"). See
// planning/char_format.md for the contract.
func (inv *Inventory) SetCurrency(name string, count int) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if count < 0 {
		count = 0
	}
	// Clamp at 999_999_999 — the largest 32-bit
	// signed value is a reasonable upper bound
	// for any single currency line in a
	// tabletop-RPG setting. Anything bigger is a
	// bug in the model's count, not real money.
	if count > 999_999_999 {
		count = 999_999_999
	}
	for i := range inv.Currency {
		if inv.Currency[i].Name == name {
			if inv.Currency[i].Count == count {
				return false
			}
			inv.Currency[i].Count = count
			return true
		}
	}
	inv.Currency = append(inv.Currency, Currency{Name: name, Count: count})
	return true
}

// RemoveCurrency deletes a currency line. Returns
// ErrItemNotFound (re-used — same semantics: name
// not present in the file).
func (inv *Inventory) RemoveCurrency(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrItemNotFound
	}
	for i := range inv.Currency {
		if inv.Currency[i].Name == name {
			inv.Currency = append(inv.Currency[:i], inv.Currency[i+1:]...)
			return nil
		}
	}
	return ErrItemNotFound
}
