package main

import (
	"context"
	"path/filepath"
	"sync/atomic"

	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/gitops"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
	llmanthropic "github.com/bestxp/narrative-ai-agent/internal/adapter/llm/anthropic"
	llmopenai "github.com/bestxp/narrative-ai-agent/internal/adapter/llm/openai"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/config"
	"github.com/bestxp/narrative-ai-agent/internal/dispatcher"
	"github.com/bestxp/narrative-ai-agent/internal/messaging"
	"github.com/bestxp/narrative-ai-agent/internal/messaging/telegram"
	vktransport "github.com/bestxp/narrative-ai-agent/internal/messaging/vk"
	"github.com/bestxp/narrative-ai-agent/internal/messaging/wschat"
	promptpkg "github.com/bestxp/narrative-ai-agent/internal/prompts"
	"github.com/bestxp/narrative-ai-agent/internal/repository/api"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	yamlfs "github.com/bestxp/narrative-ai-agent/internal/storage/fs"
	"github.com/bestxp/narrative-ai-agent/internal/usecase"
	"github.com/bestxp/narrative-ai-agent/internal/usecase/tools"
	files "github.com/bestxp/narrative-ai-agent/internal/usecase/tools/files"
)

// buildStorage opens the FileStore at the configured
// data root and returns the absolute path so the YAML
// repository layer can construct its own backend.
func buildStorage(cfg *config.Config, log zerolog.Logger) (*storage.FileStore, string) {
	absData, err := filepath.Abs(cfg.Paths.DataRoot)
	if err != nil {
		log.Fatal().Err(err).Msg("data root")
	}
	fs, err := storage.NewFileStoreWithLogger(absData, log)
	if err != nil {
		log.Fatal().Err(err).Msg("storage init")
	}
	return fs, absData
}

// buildGit wires the git operator. When cfg.Git.Disabled
// is true the operator stays nil and commits/pushes
// no-op.
func buildGit(cfg *config.Config, log zerolog.Logger) *gitops.Operator {
	if cfg.Git.Disabled {
		log.Info().Msg("git disabled (config: git.disabled=true)")
		return nil
	}
	return gitops.NewWithLogger(
		cfg.Paths.GitWorkdir, cfg.Git.Remote, cfg.Git.Branch,
		cfg.Git.CommitAuthor, cfg.Git.CommitEmail,
		cfg.Git.RemoteDisabled, log,
	)
}

// buildLLMDriver selects the primary LLM driver (openai
// or anthropic) per cfg.LLM.Driver and wires the
// narrative role's config.
func buildLLMDriver(cfg *config.Config, role config.LLMRoleConfig, slow *slowlog.Logger, log zerolog.Logger) (llm.Driver, driverClient) {
	rc := llm.RoleConfig{
		APIURL:                   role.APIURL,
		APIKey:                   role.APIKey,
		Model:                    role.Model,
		MaxTokens:                role.MaxTokens,
		UsePrefillBracket:        role.UsePrefillBracket,
		Temperature:              role.Temperature,
		RequestTimeoutSeconds:    role.RequestTimeoutSeconds,
		DisableThinking:          role.DisableThinking,
		ReasoningEffort:          role.ReasoningEffort,
		MaxEmptyRetries:          role.MaxEmptyRetries,
		EmptyRetryTimeoutSeconds: role.EmptyRetryTimeoutSeconds,
	}
	var driver llm.Driver
	switch cfg.LLM.Driver {
	case "", "openai":
		driver = llmopenai.New(rc, log)
		log.Info().Str("driver", "openai").Str("model", role.Model).Msg("llm driver ready")
	case "anthropic":
		driver = llmanthropic.NewWithSlowlog(rc, log, slow)
		log.Info().Str("driver", "anthropic").Str("model", role.Model).Msg("llm driver ready")
	default:
		log.Fatal().Str("driver", cfg.LLM.Driver).Msg("unknown llm.driver; expected openai|anthropic")
	}
	return driver, driverClient{driver: driver}
}

// buildSummarizer wires the dedicated summary role when
// configured, or falls back to the narrative role with
// clamped MaxTokens/Temperature. All five summarizer
// system prompts (summary, compaction_in_place,
// end_of_day, character_memory_maintain,
// chronicle_summary) are rendered once at startup and
// handed to the Summarizer via its setters; the
// compaction knobs are wired through SetCompactionConfig
// so the user-message templates can reference
// {{ .Compaction.* }} instead of hard-coded numbers.
func buildSummarizer(cfg *config.Config, role config.LLMRoleConfig, snap promptpkg.NarrativeConfigSnapshot, slow *slowlog.Logger, log zerolog.Logger) *usecase.Summarizer {
	var sumRole llm.RoleConfig
	var sumPrompt string
	var ok bool
	if sumRoleCfg, hasSummary := cfg.Role("summary"); hasSummary {
		sumRole = llm.RoleConfig{
			APIURL:                   sumRoleCfg.APIURL,
			APIKey:                   sumRoleCfg.APIKey,
			Model:                    sumRoleCfg.Model,
			MaxTokens:                sumRoleCfg.MaxTokens,
			Temperature:              sumRoleCfg.Temperature,
			RequestTimeoutSeconds:    sumRoleCfg.RequestTimeoutSeconds,
			DisableThinking:          sumRoleCfg.DisableThinking,
			ReasoningEffort:          sumRoleCfg.ReasoningEffort,
			MaxEmptyRetries:          sumRoleCfg.MaxEmptyRetries,
			EmptyRetryTimeoutSeconds: sumRoleCfg.EmptyRetryTimeoutSeconds,
		}
		p, err := renderSummarizerPrompt("summary.md.tmpl", snap)
		if err != nil {
			log.Warn().Err(err).Str("path", sumRoleCfg.SystemPromptPath).Msg("summary prompt unreadable; dropping to fallback")
		} else {
			sumPrompt, ok = p, true
		}
		log.Info().Str("model", sumRoleCfg.Model).Msg("summarizer: dedicated role")
	}
	if !ok {
		sumRole = llm.RoleConfig{
			APIURL:                role.APIURL,
			APIKey:                role.APIKey,
			Model:                 role.Model,
			MaxTokens:             role.MaxTokens,
			Temperature:           role.Temperature,
			RequestTimeoutSeconds: role.RequestTimeoutSeconds,
			DisableThinking:       role.DisableThinking,
			ReasoningEffort:       role.ReasoningEffort,
		}
		p, err := renderSummarizerPrompt("summary.md.tmpl", snap)
		if err != nil || p == "" {
			log.Warn().Err(err).Msg("fallback summary prompt unreadable; compaction will be drop-only")
		} else {
			sumPrompt, ok = p, true
		}
		log.Info().Str("model", role.Model).Msg("summarizer: fallback to narrative role (clamped)")
	}
	if !ok {
		return nil
	}
	var sumLLM llm.Driver
	switch cfg.LLM.Driver {
	case "anthropic":
		sumLLM = llmanthropic.New(sumRole, log)
	default:
		sumLLM = llmopenai.New(sumRole, log)
	}
	var summarizer *usecase.Summarizer
	if _, hasSummary := cfg.Role("summary"); hasSummary {
		summarizer = usecase.NewSummarizer(sumLLM, sumRole, sumPrompt, slow, log)
	} else {
		summarizer = usecase.NewFallbackSummarizer(sumLLM, sumRole, sumPrompt, slow, log)
	}
	wireSummarizerPrompt(summarizer, "compaction_in_place.md.tmpl", snap, summarizer.SetCompactionInPlacePrompt, "in-place compaction will no-op", log)
	wireSummarizerPrompt(summarizer, "end_of_day.md.tmpl", snap, summarizer.SetEndOfDayPrompt, "end-of-day protocol will no-op", log)
	wireSummarizerPrompt(summarizer, "character_memory_maintain.md.tmpl", snap, summarizer.SetCharacterMemoryPrompt, "end-of-day memory defrag will no-op", log)
	wireSummarizerPrompt(summarizer, "chronicle_summary.md.tmpl", snap, summarizer.SetChronicleSummaryPrompt, "chronicle window compression falls back to base summary prompt", log)
	summarizerData := promptpkg.NewPromptData(snap, promptpkg.CharacterData{}, promptpkg.WorldData{})
	summarizer.SetCompactionConfig(summarizerData.Compaction)
	return summarizer
}

// wireSummarizerPrompt is the shared helper for the four
// optional summarizer system-prompt setters. A render
// failure logs a warning with the given reason and leaves
// the prompt unwired (the corresponding Summarize* call
// then no-ops or falls back).
func wireSummarizerPrompt(
	s *usecase.Summarizer, name string,
	snap promptpkg.NarrativeConfigSnapshot,
	setter func(string), warnReason string,
	log zerolog.Logger,
) {
	p, err := renderSummarizerPrompt(name, snap)
	if err != nil || p == "" {
		log.Warn().Err(err).Str("template", name).Msg(warnReason)
		return
	}
	setter(p)
}

// summarizerAdapterSlots builds the four tools.*
// summarizer interfaces from a single *usecase.Summarizer
// so NPC, Lore, Chronicle and CharacterMemory compaction
// all share the same LLM role.
type summarizerAdapterSlots struct {
	npc       tools.NPCSummarizer
	lore      tools.LoreSummarizer
	chronicle tools.ChronicleSummarizer
	charMem   tools.CharacterMemorySummarizer
}

func buildSummarizerSlots(s *usecase.Summarizer) summarizerAdapterSlots {
	var slots summarizerAdapterSlots
	if s == nil {
		return slots
	}
	adapter := summarizerAdapter{s: s}
	slots.npc = adapter
	slots.lore = adapter
	slots.chronicle = adapter
	slots.charMem = adapter
	return slots
}

// buildFileToolset constructs the repository-backed
// toolset that implements tools.Tool: state, memory,
// npc, world, stage, character.
func buildFileToolset(
	fs *storage.FileStore, absData string,
	slots summarizerAdapterSlots,
	slow *slowlog.Logger, log zerolog.Logger,
) *files.Toolset {
	yamlStore, _ := yamlfs.New(absData)
	repos := api.NewYamlRepositories(yamlStore)
	ft := usecase.NewFileToolset(fs, repos, log, slow, slots.npc, slots.lore, slots.chronicle, slots.charMem)
	log.Info().Str("source", ft.Source()).Msg("file-backed toolset ready")
	return ft
}

// buildGM constructs the Game Master with its compaction
// config and wires the worldStateInvalidate hook.
func buildGM(
	cfg *config.Config, role config.LLMRoleConfig,
	systemPrompt string,
	fs *storage.FileStore, llmCli driverClient,
	fileTools *files.Toolset, summarizer *usecase.Summarizer,
	slow *slowlog.Logger, log zerolog.Logger,
) *usecase.GM {
	ss := usecase.NewSessionStartWithLogger(fs, log)
	fl := usecase.NewFirstLaunchWithLogger(fs, log)
	sysSt := usecase.NewSystemState(fs, log, slow)
	gm := usecase.NewGM(usecase.GMConfig{
		Role: llm.RoleConfig{
			Model:                    role.Model,
			MaxTokens:                role.MaxTokens,
			UsePrefillBracket:        role.UsePrefillBracket,
			Temperature:              role.Temperature,
			MaxEmptyRetries:          role.MaxEmptyRetries,
			EmptyRetryTimeoutSeconds: role.EmptyRetryTimeoutSeconds,
		},
		SystemPrompt: systemPrompt,
		Compaction: usecase.CompactionConfig{
			ContextWindow: role.ContextWindow,
			Threshold:     role.CompactionThreshold,
			KeepRecent:    role.CompactionKeepRecent,
		},
	}, fs, llmCli, ss, fl, fileTools, summarizer, sysSt, slow, cfg.LLM.TokenTracking, cfg.LLM.IncludeInReply, log)
	fileTools.SetWorldStateInvalidate(gm.InvalidateWorldState)
	return gm
}

// buildDispatcher wires the operator command dispatcher
// on top of the file toolset.
func buildDispatcher(cfg *config.Config, fs *storage.FileStore, gitOp *gitops.Operator, fileTools *files.Toolset, slow *slowlog.Logger, log zerolog.Logger) *dispatcher.Dispatcher {
	return dispatcher.New(cfg, fs, gitOp, fileTools, slow, log)
}

// buildMessagingPool constructs the per-transport clients
// (Telegram, VK) and wraps them in a MultiClient.
func buildMessagingPool(cfg *config.Config, disp *dispatcher.Dispatcher, log zerolog.Logger) (*messaging.MultiClient, []messaging.Client) {
	var clients []messaging.Client
	if cfg.Messaging.Telegram.IsConfigured() {
		tgClient, err := telegram.New(telegram.Config{
			Token:          cfg.Messaging.Telegram.Token,
			PollingTimeout: cfg.Messaging.Telegram.PollingTimeout,
			ParseMode:      cfg.Messaging.Telegram.ParseMode,
			AllowedUserIDs: cfg.Messaging.Telegram.AllowedUserIDs,
		}, log)
		if err != nil {
			log.Fatal().Err(err).Msg("telegram init")
		}
		clients = append(clients, tgClient)
		commands := disp.Commands()
		if err := tgClient.SetCommands(context.Background(), commands); err != nil {
			log.Warn().Err(err).Msg("telegram: setMyCommands failed; native menu may be stale")
		}
	}
	if cfg.Messaging.VK.IsConfigured() {
		vkClient, err := vktransport.New(vktransport.Config{
			AccessToken:      cfg.Messaging.VK.AccessToken,
			GroupID:          cfg.Messaging.VK.GroupID,
			AllowedUserIDs:   cfg.Messaging.VK.AllowedUserIDs,
			PollingWait:      cfg.Messaging.VK.PollingWait,
			DisableStreaming: cfg.Messaging.VK.DisableStreaming,
		}, log)
		if err != nil {
			log.Fatal().Err(err).Msg("vk init")
		}
		clients = append(clients, vkClient)
	}
	if cfg.Messaging.WSChat.IsConfigured() {
		wsClient, err := wschat.New(cfg.Messaging.WSChat, disp, disp.Commands(), log)
		if err != nil {
			log.Fatal().Err(err).Msg("wschat init")
		}
		clients = append(clients, wsClient)
		log.Info().Str("addr", cfg.Messaging.WSChat.ListenAddr).Msg("wschat transport enabled")
	}
	if len(clients) == 0 {
		log.Fatal().Msg("no messaging transport configured")
	}
	return messaging.NewMultiClient(clients...), clients
}

// autoSaveState holds the per-process reply counter and
// threshold used to trigger periodic git auto-saves.
type autoSaveState struct {
	count     atomic.Int64
	threshold int
}

func newAutoSaveState(cfg *config.Config) *autoSaveState {
	n := cfg.Git.AutoSave.AfterMessages
	if n < 0 {
		n = 0
	}
	return &autoSaveState{threshold: n}
}

// maybeAutoSave increments the counter on every freeform
// reply (commands are excluded) and runs a git commit +
// push when the threshold is reached. Returns the
// notify text (empty when no save ran).
func (a *autoSaveState) maybeAutoSave(ctx context.Context, log zerolog.Logger, c messaging.Client, gitOp *gitops.Operator, chatID string, verbose bool) string {
	if a.threshold <= 0 || gitOp == nil {
		return ""
	}
	if n := a.count.Add(1); int(n)%a.threshold != 0 {
		return ""
	}
	return runAutoSave(ctx, log, c, gitOp, chatID, verbose)
}
