package llm

import (
	"context"
	"encoding/json"
)

// Driver is the contract every LLM transport must implement. The
// GM (usecase) depends on this interface, not on a concrete
// *Client, so we can swap between the legacy HTTP+JSON
// implementation (Client) and the openai-go implementation
// (internal/adapter/llm/openai) without touching the call sites.
//
// A driver is constructed per role; the per-role fields
// (model, base URL, API key, timeouts, structured-output
// preferences) are baked in at construction so the GM can
// just call Stream with a request body and get back chunks.
//
// Drivers MUST be safe to call from one goroutine at a time
// for a given instance — the GM's per-chat mutex serialises
// turns, but a single Stream call may run on a worker
// goroutine (e.g. auto-retry). Drivers are NOT required to
// be safe for concurrent Stream calls on the same instance.
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

// StructuredOutputConfig is the per-role hint that tells a
// driver whether the provider supports response_format and
// tool_choice=required strict mode. The GM reads it from
// config and passes it on Stream — drivers decide which
// fields to wire (or silently ignore when the provider
// does not support the feature; see the openai driver).
//
// We expose this as a separate type rather than embedding it
// in ChatRequest because it is a *capability hint*, not a
// per-call setting. Setting StructuredOutput="strict_json"
// on a single Stream call would still be honoured by the
// driver; the role-level vendor detection is just a default
// so the bot does not send json_schema to Ollama /v1 which
// silently ignores it.
type StructuredOutputConfig struct {
	// Mode controls the wire format:
	//   "" or "off"        — do not emit response_format.
	//                         Same behaviour as legacy.
	//   "json_object"      — set response_format.type="json_object"
	//                         so the model is told to emit a
	//                         JSON object. The model is still
	//                         free to omit keys. Use this with
	//                         Ollama where strict json_schema
	//                         is not implemented.
	//   "json_schema"      — set response_format with a
	//                         strict JSON Schema (provider must
	//                         support OpenAI's
	//                         structured-outputs API; OpenAI
	//                         GPT-4o-2024-08-06+ and
	//                         OpenRouter strict mode).
	Mode string
	// Schema is the JSON Schema body to attach to
	// response_format when Mode="json_schema". Ignored
	// otherwise. Schema is the body only — the driver wraps
	// it in {"type":"json_schema", "json_schema":{...}}
	// per the OpenAI shape.
	Schema json.RawMessage
	// SchemaName is the identifier the driver emits in the
	// json_schema wrapper. Providers cache / register
	// schemas by this name; pick something stable so the
	// provider can validate against the cached version on
	// repeat calls.
	SchemaName string
	// ToolChoice controls the wire `tool_choice` field:
	//   "" or "auto"       — let the model decide.
	//   "required"         — the model MUST emit at least one
	//                         tool_call. Used in combination
	//                         with strict tool schemas so the
	//                         GM can guarantee tool side-
	//                         effects on the current turn.
	//   "none"            — no tool calls this turn.
	//   "function:<name>" — force this specific function.
	//                       (We do not currently use this from
	//                       the GM; reserved for future
	//                       "one-shot scripted" turns.)
	ToolChoice string
	// StrictTools flips the strict flag on every Tool
	// declaration (so the provider validates the model's
	// tool arguments against the schema). OpenAI strict
	// tools require the schema to set additionalProperties
	// to false and every property to be listed in
	// `required`. The domain layer is responsible for
	// emitting schemas that satisfy that constraint.
	StrictTools bool
}
