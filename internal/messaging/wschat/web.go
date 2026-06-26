package wschat

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
	"sync"
)

// distFS embeds the built frontend. We embed web/dist/index.html
// and web/dist/assets/* explicitly so the Go build fails cleanly
// when the operator has not run `npm run build` — the embed
// directive requires at least one matching file. When the dist
// folder is empty the build fails with a clear "no matching files"
// error, which is the right signal: run `make web` first.
//
// We use two patterns so adding new asset types (fonts, images)
// inside dist/assets does not require editing this file.
//
//go:embed web/dist/index.html
//go:embed web/dist/assets
var distFS embed.FS

// distRoot returns the dist subtree rooted at web/dist. Built
// lazily on first access so the package init stays side-effect free.
//
//nolint:gochecknoglobals // memoised lazy loader for the embedded FS
var distRoot = sync.OnceValues(func() (fs.FS, error) {
	return fs.Sub(distFS, "web/dist")
})

// serveStatic serves the embedded React build. Any unmatched path
// falls back to index.html so client-side routing works.
func serveStatic(w http.ResponseWriter, r *http.Request) {
	root, err := distRoot()
	if err != nil {
		http.Error(w, "static assets not embedded", http.StatusInternalServerError)

		return
	}
	// Clean path and default to index.html.
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		p = "index.html"
	}
	// Try the exact file first.
	if _, err := fs.Stat(root, p); err == nil {
		http.FileServer(http.FS(root)).ServeHTTP(w, r)

		return
	}
	// SPA fallback: serve index.html for any non-asset path.
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/"
	http.FileServer(http.FS(root)).ServeHTTP(w, r2)
}
