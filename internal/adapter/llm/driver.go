// Package llm is the adapter between the bot and the LLM
// provider. It exposes:
//
//   - a single Driver interface that every transport must
//     implement (openai-go, anthropic-go, …);
//   - a small public type set (ChatRequest, Chunk, ToolSchema,
//     Message, ToolCall, FunctionCall, Usage) shared by the
//     drivers and the GM (usecase) layer;
//   - the StructuredOutputConfig — but with h4-by-default
//     config (see internal/config), drivers hardcode
//     json_object + tool_choice=auto + strict_tools=true
//     and ignore this hint.
//
// Drivers: as of this writing there are two:
//
//   - internal/adapter/llm/openai   — openai-go v3, /v1/chat/completions
//   - internal/adapter/llm/anthropic — anthropic-sdk-go v1.48.0, /v1/messages
//
// Both implement Driver. The legacy hand-rolled HTTP+JSON
// implementation has been removed; bot boot selects one driver
// per cfg.LLM.Driver ("openai" or "anthropic"). All drivers
// hardcode the 8 production tools listed in domain.ProdTools()
// — there is no per-call or per-role toolset configuration.
package llm

import (
	"context"
	"encoding/json"
)

// Driver is the contract every LLM transport must implement.
// The GM (usecase) depends on this interface, not on a concrete
// *Driver, so we can swap transports without touching the call
// sites.
//
// Drivers MUST be safe to call from one goroutine at a time
// for a given instance — the GM's per-chat mutex serialises
// turns, but a single Stream call may run on a worker
// goroutine. Drivers are NOT required to be safe for
// concurrent Stream calls on the same instance.
type Driver interface {
	// Stream sends a chat request and invokes onChunk for every
	// parsed delta. The callback is also invoked once with
	// Done=true on the terminal chunk (or an error chunk
	// before the first delta — see Chunk.Done).
	//
	// The implementation owns the wire format. It is
	// responsible for:
	//   - translating ChatRequest to the provider's JSON
	//   - handling SSE / chunked transfer decoding
	//   - accumulating tool-call deltas into complete ToolCall
	//     entries (the GM only wants the final form)
	//   - applying the per-call timeout (req.TimeoutSeconds,
	//     falling back to the role's RequestTimeoutSeconds)
	//   - mapping provider-specific finish reasons onto the
	//     OpenAI vocabulary ("stop", "tool_calls", "length",
	//     "content_filter", …)
	//
	// The onChunk callback may return an error to abort the
	// stream early (caller cancel, transport budget exhausted).
	// A non-nil return from onChunk SHOULD cause Stream to
	// return promptly with the same error wrapped.
	Stream(ctx context.Context, req ChatRequest, onChunk func(Chunk) error) error

	// Close releases any pooled resources (HTTP keep-alive
	// connections, background goroutines). Idempotent; a
	// no-op for drivers that hold no state. Called on
	// graceful shutdown of the bot.
	Close() error
}

// StructuredOutputConfig is **kept for backward compatibility**
// with config structs that referenced it, but no longer used
// at runtime. Both drivers hardcode:
//
//   - response_format = "json_object"  (openai driver only;
//     anthropic driver has no such field, so the 4-field
//     structure is described in the system prompt instead)
//   - tool_choice = "auto"
//   - strict_tools = true
//
// If you need to change these, edit the driver, not the
// config. There is no operator-facing knob for them anymore.
type StructuredOutputConfig struct {
	Mode         string
	Schema       json.RawMessage
	SchemaName   string
	ToolChoice   string
	StrictTools  bool
}
