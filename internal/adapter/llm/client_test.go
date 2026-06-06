package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func discardLog() zerolog.Logger { return zerolog.Nop() }

// fakeSSEServer is the shared helper: it accepts a request body
// validator, a sequence of payloads, and returns the test server plus
// the captured request body for assertions.
func fakeSSEServer(t *testing.T, validate func(map[string]any), payloads ...string) (*httptest.Server, *map[string]any) {
	t.Helper()
	captured := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		captured = body
		if validate != nil {
			validate(body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, p := range payloads {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", p)
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &captured
}

func TestStream_TextOnly(t *testing.T) {
	srv, captured := fakeSSEServer(t, nil,
		`{"choices":[{"delta":{"content":"Привет"}}]}`,
		`{"choices":[{"delta":{"content":", мир"}}]}`,
		`{"choices":[{"finish_reason":"stop","delta":{}}]}`,
	)
	c := New(RoleConfig{APIURL: srv.URL, APIKey: "x", Model: "m"}, discardLog())
	var got []string
	err := c.Stream(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(ch Chunk) error {
		if ch.Done {
			return nil
		}
		if ch.Content != "" {
			got = append(got, ch.Content)
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"Привет", ", мир"}, got)
	assert.Equal(t, true, (*captured)["stream"])
}

func TestStream_ToolCallsAreDecoded(t *testing.T) {
	srv, _ := fakeSSEServer(t, nil,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"end_day","arguments":"{\"day\":3,"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"summary\":\"бой\"}"}}]}}]}`,
		`{"choices":[{"finish_reason":"tool_calls","delta":{}}]}`,
	)
	c := New(RoleConfig{APIURL: srv.URL, Model: "m"}, discardLog())
	var tools []ToolCall
	err := c.Stream(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "играем"}},
	}, func(ch Chunk) error {
		tools = append(tools, ch.ToolCalls...)
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, tools)
	// Tool calls may come in pieces; verify we at least saw end_day
	// with the id call_1.
	saw := false
	for _, tc := range tools {
		if tc.ID == "call_1" && tc.Function.Name == "end_day" {
			saw = true
		}
	}
	assert.True(t, saw, "expected end_day tool call with id=call_1, got %+v", tools)
}

func TestStream_PropagatesCallbackError(t *testing.T) {
	srv, _ := fakeSSEServer(t, nil,
		`{"choices":[{"delta":{"content":"x"}}]}`,
	)
	c := New(RoleConfig{APIURL: srv.URL, Model: "m"}, discardLog())
	stop := errors.New("user cancelled")
	err := c.Stream(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(ch Chunk) error { return stop })
	assert.ErrorIs(t, err, stop)
}

func TestStream_SendsAuthHeader(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	c := New(RoleConfig{APIURL: srv.URL, APIKey: "secret-key", Model: "m"}, discardLog())
	require.NoError(t, c.Stream(context.Background(), ChatRequest{Model: "m"}, func(Chunk) error { return nil }))
	assert.Equal(t, "Bearer secret-key", auth)
}

func TestStream_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad"}`, http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	c := New(RoleConfig{APIURL: srv.URL, Model: "m"}, discardLog())
	err := c.Stream(context.Background(), ChatRequest{Model: "m"}, func(Chunk) error { return nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "400")
	assert.Contains(t, err.Error(), "bad")
}

func TestStream_RespectsContext(t *testing.T) {
	// Hanging handler: the only way out is the client-side context
	// expiring. We use the request's per-role timeout (50ms) and
	// assert the stream errors out instead of blocking forever.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	c := New(RoleConfig{APIURL: srv.URL, Model: "m", RequestTimeoutSeconds: 1}, discardLog())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := c.Stream(ctx, ChatRequest{Model: "m"}, func(Chunk) error { return nil })
	assert.Error(t, err, "context timeout should abort the stream")
}

func TestStream_RequestIncludesMessages(t *testing.T) {
	srv, captured := fakeSSEServer(t, func(body map[string]any) {
		msgs, _ := body["messages"].([]any)
		assert.Len(t, msgs, 2)
	},
		`{"choices":[{"delta":{"content":"x"}}]}`,
	)
	c := New(RoleConfig{APIURL: srv.URL, Model: "m"}, discardLog())
	_ = c.Stream(context.Background(), ChatRequest{
		Model: "m",
		Messages: []Message{
			{Role: "system", Content: "rules"},
			{Role: "user", Content: "hi"},
		},
		Tools: []ToolSchema{{
			Name:        "end_day",
			Description: "end",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}},
	}, func(Chunk) error { return nil })
	assert.Contains(t, *captured, "tools")
}

func TestTruncate(t *testing.T) {
	assert.Equal(t, "abc", truncate("abc", 10))
	assert.Equal(t, "abc…", truncate("abcdef", 3))
}

func TestDecodeChunk_EmptyChoices(t *testing.T) {
	ch, err := decodeChunk(`{"choices":[]}`)
	require.NoError(t, err)
	assert.Empty(t, ch.Content)
}

func TestDecodeChunk_FinishReason(t *testing.T) {
	ch, err := decodeChunk(`{"choices":[{"finish_reason":"length","delta":{"content":"x"}}]}`)
	require.NoError(t, err)
	assert.Equal(t, "x", ch.Content)
	assert.Equal(t, "length", ch.Finish)
}

func TestStream_MalformedChunkIsSkipped(t *testing.T) {
	srv, _ := fakeSSEServer(t, nil,
		`not json`,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
	)
	c := New(RoleConfig{APIURL: srv.URL, Model: "m"}, discardLog())
	var got string
	require.NoError(t, c.Stream(context.Background(), ChatRequest{Model: "m"},
		func(ch Chunk) error {
			if !ch.Done {
				got += ch.Content
			}
			return nil
		}))
	assert.Equal(t, "ok", got)
}

func TestStream_DoneWithoutDoneMarker(t *testing.T) {
	// Some servers just close the connection. We should still call
	// onChunk with Done=true so the caller can flush.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"delta":{"content":"hi"}}]}` + "\n\n"))
	}))
	t.Cleanup(srv.Close)
	c := New(RoleConfig{APIURL: srv.URL, Model: "m"}, discardLog())
	var doneCount int
	require.NoError(t, c.Stream(context.Background(), ChatRequest{Model: "m"},
		func(ch Chunk) error {
			if ch.Done {
				doneCount++
			}
			return nil
		}))
	assert.Equal(t, 1, doneCount)
}

func TestAPIURL_TrailingSlashTrimmed(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	c := New(RoleConfig{APIURL: srv.URL + "/", Model: "m"}, discardLog())
	require.NoError(t, c.Stream(context.Background(), ChatRequest{Model: "m"}, func(Chunk) error { return nil }))
	assert.Equal(t, "/chat/completions", got)
}

// TestStream_RawTrace_PopulatedOnEmptyContent is the regression
// test for the "model returns [DONE] with no deltas" case. The
// chunk callback must receive a non-empty RawTrace so the slowlog
// can show what the provider actually sent. Without this the
// operator would see `content_chars: 0` with no further
// information.
func TestStream_RawTrace_PopulatedOnEmptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Provider streams two chunks with no content and no
		// tool calls, then [DONE]. Common for Ollama when the
		// model decides to produce no text.
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":null}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	c := New(RoleConfig{APIURL: srv.URL, Model: "m"}, discardLog())
	var traces [][]string
	require.NoError(t, c.Stream(context.Background(), ChatRequest{Model: "m"}, func(ch Chunk) error {
		traces = append(traces, ch.RawTrace)
		return nil
	}))
	require.NotEmpty(t, traces)
	// At least the final Done chunk should carry a populated
	// trace (it includes everything seen so far).
	last := traces[len(traces)-1]
	require.NotEmpty(t, last, "final chunk should carry the full raw trace")
	for _, p := range last {
		assert.LessOrEqual(t, len(p), 250, "raw trace entries are truncated to ~200 chars")
	}
}

// TestStream_RawTrace_CappedAt5Entries ensures the trace does
// not grow unbounded for long streams. We send 20 chunks and
// assert the final RawTrace is at most 5 entries (3 first + 2
// last, per the keepFirst/keepLast budget).
func TestStream_RawTrace_CappedAt5Entries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i < 20; i++ {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n")
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	c := New(RoleConfig{APIURL: srv.URL, Model: "m"}, discardLog())
	var lastTrace []string
	require.NoError(t, c.Stream(context.Background(), ChatRequest{Model: "m"}, func(ch Chunk) error {
		lastTrace = ch.RawTrace
		return nil
	}))
	assert.LessOrEqual(t, len(lastTrace), 5, "raw trace should be capped (got %d)", len(lastTrace))
	assert.NotEmpty(t, lastTrace)
}

// TestStream_DisableThinking_FromRole verifies that the role's
// DisableThinking flag is translated into a top-level
// `reasoning_effort: "none"` field on the wire body. This is
// the regression test for the "minimax-m3:cloud streams a long
// reasoning trace and leaves delta.content empty" issue:
// without this, the model wastes 30-60 seconds and the player
// sees a frozen "…".
//
// Wire format: Ollama via /v1/chat/completions (the surface
// the bot uses) accepts reasoning_effort, NOT the native
// `think: true/false` from /api/chat. Same field works on
// OpenAI reasoning models, xAI Grok, OpenRouter. The native
// Ollama `think` is rejected on the OpenAI-compat endpoint.
// See: https://ollama.com/blog/thinking
func TestStream_DisableThinking_FromRole(t *testing.T) {
	srv, captured := fakeSSEServer(t, nil,
		`{"choices":[{"delta":{"content":"Привет"},"finish_reason":"stop"}]}`,
	)
	c := New(RoleConfig{APIURL: srv.URL, Model: "m", DisableThinking: true}, discardLog())
	require.NoError(t, c.Stream(context.Background(), ChatRequest{Model: "m"},
		func(Chunk) error { return nil }))
	effort, ok := (*captured)["reasoning_effort"].(string)
	require.True(t, ok, "reasoning_effort should be a top-level string when DisableThinking=true (got %T)", (*captured)["reasoning_effort"])
	assert.Equal(t, "none", effort, "DisableThinking must serialise as reasoning_effort=\"none\" on the OpenAI-compat surface")
}

// TestStream_DisableThinking_NotEmittedByDefault verifies the
// negative case: when the role does not set DisableThinking and
// the caller does not set it on the request, the wire body
// must NOT carry a reasoning_effort key at all. Sending
// reasoning_effort="" for users who do not want to override
// the provider default would silently turn off thinking on
// deployments that opt in to it intentionally (Qwen3/R1 with
// Ollama default to thinking ON).
func TestStream_DisableThinking_NotEmittedByDefault(t *testing.T) {
	srv, captured := fakeSSEServer(t, nil,
		`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
	)
	c := New(RoleConfig{APIURL: srv.URL, Model: "m"}, discardLog())
	require.NoError(t, c.Stream(context.Background(), ChatRequest{Model: "m"},
		func(Chunk) error { return nil }))
	_, present := (*captured)["reasoning_effort"]
	assert.False(t, present, "reasoning_effort must be absent when no override is set")
}

// TestStream_ReasoningEffort_PerRequestOverride verifies the
// caller can set a specific effort level (e.g. "low" for
// GPT-OSS which rejects "none") per request, winning over
// the role's DisableThinking flag.
func TestStream_ReasoningEffort_PerRequestOverride(t *testing.T) {
	srv, captured := fakeSSEServer(t, nil,
		`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
	)
	c := New(RoleConfig{APIURL: srv.URL, Model: "m", DisableThinking: true}, discardLog())
	require.NoError(t, c.Stream(context.Background(), ChatRequest{
		Model:           "m",
		ReasoningEffort: "low",
	}, func(Chunk) error { return nil }))
	effort, ok := (*captured)["reasoning_effort"].(string)
	require.True(t, ok, "reasoning_effort should be a top-level string (got %T)", (*captured)["reasoning_effort"])
	assert.Equal(t, "low", effort, "per-request ReasoningEffort must win over DisableThinking")
}

// TestStream_ReasoningEffort_RoleLevel covers the GPT-OSS
// case: operator sets reasoning_effort: "low" on the role
// directly (no DisableThinking flag). The wire should
// serialise "low" verbatim.
func TestStream_ReasoningEffort_RoleLevel(t *testing.T) {
	srv, captured := fakeSSEServer(t, nil,
		`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
	)
	c := New(RoleConfig{APIURL: srv.URL, Model: "gpt-oss:20b", ReasoningEffort: "low"}, discardLog())
	require.NoError(t, c.Stream(context.Background(), ChatRequest{Model: "gpt-oss:20b"},
		func(Chunk) error { return nil }))
	effort, ok := (*captured)["reasoning_effort"].(string)
	require.True(t, ok, "reasoning_effort should be a top-level string (got %T)", (*captured)["reasoning_effort"])
	assert.Equal(t, "low", effort)
}

// TestDecodeChunk_Reasoning verifies the parser picks up
// delta.reasoning — the chain-of-thought field emitted by
// Ollama Cloud models (minimax-m3, o1 family). The reasoning
// text is surfaced separately from visible content so callers
// can log it without leaking it to the player.
func TestDecodeChunk_Reasoning(t *testing.T) {
	ch, err := decodeChunk(`{"choices":[{"delta":{"content":"","reasoning":"let me think..."}}]}`)
	require.NoError(t, err)
	assert.Empty(t, ch.Content)
	assert.Equal(t, "let me think...", ch.Reasoning)
}
