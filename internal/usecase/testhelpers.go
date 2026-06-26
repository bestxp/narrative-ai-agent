package usecase

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/rs/zerolog"
)

// NewBufLogger returns a JSON zerolog logger that writes to a
// scratch buffer. It is shared by every usecase test that needs
// to assert on emitted log records; the buffer is not exposed
// because no usecase test currently inspects it directly.
//
// Exported (DiscardLogger / NewBufLogger / ReadWhole / SeedWorld)
// so black-box tests in `package usecase_test` can reuse the
// same fixtures without duplication. The functions are
// intentionally tiny and incur zero runtime cost outside tests.
func NewBufLogger() zerolog.Logger {
	var buf strings.Builder

	return zerolog.New(&buf)
}

// DiscardLogger returns a zerolog logger that drops everything.
// Use it when the test only cares about the behaviour under
// test, not the log output.
func DiscardLogger() zerolog.Logger {
	return zerolog.Nop()
}

// ReadWhole is a tiny helper for the few tests that need to
// inspect a slowlog or other side-channel file. Using
// os.ReadFile directly in the body works too — the helper just
// keeps the test code symmetrical with the production code that
// uses io.
func ReadWhole(path string) ([]byte, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read_whole: %w", err)
	}

	return body, nil
}

// SeedWorld bootstraps an empty `world` directory so tests that
// touch state.md / plan.md / chronicle.yaml can use a known
// starting point. It writes empty stubs for every file the
// toolset may read; the tests then populate what they need.
//
// The function takes a *testing.T-shaped helper so it can call
// .Helper() without dragging the testing package into this
// non-_test file. The actual type assertion happens at the call
// site (caller passes a real *testing.T).
func SeedWorld(t testingTB, fs *storage.FileStore) {
	t.Helper()

	world := "naruto"
	_ = fs.EnsureDir("worlds/" + world + "/characters")
	_ = fs.WriteRawAtomic(storage.InfoFile, domain.BuildInfo("markus", world, nil, nil))
	_ = fs.WriteRawAtomic("worlds/"+world+"/state.md", "")
	_ = fs.WriteRawAtomic("worlds/"+world+"/plan.md", "")
	_ = fs.WriteRawAtomic("worlds/"+world+"/lore.md", "")
	_ = fs.WriteRawAtomic("worlds/"+world+"/canon.md", "")
}

// testingTB is a minimal subset of *testing.T used by
// SeedWorld so this helper can live in a non-_test.go file
// without pulling the testing package into the production
// binary.
type testingTB interface {
	Helper()
}

// FakeLLM replays a deterministic sequence of chunks. The test
// declares what the LLM should do on the first (and optionally)
// later stream call, including any tool calls. Lifted from
// gm_test.go so both white-box (`package usecase`) and
// black-box (`package usecase_test`) tests can share it.
//
// Exported so black-box tests can construct it via
// `&usecase.FakeLLM{}` and assign into `usecase.FakeChunk`
// slices for `.rounds`.
type FakeLLM struct {
	mu     sync.Mutex
	calls  int
	rounds [][]FakeChunk
}

// FakeChunk is the per-chunk payload used by FakeLLM and by
// tests that build round scripts (e.g. compaction tests).
type FakeChunk struct {
	Content  string
	ToolName string
	ToolArgs string
	ToolID   string
	Finish   string
	Usage    llm.Usage
}

// Stream implements the LLMClient interface by replaying the
// pre-recorded chunk sequence for the current call index.
func (f *FakeLLM) Stream(_ context.Context, _ llm.ChatRequest, onChunk func(llm.Chunk) error) error {
	f.mu.Lock()
	idx := f.calls
	f.calls++

	var round []FakeChunk
	if idx < len(f.rounds) {
		round = f.rounds[idx]
	}
	f.mu.Unlock()

	for _, fc := range round {
		ch := llm.Chunk{Content: fc.Content, Finish: fc.Finish, Usage: fc.Usage}
		if fc.ToolName != "" {
			ch.ToolCalls = []llm.ToolCall{{
				ID:       fc.ToolID,
				Type:     "function",
				Function: llm.FunctionCall{Name: fc.ToolName, Arguments: fc.ToolArgs},
			}}
		}

		if err := onChunk(ch); err != nil {
			return err
		}
	}

	return onChunk(llm.Chunk{Done: true})
}

// Calls returns the number of Stream invocations so far.
// Helper for tests that need to read the counter without
// racing against the mutex.
func (f *FakeLLM) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}
