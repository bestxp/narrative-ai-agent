package app

import (
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/config"
	"github.com/bestxp/narrative-ai-agent/internal/dispatcher"
	"github.com/bestxp/narrative-ai-agent/internal/messaging"
	"github.com/bestxp/narrative-ai-agent/internal/messaging/handler"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// NewForTest builds a minimal App wired with the given
// transport clients. Used by app_test.go (package
// app_test) to exercise the Run / Shutdown path without
// loading config.yaml, slowlog, LLM driver or GM.
//
// Production code must use NewApp instead.
func NewForTest(t *testing.T, clients ...messaging.Client) *App {
	t.Helper()

	log := zerolog.New(zerolog.NewConsoleWriter()).Level(zerolog.WarnLevel)

	h, err := handler.New(
		&config.Config{},
		&dispatcher.Dispatcher{},
		log,
	)
	require.NoError(t, err)

	return &App{
		log:     log,
		disp:    &dispatcher.Dispatcher{},
		pool:    messaging.NewMultiClient(clients...),
		clients: clients,
		hsys:    &healthBridge{},
		handler: h,
	}
}
