package usecase

import (
	"os"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"narrative/internal/adapter/storage"
	"narrative/internal/domain"
)

// newBufLogger returns a JSON zerolog logger that writes to the given
// buffer. It is shared by every usecase test that needs to assert on
// emitted log records.
func newBufLogger() (zerolog.Logger, *strings.Builder) {
	var buf strings.Builder
	return zerolog.New(&buf), &buf
}

// discardLogger returns a zerolog logger that drops everything. Use
// it when the test only cares about the behaviour under test, not
// the log output.
func discardLogger() zerolog.Logger {
	return zerolog.Nop()
}

// readWhole is a tiny helper for the few tests that need to
// inspect a slowlog or other side-channel file. Using os.ReadFile
// directly in the body works too — the helper just keeps the test
// code symmetrical with the production code that uses io.
func readWhole(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// seedWorld bootstraps an empty `world` directory so tests
// that touch state.md / plan.md / memorise.md can use a known
// starting point. It writes empty stubs for every file the
// toolset may read; the tests then populate what they need.
func seedWorld(t *testing.T, fs *storage.FileStore, world string) {
	t.Helper()
	require.NoError(t, fs.EnsureDir("worlds/"+world+"/characters"))
	require.NoError(t, fs.WriteRawAtomic(storage.InfoFile, domain.BuildInfo("markus", world, nil, nil)))
	require.NoError(t, fs.WriteRawAtomic("worlds/"+world+"/state.md", ""))
	require.NoError(t, fs.WriteRawAtomic("worlds/"+world+"/plan.md", ""))
	require.NoError(t, fs.WriteRawAtomic("worlds/"+world+"/memorise.md", ""))
	require.NoError(t, fs.WriteRawAtomic("worlds/"+world+"/lore.md", ""))
	require.NoError(t, fs.WriteRawAtomic("worlds/"+world+"/canon.md", ""))
}
