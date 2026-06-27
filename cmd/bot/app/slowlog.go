package app

import (
	"fmt"
	"io"

	"github.com/bestxp/narrative-ai-agent/internal/config"
	"github.com/bestxp/narrative-ai-agent/internal/logging"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/rs/zerolog"
)

// buildSlowlog picks between File-mode and Discard based on
// the config. The path is opened in append mode; the parent
// directory is created if missing.
//
// It returns both the slowlog.Logger (for structured events)
// and a logging.SlowlogWriter (for zerolog console→slowlog
// duplication). When slowlog is disabled both are no-ops.
func buildSlowlog(cfg *config.Config, log zerolog.Logger) (*slowlog.Logger, *logging.SlowlogWriter, error) {
	if !cfg.Slowlog.Enabled {
		log.Info().Msg("slowlog disabled (config: slowlog.enabled=false)")

		return slowlog.Discard(), logging.NewSlowlogWriter(io.Discard), nil
	}

	sl, err := slowlog.File(cfg.Slowlog.File)
	if err != nil {
		return nil, nil, fmt.Errorf("slowlog file open: %w", err)
	}
	// The SlowlogWriter wraps the same *os.File so that
	// zerolog's MultiWriter and slowlog.Logger.Write()
	// both serialize through their own mutexes — no
	// interleaving between JSON-line entries and
	// slowlog's own structured events.
	return sl, logging.NewSlowlogWriter(sl.Writer()), nil
}
