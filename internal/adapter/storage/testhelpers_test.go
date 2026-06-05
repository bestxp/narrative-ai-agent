package storage

import (
	"strings"

	"github.com/rs/zerolog"
)

// newBufLogger returns a logger that writes JSON to the given buffer at
// the requested level. It is a test helper exposed for the storage_test
// package via the storage package itself.
func newBufLogger(buf *strings.Builder, level string) zerolog.Logger {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		lvl = zerolog.InfoLevel
	}
	return zerolog.New(buf).Level(lvl)
}
