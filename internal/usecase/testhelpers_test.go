package usecase

import (
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
