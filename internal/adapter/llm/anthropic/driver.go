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
		// Anthropic strict input_schema is the same shape
		// as OpenAI's: top-level {type, properties, required}.
		// We pass it as a raw FunctionParameters map; the
		// SDK marshals to the wire.
		var fp map[string]any
		if err := json.Unmarshal(paramsBytes, &fp); err != nil {
			panic(fmt.Sprintf("anthropic: reparse tool %q: %v", t.Function.Name, err))
		}
		d.prodTools = append(d.prodTools, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        t.Function.Name,
				Description: anthropic.String(t.Function.Description),
				InputSchema: anthropic.ToolInputSchemaParam{
					Properties: fp,
				},
				Strict: anthropic.Bool(true),
			},
		})
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
	return d.emitBlocks(ctx, resp, onChunk)
}

// emitBlocks walks the Anthropic response.Content blocks in
// order, dispatching text and tool_use to onChunk. Thinking
// blocks (Type="thinking" or non-standard `tool_use` blocks
// with empty name+input that some providers emit) are logged
// to slowlog and dropped.
func (d *Driver) emitBlocks(ctx context.Context, resp *anthropic.Message, onChunk func(llm.Chunk) error) error {
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
		if err := onChunk(llm.Chunk{ToolCalls: tools, Finish: finish}); err != nil {
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
		if err := onChunk(llm.Chunk{Content: text.String(), Finish: finish}); err != nil {
			return err
		}
	}

	// Usage + terminal marker.
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		_ = onChunk(llm.Chunk{
			Finish: finish,
			Usage: llm.Usage{
				PromptTokens:     int(resp.Usage.InputTokens),
				CompletionTokens: int(resp.Usage.OutputTokens),
				TotalTokens:      int(resp.Usage.InputTokens + resp.Usage.OutputTokens),
			},
		})
	}
	if err := onChunk(llm.Chunk{Done: true}); err != nil {
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
		// Anthropic v1.48 has Temperature directly on params.
		params.Temperature = anthropic.Float(req.Temperature)
	}
	return params, nil
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
