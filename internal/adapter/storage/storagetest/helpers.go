// Package storagetest provides shared test helpers for the
// storage adapter and its downstream packages. It exists so
// storage_test files can live in the black-box storage_test
// package while still sharing a single buffer-backed logger.
package storagetest

import (
	"strings"

	"github.com/rs/zerolog"
)

// NewBufLogger returns a zerolog.Logger that writes JSON to buf
// at the requested level. Used by storage tests to assert
// per-write log events without leaking the test helper into
// the production package surface.
func NewBufLogger(buf *strings.Builder, level string) zerolog.Logger {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		lvl = zerolog.InfoLevel
	}

	return zerolog.New(buf).Level(lvl)
}
