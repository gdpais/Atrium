package httpapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/google/uuid"

	"atrium/apps/api/internal/auth"
	"atrium/apps/api/internal/openapi"
)

// errNoAuth is a session operation on a Server built without auth.
var errNoAuth = errors.New("httpapi: no auth service")

// CreateSession serves POST /api/v1/session: log in.
func (s *Server) CreateSession(ctx context.Context, req openapi.CreateSessionRequestObject) (openapi.CreateSessionResponseObject, error) {
	if s.auth == nil {
		return nil, errNoAuth
	}
	principal, token, err := s.auth.Login(ctx, req.Body.Email, req.Body.Password, requestFrom(ctx).client)
	var invalid *auth.InvalidError
	var limited *auth.RateLimitedError
	switch {
	case errors.As(err, &invalid):
		return openapi.CreateSession400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: openapi.BadRequestApplicationProblemPlusJSONResponse(problem(http.StatusBadRequest, invalid.Error())),
		}, nil
	case errors.Is(err, auth.ErrInvalidCredentials):
		return openapi.CreateSession401ApplicationProblemPlusJSONResponse{
			UnauthorizedApplicationProblemPlusJSONResponse: openapi.UnauthorizedApplicationProblemPlusJSONResponse(problem(http.StatusUnauthorized, "wrong email or password")),
		}, nil
	case errors.As(err, &limited):
		return openapi.CreateSession429ApplicationProblemPlusJSONResponse{
			Body:    problem(http.StatusTooManyRequests, "too many failed logins for this account or from this address; try again later"),
			Headers: openapi.CreateSession429ResponseHeaders{RetryAfter: int(math.Ceil(limited.RetryAfter.Seconds()))},
		}, nil
	case err != nil:
		return nil, err
	}
	body, err := s.session(principal)
	if err != nil {
		return nil, err
	}
	return openapi.CreateSession201JSONResponse{
		Body:    body,
		Headers: openapi.CreateSession201ResponseHeaders{SetCookie: sessionCookie(token, body.ExpiresAt).String()},
	}, nil
}

// GetSession serves GET /api/v1/session: who is logged in.
func (s *Server) GetSession(ctx context.Context, _ openapi.GetSessionRequestObject) (openapi.GetSessionResponseObject, error) {
	p := requestFrom(ctx).principal
	if p == nil || s.auth == nil {
		return nil, errNoAuth
	}
	body, err := s.session(*p)
	if err != nil {
		return nil, err
	}
	return openapi.GetSession200JSONResponse(body), nil
}

// DeleteSession serves DELETE /api/v1/session: log out.
func (s *Server) DeleteSession(ctx context.Context, _ openapi.DeleteSessionRequestObject) (openapi.DeleteSessionResponseObject, error) {
	info := requestFrom(ctx)
	if info.principal == nil || s.auth == nil {
		return nil, errNoAuth
	}
	if err := s.auth.Logout(ctx, *info.principal, info.client); err != nil {
		return nil, err
	}
	return openapi.DeleteSession204Response{
		Headers: openapi.DeleteSession204ResponseHeaders{SetCookie: clearCookie().String()},
	}, nil
}

// session is p as the API shows it.
func (s *Server) session(p auth.Principal) (openapi.Session, error) {
	sessionID, err := uuid.Parse(p.Session.ID)
	if err != nil {
		return openapi.Session{}, fmt.Errorf("session ID: %w", err)
	}
	userID, err := uuid.Parse(p.User.ID)
	if err != nil {
		return openapi.Session{}, fmt.Errorf("user ID: %w", err)
	}
	roles := make([]openapi.Role, len(p.User.Roles))
	for i, r := range p.User.Roles {
		roles[i] = openapi.Role(r)
	}
	return openapi.Session{
		Id:        sessionID,
		CreatedAt: p.Session.Created,
		ExpiresAt: s.auth.ExpiresAt(p.Session),
		User: openapi.SessionUser{
			Id: userID, Email: p.User.Email, DisplayName: p.User.DisplayName, Roles: roles,
		},
	}, nil
}

// sessionCookie holds token until expires, the session's absolute end, so
// that a wallboard's browser keeps it across restarts; the idle timeout is
// the API's to enforce.
func sessionCookie(token string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name: SessionCookie, Value: token, Path: "/",
		MaxAge:   max(int(time.Until(expires).Seconds()), 1),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	}
}

// clearCookie tells the browser to drop the session cookie.
func clearCookie() *http.Cookie {
	return &http.Cookie{
		Name: SessionCookie, Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	}
}
