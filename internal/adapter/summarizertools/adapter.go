// Package summarizertools adapts a *usecase.Summarizer (which
// returns rich result structs with metadata: Compressed,
// BeforeCount, OutputChars, ...) into the four flat
// ([]byte, error) interfaces consumed by the file toolset
// (internal/usecase/tools/files):
//
//   - tools.NPCSummarizer
//   - tools.LoreSummarizer
//   - tools.ChronicleSummarizer
//   - tools.CharacterMemorySummarizer
//
// Why this adapter lives in its own package:
//
//   - main.go originally held the shim. The shim is too
//     mechanical to be "domain logic", and too uniform to
//     warrant one file per type. A package gives us a single
//     place to verify the structural match against all four
//     tools.*Summarizer interfaces and to unit-test the
//     pass-through behaviour in isolation.
//   - Keeps the tools → usecase dependency one-way:
//     usecase.Summarizer is the wide type; the adapter is
//     the narrow seam that the tools package plugs into.
package summarizertools

import (
	"context"
	"fmt"

	"github.com/bestxp/narrative-ai-agent/internal/usecase"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
)

// Compile-time assertions that Adapter satisfies each
// tool interface. Drift here means a tools.*Summarizer
// signature changed; the build will fail loudly.
var (
	_ tools.NPCSummarizer             = (*Adapter)(nil)
	_ tools.LoreSummarizer            = (*Adapter)(nil)
	_ tools.ChronicleSummarizer       = (*Adapter)(nil)
	_ tools.CharacterMemorySummarizer = (*Adapter)(nil)
)

// Adapter wraps a *usecase.Summarizer into the four
// tools.*Summarizer interfaces. All four methods share
// the same underlying *usecase.Summarizer; the adapter
// is what makes the API surface match the per-tool
// type.
type Adapter struct {
	s *usecase.Summarizer
}

// New builds the adapter. A nil summarizer is allowed —
// every method then short-circuits to "no compression".
// This mirrors the behaviour the file toolset needs
// when the operator disabled the summarizer role.
func New(s *usecase.Summarizer) *Adapter {
	return &Adapter{s: s}
}

// Slots is the bag of four tool interfaces that the file
// toolset wants. Bundling them keeps the composition root
// (cmd/bot/app) to a single New call per summarizer role
// instead of four separate WithXxx options.
type Slots struct {
	NPC           tools.NPCSummarizer
	Lore          tools.LoreSummarizer
	Chronicle     tools.ChronicleSummarizer
	CharacterMem  tools.CharacterMemorySummarizer
	HasSummarizer bool
}

// BuildSlots returns a Slots value with all four fields
// pointing at the same adapter. When s == nil the fields
// stay nil; HasSummarizer is false. Callers can pass the
// nil slots straight into NewFileToolset — the toolset
// is responsible for the nil guards (see
// internal/usecase/tools/files/toolset.go).
func BuildSlots(s *usecase.Summarizer) Slots {
	if s == nil {
		return Slots{}
	}

	a := New(s)

	return Slots{
		NPC:           a,
		Lore:          a,
		Chronicle:     a,
		CharacterMem:  a,
		HasSummarizer: true,
	}
}

// SummarizeNPC delegates to the usecase Summarizer and
// flattens the NPCSummaryResult to the (body, error)
// surface the file toolset expects. Non-compressed runs
// (where the model decided the profile was already tight)
// return the original body so the caller can leave the
// file untouched.
func (a *Adapter) SummarizeNPC(
	ctx context.Context,
	displayName, world string,
	yamlBody, chronicleTail []byte,
) ([]byte, error) {
	res, err := a.s.SummarizeNPC(ctx, displayName, world, yamlBody, chronicleTail)
	if err != nil {
		return nil, fmt.Errorf("summarize npc: %w", err)
	}

	return res.Body, nil
}

// SummarizeLore delegates to the usecase Summarizer and
// returns the compacted body (or the input unchanged
// when the model decided lore was already tight).
func (a *Adapter) SummarizeLore(
	ctx context.Context,
	world string,
	loreBody, chronicleTail, stateMD []byte,
) ([]byte, error) {
	res, err := a.s.SummarizeLore(ctx, world, loreBody, chronicleTail, stateMD)
	if err != nil {
		return nil, fmt.Errorf("summarize lore: %w", err)
	}

	return res.Body, nil
}

// SummarizeChronicle delegates to the usecase Summarizer
// and returns the distilled memory text for the window.
// An empty body means "no compression happened"; callers
// treat that as a no-op.
func (a *Adapter) SummarizeChronicle(
	ctx context.Context,
	world string,
	startDay, endDay int,
	fullChronicle string,
) ([]byte, error) {
	res, err := a.s.SummarizeChronicle(ctx, world, startDay, endDay, fullChronicle)
	if err != nil {
		return nil, fmt.Errorf("summarize chronicle: %w", err)
	}

	return res.Body, nil
}

// SummarizeCharacterMemory delegates to the usecase
// Summarizer and returns the new memory.yaml body. The
// caller is responsible for validating parseability —
// the adapter just passes the body through.
func (a *Adapter) SummarizeCharacterMemory(
	ctx context.Context,
	world, character string,
	memoryBody, chronicleTail []byte,
) ([]byte, error) {
	res, err := a.s.SummarizeCharacterMemory(ctx, world, character, memoryBody, chronicleTail)
	if err != nil {
		return nil, fmt.Errorf("summarize character memory: %w", err)
	}

	return res.Body, nil
}
