package domain

import (
	"bytes"
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Info is the entire content of info.yaml: a pure YAML document
// describing the active character/world of the multiverse plus the
// known-but-inactive characters and worlds the player can switch
// back to. There is no markdown tail — the skill's anchor
// checklist lives next to the system prompt (prompts/narrative.md)
// because every operator already opens that file at session start
// anyway, and duplicating anchors in a separate file added more
// drift than it saved.
type Info struct {
	ActiveCharacter string   `yaml:"active_character"`
	ActiveWorld     string   `yaml:"active_world"`
	Characters      []string `yaml:"characters"`
	Worlds          []string `yaml:"worlds"`
}

// ActiveCharacterPointer returns the canonical "characters/<dir>"
// pointer, or "" if no character is registered.
func (i Info) ActiveCharacterPointer() string {
	if i.ActiveCharacter == "" {
		return ""
	}

	return "characters/" + i.ActiveCharacter
}

// ActiveWorldPointer returns the canonical "worlds/<dir>" pointer.
func (i Info) ActiveWorldPointer() string {
	if i.ActiveWorld == "" {
		return ""
	}

	return "worlds/" + i.ActiveWorld
}

// MarshalInfo renders the YAML representation of i. The output is
// stable: keys appear in struct-declaration order thanks to the
// yaml.v3 default.
func (i Info) MarshalInfo() (string, error) {
	out, err := yaml.Marshal(i)
	if err != nil {
		return "", fmt.Errorf("marshal info: %w", err)
	}

	return string(out), nil
}

// ParseInfo decodes an info.yaml body into an Info. An empty body
// or a structurally invalid document are errors. An all-zero but
// well-formed Info (a freshly created placeholder) is accepted:
// the bot is allowed to come up with no active character or world
// and the dispatcher will prompt the operator to run /launch.
func ParseInfo(content string) (Info, error) {
	if content == "" {
		return Info{}, errors.New("info.yaml: empty document")
	}
	var info Info
	dec := yaml.NewDecoder(bytes.NewReader([]byte(content)))
	dec.KnownFields(false)
	if err := dec.Decode(&info); err != nil {
		return Info{}, fmt.Errorf("parse info.yaml: %w", err)
	}

	return info, nil
}

// BuildInfo composes a fresh info.yaml body. The active character
// and world are always present in their respective lists; extras
// passed via allChars and allWorlds are deduped against the
// active value. An empty primary produces an empty slice — used
// by SessionStart to bootstrap a placeholder that /launch can
// later fill.
func BuildInfo(activeChar, activeWorld string, allChars, allWorlds []string) string {
	info := Info{
		ActiveCharacter: activeChar,
		ActiveWorld:     activeWorld,
		Characters:      mergeUnique(allChars, activeChar),
		Worlds:          mergeUnique(allWorlds, activeWorld),
	}
	body, _ := info.MarshalInfo()

	return body
}

func mergeUnique(extras []string, primary string) []string {
	if primary == "" {
		// Empty registry: keep the slice nil so it round-trips as
		// a missing key, not as `[]` in the YAML.
		if len(extras) == 0 {
			return nil
		}
		out := make([]string, 0, len(extras))
		for _, e := range extras {
			if e == "" {
				continue
			}
			out = append(out, e)
		}
		if len(out) == 0 {
			return nil
		}

		return out
	}
	seen := map[string]bool{primary: true}
	out := []string{primary}
	for _, e := range extras {
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}

	return out
}
