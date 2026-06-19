package wschat

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
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

// distRoot is the dist subtree rooted at web/dist, set up in init.
var distRoot fs.FS

func init() {
	sub, err := fs.Sub(distFS, "web/dist")
	if err != nil {
		panic("wschat: embed web/dist missing: " + err.Error())
	}
	distRoot = sub
}

// serveStatic serves the embedded React build. Any unmatched path
// falls back to index.html so client-side routing works.
func serveStatic(w http.ResponseWriter, r *http.Request) {
	// Clean path and default to index.html.
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		p = "index.html"
	}
	// Try the exact file first.
	if _, err := fs.Stat(distRoot, p); err == nil {
		http.FileServer(http.FS(distRoot)).ServeHTTP(w, r)
		return
	}
	// SPA fallback: serve index.html for any non-asset path.
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/"
	http.FileServer(http.FS(distRoot)).ServeHTTP(w, r2)
}
