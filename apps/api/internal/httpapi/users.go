package httpapi

import (
	"context"
	"errors"
	"math"
	"net/http"

	"github.com/google/uuid"

	"atrium/apps/api/internal/audit"
	"atrium/apps/api/internal/auth"
	"atrium/apps/api/internal/openapi"
	"atrium/apps/api/internal/users"
)

// Users reads and changes users and their roles, for admins:
// *users.Service.
type Users interface {
	List(ctx context.Context, status string) ([]users.User, error)
	Get(ctx context.Context, id uuid.UUID) (users.User, error)
	Create(ctx context.Context, caller users.Caller, in users.NewUser) (users.User, error)
	Update(ctx context.Context, caller users.Caller, id uuid.UUID, change users.Change) (users.User, error)
	SetPassword(ctx context.Context, caller users.Caller, id uuid.UUID, secret string) error
	Grant(ctx context.Context, caller users.Caller, id uuid.UUID, role string) (users.User, error)
	Revoke(ctx context.Context, caller users.Caller, id uuid.UUID, role string) (users.User, error)
}

var _ Users = (*users.Service)(nil)

// errNoUsers is a user operation on a Server built without a users service.
var errNoUsers = errors.New("httpapi: no users service")

// userCaller is the admin who sent a change: the guard has checked their
// session and role.
func (s *Server) userCaller(ctx context.Context) (users.Caller, error) {
	info := requestFrom(ctx)
	if info.principal == nil || s.users == nil {
		return users.Caller{}, errNoUsers
	}
	return users.Caller{
		Actor:     audit.UserActor(info.principal.User.ID, info.principal.User.Email),
		RequestID: info.client.RequestID,
	}, nil
}

// userProblem returns the problem a users error is answered with; ok is
// false for an error that is not the request's fault. A role that does not
// exist is a 404 when the path names it, a 400 when the body does.
func userProblem(err error, roleInPath bool) (p openapi.Problem, ok bool) {
	var invalid *users.InvalidError
	switch {
	case errors.As(err, &invalid):
		return problem(http.StatusBadRequest, invalid.Error()), true
	case errors.Is(err, users.ErrNotFound):
		return problem(http.StatusNotFound, "no user has this ID"), true
	case errors.Is(err, users.ErrUnknownRole) && roleInPath:
		return problem(http.StatusNotFound, "no such role"), true
	case errors.Is(err, users.ErrUnknownRole):
		return problem(http.StatusBadRequest, "roles: no such role; want viewer, operator or admin"), true
	case errors.Is(err, users.ErrEmailTaken):
		return problem(http.StatusConflict, users.ErrEmailTaken.Error()), true
	case errors.Is(err, users.ErrLastAdmin):
		return problem(http.StatusConflict, users.ErrLastAdmin.Error()), true
	case errors.Is(err, users.ErrLastRole):
		return problem(http.StatusConflict, users.ErrLastRole.Error()), true
	}
	return openapi.Problem{}, false
}

// ListUsers serves GET /api/v1/users.
func (s *Server) ListUsers(ctx context.Context, req openapi.ListUsersRequestObject) (openapi.ListUsersResponseObject, error) {
	if s.users == nil {
		return nil, errNoUsers
	}
	var status string
	if req.Params.Status != nil {
		status = string(*req.Params.Status)
		if status != users.StatusActive && status != users.StatusDisabled {
			return openapi.ListUsers400ApplicationProblemPlusJSONResponse{
				BadRequestApplicationProblemPlusJSONResponse: openapi.BadRequestApplicationProblemPlusJSONResponse(problem(http.StatusBadRequest, "status: want active or disabled")),
			}, nil
		}
	}
	list, err := s.users.List(ctx, status)
	if err != nil {
		return nil, err
	}
	out := openapi.ListUsers200JSONResponse{Items: make([]openapi.User, len(list))}
	for i, u := range list {
		if out.Items[i], err = apiUser(u); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CreateUser serves POST /api/v1/users.
func (s *Server) CreateUser(ctx context.Context, req openapi.CreateUserRequestObject) (openapi.CreateUserResponseObject, error) {
	caller, err := s.userCaller(ctx)
	if err != nil {
		return nil, err
	}
	in := users.NewUser{Email: req.Body.Email, Password: req.Body.Password}
	if req.Body.DisplayName != nil {
		in.DisplayName = *req.Body.DisplayName
	}
	for _, r := range req.Body.Roles {
		in.Roles = append(in.Roles, string(r))
	}
	created, err := s.users.Create(ctx, caller, in)
	if p, ok := userProblem(err, false); ok {
		switch p.Status {
		case http.StatusBadRequest:
			return openapi.CreateUser400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: openapi.BadRequestApplicationProblemPlusJSONResponse(p)}, nil
		case http.StatusConflict:
			return openapi.CreateUser409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: openapi.ConflictApplicationProblemPlusJSONResponse(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	body, err := apiUser(created)
	if err != nil {
		return nil, err
	}
	return openapi.CreateUser201JSONResponse{
		Body:    body,
		Headers: openapi.CreateUser201ResponseHeaders{Location: "/api/v1/users/" + created.ID},
	}, nil
}

// GetUser serves GET /api/v1/users/{id}.
func (s *Server) GetUser(ctx context.Context, req openapi.GetUserRequestObject) (openapi.GetUserResponseObject, error) {
	if s.users == nil {
		return nil, errNoUsers
	}
	u, err := s.users.Get(ctx, req.Id)
	if p, ok := userProblem(err, false); ok && p.Status == http.StatusNotFound {
		return openapi.GetUser404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: openapi.NotFoundApplicationProblemPlusJSONResponse(p)}, nil
	}
	if err != nil {
		return nil, err
	}
	body, err := apiUser(u)
	if err != nil {
		return nil, err
	}
	return openapi.GetUser200JSONResponse(body), nil
}

// UpdateUser serves PATCH /api/v1/users/{id}.
func (s *Server) UpdateUser(ctx context.Context, req openapi.UpdateUserRequestObject) (openapi.UpdateUserResponseObject, error) {
	caller, err := s.userCaller(ctx)
	if err != nil {
		return nil, err
	}
	change := users.Change{Email: req.Body.Email, DisplayName: req.Body.DisplayName}
	if req.Body.Status != nil {
		status := string(*req.Body.Status)
		change.Status = &status
	}
	u, err := s.users.Update(ctx, caller, req.Id, change)
	if p, ok := userProblem(err, false); ok {
		switch p.Status {
		case http.StatusBadRequest:
			return openapi.UpdateUser400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: openapi.BadRequestApplicationProblemPlusJSONResponse(p)}, nil
		case http.StatusNotFound:
			return openapi.UpdateUser404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: openapi.NotFoundApplicationProblemPlusJSONResponse(p)}, nil
		case http.StatusConflict:
			return openapi.UpdateUser409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: openapi.ConflictApplicationProblemPlusJSONResponse(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	body, err := apiUser(u)
	if err != nil {
		return nil, err
	}
	return openapi.UpdateUser200JSONResponse(body), nil
}

// SetUserPassword serves PUT /api/v1/users/{id}/password.
func (s *Server) SetUserPassword(ctx context.Context, req openapi.SetUserPasswordRequestObject) (openapi.SetUserPasswordResponseObject, error) {
	caller, err := s.userCaller(ctx)
	if err != nil {
		return nil, err
	}
	err = s.users.SetPassword(ctx, caller, req.Id, req.Body.Password)
	if p, ok := userProblem(err, false); ok {
		switch p.Status {
		case http.StatusBadRequest:
			return openapi.SetUserPassword400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: openapi.BadRequestApplicationProblemPlusJSONResponse(p)}, nil
		case http.StatusNotFound:
			return openapi.SetUserPassword404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: openapi.NotFoundApplicationProblemPlusJSONResponse(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.SetUserPassword204Response{}, nil
}

// GrantRole serves PUT /api/v1/users/{id}/roles/{role}.
func (s *Server) GrantRole(ctx context.Context, req openapi.GrantRoleRequestObject) (openapi.GrantRoleResponseObject, error) {
	caller, err := s.userCaller(ctx)
	if err != nil {
		return nil, err
	}
	u, err := s.users.Grant(ctx, caller, req.Id, string(req.Role))
	if p, ok := userProblem(err, true); ok && p.Status == http.StatusNotFound {
		return openapi.GrantRole404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: openapi.NotFoundApplicationProblemPlusJSONResponse(p)}, nil
	}
	if err != nil {
		return nil, err
	}
	body, err := apiUser(u)
	if err != nil {
		return nil, err
	}
	return openapi.GrantRole200JSONResponse(body), nil
}

// RevokeRole serves DELETE /api/v1/users/{id}/roles/{role}.
func (s *Server) RevokeRole(ctx context.Context, req openapi.RevokeRoleRequestObject) (openapi.RevokeRoleResponseObject, error) {
	caller, err := s.userCaller(ctx)
	if err != nil {
		return nil, err
	}
	u, err := s.users.Revoke(ctx, caller, req.Id, string(req.Role))
	if p, ok := userProblem(err, true); ok {
		switch p.Status {
		case http.StatusNotFound:
			return openapi.RevokeRole404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: openapi.NotFoundApplicationProblemPlusJSONResponse(p)}, nil
		case http.StatusConflict:
			return openapi.RevokeRole409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: openapi.ConflictApplicationProblemPlusJSONResponse(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	body, err := apiUser(u)
	if err != nil {
		return nil, err
	}
	return openapi.RevokeRole200JSONResponse(body), nil
}

// ChangeOwnPassword serves PUT /api/v1/account/password: every session of
// the user ends, the one this request came with included, so the cookie is
// cleared.
func (s *Server) ChangeOwnPassword(ctx context.Context, req openapi.ChangeOwnPasswordRequestObject) (openapi.ChangeOwnPasswordResponseObject, error) {
	info := requestFrom(ctx)
	if info.principal == nil || s.auth == nil {
		return nil, errNoAuth
	}
	err := s.auth.ChangePassword(ctx, *info.principal, req.Body.CurrentPassword, req.Body.NewPassword, info.client)
	var invalid *auth.InvalidError
	var limited *auth.RateLimitedError
	switch {
	case errors.As(err, &invalid):
		return openapi.ChangeOwnPassword400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: openapi.BadRequestApplicationProblemPlusJSONResponse(problem(http.StatusBadRequest, invalid.Error())),
		}, nil
	case errors.Is(err, auth.ErrWrongPassword):
		return openapi.ChangeOwnPassword403ApplicationProblemPlusJSONResponse(problem(http.StatusForbidden, "the current password is wrong")), nil
	case errors.As(err, &limited):
		return openapi.ChangeOwnPassword429ApplicationProblemPlusJSONResponse{
			TooManyFailuresApplicationProblemPlusJSONResponse: openapi.TooManyFailuresApplicationProblemPlusJSONResponse{
				Body:    problem(http.StatusTooManyRequests, "too many failed attempts for this account or from this address; try again later"),
				Headers: openapi.TooManyFailuresResponseHeaders{RetryAfter: int(math.Ceil(limited.RetryAfter.Seconds()))},
			},
		}, nil
	case err != nil:
		return nil, err
	}
	return openapi.ChangeOwnPassword204Response{
		Headers: openapi.ChangeOwnPassword204ResponseHeaders{SetCookie: clearCookie().String()},
	}, nil
}

// apiUser is u as the API shows it.
func apiUser(u users.User) (openapi.User, error) {
	id, err := uuid.Parse(u.ID)
	if err != nil {
		return openapi.User{}, err
	}
	roles := make([]openapi.Role, len(u.Roles))
	for i, r := range u.Roles {
		roles[i] = openapi.Role(r)
	}
	return openapi.User{
		Id: id, Email: u.Email, DisplayName: u.DisplayName, Status: openapi.Status(u.Status), Roles: roles,
		LastLoginAt: u.LastLoginAt, CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}, nil
}
