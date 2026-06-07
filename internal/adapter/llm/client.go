// Package llm is the OpenAI-compatible chat completions adapter. It
// is small on purpose: the only model interaction the bot needs is
// "send a chat with optional tool definitions, stream the response,
// dispatch any tool calls back to the orchestrator".
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// RoleConfig is a snapshot of the relevant subset of
// config.LLMRoleConfig. Keeping it independent of the config package
// lets the adapter be tested with synthetic values.
type RoleConfig struct {
	APIURL                string
	APIKey                string
	Model                 string
	MaxTokens             int
	Temperature           float64
	RequestTimeoutSeconds int
	// DisableThinking mirrors the YAML flag. When true, the
	// wire request sets `reasoning_effort: "none"` to skip
	// chain-of-thought on the providers that recognise it
	// (Ollama via /v1/chat/completions, OpenAI reasoning
	// models, xAI Grok, OpenRouter). Kept on the role so the
	// wire translation happens once at request time, not per
	// call. See ChatRequest.ReasoningEffort.
	DisableThinking bool
	// ReasoningEffort overrides DisableThinking for the
	// cases where the operator wants a level other than off
	// (e.g. "low" for GPT-OSS which rejects "none"). Empty
	// string means "no override"; when DisableThinking is
	// true and ReasoningEffort is empty we default to "none".
	ReasoningEffort string
	// MaxEmptyRetries is the number of automatic re-issues of
	// the same LLM request when the previous round produced 0
	// content. 0 disables auto-retry (the bot surfaces the
	// "model returned empty" placeholder immediately).
	MaxEmptyRetries int
	// EmptyRetryTimeoutSeconds is the per-retry HTTP timeout
	// for the auto-retry rounds. Cloud Ollama is slow under
	// load (50-90s per response on the minimax-m3:cloud tier)
	// and the default per-role timeout may be too tight when
	// the model is mid-thought. Set 0 to fall back to
	// RequestTimeoutSeconds.
	EmptyRetryTimeoutSeconds int
}

// Message is a single chat entry.
type Message struct {
	Role       string  `json:"role"`
	Content    string  `json:"content"`
	Name       string  `json:"name,omitempty"`
	ToolCallID string  `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall mirrors the OpenAI shape.
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

// ChatRequest is what we POST to /chat/completions.
type ChatRequest struct {
	Model       string       `json:"model"`
	Messages    []Message    `json:"messages"`
	Tools       []ToolSchema `json:"-"`
	WireTools   []wireTool   `json:"tools,omitempty"`
	Temperature float64      `json:"temperature"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
	Stream      bool         `json:"stream"`
	// ReasoningEffort turns off / dials down chain-of-thought
	// reasoning on providers that recognise the OpenAI-standard
	// `reasoning_effort` field. Empty string means "do not
	// emit the key at all" — for Qwen3/DeepSeek R1 that leaves
	// thinking ON (Ollama's default for thinking-capable
	// models), for OpenAI's reasoning models it falls back to
	// the provider default. Set to "none" to disable, or
	// "low"/"medium"/"high" to dial the effort down.
	//
	// Wire coverage: Ollama via /v1/chat/completions (the
	// surface the bot uses), OpenAI reasoning models, xAI
	// Grok, OpenRouter. The native Ollama `think: true/false`
	// on /api/chat is NOT accepted on the OpenAI-compat
	// surface — we have to use reasoning_effort there.
	//
	// Operators of GPT-OSS should pass "low" (not "none";
	// GPT-OSS rejects "none") and accept the residual cost.
	// See: https://ollama.com/blog/thinking
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// TimeoutSeconds overrides the role's per-request
	// timeout for this single call. 0 means "use the
	// role's RequestTimeoutSeconds". Set higher on retry
	// rounds when the model is mid-thought and the
	// provider is slow under load.
	TimeoutSeconds int `json:"-"`
	// Extra is an out-of-band bag for per-request
	// overrides that do not belong on the public
	// ChatRequest struct (so the legacy wire format
	// stays unchanged). The openai-go driver reads
	// fields like "openai.response_format" from here
	// to wire response_format / tool_choice / strict
	// tools per call without polluting the request
	// type with conditional fields. Callers that do
	// not set Extra have it default to nil and the
	// driver falls back to its role-level defaults.
	Extra map[string]any `json:"-"`
}

// ToolSchema is the public-facing tool declaration. We accept the
// already-serialised JSON Schema so the domain layer can own the
// tool definitions.
type ToolSchema struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// wireTool and wireToolFunction are the OpenAI-shaped wrappers. The
// public ToolSchema keeps just the function body for ergonomics;
// Stream() rewraps it.
type wireTool struct {
	Type     string             `json:"type"`
	Function wireToolFunction   `json:"function"`
}

type wireToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Chunk is a single streaming or non-streaming fragment.
type Chunk struct {
	Content   string     // delta text (may be empty)
	Reasoning string     // delta reasoning / chain-of-thought (o1, minimax-m3:cloud)
	ToolCalls []ToolCall // emitted on the chunk that finalises them
	Finish    string     // "stop" | "tool_calls" | "length" | ...
	Done      bool       // last chunk in the stream
	// Usage is the provider-reported token count for the entire
	// request so far. Most providers emit it on the very last
	// chunk (the one with finish_reason set); Ollama omits it
	// entirely. Callers that need a number regardless should fall
	// back to an estimate.
	Usage Usage
	// RawTrace is a sampling of the raw SSE payloads seen while
	// parsing the stream. Populated on every chunk; callers that
	// hit an "empty assistant turn" can dump it to slowlog to see
	// what the provider actually sent (truncated to 200 chars per
	// entry; keeps the first 3 and last 2 events).
	RawTrace []string
}

// Usage mirrors the OpenAI usage block. PromptTokens counts the
// input messages, CompletionTokens counts the streamed response,
// TotalTokens is the sum and the value most operators actually
// want.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Client is a thin HTTP wrapper. It exposes one entry point, Stream,
// which yields chunks via the supplied callback. Streaming is the
// only mode the bot uses; non-streaming helpers can be added later
// if some cheap LLM role prefers request/response semantics.
type Client struct {
	http  *http.Client
	role  RoleConfig
	log   zerolog.Logger
}

// New constructs a Client. The http.Client is owned by the caller in
// the sense that the caller passes the per-role timeout via context.
func New(role RoleConfig, log zerolog.Logger) *Client {
	return &Client{
		http: &http.Client{},
		role: role,
		log:  log.With().Str("component", "llm").Str("model", role.Model).Logger(),
	}
}

// Close releases the underlying *http.Client. The legacy
// transport has no pooled keep-alive connections beyond
// what *http.Client holds, so Close is a no-op kept for
// symmetry with the openai-go driver (which has its own
// connection pool). Calling Close more than once is safe.
func (c *Client) Close() error { return nil }

// Compile-time guarantee that the legacy *Client satisfies the
// Driver interface. If we change either signature the build
// fails here rather than at the call site in cmd/bot/main.go.
var _ Driver = (*Client)(nil)

// Stream calls /chat/completions with stream=true and invokes onChunk
// for every parsed SSE delta. The callback returns an error to abort
// the stream — useful when the user cancels or the transport's
// streaming budget runs out.
func (c *Client) Stream(ctx context.Context, req ChatRequest, onChunk func(Chunk) error) error {
	req.Stream = true
	// OpenAI wire format wraps every tool in {"type":"function",
	// "function": {...}}. Our internal ToolSchema keeps just the
	// function body — wrap it here.
	if len(req.Tools) > 0 {
		wire := make([]wireTool, 0, len(req.Tools))
		for _, ts := range req.Tools {
			wire = append(wire, wireTool{
				Type: "function",
				Function: wireToolFunction{
					Name:        ts.Name,
					Description: ts.Description,
					Parameters:  ts.Parameters,
				},
			})
		}
		req.Tools = nil
		req.WireTools = wire
	}
	// Translate the role's DisableThinking / ReasoningEffort
	// into a top-level `reasoning_effort` field on the wire.
	//
	// Priority:
	//   1. Caller-supplied ReasoningEffort (per-request
	//      override) wins over everything.
	//   2. Role.ReasoningEffort (explicit level like "low").
	//   3. Role.DisableThinking → "none".
	//   4. Default: leave the field off so the provider's own
	//      default applies (Qwen3/R1 default to thinking ON
	//      on Ollama, OFF on OpenAI non-reasoning models).
	//
	// The empty-string check is critical: serialising an
	// empty `reasoning_effort` would silently override the
	// provider default, which is the exact bug that bit us on
	// the minimax-m3:cloud deployment.
	if req.ReasoningEffort == "" {
		switch {
		case c.role.ReasoningEffort != "":
			req.ReasoningEffort = c.role.ReasoningEffort
		case c.role.DisableThinking:
			req.ReasoningEffort = "none"
		}
	}
	timeoutSec := c.role.RequestTimeoutSeconds
	if req.TimeoutSeconds > 0 {
		timeoutSec = req.TimeoutSeconds
	}
	if timeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		defer cancel()
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("llm: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.role.APIURL, "/")+"/chat/completions",
		bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("llm: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.role.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.role.APIKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("llm: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("llm: http %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}
	return parseSSE(resp.Body, onChunk, c.log)
}

// parseSSE reads Server-Sent Events line-by-line. Each event is a
// `data: {...}` line, terminated by an empty line. The stream ends
// when the server sends `data: [DONE]`.
func parseSSE(r io.Reader, onChunk func(Chunk) error, log zerolog.Logger) error {
	br := bufio.NewReaderSize(r, 32*1024)
	var pending bytes.Buffer
	// Diagnostic capture: when the stream yields zero content
	// (a common symptom of "Ollama returned only [DONE]" or a
	// provider that emits tool_calls without any visible text),
	// we want to see the raw payloads in the slowlog so the
	// operator can distinguish "model thought for 30s then
	// produced nothing" from "model refused" from "model
	// crashed mid-stream". We always keep the first 3 and the
	// last 2 — a 200-char preview of each is enough.
	var rawSeen []string
	const keepFirst = 3
	const keepLast = 2
	const previewLen = 200
	for {
		line, err := br.ReadString('\n')
		if err != nil && line == "" {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("llm: sse read: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			// Empty line terminates an event.
			if pending.Len() == 0 {
				continue
			}
			payload := pending.String()
			pending.Reset()
			if payload == "[DONE]" {
				_ = onChunk(Chunk{Done: true, RawTrace: rawSeen})
				return nil
			}
			// Capture raw payload for the diagnostic trace.
			// We keep the first N verbatim and the last M
			// only if the total event count exceeds the
			// first-N budget — that way a 1000-chunk stream
			// doesn't blow up the trace, but a 5-chunk
			// "broken" stream keeps everything.
			if len(rawSeen) < keepFirst+keepLast {
				prev := payload
				if len(prev) > previewLen {
					prev = prev[:previewLen] + "…"
				}
				rawSeen = append(rawSeen, prev)
			} else {
				rawSeen = append(rawSeen[keepFirst-1:], prev(payload, previewLen))
			}
			chunk, perr := decodeChunk(payload)
			if perr != nil {
				log.Warn().Err(perr).Str("payload", truncate(payload, 200)).Msg("skip malformed chunk")
				continue
			}
			chunk.RawTrace = rawSeen
			if cerr := onChunk(chunk); cerr != nil {
				return cerr
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			pending.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		// Other SSE lines (event:, id:, retry:, comments starting with
		// ':') are intentionally ignored — OpenAI servers do not use
		// them in chat completions.
	}
	// Server closed without [DONE] — treat as graceful stop.
	_ = onChunk(Chunk{Done: true, RawTrace: rawSeen})
	return nil
}

func prev(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// decodeChunk turns a single SSE JSON payload into our Chunk shape.
// The OpenAI wire format nests choices[0].delta.content /
// choices[0].delta.tool_calls. The top-level "usage" block is
// provider-emitted once per request, usually on the final chunk.
//
// Some providers (o1 family, Ollama Cloud minimax-m3:cloud) also
// emit delta.reasoning — chain-of-thought that arrives in
// parallel with empty delta.content. The reasoning text is
// surfaced to the caller via Chunk.Reasoning so it can be
// counted, logged, or surfaced as a "thinking…" status. We
// never write it to the user-visible stream — only the final
// delta.content matters for that.
func decodeChunk(payload string) (Chunk, error) {
	var raw struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Delta        struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
		Usage *Usage `json:"usage,omitempty"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return Chunk{}, err
	}
	out := Chunk{}
	if raw.Usage != nil {
		out.Usage = *raw.Usage
	}
	if len(raw.Choices) == 0 {
		return out, nil
	}
	delta := raw.Choices[0].Delta
	out.Content = delta.Content
	out.Reasoning = delta.Reasoning
	out.Finish = raw.Choices[0].FinishReason
	for _, tc := range delta.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:       tc.ID,
			Type:     tc.Type,
			Function: FunctionCall{Name: tc.Function.Name, Arguments: tc.Function.Arguments},
		})
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
