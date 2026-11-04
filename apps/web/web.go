// Package web serves Heartbeat's Web UI, the single-page app in this
// directory, from the API's origin (ADR 0020).
//
// npm run build writes the app to dist/app, which the API image builds in a
// Node stage and this package embeds. Without a build (go test, go build on
// a machine without Node) dist holds only .keep, and the UI is a placeholder
// page that says how to build it.
package web

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// ContentSecurityPolicy is sent with every UI response. Scripts come from
// the origin only, never inline; styles may be inline because Mantine sets
// CSS variables at run time. The UI calls only its own origin, and no other
// site may frame it.
const ContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; font-src 'self' data:; connect-src 'self'; object-src 'none'; " +
	"base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

const (
	// Vite names every file under assets/ by its content, so a new build
	// never reuses a name and browsers may keep them for good.
	immutable = "public, max-age=31536000, immutable"
	// index.html and the other unhashed files are revalidated on every
	// load, so a deploy takes effect at the next one.
	revalidate = "no-cache"
)

// Handler serves the UI for GET and HEAD requests:
//
//   - a file of the build, by its path: assets/ cached as immutable, the
//     rest revalidated;
//   - index.html for any other path a browser navigates to (its Accept
//     names text/html), so the app's own routes survive a reload;
//   - 404 for anything else, and always under /api, so a mistyped API path
//     answers 404 instead of a page.
//
// Register it as "GET /": the API's own patterns are more specific and win.
func Handler() http.Handler {
	app, err := fs.Sub(dist, "dist/app")
	if err != nil {
		panic(err) // a constant, valid path: fs.Sub cannot fail on it
	}
	return newHandler(app)
}

// newHandler serves the build in app, or the placeholder when app has no
// index.html.
func newHandler(app fs.FS) *handler {
	index, err := fs.ReadFile(app, "index.html")
	if err != nil {
		return &handler{index: placeholder}
	}
	return &handler{app: app, index: index}
}

// Built reports whether the embedded UI is a build of the app rather than
// the placeholder.
func Built() bool {
	_, err := fs.Stat(dist, "dist/app/index.html")
	return err == nil
}

type handler struct {
	app   fs.FS // nil without a build
	index []byte
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	header.Set("Content-Security-Policy", ContentSecurityPolicy)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "same-origin")
	header.Set("Cross-Origin-Opener-Policy", "same-origin")

	p := r.URL.Path
	if p == "/api" || strings.HasPrefix(p, "/api/") {
		http.NotFound(w, r)
		return
	}
	if name := strings.TrimPrefix(path.Clean(p), "/"); h.app != nil && name != "" && name != "." && name != "index.html" {
		if info, err := fs.Stat(h.app, name); err == nil && !info.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				header.Set("Cache-Control", immutable)
			} else {
				header.Set("Cache-Control", revalidate)
			}
			http.ServeFileFS(w, r, h.app, name)
			return
		}
	}
	if !acceptsHTML(r) {
		http.NotFound(w, r)
		return
	}
	header.Set("Cache-Control", revalidate)
	header.Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(h.index)
}

// acceptsHTML reports whether r is a browser's navigation: its Accept header
// names text/html. Scripts and images ask for other types, so a missing file
// answers 404 instead of a page their parser would choke on.
func acceptsHTML(r *http.Request) bool {
	for _, value := range r.Header.Values("Accept") {
		for _, part := range strings.Split(value, ",") {
			media, _, _ := strings.Cut(part, ";")
			if strings.EqualFold(strings.TrimSpace(media), "text/html") {
				return true
			}
		}
	}
	return false
}

var placeholder = bytes.TrimSpace([]byte(`
<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Heartbeat</title></head>
<body>
<h1>Heartbeat</h1>
<p>This API was built without its Web UI. Build it with
<code>npm --prefix apps/web ci &amp;&amp; npm --prefix apps/web run build</code>
and rebuild the API, or use the API image, which includes it.</p>
</body>
</html>
`))
