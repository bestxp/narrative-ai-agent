package llm

import "encoding/json"

// RoleConfig is a snapshot of the relevant subset of
// config.LLMRoleConfig. Keeping it independent of the config
// package lets the adapter be tested with synthetic values.
//
// Note: in the h4-by-default world, only these fields matter
// at runtime: APIURL, APIKey, Model, MaxTokens, Temperature,
// RequestTimeoutSeconds, DisableThinking, ReasoningEffort.
// MaxEmptyRetries / EmptyRetryTimeoutSeconds are kept for
// the per-call retry path inside usecase/gm.go (we no longer
// auto-retry on empty, but the field is harmless).
type RoleConfig struct {
	APIURL                  string
	APIKey                  string
	Model                   string
	MaxTokens               int
	Temperature             float64
	RequestTimeoutSeconds   int
	DisableThinking         bool
	ReasoningEffort         string
	MaxEmptyRetries         int
	EmptyRetryTimeoutSeconds int
}

// Message is a single chat entry shared by all drivers and the
// GM. The OpenAI shape (tool_call_id on tool messages,
// tool_calls on assistant messages) is preserved so the same
// history can be sent to either driver without translation.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall mirrors the OpenAI shape (used by both openai and
// anthropic drivers — the latter's "tool_use" blocks are
// converted to this shape by the anthropic driver before
// reaching the GM).
type ToolCall struct {
	Index    int          `json:"index,omitempty"`
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall is the function name + raw JSON arguments.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolSchema is the public-facing tool declaration. The
// openai and anthropic drivers convert from domain.Tool to
// this shape at startup; ChatRequest carries a slice of
// ToolSchema (rather than domain.Tool) so the driver package
// does not depend on the domain package.
type ToolSchema struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ChatRequest is what drivers turn into wire requests.
type ChatRequest struct {
	Model       string
	Messages    []Message
	Tools       []ToolSchema
	Temperature float64
	MaxTokens   int
	Stream      bool
	// ReasoningEffort is sent on the wire as the top-level
	// `reasoning_effort` field. Empty string means "omit
	// the key" (provider default applies). The h4 default
	// is "none" to skip chain-of-thought on the providers
	// that recognise it (Ollama, OpenAI reasoning models,
	// xAI Grok, OpenRouter).
	ReasoningEffort string
	// TimeoutSeconds overrides the role's per-request
	// timeout for this single call. 0 means "use the
	// role's RequestTimeoutSeconds".
	TimeoutSeconds int
}

// Chunk is the callback payload every driver delivers to the
// GM. The shape is the same regardless of provider; drivers
// convert their native deltas (OpenAI ChatCompletionChunk or
// Anthropic ContentBlockUnion) into Chunk before invoking
// onChunk.
type Chunk struct {
	// Content is the delta text (may be empty).
	Content string
	// Reasoning is the delta reasoning / chain-of-thought.
	// OpenAI: non-standard `reasoning` field (Ollama Cloud).
	// Anthropic: thinking-blocks — logged to slowlog only;
	// not delivered to the player. The Chunk field exists
	// so the GM can log it; it does NOT contribute to
	// player-visible output.
	Reasoning string
	// ToolCalls is emitted on the chunk that finalises
	// them. Empty on text-only deltas.
	ToolCalls []ToolCall
	// Finish is the OpenAI-vocabulary finish reason:
	// "stop" | "tool_calls" | "length" | "content_filter".
	// Anthropic stop_reasons are mapped to this vocabulary
	// by the anthropic driver.
	Finish string
	// Done is true on the terminal chunk of the stream.
	Done bool
	// Usage is the provider-reported token count for the
	// entire request so far. Most providers emit it on the
	// very last chunk (the one with finish_reason set);
	// Ollama omits it entirely. Callers that need a number
	// regardless should fall back to EstimateTokens.
	Usage Usage
	// RawTrace is a sampling of the raw SSE payloads seen
	// while parsing the stream. Populated on every chunk;
	// callers that hit an "empty assistant turn" can dump
	// it to slowlog to see what the provider actually sent.
	RawTrace []string
}

// Usage mirrors the OpenAI usage block. PromptTokens counts
// the input messages, CompletionTokens counts the streamed
// response, TotalTokens is the sum.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
