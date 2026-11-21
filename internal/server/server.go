// Package server serves the web UI that make build/web writes into public/ and
// the binary embeds. A path that names a file is served as that
// file; any other path without an extension gets index.html, so the app's
// client-side router reads the URL. Without a web build it serves a page that
// says to run make build/web.
//
// It serves static files only. It does not authenticate (the UI is public and
// asks for the API token itself) and does not serve the API; internal/api
// mounts this handler for every path it does not route.
package server

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed public
var public embed.FS

// Public is the embedded web build: index.html and assets/ after make
// build/web, only stub.txt before.
func Public() fs.FS {
	sub, err := fs.Sub(public, "public")
	if err != nil {
		panic(err) // "public" is a valid path, so fs.Sub cannot fail.
	}
	return sub
}

const placeholder = `<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>factory</title>
<h1>factory</h1>
<p>This binary was built without the web UI. Run <code>make build/web</code>, then <code>make build</code>.</p>
<p>The API is under <code>/api/v1</code>.</p>
</html>
`

// UI serves the web build in fsys.
func UI(fsys fs.FS) http.Handler {
	files := http.FileServerFS(fsys)
	_, err := fs.Stat(fsys, "index.html")
	built := err == nil
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !built {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write([]byte(placeholder))
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if info, err := fs.Stat(fsys, name); err == nil && !info.IsDir() && name != "index.html" {
			if strings.HasPrefix(name, "assets/") {
				// Vite puts a content hash in every asset's name.
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}
		if path.Ext(name) != "" {
			// A missing script or image must not get the app's HTML.
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		index, err := fs.ReadFile(fsys, "index.html")
		if err != nil {
			http.Error(w, "read index.html: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Method == http.MethodGet {
			_, _ = w.Write(index)
		}
	})
}
