// Package openai provides an llm.Driver implementation built
// on top of the official github.com/openai/openai-go/v3 SDK.
//
// **H4-by-default config**: this driver hardcodes the
// production wire surface (see internal/config + research.md
// for the probe results). It is the only set of parameters
// supported; there are no configuration knobs for:
//
//   - response_format: always `json_object` (the 4-field
//     narrative shape is described in the system prompt).
//   - tool_choice: always "auto" (the model decides when to
//     call tools; КОНТЕКСТ-парсинг остаётся как лог-fallback
//     в gm.go:797 на случай провайдерского thinking overflow).
//   - strict_tools: always true (every tool declaration
//     gets `strict: true` on the FunctionDefinitionParam).
//   - the 8 production tools: imported from
//     internal/domain.ProdTools() at startup.
//
// The driver speaks /v1/chat/completions (the OpenAI wire
// format every compatible provider accepts: OpenAI, OpenRouter,
// xAI, Ollama's /v1 surface, routerai.ru).
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
	"github.com/openai/openai-go/v3/shared"
	"github.com/rs/zerolog"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/llm"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
)

var _ llm.Driver = (*Driver)(nil)

// Driver is the openai-go-backed implementation of llm.Driver.
type Driver struct {
	client openaisdk.Client
	role   llm.RoleConfig
	log    zerolog.Logger
	// prodTools is the 8 production schemas (domain.ProdTools())
	// serialised to the wire format. Computed once at
	// construction so every chat request reuses the same
	// tool declarations.
	prodTools []openaisdk.ChatCompletionToolUnionParam
}

// New constructs a Driver. role carries per-role fields
// (model, base URL, API key, timeouts). The 8 production
// tools are baked in at construction.
func New(role llm.RoleConfig, log zerolog.Logger) *Driver {
	log = log.With().Str("component", "llm.openai").Str("model", role.Model).Logger()
	d := &Driver{
		client: openaisdk.NewClient(
			option.WithBaseURL(role.APIURL),
			option.WithAPIKey(role.APIKey),
			option.WithRequestTimeout(time.Duration(orDefault(role.RequestTimeoutSeconds, 120))*time.Second),
		),
		role: role,
		log:  log,
	}
	for _, t := range domain.ProdTools() {
		paramsBytes, err := t.MarshalParameters()
		if err != nil {
			panic(fmt.Sprintf("openai: marshal tool %q: %v", t.Function.Name, err))
		}
		var fp shared.FunctionParameters
		if err := json.Unmarshal(paramsBytes, &fp); err != nil {
			panic(fmt.Sprintf("openai: reparse tool %q: %v", t.Function.Name, err))
		}
		d.prodTools = append(d.prodTools, openaisdk.ChatCompletionToolUnionParam{
			OfFunction: &openaisdk.ChatCompletionFunctionToolParam{
				Function: shared.FunctionDefinitionParam{
					Name:        t.Function.Name,
					Description: openaisdk.Opt(t.Function.Description),
					Strict:      openaisdk.Opt(true),
					Parameters:  fp,
				},
			},
		})
	}
	return d
}

func (d *Driver) Close() error { return nil }

// Stream sends a chat request to the configured provider and
// invokes onChunk for every parsed delta. h4 wire surface
// (json_object + tools + auto + strict) is built in buildParams.
func (d *Driver) Stream(ctx context.Context, req llm.ChatRequest, onChunk func(llm.Chunk) error) error {
	params, err := d.buildParams(req)
	if err != nil {
		return fmt.Errorf("openai: build params: %w", err)
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

	stream := d.client.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()

	acc := newToolAccumulator(d.log)
	var rawTrace []string
	for stream.Next() {
		chunk := stream.Current()
		// Capture raw SSE payload for diagnostics on broken
		// responses (empty content + empty tools). We keep
		// only the last 5 chunks so the trace does not grow
		// unbounded on healthy round-trips.
		if raw, err := json.Marshal(chunk); err == nil {
			rawTrace = append(rawTrace, string(raw))
			if len(rawTrace) > 5 {
				rawTrace = rawTrace[len(rawTrace)-5:]
			}
		}
		for _, choice := range chunk.Choices {
			out := llm.Chunk{Finish: choice.FinishReason}
			if choice.Delta.Content != "" {
				out.Content = choice.Delta.Content
			}
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
			out.RawTrace = rawTrace
			if err := onChunk(out); err != nil {
				return fmt.Errorf("openai: chunk callback: %w", err)
			}
		}
	}
	if err := stream.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("openai: stream deadline: %w", err)
		}
		return fmt.Errorf("openai: stream: %w", err)
	}
	if err := onChunk(llm.Chunk{Done: true, RawTrace: rawTrace}); err != nil {
		return fmt.Errorf("openai: done callback: %w", err)
	}
	return nil
}

// buildParams translates ChatRequest into openai-go params.
// Wire surface is h4-hardcoded:
//
//   - response_format: {"type":"json_object"}
//   - tools: 8 prod tools from domain.ProdTools()
//   - tool_choice: "auto"
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

	// Prefill bracket: inject a synthetic assistant turn with
	// content "{" to force the model to emit JSON immediately.
	// This is the "assistant pre-fill" trick described in the
	// article — it suppresses intro paragraphs and markdown
	// wrappers from local models (Ollama).
	if d.role.UsePrefillBracket {
		d.log.Info().Bool("prefill_bracket", true).Int("msg_index", len(messages)).Msg("openai: injecting prefill assistant turn")
		messages = append(messages, openaisdk.ChatCompletionMessageParamUnion{
			OfAssistant: &openaisdk.ChatCompletionAssistantMessageParam{
				Content: openaisdk.ChatCompletionAssistantMessageParamContentUnion{
					OfString: openaisdk.Opt("{"),
				},
			},
		})
	} else {
		d.log.Info().Bool("prefill_bracket", false).Msg("openai: prefill bracket disabled")
	}

	params := openaisdk.ChatCompletionNewParams{
		Model:    shared.ChatModel(req.Model),
		Messages: messages,
		StreamOptions: openaisdk.ChatCompletionStreamOptionsParam{
			IncludeUsage: openaisdk.Opt(true),
		},
		// h4: json_object 4-field narrative. The 4 fields
		// are described in the system prompt (narrative.md
		// Режим A). Model emits clean JSON without fence.
		ResponseFormat: openaisdk.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
		},
		// h4: tool_choice=auto. Model decides when to
		// call tools. The strict schema and 8-tool list
		// are baked in via the constructor.
		ToolChoice: openaisdk.ChatCompletionToolChoiceOptionUnionParam{
			OfAuto: openaisdk.Opt("auto"),
		},
		Tools: d.prodTools,
	}
	if req.Temperature > 0 {
		params.Temperature = openaisdk.Opt(req.Temperature)
	}
	if req.MaxTokens > 0 {
		// OpenAI v3 deprecates `max_tokens` in favour of
		// `max_completion_tokens`. We send the new key on
		// the wire; providers that only accept the old key
		// (older Ollama) silently ignore it.
		params.MaxCompletionTokens = openaisdk.Opt(int64(req.MaxTokens))
	}
	if req.ReasoningEffort != "" {
		params.ReasoningEffort = shared.ReasoningEffort(req.ReasoningEffort)
	}
	return params, nil
}

// toolAccumulator merges OpenAI's streaming tool-call deltas
// into complete llm.ToolCall entries. Drops broken arguments
// (Ollama Cloud occasionally streams malformed JSON).
type toolAccumulator struct {
	buf map[int64]*llm.ToolCall
	log zerolog.Logger
}

func newToolAccumulator(log zerolog.Logger) *toolAccumulator {
	return &toolAccumulator{buf: make(map[int64]*llm.ToolCall), log: log}
}

func (a *toolAccumulator) merge(deltas []openaisdk.ChatCompletionChunkChoiceDeltaToolCall) []llm.ToolCall {
	var changed bool
	for _, d := range deltas {
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

// unquoteJSONString strips a JSON-quoted string. Used to
// read the non-standard `reasoning` field (Ollama Cloud
// streams chain-of-thought under this key).
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
