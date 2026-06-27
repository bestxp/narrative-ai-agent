package prompts

import (
	"fmt"
	"os"
	"strings"
	"text/template"
)

// RenderSummarizer renders a summarizer-side prompt
// (compaction_in_place, end_of_day,
// character_memory_maintain, lore_summary,
// memorise_summary, npc_summary, chronicle_summary,
// summary) with a config-only data-bag. The summarizer
// prompts depend only on NarrativeConfigSnapshot and
// not on per-turn context, so rendering once at startup
// is enough; the resulting string is handed to the
// summarizer's setters and re-used on every call.
//
// Returns an error if the named template is unknown or
// fails to render.
func RenderSummarizer(name string, snap NarrativeConfigSnapshot) (string, error) {
	data := NewPromptData(snap, CharacterData{}, WorldData{})

	out, err := Render(name, data)
	if err != nil {
		return "", fmt.Errorf("render summarizer prompt %q: %w", name, err)
	}

	return out, nil
}

// RenderNarrative loads the system prompt for the
// narrative role.
//
// Override-on-disk wins: if the operator configured a
// path to a hand-written .md or .md.tmpl file, it is
// read verbatim. If the file looks like a Go template
// (contains "{{") or has a .md.tmpl extension, it is
// rendered with the data-bag. Otherwise the override is
// returned as-is.
//
// When no override is configured, or the override file
// is missing on disk, the embedded narrative.md.tmpl
// is rendered.
//
// RenderNarrative is the seam that lets operators
// replace the bundled narrative prompt without
// rebuilding the binary — drop a custom .md next to
// the executable, point the role at it, restart.
func RenderNarrative(overridePath string, snap NarrativeConfigSnapshot) (string, error) {
	data := NewPromptData(snap, CharacterData{}, WorldData{})

	if overridePath == "" {
		return renderEmbeddedNarrative(data)
	}

	body, err := os.ReadFile(overridePath)
	if err != nil {
		if os.IsNotExist(err) {
			// No override file — fall through to the
			// embedded template. The operator may
			// have set the path in config but not
			// created the file yet.
			return renderEmbeddedNarrative(data)
		}

		return "", fmt.Errorf("read override %q: %w", overridePath, err)
	}

	text := strings.TrimSpace(string(body))
	// Plain markdown override (operator drops a
	// hand-written narrative.md without template
	// markers) — return as-is.
	if !strings.HasSuffix(overridePath, ".md.tmpl") && !LooksLikeTemplate(text) {
		return text, nil
	}
	// Treat as a template, render with the data-bag.
	return renderFromBody(overridePath, text, data)
}

// renderEmbeddedNarrative renders the bundled
// narrative.md.tmpl with the data-bag. Wrapped so
// the call sites in RenderNarrative stay readable.
func renderEmbeddedNarrative(data PromptData) (string, error) {
	out, err := Render("narrative.md.tmpl", data)
	if err != nil {
		return "", fmt.Errorf("render narrative prompt: %w", err)
	}

	return out, nil
}

// LooksLikeTemplate is a cheap sniff for "{{" markers
// that distinguishes plain markdown from Go-template
// source. False positives are harmless: a markdown file
// containing "{{" simply gets parsed as a template (and
// the missing-key check will fire if a real {{ is
// present).
func LooksLikeTemplate(body string) bool {
	return strings.Contains(body, "{{")
}

// renderFromBody parses body as a Go template with the
// data-bag and returns the rendered text. The wrapper
// around the embedded template cache is intentionally
// minimal: the override path is rare, so the parse cost
// is paid once per process.
//
// missingkey=error is mandatory: a typo in
// {{ .Narrative.WordLmit }} must fail loudly at Execute
// time, not silently render "<no value>".
func renderFromBody(name, body string, data PromptData) (string, error) {
	tpl, err := template.New(name).Option("missingkey=error").Parse(body)
	if err != nil {
		return "", fmt.Errorf("parse override %q: %w", name, err)
	}

	var b strings.Builder
	if err := tpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("execute override %q: %w", name, err)
	}

	return b.String(), nil
}
