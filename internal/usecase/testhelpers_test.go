package usecase

import (
	"os"
	"strings"

	"github.com/rs/zerolog"
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
