// Package config loads, validates and exposes the runtime
// configuration of the lazy-universe bot. All values live in
// config.yaml at the project root; the schema is documented inline
// on every field so the YAML file can stay terse.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the root of the YAML schema. Sections are independent
// and can be reloaded in isolation by future code if needed.
type Config struct {
	// Messaging groups every chat transport (Telegram, Discord, web,
	// ...) under a single section. Adding a new transport means
	// adding a sibling field here, not touching the rest of the
	// codebase.
	Messaging MessagingConfig `yaml:"messaging"`
	// Paths defines on-disk locations used by the bot. Both fields
	// are resolved relative to the directory of the YAML file.
	Paths PathsConfig `yaml:"paths"`
	// Git controls how state is persisted to the version-control
	// system described in the lazy-universe skill (commit, push,
	// rebase on push rejection).
	Git GitConfig `yaml:"git"`
	// Narrative is the GM's behavioural envelope: how many words per
	// reply, which language the player speaks, and so on.
	Narrative NarrativeConfig `yaml:"narrative"`
	// LLM holds the per-role language model configuration. Every
	// distinct LLM usage (narration, compaction, classification...)
	// is a separate role with its own model and prompt.
	LLM LLMConfig `yaml:"llm"`
	// Slowlog configures the per-request audit log. When disabled
	// the package is wired but every entry is dropped. When
	// enabled the path is opened in append mode; the parent
	// directory is created if missing.
	Slowlog SlowlogConfig `yaml:"slowlog"`
}

// MessagingConfig groups every chat transport under a single section.
// New transports (discord, web, ...) extend this struct without
// touching the rest of the codebase.
type MessagingConfig struct {
	// Telegram is the production messenger. The struct is kept
	// transport-specific so Discord, web and other future transports
	// can carry their own allow lists and connection options
	// alongside it.
	Telegram TelegramConfig `yaml:"telegram"`
}

// TelegramConfig is the per-transport configuration. Each transport
// owns its own allow list — a Discord allow list, when added, lives
// in a sibling struct, not in a global "access" block.
type TelegramConfig struct {
	// Token is the BotFather-issued token used to authenticate with
	// the Telegram Bot API. Treat it as a secret.
	Token string `yaml:"token"`
	// PollingTimeout is the long-poll duration in seconds for
	// GetUpdates. Larger values reduce network chatter at the cost
	// of slightly higher latency on the first message of a quiet
	// session. 30-90 is a sane range.
	PollingTimeout int `yaml:"polling_timeout"`
	// ParseMode is the default Telegram message parse mode ("",
	// "MarkdownV2", "HTML"). Empty means plain text. The bot does
	// not currently escape MarkdownV2/HTML special characters, so
	// leaving this empty is the only safe default — turning it on
	// without escaping the reply text will cause Telegram to reject
	// the edit with HTTP 400 and the stream will go silent.
	ParseMode string `yaml:"parse_mode"`
	// AllowedUserIDs is the access control list. Messages from any
	// user not on this list are silently dropped. Keep it small.
	AllowedUserIDs []int `yaml:"allowed_user_ids"`
	// ReplyToUser, when true, threads every bot reply as a
	// Telegram reply to the originating user message. Streaming
	// placeholders, command responses, and the final narrative
	// all carry reply_to_message_id. Standalone notifications
	// (auto-save, compaction, errors) explicitly set the field
	// to 0 so they appear as their own bubbles.
	ReplyToUser bool `yaml:"reply_to_user"`
}

// PathsConfig controls where the bot stores and looks for files.
type PathsConfig struct {
	// DataRoot is the directory that holds info.yaml, the characters/
	// tree and the worlds/ tree. Created on first launch if missing.
	DataRoot string `yaml:"data_root"`
	// GitWorkdir is the directory in which git add/commit/push
	// commands are run. Usually "." (the project root) so the
	// game-data tree is versioned alongside the bot binary.
	GitWorkdir string `yaml:"git_workdir"`
}

// GitConfig mirrors the operations described in the lazy-universe
// skill: commit, rebase on push, never trust the commit output.
type GitConfig struct {
	// Remote is the git remote name (e.g. "origin") used by pull and
	// push. The skill assumes a single remote.
	Remote string `yaml:"remote"`
	// Branch is the working branch. Pushed and pulled against.
	Branch string `yaml:"branch"`
	// CommitAuthor is the local-only git user.name used for bot
	// commits. Does not affect the player's global git config.
	CommitAuthor string `yaml:"commit_author"`
	// CommitEmail is the matching local-only git user.email.
	CommitEmail string `yaml:"commit_email"`
	// RemoteDisabled flips the bot into local-only mode. When true
	// the bot still commits to the local repo but skips pull/push
	// entirely — useful for single-machine development, for keeping
	// a backup on the host before remote sync is wired up, and for
	// tests. The /push command short-circuits to a "remote disabled"
	// message and SyncRebase becomes a no-op. Default false (push
	// enabled) matches the canonical lazy-universe flow.
	RemoteDisabled bool `yaml:"remote_disabled"`
	// AutoSave controls the silent "commit after N messages"
	// behaviour. AfterMessages = 0 disables auto-save entirely; the
	// operator can still trigger a save via /save. The number is
	// the count of bot replies (freeform only, not commands) —
	// counting bot replies keeps the cadence predictable and avoids
	// accidental commits from a stream of /status calls.
	AutoSave AutoSaveConfig `yaml:"auto_save"`
	// VerboseSave switches the "✅ saved" notification from a
	// one-liner to a multi-line block listing the touched files
	// and the byte delta. Off by default — most operators want
	// the short form.
	VerboseSave bool `yaml:"verbose_save"`
}

// AutoSaveConfig holds the auto-commit cadence. The default of 5
// matches a typical "one bot turn = one scene" workflow: after five
// turns the player has usually said or done something worth
// persisting.
type AutoSaveConfig struct {
	// AfterMessages is the number of bot replies between auto
	// commits. 0 disables the feature.
	AfterMessages int `yaml:"after_messages"`
}

// NarrativeConfig shapes the GM's behaviour at runtime.
type NarrativeConfig struct {
	// WordLimit is the soft cap on the GM's reply length. The
	// ResponseFormat validator flags over-limit replies; this is the
	// same number the skill calls "~350 слов".
	WordLimit int `yaml:"word_limit"`
	// Language is the GM's output language. Currently "ru" only;
	// future multilingual support can branch on this.
	Language string `yaml:"language"`
	// RulesCheckBlock controls whether the LLM-generated
	// "**ВАЛИДАЦИЯ ПРАВИЛ**" block (word count, NPC-isolation
	// confirmation, file-update list, etc.) is actually shown
	// to the player. The LLM is still asked to write the block
	// — turning this off simply strips the trailing block from
	// the reply text before it reaches Telegram. Useful for
	// production play (default off) vs. debugging (turn on to
	// see what the model is reporting about itself).
	RulesCheckBlock bool `yaml:"rules_check_block"`
	// IncludeSystemStateInPrompt, when true, includes a small
	// summary of system_state.md (last compaction, session
	// metrics) in the LLM system prompt. Off by default — the
	// file is intended as an operator-facing diagnostic and
	// shipping it to the LLM wastes tokens. Turn on only if a
	// specific bug requires the model to know about its own
	// context-window pressure.
	IncludeSystemStateInPrompt bool `yaml:"include_system_state_in_prompt"`
	// CompactionNotify, when true, sends a Telegram message to
	// the player whenever the bot drops old conversation turns
	// to stay under the configured context window. The default
	// short form is one line ("🔄 компактирую историю (22k →
	// 5.5k tok, −23 хода)"); set CompactionNotifyVerbose to true
	// to also break down before/after tokens per round.
	CompactionNotify bool `yaml:"compaction_notify"`
	// CompactionNotifyVerbose switches the compaction notice
	// from one line to a multi-line breakdown. Honoured only
	// when CompactionNotify is true.
	CompactionNotifyVerbose bool `yaml:"compaction_notify_verbose"`
}

// LLMConfig is a registry of named LLM roles. A role is a single
// concrete usage: the heavy narrative model that drives the GM, a
// cheap local model that condenses NPC files, a classifier that
// picks the right tool, and so on. Each role has its own model,
// prompt and parameters.
type LLMConfig struct {
	// Roles is a name -> configuration map. The "narrative" key is
	// mandatory and powers the GM. Other keys (e.g. "summary",
	// "classification") are wired as the bot grows.
	Roles map[string]LLMRoleConfig `yaml:"roles"`
	// DefaultTimeoutSeconds is the fallback HTTP timeout used when
	// a role does not specify its own RequestTimeoutSeconds. 120s
	// is comfortable for chat completions on a local model.
	DefaultTimeoutSeconds int `yaml:"default_timeout_seconds"`
	// TokenTracking controls how the bot reports token usage per
	// reply. "off" = no accounting at all. "estimate" = count
	// characters in the request and the streamed response and
	// divide by 4 (a coarse but provider-independent
	// approximation that works for any OpenAI-compatible API that
	// does not return a usage block, e.g. Ollama). "usage" = take
	// the value from the provider's usage block verbatim; if the
	// provider does not return one, the bot falls back to
	// estimate and logs a warning. Slowlog receives the same
	// numbers regardless of mode.
	TokenTracking string `yaml:"token_tracking"`
	// IncludeInReply appends a one-line token count to the GM's
	// reply when TokenTracking is not "off" (e.g. "🔢 ~1234
	// tok"). Operators who only want the number in slowlog can
	// flip this off without turning the count itself off.
	IncludeInReply bool `yaml:"include_in_reply"`
}

// TokenTrackingOff / Estimate / Usage are the canonical modes for
// LLMConfig.TokenTracking. Using named constants keeps the call
// sites free of stringly-typed comparisons.
const (
	TokenTrackingOff     = "off"
	TokenTrackingEstimate = "estimate"
	TokenTrackingUsage   = "usage"
)

// LLMRoleConfig describes one named LLM role.
type LLMRoleConfig struct {
	// Model is the model identifier passed verbatim to the API
	// (e.g. "qwen2.5:7b-instruct", "gpt-4o-mini",
	// "anthropic/claude-3.5-sonnet"). For Ollama this is the tag you
	// `ollama pull`-ed.
	Model string `yaml:"model"`
	// APIURL is the OpenAI-compatible chat completions endpoint.
	// Defaults to the local Ollama URL. Override to point at
	// OpenRouter, vLLM, LM-Studio, etc.
	APIURL string `yaml:"api_url"`
	// APIKey is the bearer token. Ollama ignores this; OpenAI and
	// OpenRouter require a real one. Treat as a secret.
	APIKey string `yaml:"api_key"`
	// SystemPromptPath is the file containing the role's system
	// prompt. The bot reads it on startup and sends it as the
	// "system" message of every chat completion call.
	SystemPromptPath string `yaml:"system_prompt_path"`
	// MaxTokens caps the response length. The GM narrative role
	// typically wants 1200-2000; compaction roles can stay at 400-600.
	MaxTokens int `yaml:"max_tokens"`
	// Temperature controls randomness. Higher = more creative. 0.7
	// to 0.9 is a sweet spot for narrative prose; compaction roles
	// should drop to 0.2-0.3 for deterministic output.
	Temperature float64 `yaml:"temperature"`
	// RequestTimeoutSeconds is the HTTP timeout for this role's
	// calls. Falls back to LLMConfig.DefaultTimeoutSeconds when 0.
	RequestTimeoutSeconds int `yaml:"request_timeout_seconds"`
	// ContextWindow is the soft cap on the input side of a
	// single chat-completion request for this role. When the
	// accumulated history plus the static system prompt grows
	// past CompactionThreshold * ContextWindow tokens, the bot
	// triggers a compaction: it drops the oldest conversation
	// turns down to CompactionKeepRecent and reissues the
	// request. Set 0 to disable compaction entirely; the bot
	// will then refuse to issue requests larger than
	// ContextWindow as a hard cap to avoid runaway cost.
	ContextWindow int `yaml:"context_window"`
	// CompactionThreshold is the fraction of ContextWindow at
	// which compaction is triggered. 0.7 means "compact when
	// the prompt reaches 70% of the cap"; 1.0 means "compact
	// only when we hit the cap" (more aggressive, but the
	// compaction itself may push us over for one round).
	// Default 0.7.
	CompactionThreshold float64 `yaml:"compaction_threshold"`
	// CompactionKeepRecent is the number of freshest
	// conversation turns (user+assistant+tool, counted as one
	// each) that survive a compaction. Older turns are dropped
	// from conversations[]; their facts are expected to live in
	// state.md and memorise.md which the LLM re-reads every
	// turn via the system prompt. Default 5.
	CompactionKeepRecent int `yaml:"compaction_keep_recent"`
	// MaxEmptyRetries is the number of automatic re-issues of
	// the same LLM request when the previous round produced 0
	// content (the model "thought" past its budget, the stream
	// was clipped, or `delta.tool_calls` came in headless). Each
	// retry is byte-for-byte identical to the original — same
	// messages, same temperature, same tools — and just gives
	// the provider another chance. Default 2.
	MaxEmptyRetries int `yaml:"max_empty_retries"`
	// EmptyRetryTimeoutSeconds is the per-retry HTTP timeout
	// for the auto-retry rounds. Cloud Ollama is slow under
	// load (50-90s per response on the minimax-m3:cloud tier)
	// and the default per-role timeout may be too tight when
	// the model is mid-thought. Set 0 to fall back to the
	// role's RequestTimeoutSeconds.
	EmptyRetryTimeoutSeconds int `yaml:"empty_retry_timeout_seconds"`
	// DisableThinking turns off chain-of-thought reasoning on
	// providers that recognise `reasoning_effort` (Ollama via
	// /v1/chat/completions, OpenAI reasoning models, xAI
	// Grok, OpenRouter). When true, the bot serialises
	// `reasoning_effort: "none"` in the request body so the
	// model returns visible content immediately rather than
	// streaming a long reasoning trace that leaves
	// delta.content empty. Default false — the operator opts
	// in per role, because some providers (GPT-OSS) reject
	// "none" and require a level like "low".
	DisableThinking bool `yaml:"disable_thinking"`
	// ReasoningEffort overrides DisableThinking for the
	// cases where the operator wants a level other than off
	// (e.g. "low" for GPT-OSS which rejects "none"). Empty
	// string means "no override"; when DisableThinking is
	// true and ReasoningEffort is empty we default to "none".
	// Valid values: "none" | "low" | "medium" | "high"
	// (some providers also accept "minimal" / "xhigh").
	ReasoningEffort string `yaml:"reasoning_effort"`
}

// SlowlogConfig configures the audit log that records every LLM
// request/response, every tool call, every file mutation and every
// incoming/outgoing message. Operators turn it on while debugging
// a tricky session and off again once the bug is reproduced.
type SlowlogConfig struct {
	// Enabled flips the slowlog between File-mode and Discard.
	// Default false — most production runs do not need the disk
	// I/O and the disk growth.
	Enabled bool `yaml:"enabled"`
	// File is the path the JSON-lines audit log is appended to.
	// Resolved relative to the config file's directory. A typical
	// value is "./slow.log" or an absolute path in
	// ~/.cache/lazy-universe/.
	File string `yaml:"file"`
}

// NarrativeRole is the canonical key for the GM role. Other roles
// (summary, classification, ...) use their own keys.
const NarrativeRole = "narrative"

// Role returns the configuration for a named role. The bool is false
// if the key is missing or has no model configured. Callers should
// handle the false case explicitly — there is no implicit fallback to
// the narrative role, because using a 70B model for compaction would
// be wasteful and a 7B model for narration would be lossy.
func (c *Config) Role(name string) (LLMRoleConfig, bool) {
	if r, ok := c.LLM.Roles[name]; ok && r.Model != "" {
		return r, true
	}
	return LLMRoleConfig{}, false
}

// MustRole is a convenience for tests and main wiring: it returns
// the role and panics if it is missing. Production callers should
// use Role() and handle the bool.
func (c *Config) MustRole(name string) LLMRoleConfig {
	r, ok := c.Role(name)
	if !ok {
		panic("llm: role " + name + " is not configured")
	}
	return r
}

// Load reads the YAML file at path, validates it, resolves relative
// paths against the file's directory and returns a populated Config.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	cfg.resolveRelativePaths(filepath.Dir(path))
	return &cfg, nil
}

// Validate fills defaults and rejects obviously broken setups. The
// order of checks is meaningful: messages mentioning the offending
// section come first so the operator can fix config.yaml quickly.
func (c *Config) Validate() error {
	// At least one transport must be configured.
	if !c.Messaging.Telegram.isConfigured() {
		return errors.New("messaging.telegram.token must be set to a real bot token")
	}
	if c.Messaging.Telegram.Token == "REPLACE_WITH_BOTFATHER_TOKEN" {
		return errors.New("messaging.telegram.token must be set to a real bot token")
	}
	if len(c.Messaging.Telegram.AllowedUserIDs) == 0 {
		return errors.New("messaging.telegram.allowed_user_ids must contain at least one user id")
	}
	if c.Paths.DataRoot == "" {
		c.Paths.DataRoot = "game-data"
	}
	if c.Paths.GitWorkdir == "" {
		c.Paths.GitWorkdir = "."
	}
	// Slowlog.File defaults to ./slow.log so an operator who only
	// flips `slowlog.enabled: true` gets a sensible path.
	if c.Slowlog.Enabled && c.Slowlog.File == "" {
		c.Slowlog.File = "slow.log"
	}
	// TokenTracking defaults to "off" — most operators do not need
	// the per-reply line, and the estimate mode adds a final chunk
	// of work after every response. Flip to "estimate" in
	// config.yaml to enable.
	if c.LLM.TokenTracking == "" {
		c.LLM.TokenTracking = TokenTrackingOff
	}
	switch c.LLM.TokenTracking {
	case TokenTrackingOff, TokenTrackingEstimate, TokenTrackingUsage:
		// ok
	default:
		return fmt.Errorf("llm.token_tracking must be one of off|estimate|usage, got %q", c.LLM.TokenTracking)
	}
	if c.Git.Remote == "" {
		c.Git.Remote = "origin"
	}
	if c.Git.Branch == "" {
		c.Git.Branch = "master"
	}
	// RemoteDisabled is a bool — its zero value is false, which
	// matches the default behaviour (push enabled).
	if c.Narrative.WordLimit == 0 {
		c.Narrative.WordLimit = 150
	}
	if c.Narrative.Language == "" {
		c.Narrative.Language = "ru"
	}
	// Per-role defaults. A missing key falls back to these.
	c.LLM.DefaultTimeoutSeconds = nonZero(c.LLM.DefaultTimeoutSeconds, 120)
	for name, role := range c.LLM.Roles {
		role.APIURL = nonEmpty(role.APIURL, "http://localhost:11434/v1")
		role.APIKey = nonEmpty(role.APIKey, "ollama")
		role.RequestTimeoutSeconds = nonZero(role.RequestTimeoutSeconds, c.LLM.DefaultTimeoutSeconds)
		role.MaxTokens = nonZero(role.MaxTokens, 2500)
		role.Temperature = nonZeroFloat(role.Temperature, 0.8)
		role.CompactionThreshold = nonZeroFloat(role.CompactionThreshold, 0.7)
		role.CompactionKeepRecent = nonZero(role.CompactionKeepRecent, 5)
		role.MaxEmptyRetries = nonZero(role.MaxEmptyRetries, 2)
		role.EmptyRetryTimeoutSeconds = nonZero(role.EmptyRetryTimeoutSeconds, role.RequestTimeoutSeconds)
		// system_prompt_path stays empty by default. main.go
		// will fall back to the embed.FS copy in internal/prompts.
		// Operators who want to A/B test a new prompt set the
		// path explicitly in config.yaml.
		c.LLM.Roles[name] = role
	}
	// The narrative role is mandatory — it's the only one wired today
	// but a bot that boots with zero roles is always a misconfiguration.
	narr, ok := c.LLM.Roles[NarrativeRole]
	if !ok {
		return fmt.Errorf("llm.%s role must be configured (model + system_prompt_path)", NarrativeRole)
	}
	if narr.Model == "" {
		return fmt.Errorf("llm.%s.model must be set", NarrativeRole)
	}
	return nil
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func nonZero(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func nonZeroFloat(v, def float64) float64 {
	if v == 0 {
		return def
	}
	return v
}

// resolveRelativePaths anchors every user-supplied path to the
// directory of the config file so the bot behaves the same regardless
// of where it is invoked from.
func (c *Config) resolveRelativePaths(base string) {
	if !filepath.IsAbs(c.Paths.DataRoot) {
		c.Paths.DataRoot = filepath.Join(base, c.Paths.DataRoot)
	}
	if !filepath.IsAbs(c.Paths.GitWorkdir) {
		c.Paths.GitWorkdir = filepath.Join(base, c.Paths.GitWorkdir)
	}
	for name, role := range c.LLM.Roles {
		if role.SystemPromptPath != "" && !filepath.IsAbs(role.SystemPromptPath) {
			role.SystemPromptPath = filepath.Join(base, role.SystemPromptPath)
			c.LLM.Roles[name] = role
		}
	}
}

// TelegramIsAllowed checks the Telegram allow list. Per-transport
// helpers live here; main.go can pick the right one.
func (c *Config) TelegramIsAllowed(userID int) bool {
	for _, id := range c.Messaging.Telegram.AllowedUserIDs {
		if id == userID {
			return true
		}
	}
	return false
}

// isConfigured distinguishes "user did not configure this transport"
// from "user configured it but left Token empty".
func (t TelegramConfig) isConfigured() bool {
	return t.Token != ""
}
