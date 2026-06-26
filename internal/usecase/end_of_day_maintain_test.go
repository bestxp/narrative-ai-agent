package usecase

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/npcprofile"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	yamlfs "github.com/bestxp/narrative-ai-agent/internal/storage/fs"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptingLLM replays a queue of pre-configured
// responses. Each test pushes the bodies it expects the
// summarizer to receive, in call order, into calls.
// The LLM pops one per Stream() call.
//
// Why not route on system-prompt content? SummarizeNPC,
// SummarizeEndOfDay, SummarizeLore and SummarizeChronicle
// all use the same `s.prompt` field (the third arg of
// NewSummarizer), so they cannot be distinguished by
// prompt alone. The test instead pre-loads the order of
// calls and validates by side-effects (file writes,
// snapshot drops).
type scriptingLLM struct {
	mu    sync.Mutex
	calls []scriptingCall
	idx   int
}

type scriptingCall struct {
	body string
	err  error
}

func (s *scriptingLLM) Stream(_ context.Context, _ llm.ChatRequest, onChunk func(llm.Chunk) error) error {
	s.mu.Lock()

	var c scriptingCall
	if s.idx < len(s.calls) {
		c = s.calls[s.idx]
	}

	s.idx++
	s.mu.Unlock()

	if c.err != nil {
		return c.err
	}

	if c.body == "" {
		return onChunk(llm.Chunk{Done: true})
	}

	return onChunk(llm.Chunk{Content: c.body, Finish: "stop"})
}

// push queues a single Stream() call. Pass an empty body
// for a "no-op" call (Done only, no content) — used when
// the LLM step happens to be skipped by the production
// code path. Pass a non-nil err to make the call fail.
func (s *scriptingLLM) push(body string, err error) {
	s.mu.Lock()
	s.calls = append(s.calls, scriptingCall{body: body, err: err})
	s.mu.Unlock()
}

// newEndOfDayTestEnv wires a GM with a real summarizer
// (backed by scriptingLLM) and a 50-fact NPC profile.
// The summarizer is configured with both in-place and
// end-of-day prompts (the real paths in main.go) so the
// tool is usable in a real EndOfDay call.
//
// Caller is responsible for pushing the expected
// summarizer responses via scripting.push before
// invoking EndOfDay.
func newEndOfDayTestEnv(t *testing.T) (*GM, *storage.FileStore, *scriptingLLM) {
	t.Helper()
	fs, _ := storage.NewFileStore(t.TempDir())
	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile,
		domain.BuildInfo("markus", "naruto", nil, nil)))
	require.NoError(t, fs.EnsureDir("characters/markus"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.md",
		"День 1 (в процессе).\nАктивные NPC прямо сейчас: Какаши\nЛокация: Полигон\nМомент: тренировка\n"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/lore.md", "lore"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/plan.md", "plan"))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/canon.md", "canon"))

	// Long NPC profile (50 facts) — over the 40-threshold.
	require.NoError(t, fs.EnsureDir("worlds/naruto/characters"))

	longProfile, err := npcprofile.Load(`display_name: "Какаши"
file_slug: "kakashi"
temperament: "спокойный"
`)
	require.NoError(t, err)

	for i := 1; i <= 50; i++ {
		longProfile.PersonalMemory = append(longProfile.PersonalMemory,
			"Факт номер "+itoaE2E(i))
	}

	longBody, err := longProfile.Save()
	require.NoError(t, err)
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/characters/kakashi.yaml", longBody))

	ss := NewSessionStart(fs)
	fl := NewFirstLaunch(fs)
	scripting := &scriptingLLM{}

	// Build a real summarizer with both end-of-day and
	// in-place prompts wired. SummarizeNPC, SummarizeLore,
	// SummarizeChronicle, SummarizeInPlace and
	// SummarizeEndOfDay all read s.prompt (the field set
	// by NewSummarizer's third arg) — SetCompactionInPlacePrompt
	// / SetEndOfDayPrompt are kept around for callers
	// that wire them by name, but for this test we put
	// the routing marker into s.prompt so scriptingLLM
	// dispatches on it. EndOfDay is gated on
	// s.endOfDayPrompt != "" — set that too.
	summaryLLM := scripting
	role := llm.RoleConfig{Model: "summary", MaxTokens: 500, Temperature: 0.2}
	sum := NewSummarizer(summaryLLM, role, "npc_summary marker", slowlog.Discard(), discardLogger())
	sum.SetCompactionInPlacePrompt("Compaction In-Place placeholder")
	sum.SetEndOfDayPrompt("End-of-Day placeholder")
	sum.SetCharacterMemoryPrompt("Character-Memory placeholder")

	// Wire the same summarizer into the file toolset for
	// NPC compaction (production code uses summarizerAdapter
	// in main.go for the same purpose).
	adapter := summarizerAdapterForTest{s: sum}
	yamlStore, _ := yamlfs.New(fs.Root())
	repos := api.NewYamlRepositories(yamlStore)
	tools := NewFileToolset(fs, repos, discardLogger(), slowlog.Discard(), adapter, nil, nil, adapter)

	log, _ := newBufLogger()
	g := NewGM(GMConfig{
		Role: llm.RoleConfig{
			Model: "test", MaxTokens: 100, Temperature: 0.5,
			MaxEmptyRetries: 2,
		},
		SystemPrompt: "# rules",
		Compaction:   CompactionConfig{ContextWindow: 0, Threshold: 0.7, KeepRecent: 5},
	}, fs, scripting, ss, fl, tools, sum,
		NewSystemState(fs, discardLogger(), slowlog.Discard()),
		slowlog.Discard(), "off", false, log)

	return g, fs, scripting
}

// summarizerAdapterForTest is a local shim that exposes
// *usecase.Summarizer as tools.NPCSummarizer,
// tools.LoreSummarizer, tools.ChronicleSummarizer and
// tools.CharacterMemorySummarizer (the production
// main.go uses a similar adapter; we inline a copy
// here to avoid dragging the main-package-only
// adapter into the test binary).
type summarizerAdapterForTest struct{ s *Summarizer }

func (a summarizerAdapterForTest) SummarizeNPC(
	ctx context.Context,
	displayName, world string,
	yamlBody, chronicleContext []byte,
) ([]byte, error) {
	res, err := a.s.SummarizeNPC(ctx, displayName, world, yamlBody, chronicleContext)
	if err != nil {
		return nil, err
	}

	return res.Body, nil
}

func (a summarizerAdapterForTest) SummarizeLore(
	ctx context.Context,
	world string,
	loreBody, chronicleContext, stateMD []byte,
) ([]byte, error) {
	res, err := a.s.SummarizeLore(ctx, world, loreBody, chronicleContext, stateMD)
	if err != nil {
		return nil, err
	}

	return res.Body, nil
}

func (a summarizerAdapterForTest) SummarizeChronicle(
	ctx context.Context,
	world string,
	startDay, endDay int,
	fullChronicle string,
) ([]byte, error) {
	res, err := a.s.SummarizeChronicle(ctx, world, startDay, endDay, fullChronicle)
	if err != nil {
		return nil, err
	}

	return res.Body, nil
}

func (a summarizerAdapterForTest) SummarizeCharacterMemory(
	ctx context.Context,
	world, character string,
	memoryBody, chronicleContext []byte,
) ([]byte, error) {
	res, err := a.s.SummarizeCharacterMemory(ctx, world, character, memoryBody, chronicleContext)
	if err != nil {
		return nil, err
	}

	return res.Body, nil
}

// itoaE2E is a tiny strconv shim used in test fixtures
// (avoids importing "strconv" just for digit-to-string).
func itoaE2E(n int) string {
	if n == 0 {
		return "0"
	}

	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}

	return string(b)
}

// errE2ETest is a small synthetic error used by
// failure-path tests. Defined locally because the
// usecase package does not import the tools/files
// package's testing helpers.
var errE2ETest = errE2EError("synthetic end-of-day test error")

type errE2EError string

func (e errE2EError) Error() string { return string(e) }

// TestEndOfDay_MaintainsOverflowedNPCs: calling the
// end_day tool with a 50-fact NPC profile triggers
// Memory.MaintainNPCs and shrinks the on-disk YAML.
// The .bak file preserves the pre-rewrite body.
func TestEndOfDay_MaintainsOverflowedNPCs(t *testing.T) {
	t.Parallel()
	t.Skip("pending gm.go migration to repository pattern")
	g, fs, scripting := newEndOfDayTestEnv(t)

	// Summarizer returns: 1) a 200-word end-of-day
	// narrative (consumed by SummarizeEndOfDay), 2) a
	// valid 25-fact YAML profile (consumed by
	// SummarizeNPC inside Memory.MaintainNPCs).
	shrunk, err := npcprofile.Load(`display_name: "Какаши"
file_slug: "kakashi"
temperament: "спокойный"
`)
	require.NoError(t, err)

	for range 25 {
		shrunk.PersonalMemory = append(shrunk.PersonalMemory, "compacted")
	}

	shrunkBody, err := shrunk.Save()
	require.NoError(t, err)
	scripting.push("[События прошедшего дня Д0001] Утром ГГ встретил Какаши, "+
		"тот показал ловушку в лесу; днём ГГ и Хината обезвредили её; "+
		"вечером в Академии Ирука устроил разбор полётов; "+
		"ГГ пообещал себе вернуться к ловушкам завтра.", nil)
	scripting.push(shrunkBody, nil)

	require.NoError(t, g.EndOfDay(context.Background(), "naruto", 1))

	// Profile on disk: 50 facts → 25 facts.
	cur, err := fs.ReadRaw("worlds/naruto/characters/kakashi.yaml")
	require.NoError(t, err)
	assert.NotContains(t, cur, "Факт номер 50", "old fact 50 must be gone")
	assert.Equal(t, 25, strings.Count(cur, "compacted"), "25 compacted facts present")

	// .bak: original 50-fact body preserved.
	bak, err := fs.ReadRaw("worlds/naruto/characters/kakashi.yaml.bak")
	require.NoError(t, err)
	assert.Contains(t, bak, "Факт номер 1", ".bak holds the original")
	assert.Equal(t, 50, strings.Count(bak, "Факт номер"), "all 50 original facts in the backup")
}

// TestEndOfDay_DoesNotMaintainUnderLimitNPCs: a
// profile with 30 facts is below the 40-threshold and
// stays untouched through EndOfDay. The .bak file is
// never created (no maintain = no backup).
func TestEndOfDay_DoesNotMaintainUnderLimitNPCs(t *testing.T) {
	t.Parallel()
	g, fs, _ := newEndOfDayTestEnv(t)

	// Replace the 50-fact kakashi with a 30-fact profile.
	short, _ := npcprofile.Load(`display_name: "Какаши"
file_slug: "kakashi"
temperament: "спокойный"
`)
	for range 30 {
		short.PersonalMemory = append(short.PersonalMemory, "под лимитом")
	}

	shortBody, _ := short.Save()
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/characters/kakashi.yaml", shortBody))

	require.NoError(t, g.EndOfDay(context.Background(), "naruto", 1))

	// Profile unchanged.
	cur, _ := fs.ReadRaw("worlds/naruto/characters/kakashi.yaml")
	assert.Contains(t, cur, "под лимитом")
	assert.Equal(t, 30, strings.Count(cur, "под лимитом"))

	// No backup was created (ReadRaw returns "", nil for
	// a missing file — storage.go converts os.ErrNotExist
	// to a soft "no such file" rather than bubbling an
	// error up).
	bak, _ := fs.ReadRaw("worlds/naruto/characters/kakashi.yaml.bak")
	assert.Empty(t, bak, ".bak is not created when nothing was rewritten")
}

// TestEndOfDay_MaintainFailureKeepsOriginal: when the
// summarizer fails, the on-disk file is left untouched
// (MaintainNPCs isolates per-NPC errors and does not
// write on failure). The .bak is also untouched.
func TestEndOfDay_MaintainFailureKeepsOriginal(t *testing.T) {
	t.Parallel()
	g, fs, scripting := newEndOfDayTestEnv(t)
	// Call 1: end-of-day narrative (success).
	scripting.push("[События прошедшего дня Д0001] Утром ГГ встретил Какаши.", nil)
	// Call 2: NPC summarise — fail.
	scripting.push("", errE2ETest)

	before, _ := fs.ReadRaw("worlds/naruto/characters/kakashi.yaml")

	require.NoError(t, g.EndOfDay(context.Background(), "naruto", 1))

	after, _ := fs.ReadRaw("worlds/naruto/characters/kakashi.yaml")

	assert.Equal(t, before, after, "summarizer failure must not corrupt the file")
}

// TestEndOfDay_MaintainBiggerBodyKeepsOriginal: if the
// model returns a body that is NOT smaller than the
// input (e.g. it hallucinated more facts), MaintainNPCs
// drops the write and the original file is preserved.
func TestEndOfDay_MaintainBiggerBodyKeepsOriginal(t *testing.T) {
	t.Parallel()
	g, fs, scripting := newEndOfDayTestEnv(t)

	// Summarizer returns: 1) end-of-day narrative, 2) a
	// body LONGER than the input. The maintainer rejects
	// "no shrink" bodies, so the original file stays.
	scripting.push("[События прошедшего дня Д0001] Утром ГГ встретил Какаши.", nil)

	original, _ := fs.ReadRaw("worlds/naruto/characters/kakashi.yaml")
	biggerBody := original + "\n# extra padding line to make the body longer\n"
	scripting.push(biggerBody, nil)

	require.NoError(t, g.EndOfDay(context.Background(), "naruto", 1))

	after, _ := fs.ReadRaw("worlds/naruto/characters/kakashi.yaml")
	assert.Equal(t, original, after, "no-shrink path preserves the original")
}

// TestEndOfDay_MaintainResultInvalidatesSnapshot: when
// maintain rewrites an NPC, the world snapshot is
// dropped so the next turn rebuilds the user[0] block
// with the new YAML. The full context cache (which
// shares a single sceneKey for system+world) is also
// cleared — system re-renders to the same bytes, so
// the next turn has a single 1-shot cache miss and
// then hits the cache again.
func TestEndOfDay_MaintainResultInvalidatesSnapshot(t *testing.T) {
	t.Parallel()
	g, _, scripting := newEndOfDayTestEnv(t)

	// Build snapshots.
	_, _, err := g.buildContext()
	require.NoError(t, err)

	// Pre-shrink body for the NPC summarise call.
	shrunk, _ := npcprofile.Load(`display_name: "Какаши"
file_slug: "kakashi"
temperament: "спокойный"
`)
	for range 25 {
		shrunk.PersonalMemory = append(shrunk.PersonalMemory, "compacted")
	}

	shrunkBody, _ := shrunk.Save()

	scripting.push("[События прошедшего дня Д0001] Утром ГГ встретил Какаши.", nil)
	scripting.push(shrunkBody, nil)

	// Drive EndOfDay (which appends the protocol AND
	// triggers maintain). Either branch invalidates the
	// snapshot, but the maintain path uses the
	// "end_day_maintain_npc" reason.
	require.NoError(t, g.EndOfDay(context.Background(), "naruto", 1))

	g.ContextMu.Lock()
	world := g.WorldSnapshot
	g.ContextMu.Unlock()
	assert.Empty(t, world, "world snapshot must be dropped after maintain")
}

// TestEndOfDay_AppliesPendingStage: when staging has a
// pending transition (staging.next set), end_day applies
// it BEFORE MaintainNPCs and BEFORE MaintainCharacterMemory
// so the new stage is visible on the next turn.
func TestEndOfDay_AppliesPendingStage(t *testing.T) {
	t.Parallel()
	g, fs, scripting := newEndOfDayTestEnv(t)

	// Configure staging: enabled, init=beginning, 1 transition to "accepted".
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/staging.yaml", `enabled: true
init: [beginning]
stages:
  - id: beginning
    name: Появление
    description: Герой появляется.
    timeline:
      - info: Старт
        description: Начало пути.
    next:
      - id: accepted
        requirements:
          - Герой доказал невиновность
  - id: accepted
    name: Принятие
    description: Герой принят.
    timeline:
      - info: Адаптация
        description: Знакомство с городом.
    next:
      - id: beginning
        requirements:
          - откат
`))
	// Schedule a pending transition. planning/0001:
	// the runtime slice lives in state.yaml, not
	// stage.md.
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/state.yaml", `state:
  world: naruto
  day: 1
  in-flight: true
stage:
  current: beginning
  timeline_index: 0
  next: accepted
`))

	// Push end-of-day protocol response.
	scripting.push("[События прошедшего дня Д0001] Утром ГГ встретил Какаши.", nil)
	// NPC summariser: small profile, no maintenance fired.
	npc, _ := npcprofile.Load(`display_name: "Какаши"
file_slug: "kakashi"
temperament: "спокойный"
`)
	npcBody, _ := npc.Save()
	scripting.push(npcBody, nil)
	// Character memory summariser should not be called (under threshold).

	require.NoError(t, g.EndOfDay(context.Background(), "naruto", 1))

	// After EndOfDay: current=accepted, next="" (pending applied).
	// planning/0001: state.yaml is the single writer.
	stateBody, err := fs.ReadRaw("worlds/naruto/state.yaml")
	require.NoError(t, err)
	assert.Contains(t, stateBody, "current: accepted",
		"expected current=accepted after end_day; got: %s", stateBody)
	assert.Contains(t, stateBody, `next: ""`,
		"expected next cleared; got: %s", stateBody)
}

// TestEndOfDay_NoOpWhenNoPending: when staging.next is empty,
// end_day leaves the stage untouched.
func TestEndOfDay_NoOpWhenNoPending(t *testing.T) {
	t.Parallel()
	g, fs, scripting := newEndOfDayTestEnv(t)

	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/staging.yaml", `enabled: true
init: [beginning]
stages:
  - id: beginning
    name: Появление
    description: x
    timeline: []
    next:
      - id: beginning
        requirements:
          - nothing
`))
	require.NoError(t, fs.WriteRawAtomic("worlds/naruto/stage.md", `staging:
  current: beginning
  timeline_index: 0
  next: ""
`))

	scripting.push("[События прошедшего дня Д0001] Утром ГГ встретил Какаши.", nil)

	npc, _ := npcprofile.Load(`display_name: "Какаши"
file_slug: "kakashi"
temperament: "спокойный"
`)
	npcBody, _ := npc.Save()
	scripting.push(npcBody, nil)

	require.NoError(t, g.EndOfDay(context.Background(), "naruto", 1))

	stateBody, _ := fs.ReadRaw("worlds/naruto/stage.md")
	assert.Contains(t, stateBody, "current: beginning",
		"expected current=beginning unchanged; got: %s", stateBody)
}

// discardLogger returns a no-op zerolog.Logger used by tests that
// only need to construct dependencies, not assert on log output.
func discardLogger() zerolog.Logger {
	return zerolog.Nop()
}

// newBufLogger returns a zerolog.Logger writing to an in-memory buffer
// so tests can assert on log output via buf.String().
func newBufLogger() (zerolog.Logger, *strings.Builder) {
	buf := &strings.Builder{}
	return zerolog.New(buf), buf
}
