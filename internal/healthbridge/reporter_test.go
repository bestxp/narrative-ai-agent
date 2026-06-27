package healthbridge_test

import (
	"context"
	"testing"
	"time"

	"github.com/bestxp/narrative-ai-agent/internal/healthbridge"
	"github.com/bestxp/narrative-ai-agent/internal/messaging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubClient implements messaging.Client with just enough
// surface for the healthbridge tests. Unused methods panic
// if accidentally called — the bridge only touches Health().
type stubClient struct {
	name string
	snap messaging.HealthReport
}

func (s *stubClient) Name() string                   { return s.name }
func (s *stubClient) Run(_ context.Context) error    { return nil }
func (s *stubClient) Health() messaging.HealthReport { return s.snap }
func (s *stubClient) Send(_ context.Context, _ messaging.OutgoingMessage) error {
	return nil
}

//nolint:ireturn // stub returns interface to satisfy messaging.Client
func (s *stubClient) StartStream(_ context.Context, _ string, _ int) (messaging.StreamSession, error) {
	return nil, messaging.ErrStreamingDisabled
}
func (s *stubClient) IsAllowed(_ string) bool { return true }
func (s *stubClient) SetCommands(_ context.Context, _ []messaging.BotCommand) error {
	return nil
}

func TestNewReporter_NilClients(t *testing.T) {
	t.Parallel()

	r := healthbridge.NewReporter(nil)
	reports := r.Reports()
	assert.Empty(t, reports, "nil clients must produce empty report slice, not nil")
}

func TestReporter_MapsNameAndState(t *testing.T) {
	t.Parallel()

	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	clients := []messaging.Client{
		&stubClient{name: "telegram", snap: messaging.HealthReport{
			Name: "telegram", State: messaging.StateConnected, StartedAt: stamp,
		}},
		&stubClient{name: "vk", snap: messaging.HealthReport{
			Name: "vk", State: messaging.StateReconnect, Message: "retrying",
		}},
	}

	r := healthbridge.NewReporter(clients)
	reports := r.Reports()

	require.Len(t, reports, 2)

	assert.Equal(t, "telegram", string(reports[0].Name))
	assert.Equal(t, "connected", string(reports[0].State))
	assert.Equal(t, "2026-01-02T03:04:05Z", reports[0].StartedAt)
	assert.Empty(t, reports[0].Message)

	assert.Equal(t, "vk", string(reports[1].Name))
	assert.Equal(t, "reconnecting", string(reports[1].State))
	assert.Empty(t, reports[1].StartedAt, "zero StartedAt must omit the timestamp")
	assert.Equal(t, "retrying", reports[1].Message)
}
