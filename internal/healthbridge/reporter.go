// Package healthbridge adapts a slice of messaging.Client (which
// exposes Health() returning messaging.HealthReport) to the
// health.Reporter surface the HTTP probe handler expects.
//
// Why this adapter lives in its own package:
//
//   - The messaging layer and the health layer are two
//     independent packages with no shared base type for the
//     "client status" concept (messaging.HealthState vs.
//     health.Status). The bridge translates one enum into
//     the other and reformats time.Time to RFC3339 string.
//   - main.go originally held the conversion as a tiny
//     struct. Promoting it to a package makes it unit-testable
//     in isolation and lets future transports (e.g. an HTTP
//     webhook) plug in without touching either layer.
package healthbridge

import (
	"time"

	"github.com/bestxp/narrative-ai-agent/internal/health"
	"github.com/bestxp/narrative-ai-agent/internal/messaging"
)

// Reporter is the health.Reporter implementation backed by a
// slice of messaging.Client. The Reports method snapshots the
// current state of every registered client on each call —
// cheap, called only on probe.
type Reporter struct {
	clients []messaging.Client
}

// NewReporter returns a health.Reporter that snapshots each
// client's Health() at probe time. The clients slice is held
// by reference; callers that mutate it after construction
// will see the new clients on subsequent probes (intended
// for tests; production wires a stable slice).
func NewReporter(clients []messaging.Client) *Reporter {
	return &Reporter{clients: clients}
}

// Reports converts each messaging.Client.Health() snapshot to
// the health.Report shape (string enum + RFC3339 timestamp).
// The transport's Name is mapped 1:1 to the report Name field
// so operators see "telegram" / "vk" / "wschat" on the probe
// response.
func (r *Reporter) Reports() []health.Report {
	out := make([]health.Report, 0, len(r.clients))
	for _, c := range r.clients {
		snap := c.Health()

		var startedAt string
		if !snap.StartedAt.IsZero() {
			startedAt = snap.StartedAt.UTC().Format(time.RFC3339)
		}

		out = append(out, health.Report{
			Name:      health.Status(snap.Name),
			State:     health.Status(snap.State),
			StartedAt: startedAt,
			Message:   snap.Message,
		})
	}

	return out
}

// Compile-time check: Reporter satisfies health.Reporter.
// health.Reporter is the interface the HTTP probe calls;
// we rely on a structural match here. golangci-lint catches
// drift via the staticcheck `iface` checker at the
// consumer.
var _ health.Reporter = (*Reporter)(nil)
