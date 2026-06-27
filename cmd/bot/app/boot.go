package app

import (
	"fmt"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/gitops"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/llmclient"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/summarizertools"
	"github.com/bestxp/narrative-ai-agent/internal/config"
	"github.com/bestxp/narrative-ai-agent/internal/dispatcher"
	"github.com/bestxp/narrative-ai-agent/internal/logging"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/usecase"
	"github.com/rs/zerolog"
)

// boot loads the config, builds the slowlog file, and
// returns the wired logger. Every later subsystem in New
// reads from these three values; keeping the sequence in
// one helper makes the "what runs first" story explicit.
func boot(configPath, logLevel string, logPretty, disableLLM bool) (*config.Config, zerolog.Logger, *slowlog.Logger, error) {
	cfg, err := bootConfig(configPath)
	if err != nil {
		return nil, zerolog.Logger{}, nil, fmt.Errorf("config load: %w", err)
	}

	slow, slowWriter, err := buildSlowlog(cfg, logging.New(logging.Config{
		Level:  orDefault(logLevel, "info"),
		Pretty: logPretty,
	}))
	if err != nil {
		return nil, zerolog.Logger{}, nil, fmt.Errorf("slowlog init: %w", err)
	}

	log := bootLog(cfg, logLevel, logPretty, cfg.Slowlog.Enabled, slowWriter)
	log.Info().Str("config", configPath).Bool("no_llm", disableLLM).Msg("starting lazy-universe bot")

	if !gitops.IsRepo(cfg.Paths.GitWorkdir) {
		log.Warn().Str("workdir", cfg.Paths.GitWorkdir).Msg("not a git repo — commits will fail")
	}

	return cfg, log, slow, nil
}

// wireDomain builds the domain layer: storage, file
// toolset, GM, dispatcher, git operator. Everything except
// the messaging transports lives here. The bool
// disableLLM is passed through so the dispatcher can skip
// AttachGM when the operator wants a validation-only run.
func wireDomain(
	cfg *config.Config,
	role config.LLMRoleConfig,
	prov config.ProviderConfig,
	systemPrompt string,
	llmCli *llmclient.Driver,
	summarizer *usecase.Summarizer,
	slots summarizertools.Slots,
	slow *slowlog.Logger,
	log zerolog.Logger,
	disableLLM bool,
) (*storage.FileStore, *api.Repositories, *gitops.Operator, *usecase.GM, *usecase.SystemState, *dispatcher.Dispatcher) {
	fs, absData := buildStorage(cfg, log)
	fileTools, repos := buildFileToolset(fs, absData, slots, slow, log)
	gitOp := buildGit(cfg, log)
	gm, sysSt := buildGM(cfg, role, prov, systemPrompt, fs, llmCli, fileTools, repos, summarizer, slow, log)
	disp := buildDispatcher(cfg, fs, gitOp, fileTools, slow, log)

	if disableLLM {
		log.Warn().Msg("gm disabled via --no-llm; freeform will echo + validate only")
	}

	return fs, repos, gitOp, gm, sysSt, disp
}
