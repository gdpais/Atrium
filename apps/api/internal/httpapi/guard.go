package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/auth"
)

// SessionCookie is the session cookie's name. The __Host- prefix makes a
// browser keep it only when it is Secure, set over HTTPS (or localhost),
// with Path=/ and no Domain, so no other host can set or read it.
const SessionCookie = "__Host-heartbeat_session"

// requestIDHeader carries the ID audit records and logs give a request.
const requestIDHeader = "X-Request-Id"

// maxBodyBytes bounds a request body: the operations take small JSON
// objects.
const maxBodyBytes = 1 << 20

// request is what the guard learnt about a request, for its handler.
type request struct {
	client auth.Client
	// principal is who sent it; nil for an operation that needs no session.
	principal *auth.Principal
}

type requestKey struct{}

// requestFrom returns what the guard put in ctx.
func requestFrom(ctx context.Context) *request {
	r, _ := ctx.Value(requestKey{}).(*request)
	if r == nil {
		return &request{}
	}
	return r
}

// guard runs before every operation, in this order: it gives the request
// an ID; refuses an unsafe method whose Origin is not the site; then, unless
// the operation is public, refuses a request without a live session, or
// whose user lacks the operation's role. Each refusal is a problem, and the
// operation never runs. An operation without a rule is refused too, though
// NewServer fails before one can be served.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := audit.NewID()
		if err != nil {
			s.logger.ErrorContext(r.Context(), "request ID", "error", err)
			writeProblem(w, http.StatusInternalServerError, "")
			return
		}
		w.Header().Set(requestIDHeader, id.String())
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		info := &request{client: auth.Client{Address: clientAddress(r, s.trustedProxies), RequestID: id.String()}}

		if unsafe(r.Method) && !s.fromSite(r) {
			writeProblem(w, http.StatusForbidden, s.originProblem())
			return
		}
		rule, ok := s.rules[r.Pattern]
		if !ok {
			s.logger.ErrorContext(r.Context(), "no access rule for the operation; refused", "pattern", r.Pattern)
			writeProblem(w, http.StatusForbidden, "")
			return
		}
		if !rule.public {
			principal, status, detail := s.authenticate(r)
			if status != 0 {
				if status == http.StatusUnauthorized {
					http.SetCookie(w, clearCookie())
				}
				writeProblem(w, status, detail)
				return
			}
			if rule.role != "" && !auth.Allows(principal.User.Roles, rule.role) {
				writeProblem(w, http.StatusForbidden, fmt.Sprintf("this operation needs the %s role or a higher one", rule.role))
				return
			}
			info.principal = &principal
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestKey{}, info)))
	})
}

// authenticate returns who the session cookie belongs to, or the status
// and detail of the problem to answer with.
func (s *Server) authenticate(r *http.Request) (auth.Principal, int, string) {
	cookie, err := r.Cookie(SessionCookie)
	if err != nil || cookie.Value == "" {
		return auth.Principal{}, http.StatusUnauthorized, "log in first: the request has no session cookie"
	}
	if s.auth == nil {
		return auth.Principal{}, http.StatusServiceUnavailable, ""
	}
	principal, err := s.auth.Authenticate(r.Context(), cookie.Value)
	if errors.Is(err, auth.ErrNoSession) {
		return auth.Principal{}, http.StatusUnauthorized, "log in again: the session has ended"
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "authenticate the request", "error", err)
		return auth.Principal{}, http.StatusInternalServerError, ""
	}
	return principal, 0, ""
}

// unsafe reports a method that changes state (RFC 9110 §9.2.1).
func unsafe(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return false
	}
	return true
}

// fromSite reports whether r carries one Origin header, equal to the site.
// Browsers send it with every unsafe request, same-origin included, so a
// page on another site cannot make one with the user's cookie.
func (s *Server) fromSite(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	return s.site != "" && len(origins) == 1 && origins[0] == s.site
}

func (s *Server) originProblem() string {
	if s.site == "" {
		return "the API refuses unsafe methods until its site is configured (HEARTBEAT_API_SITE_ORIGIN)"
	}
	return "the Origin header must be " + s.site
}
