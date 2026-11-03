// Package httpapi implements the operations of the API's contract,
// apps/api/openapi.yaml.
//
// Server implements the strict server interface that oapi-codegen generates
// into package openapi; Register serves it on a ServeMux. Handlers get typed
// requests and return typed responses, so a response the spec does not
// declare does not compile. Before any handler runs, a guard checks the
// request's Origin, session and role against the rule its operation
// declares in the spec.
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"sync/atomic"
	"time"

	api "heartbeat/apps/api"
	"heartbeat/apps/api/internal/auth"
	"heartbeat/apps/api/internal/openapi"
)

// Server implements [openapi.StrictServerInterface].
type Server struct {
	readiness *readiness
	// draining makes GET /readyz answer 503 once shutdown has begun.
	draining atomic.Bool
	logger   *slog.Logger
	// rules are the operations' access rules, by ServeMux pattern.
	rules          map[string]rule
	auth           Authenticator
	inventory      Inventory
	site           string
	trustedProxies []netip.Prefix
	// probeDefinitions serves /api/v1/probe-definitions.
	probeDefinitions ProbeDefinitions
	// databaseTargets serves /api/v1/database-targets.
	databaseTargets DatabaseTargets
}

// The generated interface lists every operation of the spec, so an
// operation added to openapi.yaml without a handler fails to compile here.
var _ openapi.StrictServerInterface = (*Server)(nil)

// Authenticator logs users in and out and authenticates their sessions:
// *auth.Service.
type Authenticator interface {
	Login(ctx context.Context, email, secret string, client auth.Client) (auth.Principal, string, error)
	Authenticate(ctx context.Context, token string) (auth.Principal, error)
	Logout(ctx context.Context, p auth.Principal, client auth.Client) error
	// ExpiresAt is when session ends however active it is.
	ExpiresAt(session auth.Session) time.Time
}

var _ Authenticator = (*auth.Service)(nil)

// Options configure a Server.
type Options struct {
	// Logger receives logs; nil discards them.
	Logger *slog.Logger
	// Checks are what GET /readyz runs, all of them bounded together by
	// ReadinessTimeout.
	Checks           []Check
	ReadinessTimeout time.Duration
	// Auth logs users in and authenticates sessions. Without it, every
	// operation that needs a session answers 503, and logging in fails.
	Auth Authenticator
	// Site is the origin unsafe methods must come from, such as
	// https://heartbeat.example.com, as a browser writes it in the Origin
	// header. Empty refuses every unsafe method.
	Site string
	// TrustedProxies are the peers whose X-Forwarded-For names the client.
	TrustedProxies []netip.Prefix
	// Inventory serves the environment, application and component
	// operations. Without it, they fail with 500.
	Inventory Inventory
	// ProbeDefinitions serves the probe definition operations; without it
	// they fail.
	ProbeDefinitions ProbeDefinitions
	// DatabaseTargets serves the database target operations; without it
	// they fail.
	DatabaseTargets DatabaseTargets
}

// NewServer returns a Server. It fails when an operation of the spec does
// not declare who may call it.
func NewServer(opts Options) (*Server, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	rules, err := accessRules(api.Spec)
	if err != nil {
		return nil, err
	}
	return &Server{
		readiness:        &readiness{checks: opts.Checks, timeout: opts.ReadinessTimeout, logger: logger},
		logger:           logger,
		rules:            rules,
		auth:             opts.Auth,
		inventory:        opts.Inventory,
		probeDefinitions: opts.ProbeDefinitions,
		databaseTargets:  opts.DatabaseTargets,
		site:             opts.Site,
		trustedProxies:   opts.TrustedProxies,
	}, nil
}

// Register serves every operation of the spec on mux, under the method and
// path patterns the spec declares (GET /healthz, POST /api/v1/session, ...),
// each behind the guard.
//
// The guard wraps what the generated code registers, so it runs before
// the path and query parameters are bound: a request without a session is
// answered 401, never 400 for a malformed ID, and the guard's refusals
// read no body.
func (s *Server) Register(mux *http.ServeMux) {
	strict := openapi.NewStrictHandlerWithOptions(s, []openapi.StrictMiddlewareFunc{s.closedBodies}, openapi.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeProblem(w, http.StatusBadRequest, err.Error())
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			var invalid *requestError
			if errors.As(err, &invalid) {
				writeProblem(w, http.StatusBadRequest, invalid.Error())
				return
			}
			s.logger.ErrorContext(r.Context(), "request failed", "pattern", r.Pattern,
				"request_id", requestFrom(r.Context()).client.RequestID, "error", err)
			writeProblem(w, http.StatusInternalServerError, fmt.Sprintf("the API could not serve the request; its ID is in the %s header", requestIDHeader))
		},
	})
	openapi.HandlerWithOptions(strict, openapi.StdHTTPServerOptions{
		BaseRouter: guardedMux{ServeMux: mux, guard: func(h http.Handler) http.Handler { return s.guard(s.keepBody(h)) }},
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeProblem(w, http.StatusBadRequest, err.Error())
		},
	})
}

// guardedMux registers each handler behind guard. The ServeMux sets
// r.Pattern before it calls the handler, so the guard finds the
// operation's rule.
type guardedMux struct {
	*http.ServeMux
	guard func(http.Handler) http.Handler
}

func (m guardedMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.ServeMux.Handle(pattern, m.guard(http.HandlerFunc(handler)))
}

// Drain makes GET /readyz answer 503 from now on, so that the API leaves
// its Service while it still serves the requests already sent to it.
func (s *Server) Drain() {
	if !s.draining.Swap(true) {
		s.logger.Info("readiness changed", "status", openapi.NotReady, "reasons", []string{"shutting down"})
	}
}

// GetHealthz serves GET /healthz: liveness. It checks no dependency, so a
// PostgreSQL or Redis outage never restarts the pod; readiness takes it out
// of its Service instead.
func (s *Server) GetHealthz(context.Context, openapi.GetHealthzRequestObject) (openapi.GetHealthzResponseObject, error) {
	return openapi.GetHealthz200TextResponse("ok"), nil
}

// GetReadyz serves GET /readyz: 200 when every readiness check passes, 503
// otherwise and once the API is draining. The endpoint is unauthenticated
// and kubelet probes only need the status code, so the body carries the
// status alone; which check failed and why is logged.
func (s *Server) GetReadyz(ctx context.Context, _ openapi.GetReadyzRequestObject) (openapi.GetReadyzResponseObject, error) {
	if !s.draining.Load() && s.readiness.ready(ctx) {
		return openapi.GetReadyz200JSONResponse{Status: openapi.Ready}, nil
	}
	return openapi.GetReadyz503JSONResponse{Status: openapi.NotReady}, nil
}
