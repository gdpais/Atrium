package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strings"
	"testing"
	"time"

	"heartbeat/apps/api/internal/auth"
)

const site = "https://heartbeat.example.com"

// fakeAuth is an Authenticator with one known session, under token.
type fakeAuth struct {
	principal auth.Principal
	token     string
	// loginErr and authErr, when set, are what Login and Authenticate
	// return.
	loginErr, authErr, logoutErr error

	logins  []loginCall
	logouts []logoutCall
}

type loginCall struct {
	email, secret string
	client        auth.Client
}

type logoutCall struct {
	principal auth.Principal
	client    auth.Client
}

func newFakeAuth(roles ...string) *fakeAuth {
	return &fakeAuth{
		token: "dG9rZW4tb2YtdGhlLXdhbGxib2FyZC1zZXNzaW9uLTAx",
		principal: auth.Principal{
			Session: auth.Session{
				ID: "6f1c1c55-2f0a-4b8e-9d6b-7f4c2b1a9e01", UserID: "0b4e7a52-8f43-4d0e-a1b6-3c9d2e5f7a10",
				Address: "198.51.100.7", Created: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC),
			},
			User: auth.User{
				ID: "0b4e7a52-8f43-4d0e-a1b6-3c9d2e5f7a10", Email: "wall@example.com", DisplayName: "Wallboard", Roles: roles,
			},
		},
	}
}

func (f *fakeAuth) Login(_ context.Context, email, secret string, client auth.Client) (auth.Principal, string, error) {
	f.logins = append(f.logins, loginCall{email, secret, client})
	if f.loginErr != nil {
		return auth.Principal{}, "", f.loginErr
	}
	return f.principal, f.token, nil
}

func (f *fakeAuth) Authenticate(_ context.Context, token string) (auth.Principal, error) {
	if f.authErr != nil {
		return auth.Principal{}, f.authErr
	}
	if token != f.token {
		return auth.Principal{}, auth.ErrNoSession
	}
	return f.principal, nil
}

func (f *fakeAuth) Logout(_ context.Context, p auth.Principal, client auth.Client) error {
	f.logouts = append(f.logouts, logoutCall{p, client})
	return f.logoutErr
}

func (f *fakeAuth) ExpiresAt(s auth.Session) time.Time { return time.Now().Add(7 * 24 * time.Hour) }

// serveRequest sends req to a mux with the server's operations registered.
func serveRequest(server *Server, req *http.Request) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	server.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// browserRequest builds a request from a browser on the site, with the session
// cookie when token is set.
func browserRequest(method, path, body, token string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "198.51.100.7:51000"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("Origin", site)
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	}
	return req
}

const credentials = `{"email":"wall@example.com","password":"correct horse battery staple"}`

// problemOf decodes a problem response, and fails unless it is one with
// status.
func problemOf(t *testing.T, rec *httptest.ResponseRecorder, status int) (detail string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("Content-Type %q, want application/problem+json", got)
	}
	var p struct {
		Type, Title, Detail string
		Status              int
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("%s: %v", rec.Body, err)
	}
	if p.Type != "about:blank" || p.Status != status || p.Title != http.StatusText(status) {
		t.Fatalf("problem %+v", p)
	}
	return p.Detail
}

// cleared reports whether rec tells the browser to drop the session cookie.
func cleared(t *testing.T, rec *httptest.ResponseRecorder) bool {
	t.Helper()
	for _, header := range rec.Header().Values("Set-Cookie") {
		c, err := http.ParseSetCookie(header)
		if err != nil {
			t.Fatal(err)
		}
		if c.Name == SessionCookie && c.Value == "" && c.MaxAge < 0 && c.Path == "/" && c.Secure {
			return true
		}
	}
	return false
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// Every response carries a new request ID, the one audit records get.
func TestGuardGivesEachRequestAnID(t *testing.T) {
	fake := newFakeAuth("viewer")
	server := newServer(t, Options{Auth: fake, Site: site})
	seen := map[string]bool{}
	for _, req := range []*http.Request{
		browserRequest(http.MethodGet, "/healthz", "", ""),
		browserRequest(http.MethodGet, "/api/v1/session", "", ""),
		browserRequest(http.MethodPost, "/api/v1/session", credentials, ""),
	} {
		id := serveRequest(server, req).Header().Get(requestIDHeader)
		if !uuidPattern.MatchString(id) || seen[id] {
			t.Fatalf("%s %s: request ID %q, want a new UUID", req.Method, req.URL.Path, id)
		}
		seen[id] = true
	}
	if got, want := fake.logins[0].client.RequestID, ""; got == want {
		t.Fatal("the login got no request ID")
	}
}

// An unsafe method must come from the site, by one Origin header equal to
// it, or the operation never runs, logging in included.
func TestGuardChecksTheOriginOfUnsafeMethods(t *testing.T) {
	for name, origins := range map[string][]string{
		"none":             nil,
		"another site":     {"https://evil.example"},
		"null":             {"null"},
		"a trailing slash": {site + "/"},
		"http":             {"http://heartbeat.example.com"},
		"two":              {site, site},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeAuth("viewer")
			server := newServer(t, Options{Auth: fake, Site: site})
			for _, req := range []*http.Request{
				browserRequest(http.MethodPost, "/api/v1/session", credentials, ""),
				browserRequest(http.MethodDelete, "/api/v1/session", "", fake.token),
			} {
				req.Header.Del("Origin")
				for _, origin := range origins {
					req.Header.Add("Origin", origin)
				}
				rec := serveRequest(server, req)
				if detail := problemOf(t, rec, http.StatusForbidden); detail != "the Origin header must be "+site {
					t.Fatalf("%s: detail %q", req.Method, detail)
				}
			}
			if len(fake.logins) != 0 || len(fake.logouts) != 0 {
				t.Fatalf("%d logins, %d logouts ran", len(fake.logins), len(fake.logouts))
			}
		})
	}

	t.Run("safe methods need none", func(t *testing.T) {
		fake := newFakeAuth("viewer")
		req := browserRequest(http.MethodGet, "/api/v1/session", "", fake.token)
		req.Header.Set("Origin", "https://evil.example")
		if rec := serveRequest(newServer(t, Options{Auth: fake, Site: site}), req); rec.Code != http.StatusOK {
			t.Fatalf("GET = %d, want 200", rec.Code)
		}
	})
	t.Run("no site configured", func(t *testing.T) {
		fake := newFakeAuth("viewer")
		rec := serveRequest(newServer(t, Options{Auth: fake}), browserRequest(http.MethodPost, "/api/v1/session", credentials, ""))
		if detail := problemOf(t, rec, http.StatusForbidden); !strings.Contains(detail, "HEARTBEAT_API_SITE_ORIGIN") {
			t.Fatalf("detail %q, want it to name the setting", detail)
		}
		if len(fake.logins) != 0 {
			t.Fatal("a login ran without a site")
		}
	})
}

// An operation that needs a session refuses a request without a live one,
// and tells the browser to drop a dead cookie.
func TestGuardNeedsALiveSession(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cookie  string
		authErr error
		noAuth  bool
		status  int
		clear   bool
	}{
		{"no cookie", "", nil, false, http.StatusUnauthorized, true},
		{"an ended session", "ended", nil, false, http.StatusUnauthorized, true},
		{"Redis or PostgreSQL failing", "dG9rZW4tb2YtdGhlLXdhbGxib2FyZC1zZXNzaW9uLTAx", errors.New("dial tcp: connection refused"), false, http.StatusInternalServerError, false},
		{"no auth service", "dG9rZW4tb2YtdGhlLXdhbGxib2FyZC1zZXNzaW9uLTAx", nil, true, http.StatusServiceUnavailable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeAuth("viewer")
			fake.authErr = tc.authErr
			opts := Options{Auth: fake, Site: site}
			if tc.noAuth {
				opts.Auth = nil
			}
			server := newServer(t, opts)
			for _, method := range []string{http.MethodGet, http.MethodDelete} {
				rec := serveRequest(server, browserRequest(method, "/api/v1/session", "", tc.cookie))
				problemOf(t, rec, tc.status)
				if got := cleared(t, rec); got != tc.clear {
					t.Fatalf("%s: cookie cleared %t, want %t", method, got, tc.clear)
				}
				if strings.Contains(rec.Body.String(), "connection refused") {
					t.Fatalf("%s: the problem shows the internal error: %s", method, rec.Body)
				}
			}
			if len(fake.logouts) != 0 {
				t.Fatal("a logout ran without a session")
			}
		})
	}
}

// A user needs the operation's role, or a higher one.
func TestGuardChecksTheRole(t *testing.T) {
	for _, tc := range []struct {
		roles  []string
		need   auth.Role
		status int
	}{
		{[]string{"viewer"}, auth.Viewer, http.StatusOK},
		{[]string{"admin"}, auth.Viewer, http.StatusOK},
		{[]string{"viewer"}, auth.Operator, http.StatusForbidden},
		{[]string{"operator"}, auth.Admin, http.StatusForbidden},
		{[]string{"owner"}, auth.Viewer, http.StatusForbidden},
		{nil, auth.Viewer, http.StatusForbidden},
		{nil, "", http.StatusOK},
	} {
		fake := newFakeAuth(tc.roles...)
		server := newServer(t, Options{Auth: fake, Site: site})
		server.rules["GET /api/v1/session"] = rule{role: tc.need}
		rec := serveRequest(server, browserRequest(http.MethodGet, "/api/v1/session", "", fake.token))
		if rec.Code != tc.status {
			t.Fatalf("roles %v, operation needs %q: %d, want %d", tc.roles, tc.need, rec.Code, tc.status)
		}
		if tc.status == http.StatusForbidden {
			if detail := problemOf(t, rec, http.StatusForbidden); !strings.Contains(detail, string(tc.need)+" role") {
				t.Fatalf("detail %q, want it to name the role", detail)
			}
		}
	}
}

// A request body over 1 MiB is refused before it is read whole.
func TestGuardBoundsTheBody(t *testing.T) {
	fake := newFakeAuth("viewer")
	server := newServer(t, Options{Auth: fake, Site: site})
	body := `{"email":"wall@example.com","password":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	problemOf(t, serveRequest(server, browserRequest(http.MethodPost, "/api/v1/session", body, "")), http.StatusBadRequest)
	if len(fake.logins) != 0 {
		t.Fatal("the login ran with an oversized body")
	}
}

// A login answers 201 with the session and its cookie: __Host- prefixed,
// HttpOnly, Secure, SameSite=Lax, Path=/, no Domain, kept until the
// session's absolute end.
func TestCreateSession(t *testing.T) {
	fake := newFakeAuth("viewer")
	server := newServer(t, Options{Auth: fake, Site: site, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}})
	req := browserRequest(http.MethodPost, "/api/v1/session", credentials, "")
	req.RemoteAddr = "10.0.0.2:4000"
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	rec := serveRequest(server, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201: %s", rec.Code, rec.Body)
	}
	if len(fake.logins) != 1 {
		t.Fatalf("%d logins, want 1", len(fake.logins))
	}
	call := fake.logins[0]
	if call.email != "wall@example.com" || call.secret != "correct horse battery staple" ||
		call.client.Address != netip.MustParseAddr("198.51.100.7") || call.client.RequestID != rec.Header().Get(requestIDHeader) {
		t.Fatalf("Login(%q, %q, %+v)", call.email, call.secret, call.client)
	}

	cookies := rec.Header().Values("Set-Cookie")
	if len(cookies) != 1 {
		t.Fatalf("Set-Cookie %v, want one", cookies)
	}
	c, err := http.ParseSetCookie(cookies[0])
	if err != nil {
		t.Fatal(err)
	}
	maxAge := 7 * 24 * time.Hour / time.Second
	if c.Name != "__Host-heartbeat_session" || c.Value != fake.token || c.Path != "/" || c.Domain != "" ||
		!c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.MaxAge < int(maxAge)-60 || c.MaxAge > int(maxAge) {
		t.Fatalf("cookie %s", cookies[0])
	}

	var body struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"created_at"`
		ExpiresAt time.Time `json:"expires_at"`
		User      struct {
			ID, Email, DisplayName string
			Roles                  []string
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	p := fake.principal
	if body.ID != p.Session.ID || !body.CreatedAt.Equal(p.Session.Created) || body.ExpiresAt.IsZero() ||
		body.User.ID != p.User.ID || body.User.Email != p.User.Email || len(body.User.Roles) != 1 || body.User.Roles[0] != "viewer" {
		t.Fatalf("body %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), fake.token) {
		t.Fatal("the body shows the token")
	}
}

// Each refused login is a problem with its own status; none sets a cookie.
func TestCreateSessionRefusals(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		body       string
		status     int
		detail     string
		retryAfter string
	}{
		{"wrong credentials", auth.ErrInvalidCredentials, credentials, http.StatusUnauthorized, "wrong email or password", ""},
		{"unreadable request", &auth.InvalidError{}, credentials, http.StatusBadRequest, "", ""},
		{"rate limited", &auth.RateLimitedError{RetryAfter: 61200 * time.Millisecond}, credentials, http.StatusTooManyRequests, "try again later", "62"},
		{"rate limited, under a second left", &auth.RateLimitedError{RetryAfter: 300 * time.Millisecond}, credentials, http.StatusTooManyRequests, "try again later", "1"},
		{"failing dependency", errors.New("dial tcp 10.0.0.9:5432: connection refused"), credentials, http.StatusInternalServerError, requestIDHeader, ""},
		{"not JSON", nil, "email=wall@example.com", http.StatusBadRequest, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeAuth("viewer")
			fake.loginErr = tc.err
			rec := serveRequest(newServer(t, Options{Auth: fake, Site: site}), browserRequest(http.MethodPost, "/api/v1/session", tc.body, ""))
			if detail := problemOf(t, rec, tc.status); !strings.Contains(detail, tc.detail) {
				t.Fatalf("detail %q, want %q in it", detail, tc.detail)
			}
			if got := rec.Header().Get("Retry-After"); got != tc.retryAfter {
				t.Fatalf("Retry-After %q, want %q", got, tc.retryAfter)
			}
			if cookies := rec.Header().Values("Set-Cookie"); len(cookies) != 0 {
				t.Fatalf("Set-Cookie %v on a refused login", cookies)
			}
			if strings.Contains(rec.Body.String(), "10.0.0.9") {
				t.Fatalf("the problem shows the internal error: %s", rec.Body)
			}
		})
	}
}

func TestGetSession(t *testing.T) {
	fake := newFakeAuth("operator")
	rec := serveRequest(newServer(t, Options{Auth: fake, Site: site}), browserRequest(http.MethodGet, "/api/v1/session", "", fake.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		ID   string `json:"id"`
		User struct {
			Email string   `json:"email"`
			Roles []string `json:"roles"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ID != fake.principal.Session.ID || body.User.Email != "wall@example.com" || len(body.User.Roles) != 1 || body.User.Roles[0] != "operator" {
		t.Fatalf("body %s", rec.Body)
	}
}

// Logging out ends the session the cookie names and clears the cookie.
func TestDeleteSession(t *testing.T) {
	fake := newFakeAuth("viewer")
	server := newServer(t, Options{Auth: fake, Site: site})
	rec := serveRequest(server, browserRequest(http.MethodDelete, "/api/v1/session", "", fake.token))
	if rec.Code != http.StatusNoContent || !cleared(t, rec) {
		t.Fatalf("status %d, Set-Cookie %v; want 204 and the cookie cleared", rec.Code, rec.Header().Values("Set-Cookie"))
	}
	if len(fake.logouts) != 1 || fake.logouts[0].principal.Session.ID != fake.principal.Session.ID ||
		fake.logouts[0].client.RequestID != rec.Header().Get(requestIDHeader) {
		t.Fatalf("logouts %+v", fake.logouts)
	}

	fake.logoutErr = errors.New("redis: connection pool timeout")
	rec = serveRequest(server, browserRequest(http.MethodDelete, "/api/v1/session", "", fake.token))
	problemOf(t, rec, http.StatusInternalServerError)
	if cleared(t, rec) {
		t.Fatal("a failed logout cleared the cookie: the session would live on unseen")
	}
}
