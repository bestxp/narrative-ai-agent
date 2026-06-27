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
	"slices"

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
	// Health configures the k8s-style health HTTP server. When
	// ListenAddr is non-empty the bot serves /healthz, /readyz
	// and /health on that address. Set it to "" to disable
	// the health server (the bot still runs normally, but
	// k8s / docker-compose cannot probe its readiness).
	Health HealthConfig `yaml:"health"`
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
	// VK is the VKontakte messenger transport. Disabled when
	// AccessToken or GroupID is empty.
	VK VKConfig `yaml:"vk"`
	// WSChat is the browser WebSocket chat used during development
	// instead of VK/Telegram. It serves an embedded React app and
	// a WebSocket endpoint on its own port. The wire protocol is
	// JSON envelopes (type+payload) with bearer-token auth, designed
	// to extend to a production WebSocket API.
	WSChat WSChatConfig `yaml:"wschat"`
}

// WSChatConfig configures the browser WebSocket chat transport.
// It runs on a separate port and is intended for development: the
// operator opens the served page in a browser and chats with the
// bot directly. Auth is a static bearer token (dev_token) checked
// on the WebSocket upgrade and on the HTTP API endpoints. The
// allowed_tokens list is the hook for a production deployment
// where tokens are issued per-user; in dev a single dev_token is
// enough.
type WSChatConfig struct {
	// Enabled turns the transport on. When false the wschat client
	// is not constructed even if ListenAddr is set.
	Enabled bool `yaml:"enabled"`
	// ListenAddr is the bind address for the HTTP+WS server, e.g.
	// ":8090" or "127.0.0.1:8090".
	ListenAddr string `yaml:"listen_addr"`
	// DevToken is the single bearer token accepted in dev. The
	// browser client passes it as ?token=<dev_token> on the WS
	// upgrade and as Authorization: Bearer on HTTP API calls. Treat
	// as a secret; keep it in config.yaml (gitignored).
	DevToken string `yaml:"dev_token"`
	// AllowedTokens is the multi-token allow list for the
	// production path. In dev it can stay empty (DevToken is
	// enough); in prod each user gets their own token. A request
	// is accepted when its token matches DevToken OR any entry in
	// AllowedTokens.
	AllowedTokens []string `yaml:"allowed_tokens"`
	// ChatID is the logical chat id used by the wschat transport
	// for the dev session. The GM keys conversation history on
	// this id. "dev" is a sensible default.
	ChatID string `yaml:"chat_id"`
}

// IsConfigured returns true when the wschat transport is enabled
// and has a bind address.
func (w WSChatConfig) IsConfigured() bool {
	return w.Enabled && w.ListenAddr != ""
}

// IsAllowed reports whether the given bearer token is accepted by
// the wschat transport. The token matches when it equals DevToken
// or appears in AllowedTokens.
func (w WSChatConfig) IsAllowed(token string) bool {
	if token == "" {
		return false
	}

	if w.DevToken != "" && token == w.DevToken {
		return true
	}

	return slices.Contains(w.AllowedTokens, token)
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

// GitConfig controls how state is persisted to the version-control
// system. When Disabled is true, all git operations (commit, push,
// pull) are skipped — the bot runs without any git integration.
type GitConfig struct {
	// Disabled turns off git entirely. When true the bot does
	// not commit, push, or pull — state lives only on disk.
	// /save, /commit, /push return "git отключён".
	// Auto-save is skipped. Useful when no git repo is available
	// or when git persistence is not needed. Default false.
	Disabled bool `yaml:"disabled"`
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

// HealthConfig exposes the k8s-style health server. Three
// endpoints are served:
//
//	GET /healthz — liveness probe. 200 once the process is up.
//	GET /readyz  — readiness probe. 200 when at least one
//	               configured messaging client reports
//	               HealthState == connected.
//	GET /health  — same payload as /readyz but always returns
//	               JSON for human / log inspection.
//
// Probes are pure reads: no auth, no rate limiting, no body. The
// server is stdlib net/http only — no metrics framework, no
// /metrics endpoint. Add one here if you ever need Prometheus
// scraping; the codebase intentionally stays small.
type HealthConfig struct {
	// ListenAddr is the bind address for the health server, in
	// net.Listen format. ":8080" binds to all interfaces on
	// port 8080; "127.0.0.1:8080" binds to the loopback only.
	// Empty string disables the server.
	ListenAddr string `yaml:"listen_addr"`
	// ReadHeaderTimeout / ReadTimeout / WriteTimeout are bounded
	// defaults; the underlying server is configured inside the
	// health package. Operators do not need to tune them.
}

// LLMConfig is a registry of named LLM roles. A role is a single
// concrete usage: the heavy narrative model that drives the GM, a
// cheap local model that condenses NPC files, a classifier that
// picks the right tool, and so on. Each role has its own model,
// prompt and parameters.
//
// LLMConfig.Providers carries the SDK + endpoint configuration
// (driver, api_url, api_key, context_window). Roles reference
// providers by name via the "provider" key. A single provider can
// back many roles (e.g. one Ollama URL for narrative + summary +
// classification) and one role can override its provider's model
// if the operator wants different weights on the same endpoint.
type LLMConfig struct {
	// Providers is a name -> configuration map. At least one
	// provider is mandatory (the narrative role points to one).
	// Multiple providers are useful when the operator wants to
	// mix endpoints (local Ollama + cloud OpenRouter + direct
	// Anthropic) without duplicating api_url/api_key per role.
	Providers map[string]ProviderConfig `yaml:"providers"`
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

// ProviderConfig describes a single LLM endpoint. Multiple roles
// can share one provider; a role overrides only what differs (model,
// disable_thinking, etc.).
type ProviderConfig struct {
	// Driver selects the LLM SDK. The h4-by-default wire surface
	// (json_object + 8 tools + tool_choice=auto + strict_tools=true
	// on openai; system-prompt + 8 tools + tool_choice=auto +
	// strict on anthropic) is hardcoded inside each driver —
	// there are no per-call overrides.
	//
	//   "openai" use github.com/openai/openai-go/v3 over
	//     /v1/chat/completions (Ollama, OpenAI, OpenRouter,
	//     routerai.ru, etc.).
	//   "anthropic" uses github.com/anthropics/anthropic-sdk-go
	//     over /v1/messages (Ollama, OpenRouter, Anthropic
	//     direct). The driver auto-detects which auth header to
	//     use based on the API URL: ollama.com takes
	//     "Authorization: Bearer", everyone else takes the
	//     standard "x-api-key" header.
	Driver string `yaml:"driver"`
	// APIURL is the OpenAI-compatible chat completions endpoint.
	// Defaults to the local Ollama URL. Override to point at
	// OpenRouter, vLLM, LM-Studio, etc.
	APIURL string `yaml:"api_url"`
	// APIKey is the bearer token. Ollama ignores this; OpenAI and
	// OpenRouter require a real one. Treat as a secret.
	APIKey string `yaml:"api_key"`
	// ContextWindow is the soft cap on the input side of a single
	// chat-completion request. When the accumulated history plus
	// the static system prompt grows past CompactionThreshold *
	// ContextWindow tokens, the bot triggers a compaction: it
	// drops the oldest conversation turns down to
	// CompactionKeepRecent and reissues the request. Set 0 to
	// disable compaction entirely; the bot will then refuse to
	// issue requests larger than ContextWindow as a hard cap to
	// avoid runaway cost.
	ContextWindow int `yaml:"context_window"`
}

// TokenTrackingOff / Estimate / Usage are the canonical modes for
// LLMConfig.TokenTracking. Using named constants keeps the call
// sites free of stringly-typed comparisons.
const (
	TokenTrackingOff      = "off"
	TokenTrackingEstimate = "estimate"
	TokenTrackingUsage    = "usage"
)

// LLMRoleConfig describes one named LLM role.
type LLMRoleConfig struct {
	// Provider is the name of the LLMConfig.Providers entry this
	// role routes through. Mandatory — a role without a provider
	// reference is a misconfiguration (the bot cannot fabricate
	// an endpoint).
	Provider string `yaml:"provider"`
	// Model is the model identifier passed verbatim to the API
	// (e.g. "qwen2.5:7b-instruct", "gpt-4o-mini",
	// "anthropic/claude-3.5-sonnet"). For Ollama this is the tag
	// you `ollama pull`-ed. Empty means "use the default model
	// declared on the provider" (LLMConfig.Providers[name].Model
	// is reserved for future use; today the role Model is
	// mandatory and the provider has no default of its own).
	Model string `yaml:"model"`
	// SystemPromptPath is the file containing the role's system
	// prompt. The bot reads it on startup and sends it as the
	// "system" message of every chat completion call.
	SystemPromptPath string `yaml:"system_prompt_path"`
	// MaxTokens caps the response length. The GM narrative role
	// typically wants 1200-2000; compaction roles can stay at 400-600.
	MaxTokens int `yaml:"max_tokens"`
	// UsePrefillBracket forces the assistant turn to start with
	// "{". When true, the driver injects a fake assistant message
	// content="{" right before the real request. This prefill
	// trick nudges local models (Ollama) to emit JSON immediately
	// instead of writing an intro paragraph. Default false.
	UsePrefillBracket bool `yaml:"use_prefill_bracket"`
	// Temperature controls randomness. Higher = more creative. 0.7
	// to 0.9 is a sweet spot for narrative prose; compaction roles
	// should drop to 0.2-0.3 for deterministic output.
	Temperature float64 `yaml:"temperature"`
	// RequestTimeoutSeconds is the HTTP timeout for this role's
	// calls. Falls back to LLMConfig.DefaultTimeoutSeconds when 0.
	RequestTimeoutSeconds int `yaml:"request_timeout_seconds"`
	// CompactionThreshold is the fraction of ProviderConfig.ContextWindow at
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

// Role returns the (role, provider) pair for a named role. The
// bool is false if the key is missing, has no model, or
// references an undefined provider. Callers should handle the
// false case explicitly — there is no implicit fallback to the
// narrative role, because using a 70B model for compaction would
// be wasteful and a 7B model for narration would be lossy.
func (c *Config) Role(name string) (LLMRoleConfig, ProviderConfig, bool) {
	r, ok := c.LLM.Roles[name]
	if !ok || r.Model == "" {
		return LLMRoleConfig{}, ProviderConfig{}, false
	}

	prov, ok := c.LLM.Providers[r.Provider]
	if !ok {
		return LLMRoleConfig{}, ProviderConfig{}, false
	}

	return r, prov, true
}

// ProviderByName returns the configuration for a named provider.
// The bool is false if the key is missing. The provider
// resolution path is the single source of truth for endpoint
// configuration; everywhere else (buildLLMDriver,
// buildSummarizer, buildGM) gets its url/key/driver through
// this lookup.
func (c *Config) ProviderByName(name string) (ProviderConfig, bool) {
	if p, ok := c.LLM.Providers[name]; ok {
		return p, true
	}

	return ProviderConfig{}, false
}

// MustRole is a convenience for tests and main wiring: it returns
// the role and panics if it is missing. Production callers should
// use Role() and handle the bool.
func (c *Config) MustRole(name string) LLMRoleConfig {
	r, _, ok := c.Role(name)
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
//
// config validation traverses every nested section (messaging, llm, prompts, storage);
// Validate checks every section of the config (messaging, paths,
// git, narrative, llm.providers, llm.roles) and applies default
// values. The order is meaningful: messaging invariants come
// first so the operator sees the offending transport before any
// LLM-shaped error message. Per-section helpers keep each
// validator small.
func (c *Config) Validate() error {
	if err := c.validateMessaging(); err != nil {
		return err
	}

	c.applyDefaults()

	if err := c.validateProviders(); err != nil {
		return err
	}

	if err := c.validateRoles(); err != nil {
		return err
	}

	return c.validateLLMBlockers()
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

// TelegramIsAllowed checks the Telegram allow list. Per-transport
// helpers live here; main.go can pick the right one.
func (c *Config) TelegramIsAllowed(userID int) bool {
	return slices.Contains(c.Messaging.Telegram.AllowedUserIDs, userID)
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

// VKConfig is the per-transport configuration for VKontakte.
// The bot uses VK's Bots Long Poll API to receive messages
// and the messages.send / messages.edit API calls for outgoing.
//
// Required token permissions (Manage → Settings → API usage →
// Create token → select these scopes):
//
//   - messages         — send, edit, read messages
//   - messages.setActivity — typing indicator
//   - groups            — long poll (groups.getLongPollServer,
//     groups.setLongPollSettings)
type VKConfig struct {
	// AccessToken is the VK community token (obtained from
	// vk.com/settings?act=token or the community admin panel).
	// The token must have the following permissions enabled:
	//   messages       — send, edit, read messages
	//   messages.setActivity — typing indicator
	//   groups         — long poll server & settings
	AccessToken string `yaml:"access_token"`
	// GroupID is the VK community (group) identifier. Required
	// for Bots Long Poll and for group_id in messages.send.
	GroupID int `yaml:"group_id"`
	// AllowedUserIDs is the access control list. Messages from
	// VK users not on this list are silently dropped.
	AllowedUserIDs []int `yaml:"allowed_user_ids"`
	// PollingWait is the long-poll wait timeout in seconds.
	// Defaults to 25 (same as VK recommendation).
	PollingWait int `yaml:"polling_wait"`
	// DisableStreaming turns off word-by-word streaming for VK.
	// When true the bot sends a single complete message at the
	// end instead of editing a placeholder repeatedly. This
	// avoids VK's flood-control (1000 messages/hour limit) at
	// the cost of no live typing illusion. Default false.
	DisableStreaming bool `yaml:"disable_streaming"`
}

// IsConfigured distinguishes "user did not configure this transport"
// from "user configured it but left Token empty".
func (t TelegramConfig) IsConfigured() bool {
	return t.Token != ""
}

// IsConfigured returns true when the VK transport has enough
// configuration to start (access token + group id).
func (v VKConfig) IsConfigured() bool {
	return v.AccessToken != "" && v.GroupID > 0
}

// validateMessaging checks that at least one transport is wired and
// that its per-transport invariants hold (real bot token, non-empty
// allow list, etc.).
func (c *Config) validateMessaging() error {
	tgOK := c.Messaging.Telegram.IsConfigured()
	vkOK := c.Messaging.VK.IsConfigured()

	wsOK := c.Messaging.WSChat.IsConfigured()
	if !tgOK && !vkOK && !wsOK {
		return errors.New("at least one messaging transport must be configured (telegram, vk or wschat)")
	}

	if tgOK {
		if c.Messaging.Telegram.Token == "REPLACE_WITH_BOTFATHER_TOKEN" {
			return errors.New("messaging.telegram.token must be set to a real bot token")
		}

		if len(c.Messaging.Telegram.AllowedUserIDs) == 0 {
			return errors.New("messaging.telegram.allowed_user_ids must contain at least one user id")
		}
	}

	if vkOK {
		if len(c.Messaging.VK.AllowedUserIDs) == 0 {
			return errors.New("messaging.vk.allowed_user_ids must contain at least one user id")
		}

		if c.Messaging.VK.PollingWait == 0 {
			c.Messaging.VK.PollingWait = 25
		}
	}

	if wsOK {
		if c.Messaging.WSChat.DevToken == "" && len(c.Messaging.WSChat.AllowedTokens) == 0 {
			return errors.New("messaging.wschat.dev_token (or allowed_tokens) must be set when wschat is enabled")
		}

		if c.Messaging.WSChat.ChatID == "" {
			c.Messaging.WSChat.ChatID = "dev"
		}
	}

	return nil
}

// applyDefaults sets every default that does not depend on per-role
// or per-provider data. Defaults that need a per-key fallback (role
// / provider) live in validateRoles / validateProviders.
func (c *Config) applyDefaults() {
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

	c.LLM.DefaultTimeoutSeconds = nonZero(c.LLM.DefaultTimeoutSeconds, 120)
}

// validateProviders walks every llm.providers entry, applies
// per-provider defaults (driver=openai, api_url/api_key=ollama
// defaults) and rejects unknown drivers. Defaults belong to a
// single source of truth so the operator can omit a driver field
// and still get a working bot.
func (c *Config) validateProviders() error {
	provDefaultURL := "http://localhost:11434/v1"
	provDefaultKey := "ollama"

	// Iterate the providers and apply per-provider defaults
	// (driver=openai, api_url/api_key=ollama defaults).
	for name, prov := range c.LLM.Providers {
		prov.Driver = nonEmpty(prov.Driver, "openai")

		switch prov.Driver {
		case "openai", "anthropic":
			// ok
		default:
			return fmt.Errorf("llm.providers.%s.driver must be one of openai|anthropic, got %q", name, prov.Driver)
		}

		prov.APIURL = nonEmpty(prov.APIURL, provDefaultURL)
		prov.APIKey = nonEmpty(prov.APIKey, provDefaultKey)
		c.LLM.Providers[name] = prov
	}

	return nil
}

// validateRoles walks every llm.roles entry, verifies the role
// references a defined provider, applies per-role defaults
// (temperature, max_tokens, compaction knobs).
func (c *Config) validateRoles() error {
	for name, role := range c.LLM.Roles {
		if role.Provider == "" {
			return fmt.Errorf("llm.roles.%s.provider must reference a providers key", name)
		}

		if _, ok := c.LLM.Providers[role.Provider]; !ok {
			return fmt.Errorf("llm.roles.%s.provider %q is not defined in llm.providers", name, role.Provider)
		}

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

	return nil
}

// validateLLMBlockers covers post-default LLM invariants that
// belong together: token_tracking enum, role "narrative"
// presence, narrative.model non-empty. The narrative role is
// the only one wired today; a bot that boots with zero roles is
// always a misconfiguration.
func (c *Config) validateLLMBlockers() error {
	switch c.LLM.TokenTracking {
	case TokenTrackingOff, TokenTrackingEstimate, TokenTrackingUsage:
		// ok
	default:
		return fmt.Errorf("llm.token_tracking must be one of off|estimate|usage, got %q", c.LLM.TokenTracking)
	}

	narr, ok := c.LLM.Roles[NarrativeRole]
	if !ok {
		return fmt.Errorf("llm.%s role must be configured (provider + model + system_prompt_path)", NarrativeRole)
	}

	if narr.Model == "" {
		return fmt.Errorf("llm.%s.model must be set", NarrativeRole)
	}

	if _, ok := c.LLM.Providers[narr.Provider]; !ok {
		return fmt.Errorf("llm.%s.provider %q is not defined in llm.providers", NarrativeRole, narr.Provider)
	}

	return nil
}
