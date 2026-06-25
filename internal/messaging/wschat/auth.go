package wschat

import (
	"net/http"
	"slices"
	"strings"
)

// AuthConfig is the token allow list used by the auth middleware.
// A request is accepted when its bearer token matches DevToken or
// appears in AllowedTokens. The wschat client builds this from
// config.WSChatConfig at startup.
type AuthConfig struct {
	DevToken      string
	AllowedTokens []string
}

// IsAccepted reports whether the given bearer token is on the allow
// list. Empty tokens are always rejected.
func (a AuthConfig) IsAccepted(token string) bool {
	if token == "" {
		return false
	}
	if a.DevToken != "" && token == a.DevToken {
		return true
	}

	return slices.Contains(a.AllowedTokens, token)
}

// bearerFromRequest extracts the bearer token from either the
// Authorization header (Bearer <token>) or the ?token=<token> query
// parameter. The query fallback is what the WebSocket upgrade uses
// because browsers cannot set headers on the WS handshake; the
// header path is what the HTTP API calls use.
func bearerFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if token, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(token)
		}

		return strings.TrimSpace(h)
	}
	if t := r.URL.Query().Get("token"); t != "" {
		return t
	}

	return ""
}

// requireAuth is the HTTP middleware that rejects requests whose
// bearer token is not on the allow list. On rejection it writes a
// 401 with a small JSON body and returns false so the caller can
// short-circuit. The middleware is intentionally simple — no
// rate limiting, no IP allow list — because the dev server is
// expected to bind to 127.0.0.1.
func requireAuth(w http.ResponseWriter, r *http.Request, auth AuthConfig) bool {
	token := bearerFromRequest(r)
	if !auth.IsAccepted(token) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("WWW-Authenticate", "Bearer")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))

		return false
	}

	return true
}
