// Package prompts serves the bundled skill prompts. The source
// files live in this directory; they are baked into the
// binary at build time via //go:embed so the operator does not
// have to ship them next to the executable.
//
// Why embed instead of a relative path at runtime:
//
//   - The skill files are *behaviour* of the bot, not data.
//     A typo or a missing file would change how the GM
//     reasons, silently, and the operator would have no clue
//     which version of the prompt is actually in use.
//   - Single-file deploys: `bot-windows-amd64.exe config.yaml`
//     is enough. No need to remember to copy the prompts dir.
//   - The fallback chain is explicit: an override on disk
//     wins (cfg.SystemPromptPath), else the embedded default
//     is used. There is no "find prompts/ relative to
//     executable" path-resolution surprise.
//
// If a future role needs its own prompt, add the .md file
// to this directory and call Bundled("name.md") — go:embed
// will pick it up automatically thanks to the wildcard.
package prompts

import (
	"embed"
	"fmt"
	"os"
	"strings"
)

//go:embed *.md
var bundled embed.FS

// List returns the names of all bundled prompt files
// (e.g. "narrative.md", "summary.md"). main.go uses this to
// log at startup which prompts are baked into the binary.
func List() []string {
	entries, err := bundled.ReadDir(".")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			out = append(out, e.Name())
		}
	}
	return out
}

// Bundled returns the contents of an embedded prompt file.
// It panics if the name is missing — the prompt is part of
// the binary's behaviour, and a missing file at runtime is
// a build-time mistake that the operator should not have to
// debug.
func Bundled(name string) string {
	data, err := bundled.ReadFile(name)
	if err != nil {
		panic("prompts: bundled file missing: " + name + " (rebuild the binary)")
	}
	return string(data)
}

// LoadOption toggles LoadSystemPrompt behaviour. The zero
// value is the recommended default.
type LoadOption func(*loadOpts)

type loadOpts struct {
	// Required: panic if no source is found. Production
	// code uses this — a missing prompt is a fatal
	// misconfiguration, not a soft warning. Tests that
	// exercise the "no override + no default" path turn
	// it off.
	Required bool
}

// WithRequired makes LoadSystemPrompt panic when no source
// resolves. This is the production default.
func WithRequired() LoadOption {
	return func(o *loadOpts) { o.Required = true }
}

// LoadSystemPrompt resolves a system prompt by name.
//
// Resolution order:
//
//  1. If overridePath is non-empty AND points to a readable
//     file, read it from disk and return.
//  2. Otherwise read the embedded default with the same name
//     (e.g. "narrative.md").
//  3. If both fail and WithRequired was passed, panic with
//     a clear message. Otherwise return an empty string and
//     the error.
//
// The two-arg form (override + defaultName) is the common
// path: cfg.Narrative.SystemPromptPath + "narrative.md".
func LoadSystemPrompt(overridePath, defaultName string, opts ...LoadOption) (string, error) {
	o := loadOpts{Required: true}
	for _, fn := range opts {
		fn(&o)
	}

	if overridePath != "" {
		data, err := os.ReadFile(overridePath)
		if err == nil {
			return strings.TrimSpace(string(data)), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("prompts: read override %q: %w", overridePath, err)
		}
	}

	data := Bundled(defaultName)
	return strings.TrimSpace(data), nil
}
