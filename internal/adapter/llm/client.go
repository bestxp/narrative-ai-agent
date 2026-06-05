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
	ToolCalls []ToolCall // emitted on the chunk that finalises them
	Finish    string     // "stop" | "tool_calls" | "length" | ...
	Done      bool       // last chunk in the stream
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
	if c.role.RequestTimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(c.role.RequestTimeoutSeconds)*time.Second)
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
				_ = onChunk(Chunk{Done: true})
				return nil
			}
			chunk, perr := decodeChunk(payload)
			if perr != nil {
				log.Warn().Err(perr).Str("payload", truncate(payload, 200)).Msg("skip malformed chunk")
				continue
			}
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
	_ = onChunk(Chunk{Done: true})
	return nil
}

// decodeChunk turns a single SSE JSON payload into our Chunk shape.
// The OpenAI wire format nests choices[0].delta.content /
// choices[0].delta.tool_calls.
func decodeChunk(payload string) (Chunk, error) {
	var raw struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Delta        struct {
				Content   string `json:"content"`
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
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return Chunk{}, err
	}
	out := Chunk{}
	if len(raw.Choices) == 0 {
		return out, nil
	}
	delta := raw.Choices[0].Delta
	out.Content = delta.Content
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
