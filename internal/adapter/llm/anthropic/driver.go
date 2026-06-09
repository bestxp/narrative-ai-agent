// Package anthropic provides an llm.Driver implementation built
// on top of the official github.com/anthropics/anthropic-sdk-go
// SDK.
//
// **H4-by-default config**: this driver hardcodes the production
// wire surface for the Anthropic Messages API:
//
//   - tool_choice: "auto" (the model decides when to call tools;
//     the КОНТЕКСТ-парсинг fallback in gm.go handles edge cases
//     where the model emits thinking-blocks that overflow).
//   - strict_tools: true on every tool (Anthropic `strict: true`
//     validates the model's tool arguments against the schema).
//   - the 8 production tools: imported from
//     internal/domain.ProdTools() at startup.
//   - system prompt: 4-field narrative shape is described in the
//     system prompt (narrative.md Режим A on openai; the
//     anthropic driver has no `response_format.json_object`
//     equivalent, so the model is told via prompt instead).
//
// Thinking-blocks emitted by the provider (a non-standard
// extension some Anthropic-compatible providers, notably
// openrouter, return as `tool_use` blocks with empty `name`
// and `input`) are:
//   - logged to slowlog as `anthropic.thinking` events with the
//     raw text;
//   - dropped from the player-visible stream;
//   - not counted in usage (the provider's `usage` block does
//     not include thinking tokens).
//
// Multi-block responses: anthropic responses are a list of
// content blocks (`text` + `tool_use` + `thinking`). The driver
// emits one llm.Chunk per block in order, so the GM sees a
// stream of "first text, then tools" or "first tools, then
// text" — the same model the openai driver produces.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
)

var _ llm.Driver = (*Driver)(nil)

// Driver is the anthropic-sdk-go-backed implementation of
// llm.Driver. Construction wires the per-role fields (model,
// base URL, API key, timeouts); the 8 production tools and the
// h4 wire surface are baked in.
type Driver struct {
	client    anthropic.Client
	role      llm.RoleConfig
	log       zerolog.Logger
	prodTools []anthropic.ToolUnionParam
	// thinkingAsAuthToken is true when the API URL points
	// at a provider that does not accept `x-api-key` and
	// requires `Authorization: Bearer` (e.g. ollama.com).
	// Auto-detected from role.APIURL.
	thinkingAsAuthToken bool
	// slow is optional; when set, thinking blocks are
	// emitted as slowlog events. If nil, thinking is
	// silently dropped.
	slow *slowlog.Logger
}

// New constructs a Driver. role carries per-role fields
// (model, base URL, API key, timeouts). The 8 production
// tools are baked in at construction.
func New(role llm.RoleConfig, log zerolog.Logger) *Driver {
	return newWithSlowlog(role, log, nil)
}

// NewWithSlowlog is the slowlog-enabled constructor used by
// the bot at boot. probe tools can use New (without slowlog)
// since they don't need diagnostics.
func NewWithSlowlog(role llm.RoleConfig, log zerolog.Logger, slow *slowlog.Logger) *Driver {
	return newWithSlowlog(role, log, slow)
}

func newWithSlowlog(role llm.RoleConfig, log zerolog.Logger, slow *slowlog.Logger) *Driver {
	log = log.With().Str("component", "llm.anthropic").Str("model", role.Model).Logger()
	d := &Driver{
		role: role,
		log:  log,
		slow: slow,
	}
	// ollama.com (and a handful of other Anthropic-compatible
	// providers) accept only Authorization: Bearer, not
	// x-api-key. Auto-detect by host.
	d.thinkingAsAuthToken = isBearerHost(role.APIURL)

	clientOpts := []option.RequestOption{
		option.WithBaseURL(role.APIURL),
		option.WithRequestTimeout(time.Duration(orDefault(role.RequestTimeoutSeconds, 120)) * time.Second),
		// Prompt caching: marks system prompt, tools, and
		// long-lived user content as cacheable breakpoints so
		// repeated turns reuse the cached prefix instead of
		// re-reading the full prompt. The beta header is
		// required by ollama.com and older Anthropic API
		// versions; newer deployments recognise cache_control
		// natively.
		option.WithHeaderAdd("anthropic-beta", string(anthropic.AnthropicBetaPromptCaching2024_07_31)),
	}
	if d.thinkingAsAuthToken {
		clientOpts = append(clientOpts, option.WithAuthToken(role.APIKey))
	} else {
		clientOpts = append(clientOpts, option.WithAPIKey(role.APIKey))
	}
	d.client = anthropic.NewClient(clientOpts...)

	for _, t := range domain.ProdTools() {
		paramsBytes, err := t.MarshalParameters()
		if err != nil {
			panic(fmt.Sprintf("anthropic: marshal tool %q: %v", t.Function.Name, err))
		}
		// Anthropic's input_schema has specific top-level fields:
		//   {type, properties, required}
		// with additionalProperties passed through ExtraFields.
		// We cannot dump the whole schema into Properties like
		// OpenAI — that puts "additionalProperties" (a bool) inside
		// the properties dict, and the Anthropic SDK tries to
		// unmarshal it as api.ToolProperty (a struct), producing
		// "cannot unmarshal bool into Go struct field".
		var schema map[string]any
		if err := json.Unmarshal(paramsBytes, &schema); err != nil {
			panic(fmt.Sprintf("anthropic: reparse tool %q: %v", t.Function.Name, err))
		}
		inputSchema := anthropic.ToolInputSchemaParam{}
		if props, ok := schema["properties"].(map[string]any); ok {
			inputSchema.Properties = props
		}
		if req, ok := schema["required"].([]any); ok {
			required := make([]string, 0, len(req))
			for _, r := range req {
				if s, ok := r.(string); ok {
					required = append(required, s)
				}
			}
			inputSchema.Required = required
		}
		// Pass through additionalProperties (bool) and any
		// other top-level schema keys the API might accept.
		// This must go through ExtraFields so the SDK
		// MarshalJSON picks it up and does not try to type-
		// coerce it into api.ToolProperty.
		extra := make(map[string]any)
		if ap, ok := schema["additionalProperties"]; ok {
			extra["additionalProperties"] = ap
		}
		if len(extra) > 0 {
			inputSchema.ExtraFields = extra
		}
		d.prodTools = append(d.prodTools, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        t.Function.Name,
				Description: anthropic.String(t.Function.Description),
				InputSchema: inputSchema,
				Strict:      anthropic.Bool(true),
			},
		})
	}
	// Prompt caching: mark the last tool as a cache breakpoint
	// so the tools prefix is cached across turns. This saves
	// ~1.5k tokens of tool declarations on every request.
	if len(d.prodTools) > 0 {
		tool := d.prodTools[len(d.prodTools)-1].OfTool
		if tool != nil {
			tool.CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
	}
	return d
}

func (d *Driver) Close() error { return nil }

// Stream sends a chat request to the configured provider and
// invokes onChunk for every parsed delta. h4 wire surface
// (system-prompt for narrative, tools=auto, strict=true) is
// built in buildRequest.
func (d *Driver) Stream(ctx context.Context, req llm.ChatRequest, onChunk func(llm.Chunk) error) error {
	params, err := d.buildRequest(req)
	if err != nil {
		return fmt.Errorf("anthropic: build request: %w", err)
	}

	timeout := req.TimeoutSeconds
	if timeout <= 0 {
		timeout = d.role.RequestTimeoutSeconds
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
	}

	// Anthropic SDK v1.48 has a non-streaming New() and
	// NewStreaming(). We use non-streaming because:
	//   1. the openai driver streams, but the upstream
	//      Messages API is request/response; we accept the
	//      latency cost for cleaner code;
	//   2. thinking-block ordering is preserved by the
	//      server-side response (tools come first, then text,
	//      then end_turn).
	//
	// Operators who need lower TTFT can swap in NewStreaming
	// later — the chunk shape stays the same.
	resp, err := d.client.Messages.New(ctx, params)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("anthropic: deadline: %w", err)
		}
		return fmt.Errorf("anthropic: request: %w", err)
	}
	// Capture raw response for diagnostics on broken/empty
	// responses. Non-streaming API, so this is the full body.
	var rawTrace []string
	if raw, err := json.Marshal(resp); err == nil {
		rawTrace = []string{string(raw)}
	}
	return d.emitBlocks(ctx, resp, rawTrace, onChunk)
}

// emitBlocks walks the Anthropic response.Content blocks in
// order, dispatching text and tool_use to onChunk. Thinking
// blocks (Type="thinking" or non-standard `tool_use` blocks
// with empty name+input that some providers emit) are logged
// to slowlog and dropped.
func (d *Driver) emitBlocks(ctx context.Context, resp *anthropic.Message, rawTrace []string, onChunk func(llm.Chunk) error) error {
	// OpenAI finish vocabulary mapping.
	finish := mapAnthropicStopReason(resp.StopReason)

	// First emit any tool_use blocks as a single chunk.
	// (Anthropic returns tool_use blocks in the order the
	// model emitted them; we forward all of them in one
	// chunk so the GM's tool-dispatch loop can batch.)
	var tools []llm.ToolCall
	for _, b := range resp.Content {
		tu := b.AsToolUse()
		if tu.Type == "" {
			continue
		}
		// Filter thinking-blocks disguised as tool_use
		// (openrouter convention).
		if tu.Name == "" && len(tu.Input) == 0 {
			d.logThinking(string(tu.ID), string(tu.Input))
			continue
		}
		tools = append(tools, llm.ToolCall{
			ID:   tu.ID,
			Type: "function",
			Function: llm.FunctionCall{
				Name:      tu.Name,
				Arguments: string(tu.Input),
			},
		})
	}
	if len(tools) > 0 {
		if err := onChunk(llm.Chunk{ToolCalls: tools, Finish: finish, RawTrace: rawTrace}); err != nil {
			return err
		}
	}

	// Then emit text blocks as one chunk (concatenated).
	var text strings.Builder
	for _, b := range resp.Content {
		tb := b.AsText()
		if tb.Type == "" {
			continue
		}
		text.WriteString(tb.Text)
	}
	if text.Len() > 0 {
		if err := onChunk(llm.Chunk{Content: text.String(), Finish: finish, RawTrace: rawTrace}); err != nil {
			return err
		}
	}

	// Usage + terminal marker.
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		_ = onChunk(llm.Chunk{
			Finish:   finish,
			RawTrace: rawTrace,
			Usage: llm.Usage{
				PromptTokens:     int(resp.Usage.InputTokens),
				CompletionTokens: int(resp.Usage.OutputTokens),
				TotalTokens:      int(resp.Usage.InputTokens + resp.Usage.OutputTokens),
			},
		})
	}
	if err := onChunk(llm.Chunk{Done: true, RawTrace: rawTrace}); err != nil {
		return fmt.Errorf("anthropic: done callback: %w", err)
	}
	return nil
}

// buildRequest translates ChatRequest into anthropic.MessageNewParams.
// Wire surface: 8 prod tools, tool_choice=auto, strict=true.
func (d *Driver) buildRequest(req llm.ChatRequest) (anthropic.MessageNewParams, error) {
	messages := make([]anthropic.MessageParam, 0, len(req.Messages))
	var systemBlocks []anthropic.TextBlockParam
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			systemBlocks = append(systemBlocks, anthropic.TextBlockParam{Text: m.Content})
		case "user":
			messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		case "assistant":
			// Re-emit assistant turns: text + tool_use.
			// The GM puts tool_use IDs back so the next
			// request can carry tool_result blocks.
			var blocks []anthropic.ContentBlockParamUnion
			if m.Content != "" {
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfText: &anthropic.TextBlockParam{Text: m.Content},
				})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolUse: &anthropic.ToolUseBlockParam{
						ID:    tc.ID,
						Name:  tc.Function.Name,
						Input: json.RawMessage(tc.Function.Arguments),
					},
				})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfText: &anthropic.TextBlockParam{Text: ""},
				})
			}
			messages = append(messages, anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleAssistant,
				Content: blocks,
			})
		case "tool":
			messages = append(messages, anthropic.NewUserMessage(
				anthropic.NewToolResultBlock(m.ToolCallID, m.Content, false),
			))
		default:
			return anthropic.MessageNewParams{}, fmt.Errorf("anthropic: unknown message role %q", m.Role)
		}
	}

	// Prompt caching: mark the last system block as a cache
	// breakpoint. The system prompt is ~24k chars and rarely
	// changes between turns — caching it saves ~6k input tokens
	// per request after the first.
	if len(systemBlocks) > 0 {
		systemBlocks[len(systemBlocks)-1].CacheControl = anthropic.NewCacheControlEphemeralParam()
	}

	// Prompt caching: mark the first user message content as a
	// cache breakpoint (index:1 caching). The first user message
	// carries the full world state (state.md, NPC profiles, etc.)
	// which changes slowly across turns. This is the most
	// impactful cache target: it caches everything from system
	// prompt through tools through the first user message.
	for i, msg := range messages {
		if msg.Role == anthropic.MessageParamRoleUser {
			messages[i] = anthropic.MessageParam{
				Role:    msg.Role,
				Content: appendCacheControlToFirstUserBlock(msg.Content),
			}
			break
		}
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: int64(orDefault(req.MaxTokens, 1500)),
		System:    systemBlocks,
		Messages:  messages,
		Tools:     d.prodTools,
		ToolChoice: anthropic.ToolChoiceUnionParam{
			OfAuto: &anthropic.ToolChoiceAutoParam{},
		},
	}
	if req.Temperature > 0 {
		params.Temperature = anthropic.Float(req.Temperature)
	}
	// When the operator set disable_thinking=true (or
	// reasoning_effort=none), explicitly send
	// thinking:{type:"disabled"} so Anthropic-compatible
	// providers do not force extended thinking. Without
	// this, RouterAI and other proxies default to enabled
	// thinking, which leaks reasoning text into the stream
	// and wastes ~1.5k output tokens per turn.
	if d.role.DisableThinking || d.role.ReasoningEffort == "none" {
		disabled := anthropic.NewThinkingConfigDisabledParam()
		params.Thinking = anthropic.ThinkingConfigParamUnion{
			OfDisabled: &disabled,
		}
	}
	return params, nil
}

// appendCacheControlToFirstUserBlock walks a Content slice and
// sets CacheControl on the first text-like block. This marks
// the index:1 cache breakpoint so Anthropic caches system
// prompt + tools + the world-state user message across turns.
// Subsequent user messages (narrative turns) are NOT cached.
func appendCacheControlToFirstUserBlock(content []anthropic.ContentBlockParamUnion) []anthropic.ContentBlockParamUnion {
	for i := range content {
		b := &content[i]
		if b.OfText != nil {
			b.OfText.CacheControl = anthropic.NewCacheControlEphemeralParam()
			return content
		}
		if b.OfToolResult != nil {
			b.OfToolResult.CacheControl = anthropic.NewCacheControlEphemeralParam()
			return content
		}
	}
	return content
}

// logThinking records a thinking-block to slowlog. Provider
// convention: openrouter returns empty-name tool_use blocks;
// we treat them as thinking.
func (d *Driver) logThinking(id, raw string) {
	if d.slow == nil {
		return
	}
	_ = d.slow.Write("anthropic.thinking", "", map[string]any{
		"id":        id,
		"raw_chars": len(raw),
	})
}

// mapAnthropicStopReason normalises the Anthropic stop_reason
// vocabulary onto the OpenAI set the GM already understands.
func mapAnthropicStopReason(sr anthropic.StopReason) string {
	switch string(sr) {
	case "end_turn":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	default:
		return string(sr)
	}
}

// isBearerHost returns true when the API URL belongs to a
// provider that requires `Authorization: Bearer` instead of
// `x-api-key`. Today: ollama.com (and its Anarchy/proxy
// mirrors). Anthropic / OpenRouter / Bedrock all accept the
// standard `x-api-key` header.
func isBearerHost(apiURL string) bool {
	return strings.Contains(apiURL, "ollama.com") || strings.Contains(apiURL, "/api")
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
