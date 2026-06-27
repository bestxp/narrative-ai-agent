package wschat

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/structured"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

// newTestWS creates a real *websocket.Conn backed by an httptest
// server. The caller is responsible for closing it.
func newTestWS(t *testing.T) *websocket.Conn {
	t.Helper()

	upgrader := websocket.Upgrader{
		CheckOrigin: func(_ *http.Request) bool { return true },
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		// Hold the server-side connection open until the test closes it.
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				conn.Close()

				return
			}
		}
	}))
	t.Cleanup(server.Close)

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	client, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial test server: %v", err)
	}

	if resp != nil {
		_ = resp.Body.Close()
	}

	return client
}

func TestWSSession_CurrentTextRendersHeadersDuringStream(t *testing.T) {
	t.Parallel()

	client := newTestWS(t)
	defer client.Close()

	s := newSession(client, "chat-1", zerolog.New(nil))

	s.accumulateDelta("------ NARRATIVE ------\n\nДень 24")

	got := s.currentText()
	want := structured.HeaderDialogue + "\nДень 24\n"

	if got != want {
		t.Fatalf("currentText during stream = %q, want %q", got, want)
	}

	s.accumulateDelta(", лесная дорога. - Диалог.")

	got = s.currentText()
	want = structured.HeaderDialogue + "\nДень 24, лесная дорога. - Диалог.\n"

	if got != want {
		t.Fatalf("currentText after second delta = %q, want %q", got, want)
	}
}

func TestWSSession_FinalTextRendersAllSections(t *testing.T) {
	t.Parallel()

	client := newTestWS(t)
	defer client.Close()

	s := newSession(client, "chat-1", zerolog.New(nil))

	s.accumulateDelta("------ NARRATIVE ------\n\nКонец")

	got := s.finalText()

	if !strings.Contains(got, structured.HeaderDialogue) {
		t.Fatalf("finalText missing dialogue header, got %q", got)
	}

	if !strings.Contains(got, structured.HeaderContext) {
		t.Fatalf("finalText missing context header, got %q", got)
	}

	if !strings.Contains(got, structured.HeaderFuture) {
		t.Fatalf("finalText missing future header, got %q", got)
	}

	if !strings.Contains(got, structured.HeaderValidation) {
		t.Fatalf("finalText missing validation header, got %q", got)
	}
}
