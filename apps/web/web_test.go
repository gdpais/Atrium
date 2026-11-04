package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

var build = fstest.MapFS{
	"index.html":             {Data: []byte(`<!doctype html><div id="root"></div><script type="module" src="/assets/index-abc123.js"></script>`)},
	"assets/index-abc123.js": {Data: []byte(`console.log("app")`)},
	"favicon.svg":            {Data: []byte(`<svg/>`)},
}

func serve(t *testing.T, h http.Handler, method, target, accept string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const browser = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"

func TestHandlerServesTheBuild(t *testing.T) {
	h := newHandler(build)
	for _, tc := range []struct {
		name, method, target, accept string
		code                         int
		body, cache, contentType     string
	}{
		{"the app at /", "GET", "/", browser, 200, `<div id="root">`, revalidate, "text/html; charset=utf-8"},
		{"index.html by name, without a redirect", "GET", "/index.html", browser, 200, `<div id="root">`, revalidate, "text/html; charset=utf-8"},
		{"a client route on reload", "GET", "/applications/42", browser, 200, `<div id="root">`, revalidate, "text/html; charset=utf-8"},
		{"HEAD of a client route", "HEAD", "/environments", browser, 200, "", revalidate, "text/html; charset=utf-8"},
		{"a hashed asset", "GET", "/assets/index-abc123.js", "*/*", 200, `console.log("app")`, immutable, "text/javascript; charset=utf-8"},
		{"an unhashed file", "GET", "/favicon.svg", "image/*", 200, "<svg/>", revalidate, "image/svg+xml"},
		{"a missing asset", "GET", "/assets/index-old.js", "*/*", 404, "404 page not found", "", ""},
		{"a missing file a script asks for", "GET", "/config.json", "application/json", 404, "", "", ""},
		{"an unknown API path, even from a browser", "GET", "/api/v1/nothing", browser, 404, "404 page not found", "", ""},
		{"/api itself", "GET", "/api", browser, 404, "", "", ""},
		{"a path that climbs out", "GET", "/assets/../../../etc/passwd", "*/*", 404, "", "", ""},
		{"a directory", "GET", "/assets/", "*/*", 404, "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := serve(t, h, tc.method, tc.target, tc.accept)
			if w.Code != tc.code {
				t.Fatalf("code %d, want %d; body %q", w.Code, tc.code, w.Body)
			}
			if !strings.Contains(w.Body.String(), tc.body) {
				t.Errorf("body %q, want it to contain %q", w.Body, tc.body)
			}
			if got := w.Header().Get("Cache-Control"); got != tc.cache {
				t.Errorf("Cache-Control %q, want %q", got, tc.cache)
			}
			if tc.contentType != "" && w.Header().Get("Content-Type") != tc.contentType {
				t.Errorf("Content-Type %q, want %q", w.Header().Get("Content-Type"), tc.contentType)
			}
			if got := w.Header().Get("Content-Security-Policy"); got != ContentSecurityPolicy {
				t.Errorf("Content-Security-Policy %q, want %q", got, ContentSecurityPolicy)
			}
			if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options %q, want nosniff", got)
			}
		})
	}
}

// The policy forbids inline script and every other origin, the two things
// that would let injected markup run code or send data away.
func TestContentSecurityPolicyForbidsInlineScriptAndOtherOrigins(t *testing.T) {
	directives := map[string]string{}
	for _, d := range strings.Split(ContentSecurityPolicy, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(d), " ")
		directives[name] = value
	}
	for name, want := range map[string]string{
		"default-src": "'self'", "script-src": "'self'", "connect-src": "'self'",
		"object-src": "'none'", "frame-ancestors": "'none'", "base-uri": "'self'",
	} {
		if directives[name] != want {
			t.Errorf("%s is %q, want %q", name, directives[name], want)
		}
	}
	if strings.Contains(directives["script-src"], "unsafe") {
		t.Errorf("script-src allows %q", directives["script-src"])
	}
}

func TestHandlerWithoutABuildServesThePlaceholder(t *testing.T) {
	h := newHandler(fstest.MapFS{})
	w := serve(t, h, "GET", "/anything", browser)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "built without its Web UI") {
		t.Fatalf("code %d, body %q; want the placeholder", w.Code, w.Body)
	}
	if w := serve(t, h, "GET", "/assets/index.js", "*/*"); w.Code != 404 {
		t.Fatalf("an asset without a build: code %d, want 404", w.Code)
	}
}

// The embedded UI is whatever is in dist: a build after npm run build, the
// placeholder otherwise. Either way it serves a page at /.
func TestEmbeddedUIServesAPage(t *testing.T) {
	w := serve(t, Handler(), "GET", "/", browser)
	if w.Code != 200 {
		t.Fatalf("code %d, want 200", w.Code)
	}
	want := "built without its Web UI"
	if Built() {
		want = `<div id="root">`
	}
	if !strings.Contains(w.Body.String(), want) {
		t.Fatalf("body %q, want it to contain %q (Built() = %v)", w.Body, want, Built())
	}
}
