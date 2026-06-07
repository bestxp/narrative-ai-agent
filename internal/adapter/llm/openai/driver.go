// Package openai provides an llm.Driver implementation built
// on top of the official github.com/openai/openai-go/v3 SDK.
//
// The driver speaks the OpenAI Chat Completions API wire format
// — the same wire format every OpenAI-compatible provider
// accepts (OpenAI itself, OpenRouter, xAI, Ollama's
// /v1/chat/completions surface, etc.). It supports the full
// feature surface we expect to need:
//
//   - Streaming with content + reasoning + tool_calls
//     deltas. Reasoning is read from the non-standard
//     `reasoning` field Ollama Cloud emits; other providers
//     do not send it, the field is simply absent and the
//     driver ignores it.
//   - Usage accounting via `stream_options.include_usage`
//     (set automatically). Without this flag most providers
//     send no usage block and the GM has to estimate tokens
//     from text length.
//   - response_format: json_object and json_schema (strict
//     mode, used by the GM's structured-output schemas).
//   - tool_choice: auto / required / none / function(name).
//   - Strict tool schemas (the `strict: true` flag on
//     FunctionDefinitionParam). OpenAI strict tools require
//     additionalProperties=false and a closed `required`
//     list — the domain layer is responsible for emitting
//     schemas that satisfy the constraint.
//   - Per-request timeout (option.WithRequestTimeout) and
//     reasoning_effort (top-level field) to disable
//     thinking on providers that recognise it.
//
// The driver is the second of two implementations in the
// project; the legacy one lives in the parent package
// (internal/adapter/llm/client.go) and is selected by
// config.LLM.Driver = "legacy". Both share the Driver
// interface so the GM (internal/usecase) does not know
// which transport is wired in.
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/respjson"
	"github.com/openai/openai-go/v3/shared"
	"github.com/rs/zerolog"

	llm "narrative/internal/adapter/llm"
)

// Compile-time guarantee that *Driver satisfies llm.Driver.
// The build fails here rather than at the call site
// (cmd/bot/main.go) if either signature drifts.
var _ llm.Driver = (*Driver)(nil)

// Driver is the openai-go-backed implementation of
// llm.Driver. Construct one per role via New; the per-role
// fields (model, base URL, API key, timeouts) are baked in
// at construction so the GM can call Stream with just the
// request body.
type Driver struct {
	client openaisdk.Client
	role   llm.RoleConfig
	log    zerolog.Logger
	// structured is the per-role capability hint the GM
	// passes through llm.StructuredOutputConfig. The driver
	// reads it from each ChatRequest's `Extra` field — see
	// the Stream method for the encoding — and emits the
	// appropriate response_format on the wire. The default
	// is "off", which mirrors the legacy driver and keeps
	// behaviour identical for providers that do not honour
	// structured outputs.
	structured llm.StructuredOutputConfig
}

// New constructs a Driver for a single role. baseURL is the
// provider's OpenAI-compat endpoint (e.g.
// "http://localhost:11434/v1" for Ollama, "https://api.openai.com/v1"
// for OpenAI, "https://openrouter.ai/api/v1" for OpenRouter).
// apiKey is the bearer token; providers that do not require
// auth accept any non-empty value (the legacy code uses
// "ollama" for Ollama local).
//
// structured is the role's default structured-output
// capability hint. It can be overridden per-request via
// ChatRequest's Extra field (see Stream).
func New(role llm.RoleConfig, structured llm.StructuredOutputConfig, log zerolog.Logger) *Driver {
	log = log.With().Str("component", "llm.openai").Str("model", role.Model).Logger()
	return &Driver{
		client: openaisdk.NewClient(
			option.WithBaseURL(role.APIURL),
			option.WithAPIKey(role.APIKey),
			option.WithRequestTimeout(time.Duration(orDefault(role.RequestTimeoutSeconds, 120))*time.Second),
		),
		role:       role,
		log:        log,
		structured: structured,
	}
}

// Close is a no-op for the openai-go driver; the SDK does
// not pool long-lived connections beyond the standard
// http.Client transport, so there is nothing to release
// when the role goes out of scope. The method exists so
// *Driver implements llm.Driver symmetrically with the
// legacy *llm.Client.
func (d *Driver) Close() error { return nil }

// Stream sends a chat request to the configured provider
// and invokes onChunk for every parsed delta.
//
// The onChunk callback receives the same llm.Chunk shape
// the legacy driver emits, so downstream code (the GM, the
// summarizer) does not need to know which transport is in
// use. Tool-call deltas are accumulated across chunks
// before being delivered (the GM only wants the final,
// fully-formed ToolCall entries — partial deltas are an
// implementation detail of the OpenAI stream format).
//
// Per-call overrides for response_format / tool_choice /
// strict-tools are read from the request's Extra map under
// the keys defined in this package (see "extra" constants
// below). When an override is absent the role-level default
// (the structured field passed to New) is used.
//
// The implementation propagates onChunk errors by returning
// them wrapped. The stream is closed before the function
// returns so the underlying HTTP body is always released.
func (d *Driver) Stream(ctx context.Context, req llm.ChatRequest, onChunk func(llm.Chunk) error) error {
	params, err := d.buildParams(req)
	if err != nil {
		return fmt.Errorf("openai: build params: %w", err)
	}

	// Per-call timeout: the per-request override in
	// req.TimeoutSeconds wins over the role-level value
	// baked into the client at construction time. The
	// context-based timeout is layered on top so callers
	// can also impose a deadline via context.WithTimeout.
	timeout := req.TimeoutSeconds
	if timeout <= 0 {
		timeout = d.role.RequestTimeoutSeconds
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
	}

	stream := d.client.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()

	// accumulator merges streaming tool-call deltas into
	// complete ToolCall entries. OpenAI streams a function
	// name on the first delta and arguments incrementally;
	// downstream code expects the final, merged form.
	acc := newToolAccumulator(d.log)

	for stream.Next() {
		chunk := stream.Current()
		for _, choice := range chunk.Choices {
			out := llm.Chunk{
				Finish: choice.FinishReason,
			}
			if choice.Delta.Content != "" {
				out.Content = choice.Delta.Content
			}
			// Reasoning is non-standard (Ollama Cloud
			// streams it under "reasoning" on each delta).
			// Read it from ExtraFields when present; the
			// SDK does not expose a typed field.
			if f, ok := choice.Delta.JSON.ExtraFields["reasoning"]; ok && f.Valid() {
				if s := unquoteJSONString(f.Raw()); s != "" {
					out.Reasoning = s
				}
			}
			if len(choice.Delta.ToolCalls) > 0 {
				merged := acc.merge(choice.Delta.ToolCalls)
				if merged != nil {
					out.ToolCalls = merged
				}
			}
			if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
				out.Usage = llm.Usage{
					PromptTokens:     int(chunk.Usage.PromptTokens),
					CompletionTokens: int(chunk.Usage.CompletionTokens),
					TotalTokens:      int(chunk.Usage.TotalTokens),
				}
			}
			if err := onChunk(out); err != nil {
				return fmt.Errorf("openai: chunk callback: %w", err)
			}
		}
	}
	if err := stream.Err(); err != nil {
		// surface context errors with a stable prefix so
		// the GM's slowlog filter can pick them out.
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("openai: stream deadline: %w", err)
		}
		return fmt.Errorf("openai: stream: %w", err)
	}
	// Final "Done" marker so the GM's accumulator has a
	// last-chance hook. Mirrors the legacy driver.
	if err := onChunk(llm.Chunk{Done: true}); err != nil {
		return fmt.Errorf("openai: done callback: %w", err)
	}
	return nil
}

// buildParams translates our internal ChatRequest into the
// openai-go ChatCompletionNewParams. The translation is
// deliberately lossless: every field we set on ChatRequest
// has a corresponding field on the SDK type.
//
// Message conversion keeps the OpenAI tool-call contract:
// assistant turns that emitted tool calls are re-emitted
// on subsequent requests with the same tool_call_id so
// the provider can match tool results back to the original
// call. The legacy driver does the same; see the comment
// on ChatRequest.ToolCalls.
func (d *Driver) buildParams(req llm.ChatRequest) (openaisdk.ChatCompletionNewParams, error) {
	messages := make([]openaisdk.ChatCompletionMessageParamUnion, 0, len(req.Messages))
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			messages = append(messages, openaisdk.SystemMessage(m.Content))
		case "user":
			messages = append(messages, openaisdk.UserMessage(m.Content))
		case "assistant":
			calls := make([]openaisdk.ChatCompletionMessageToolCallUnionParam, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				calls = append(calls, openaisdk.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openaisdk.ChatCompletionMessageFunctionToolCallParam{
						ID: tc.ID,
						Function: openaisdk.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      tc.Function.Name,
							Arguments: tc.Function.Arguments,
						},
					},
				})
			}
			msg := openaisdk.ChatCompletionAssistantMessageParam{
				Content: openaisdk.ChatCompletionAssistantMessageParamContentUnion{
					OfString: openaisdk.Opt(m.Content),
				},
				ToolCalls: calls,
			}
			messages = append(messages, openaisdk.ChatCompletionMessageParamUnion{
				OfAssistant: &msg,
			})
		case "tool":
			messages = append(messages, openaisdk.ToolMessage(m.Content, m.ToolCallID))
		default:
			return openaisdk.ChatCompletionNewParams{}, fmt.Errorf("openai: unknown message role %q", m.Role)
		}
	}

	params := openaisdk.ChatCompletionNewParams{
		Model:    shared.ChatModel(req.Model),
		Messages: messages,
		StreamOptions: openaisdk.ChatCompletionStreamOptionsParam{
			// Without include_usage the provider never
			// reports token totals on the stream — Ollama
			// omits the field entirely, OpenAI sends
			// usage only on the final chunk when this flag
			// is set.
			IncludeUsage: openaisdk.Opt(true),
		},
	}
	if req.Temperature > 0 {
		params.Temperature = openaisdk.Opt(req.Temperature)
	}
	if req.MaxTokens > 0 {
		// OpenAI v3 deprecates `max_tokens` in favour of
		// `max_completion_tokens`. We send the new key on
		// the wire; providers that only accept the old key
		// (older Ollama) silently ignore it. The legacy
		// driver emits `max_tokens` for the same reason.
		params.MaxCompletionTokens = openaisdk.Opt(int64(req.MaxTokens))
	}
	if req.ReasoningEffort != "" {
		// Wire top-level `reasoning_effort` per the
		// OpenAI standard. Note: this is NOT
		// chat_template_kwargs.think — that field is
		// Ollama-native and is not accepted on the
		// /v1/chat/completions surface. The cast to
		// shared.ReasoningEffort is lossless (it is a
		// string alias); the SDK validates the value
		// against the allowed set on marshal.
		params.ReasoningEffort = shared.ReasoningEffort(req.ReasoningEffort)
	}

	// Tools: convert the public ToolSchema list into
	// ChatCompletionToolUnionParam. Strict flag is honoured
	// on the inner FunctionDefinitionParam.
	if len(req.Tools) > 0 {
		tools := make([]openaisdk.ChatCompletionToolUnionParam, 0, len(req.Tools))
		for _, t := range req.Tools {
			params := shared.FunctionParameters{}
			if len(t.Parameters) > 0 {
				if err := json.Unmarshal(t.Parameters, &params); err != nil {
					return openaisdk.ChatCompletionNewParams{}, fmt.Errorf("openai: tool %q parameters: %w", t.Name, err)
				}
			}
			tools = append(tools, openaisdk.ChatCompletionToolUnionParam{
				OfFunction: &openaisdk.ChatCompletionFunctionToolParam{
					Function: shared.FunctionDefinitionParam{
						Name:        t.Name,
						Description: openaisdk.Opt(t.Description),
						Strict:      openaisdk.Opt(extraBool(req, extraStrictTools, d.structured.StrictTools)),
						Parameters:  params,
					},
				},
			})
		}
		params.Tools = tools
	}

	// Tool choice: per-request override wins; otherwise
	// the role-level default. The mapping is intentional:
	//   "auto"     -> openai-go OfAuto("auto")
	//   "required" -> openai-go OfAuto("required")
	//   "none"     -> openai-go OfAuto("none")
	//   "function:<name>" -> openai-go OfFunction(name)
	tcStr := extraString(req, extraToolChoice, d.structured.ToolChoice)
	if tcStr != "" {
		if name, ok := strings.CutPrefix(tcStr, "function:"); ok {
			params.ToolChoice = openaisdk.ChatCompletionToolChoiceOptionUnionParam{
				OfFunctionToolChoice: &openaisdk.ChatCompletionNamedToolChoiceParam{
					Function: openaisdk.ChatCompletionNamedToolChoiceFunctionParam{Name: name},
				},
			}
		} else {
			params.ToolChoice = openaisdk.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: openaisdk.Opt(tcStr),
			}
		}
	}

	// Response format: per-request override wins; default
	// to the role-level StructuredOutputConfig.
	mode := extraString(req, extraResponseFormat, d.structured.Mode)
	switch mode {
	case "json_schema":
		schemaBytes := extraBytes(req, extraResponseSchema, d.structured.Schema)
		name := extraString(req, extraResponseSchemaName, d.structured.SchemaName)
		if name == "" {
			name = "narrative_response"
		}
		var schema any
		if len(schemaBytes) > 0 {
			if err := json.Unmarshal(schemaBytes, &schema); err != nil {
				return openaisdk.ChatCompletionNewParams{}, fmt.Errorf("openai: response schema: %w", err)
			}
		}
		params.ResponseFormat = openaisdk.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:        name,
					Description: openaisdk.Opt("Structured GM response"),
					Strict:      openaisdk.Opt(true),
					Schema:      schema,
				},
			},
		}
	case "json_object":
		params.ResponseFormat = openaisdk.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
		}
	}

	return params, nil
}

// toolAccumulator merges OpenAI's streaming tool-call deltas
// into complete llm.ToolCall entries. OpenAI streams the
// function name on the first delta (Index 0) and the
// arguments incrementally on subsequent deltas with the
// same Index; the driver surfaces a fully-merged entry the
// first time the index is "complete enough" (we never
// know when the provider is done without a finish_reason,
// so we emit the merge on every delta that touches the
// index, and rely on the provider's terminal delta — the
// one carrying finish_reason="tool_calls" — to deliver the
// final form).
type toolAccumulator struct {
	// buf[index] = partial ToolCall. The index is the
	// int64 the SDK returns on each delta; the public
	// llm.ToolCall.Index is a plain int because the
	// legacy driver only ever sees int, so we cast
	// once on store.
	buf map[int64]*llm.ToolCall
	// log receives a warn line every time we drop
	// a tool call whose accumulated Arguments are
	// not a valid JSON object. nil disables the log.
	log zerolog.Logger
}

func newToolAccumulator(log zerolog.Logger) *toolAccumulator {
	return &toolAccumulator{buf: make(map[int64]*llm.ToolCall), log: log}
}

func (a *toolAccumulator) merge(deltas []openaisdk.ChatCompletionChunkChoiceDeltaToolCall) []llm.ToolCall {
	var changed bool
	for _, d := range deltas {
		// Filter broken deltas (Ollama Cloud sometimes
		// emits `delta.tool_calls: [{}]` with no name
		// and no arguments). Skip them rather than
		// emitting an empty ToolCall that the GM would
		// have to special-case.
		if d.Function.Name == "" && d.Function.Arguments == "" {
			continue
		}
		idx := d.Index
		if idx < 0 {
			idx = 0
		}
		entry, ok := a.buf[idx]
		if !ok {
			entry = &llm.ToolCall{Index: int(idx), Type: "function"}
			a.buf[idx] = entry
		}
		if d.ID != "" {
			entry.ID = d.ID
		}
		if d.Type != "" {
			entry.Type = d.Type
		}
		if d.Function.Name != "" {
			entry.Function.Name = d.Function.Name
		}
		if d.Function.Arguments != "" {
			entry.Function.Arguments += d.Function.Arguments
		}
		changed = true
	}
	if !changed {
		return nil
	}
	// Post-merge validation: a fully-accumulated
	// tool call's Arguments field must be a valid
	// JSON object (or null/empty for parameter-less
	// tools). Ollama Cloud occasionally streams a
	// double-wrapped or truncated value (e.g.
	// `{"x": ...}{"y":...}` or `{"x": }`) which
	// passes the per-delta filter above but blows
	// up the openai-go SDK on the next turn when it
	// tries to re-serialise the call into the
	// conversation history. We drop the call here
	// and surface a warning so the GM can fall back
	// to the КОНТЕКСТ-директивы path. Only objects
	// (and the literal "null") are accepted — a
	// truncated or duplicated JSON is a hard
	// reject, never a partial rescue.
	dropped := 0
	for idx, e := range a.buf {
		if e.Function.Arguments == "" {
			continue
		}
		var probe map[string]any
		if err := json.Unmarshal([]byte(e.Function.Arguments), &probe); err != nil {
			delete(a.buf, idx)
			dropped++
		}
	}
	if dropped > 0 {
		if l := a.log; l.GetLevel() != zerolog.Disabled {
			// One line per dropped call is enough —
			// the operator can rerun the same prompt
			// against the slowlog to inspect the
			// raw deltas. We do not include the
			// arguments because they may contain
			// PII the model was relaying.
			l.Warn().
				Int("dropped", dropped).
				Msg("openai.llm: dropped tool calls with invalid arguments JSON (Ollama double-wrap / truncation)")
		}
	}
	if len(a.buf) == 0 {
		return nil
	}
	out := make([]llm.ToolCall, 0, len(a.buf))
	for _, e := range a.buf {
		out = append(out, *e)
	}
	return out
}

// unquoteJSONString strips a JSON-quoted string and
// unescapes common sequences. Used to read non-standard
// fields (Ollama reasoning) from the SDK's respjson.Field
// raw accessor.
func unquoteJSONString(raw string) string {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return raw
	}
	inner := raw[1 : len(raw)-1]
	var out strings.Builder
	for i := 0; i < len(inner); i++ {
		if inner[i] == '\\' && i+1 < len(inner) {
			switch inner[i+1] {
			case 'n':
				out.WriteByte('\n')
			case 't':
				out.WriteByte('\t')
			case 'r':
				out.WriteByte('\r')
			case '"':
				out.WriteByte('"')
			case '\\':
				out.WriteByte('\\')
			default:
				out.WriteByte(inner[i+1])
			}
			i++
			continue
		}
		out.WriteByte(inner[i])
	}
	return out.String()
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// extraString / extraBool / extraBytes read per-request
// overrides for fields that are not part of the public
// ChatRequest struct (we keep the public surface narrow).
// Callers stash overrides in req.Extra (a map[string]any)
// under the keys defined below.
func extraString(req llm.ChatRequest, key, def string) string {
	if v, ok := req.Extra[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

func extraBool(req llm.ChatRequest, key string, def bool) bool {
	if v, ok := req.Extra[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

func extraBytes(req llm.ChatRequest, key string, def []byte) []byte {
	if v, ok := req.Extra[key]; ok {
		switch t := v.(type) {
		case []byte:
			return t
		case json.RawMessage:
			return t
		case string:
			return []byte(t)
		}
	}
	return def
}

// Per-request override keys stashed in ChatRequest.Extra.
// We avoid extending the public ChatRequest type because
// it is shared with the legacy driver, which has its own
// (different) handling for these fields. Drivers that do
// not recognise an Extra key simply ignore it.
const (
	extraResponseFormat  = "openai.response_format"
	extraResponseSchema  = "openai.response_schema"
	extraResponseSchemaName = "openai.response_schema_name"
	extraToolChoice      = "openai.tool_choice"
	extraStrictTools     = "openai.strict_tools"
)

// _ keeps the respjson import referenced for future use;
// the field-level reads above are stable across v3 but if
// the SDK ever changes the access path the import will
// already be in place.
var _ = respjson.Field{}
