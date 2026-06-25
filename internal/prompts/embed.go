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
//
// If a future role needs its own prompt, add the .md.tmpl
// file to this directory and call
// `prompts.Render("<name>.md.tmpl", data)` — go:embed will
// pick it up automatically thanks to the wildcard.
//
// Templates are *code*, not data — same lifecycle as .go:
// a typo = a build-time error, never a silent runtime drift.
package prompts

import (
	"embed"
	"fmt"
	"strings"
	"sync"
	"text/template"
)

//go:embed *.md.tmpl
var bundledFS embed.FS

// List returns the names of all bundled prompt
// templates. The list is used at startup to log which
// prompts are baked into the binary — operators can
// spot a missing template (a typo in a `//go:embed`
// pattern, or a forgotten file) at a glance.
func List() []string {
	entries, err := bundledFS.ReadDir(".")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md.tmpl") {
			out = append(out, e.Name())
		}
	}
	return out
}

// templateCache caches parsed templates keyed by filename.
// Parsing is cheap, but the GM hits the same template
// thousands of times per day — caching is the right
// thing. Templates never change at runtime (they are
// embedded and stable per process), so no invalidation
// API is needed.
var templateCache sync.Map // map[string]*template.Template

// Render reads the named prompt from the embedded FS,
// parses it as a Go template with missingkey=error,
// and executes it with data. The named template's body
// is the only template — no `{{ template "name" . }}`
// cross-file refs, to keep the embed.FS flat and
// predictable.
//
// A typo in `{{ .Narrtive.WordLimit }}` fails loudly at
// Execute time thanks to missingkey=error, not silently
// with `<no value>`.
func Render(name string, data PromptData) (string, error) {
	if !strings.HasSuffix(name, ".md.tmpl") {
		return "", fmt.Errorf("prompts: Render only accepts .md.tmpl files, got %q", name)
	}
	src, err := bundledFS.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("prompts: template not found: %q", name)
	}
	tpl, err := getOrParse(name, string(src))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := tpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("prompts: render %q: %w", name, err)
	}
	return b.String(), nil
}

// getOrParse returns the parsed template for name,
// parsing and caching on first call.
func getOrParse(name, src string) (*template.Template, error) {
	if v, ok := templateCache.Load(name); ok {
		tpl, ok := v.(*template.Template)
		if !ok {
			return nil, fmt.Errorf("prompts: cache for %q has unexpected type %T", name, v)
		}
		return tpl, nil
	}
	tpl, err := template.New(name).
		Option("missingkey=error").
		Funcs(template.FuncMap{
			"add1": func(i int) int { return i + 1 },
			"add":  func(a, b int) int { return a + b },
			"sub":  func(a, b int) int { return a - b },
			"mul":  func(a, b int) int { return a * b },
			"join": strings.Join,
			"trim": strings.TrimSpace,
			"pad5": func(i int) string { return fmt.Sprintf("%05d", i) },
		}).
		Parse(src)
	if err != nil {
		return nil, fmt.Errorf("prompts: parse %q: %w", name, err)
	}
	templateCache.Store(name, tpl)
	return tpl, nil
}

// ResetTemplateCache clears the parsed-template cache.
// Tests use it to assert the parsing path; production
// code does not need it.
func ResetTemplateCache() {
	templateCache.Range(func(k, _ any) bool {
		templateCache.Delete(k)
		return true
	})
}

// RenderSummarizerUser renders a summarizer user-message
// template with ONLY the SummarizerData sub-struct
// populated. Unlike Render (which takes a full PromptData
// built from a NarrativeConfigSnapshot), this entry point
// is for per-call user messages where the config-snapshot
// is irrelevant — the user message depends on raw inputs
// (world, day, file bodies, messages) that the Summarizer
// collects at call time. The rest of PromptData is left
// zero; the summarizer user templates only reference
// {{ .Summarizer.* }}.
func RenderSummarizerUser(name string, sum *SummarizerData) (string, error) {
	data := PromptData{Summarizer: sum}
	return Render(name, data)
}
