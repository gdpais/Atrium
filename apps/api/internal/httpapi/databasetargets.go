package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"atrium/apps/api/internal/audit"
	"atrium/apps/api/internal/inventory"
	"atrium/apps/api/internal/openapi"
)

// DatabaseTargets reads and changes database targets: *inventory.Service.
type DatabaseTargets interface {
	ListDatabaseTargets(ctx context.Context, filter inventory.DatabaseTargetFilter, page inventory.Page) (inventory.DatabaseTargetPage, error)
	GetDatabaseTarget(ctx context.Context, id uuid.UUID) (inventory.DatabaseTarget, error)
	CreateDatabaseTarget(ctx context.Context, caller inventory.Caller, in inventory.NewDatabaseTarget) (inventory.DatabaseTarget, error)
	UpdateDatabaseTarget(ctx context.Context, caller inventory.Caller, id uuid.UUID, change inventory.DatabaseTargetChange) (inventory.DatabaseTarget, error)
	DeleteDatabaseTarget(ctx context.Context, caller inventory.Caller, id uuid.UUID) error
}

var _ DatabaseTargets = (*inventory.Service)(nil)

// errNoDatabaseTargets is a database target operation on a Server built
// without them.
var errNoDatabaseTargets = errors.New("httpapi: no database targets service")

// targetCaller is who sent a change to a target: the guard has checked
// their session and role.
func (s *Server) targetCaller(ctx context.Context) (inventory.Caller, error) {
	info := requestFrom(ctx)
	if info.principal == nil || s.databaseTargets == nil {
		return inventory.Caller{}, errNoDatabaseTargets
	}
	return inventory.Caller{
		Actor:     audit.UserActor(info.principal.User.ID, info.principal.User.Email),
		RequestID: info.client.RequestID,
	}, nil
}

func databaseTargetOut(t inventory.DatabaseTarget) openapi.DatabaseTarget {
	return openapi.DatabaseTarget{
		Id: t.ID, EnvironmentId: t.EnvironmentID, Engine: openapi.DatabaseEngine(t.Engine), Name: t.Name,
		Host: t.Host, Port: t.Port, DatabaseName: t.DatabaseName, CredentialRef: t.CredentialRef,
		Status: openapi.Status(t.Status), Labels: t.Labels, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

// ListDatabaseTargets serves GET /api/v1/database-targets.
func (s *Server) ListDatabaseTargets(ctx context.Context, req openapi.ListDatabaseTargetsRequestObject) (openapi.ListDatabaseTargetsResponseObject, error) {
	if s.databaseTargets == nil {
		return nil, errNoDatabaseTargets
	}
	filter := inventory.DatabaseTargetFilter{Status: status(req.Params.Status)}
	if req.Params.EnvironmentId != nil {
		filter.EnvironmentID = *req.Params.EnvironmentId
	}
	targets, err := s.databaseTargets.ListDatabaseTargets(ctx, filter, page(req.Params.Limit, req.Params.Cursor))
	if p, ok := failure(err, "database target"); ok && p.Status == http.StatusBadRequest {
		return openapi.ListDatabaseTargets400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(p)}, nil
	}
	if err != nil {
		return nil, err
	}
	out := openapi.ListDatabaseTargets200JSONResponse{Items: make([]openapi.DatabaseTarget, len(targets.Items)), NextCursor: nextCursor(targets.NextCursor)}
	for i, t := range targets.Items {
		out.Items[i] = databaseTargetOut(t)
	}
	return out, nil
}

// GetDatabaseTarget serves GET /api/v1/database-targets/{id}.
func (s *Server) GetDatabaseTarget(ctx context.Context, req openapi.GetDatabaseTargetRequestObject) (openapi.GetDatabaseTargetResponseObject, error) {
	if s.databaseTargets == nil {
		return nil, errNoDatabaseTargets
	}
	t, err := s.databaseTargets.GetDatabaseTarget(ctx, req.Id)
	if p, ok := failure(err, "database target"); ok && p.Status == http.StatusNotFound {
		return openapi.GetDatabaseTarget404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
	}
	if err != nil {
		return nil, err
	}
	return openapi.GetDatabaseTarget200JSONResponse(databaseTargetOut(t)), nil
}

// CreateDatabaseTarget serves POST /api/v1/database-targets.
func (s *Server) CreateDatabaseTarget(ctx context.Context, req openapi.CreateDatabaseTargetRequestObject) (openapi.CreateDatabaseTargetResponseObject, error) {
	caller, err := s.targetCaller(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	in := inventory.NewDatabaseTarget{
		EnvironmentID: b.EnvironmentId, Engine: deref(b.Engine), Name: b.Name, Host: b.Host,
		DatabaseName: deref(b.DatabaseName), CredentialRef: b.CredentialRef, Status: status(b.Status), Labels: labelsIn(b.Labels),
	}
	if b.Port != nil {
		// Zero is the default to inventory; the spec does not allow it.
		if *b.Port == 0 {
			return openapi.CreateDatabaseTarget400ApplicationProblemPlusJSONResponse{
				BadRequestApplicationProblemPlusJSONResponse: badRequest(problem(http.StatusBadRequest, "port: 0: want 1 to 65535, or leave it out for the engine's default")),
			}, nil
		}
		in.Port = *b.Port
	}
	t, err := s.databaseTargets.CreateDatabaseTarget(ctx, caller, in)
	if p, ok := failure(err, "database target"); ok {
		switch p.Status {
		case http.StatusBadRequest:
			return openapi.CreateDatabaseTarget400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(p)}, nil
		case http.StatusConflict:
			return openapi.CreateDatabaseTarget409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: conflict(p)}, nil
		case http.StatusUnprocessableEntity:
			return openapi.CreateDatabaseTarget422ApplicationProblemPlusJSONResponse{NoOwnerApplicationProblemPlusJSONResponse: noOwner(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.CreateDatabaseTarget201JSONResponse{
		Body:    databaseTargetOut(t),
		Headers: openapi.CreateDatabaseTarget201ResponseHeaders{Location: fmt.Sprintf("/api/v1/database-targets/%s", t.ID)},
	}, nil
}

// UpdateDatabaseTarget serves PATCH /api/v1/database-targets/{id}.
func (s *Server) UpdateDatabaseTarget(ctx context.Context, req openapi.UpdateDatabaseTargetRequestObject) (openapi.UpdateDatabaseTargetResponseObject, error) {
	caller, err := s.targetCaller(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	change := inventory.DatabaseTargetChange{
		Host: b.Host, Port: b.Port, DatabaseName: b.DatabaseName, CredentialRef: b.CredentialRef, Status: optional(b.Status),
	}
	if b.Labels != nil {
		labels := map[string]string(*b.Labels)
		change.Labels = &labels
	}
	t, err := s.databaseTargets.UpdateDatabaseTarget(ctx, caller, req.Id, change)
	if p, ok := failure(err, "database target"); ok {
		switch p.Status {
		case http.StatusBadRequest:
			return openapi.UpdateDatabaseTarget400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(p)}, nil
		case http.StatusNotFound:
			return openapi.UpdateDatabaseTarget404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
		case http.StatusConflict:
			return openapi.UpdateDatabaseTarget409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: conflict(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.UpdateDatabaseTarget200JSONResponse(databaseTargetOut(t)), nil
}

// DeleteDatabaseTarget serves DELETE /api/v1/database-targets/{id}.
func (s *Server) DeleteDatabaseTarget(ctx context.Context, req openapi.DeleteDatabaseTargetRequestObject) (openapi.DeleteDatabaseTargetResponseObject, error) {
	caller, err := s.targetCaller(ctx)
	if err != nil {
		return nil, err
	}
	err = s.databaseTargets.DeleteDatabaseTarget(ctx, caller, req.Id)
	if p, ok := failure(err, "database target"); ok {
		switch p.Status {
		case http.StatusNotFound:
			return openapi.DeleteDatabaseTarget404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
		case http.StatusConflict:
			return openapi.DeleteDatabaseTarget409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: conflict(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.DeleteDatabaseTarget204Response{}, nil
}
