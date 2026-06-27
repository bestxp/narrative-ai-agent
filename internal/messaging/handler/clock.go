package handler

import "time"

//nolint:gochecknoglobals // package-level clock seam; tests override it directly
var clock = func() time.Time { return time.Now().UTC() }

// nowUTC returns the current UTC time. The seam exists for
// tests that want to pin the "now" value passed to
// system_state.md audit writes.
func nowUTC() time.Time {
	return clock()
}
