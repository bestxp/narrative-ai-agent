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
