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

// Inventory reads and changes environments, applications and their
// components: *inventory.Service.
type Inventory interface {
	ListEnvironments(ctx context.Context, status string) ([]inventory.Environment, error)
	GetEnvironment(ctx context.Context, id uuid.UUID) (inventory.Environment, error)
	CreateEnvironment(ctx context.Context, caller inventory.Caller, in inventory.NewEnvironment) (inventory.Environment, error)
	UpdateEnvironment(ctx context.Context, caller inventory.Caller, id uuid.UUID, change inventory.EnvironmentChange) (inventory.Environment, error)
	DeleteEnvironment(ctx context.Context, caller inventory.Caller, id uuid.UUID) error

	ListApplications(ctx context.Context, filter inventory.ApplicationFilter, page inventory.Page) (inventory.ApplicationPage, error)
	GetApplication(ctx context.Context, id uuid.UUID) (inventory.Application, error)
	CreateApplication(ctx context.Context, caller inventory.Caller, in inventory.NewApplication) (inventory.Application, error)
	UpdateApplication(ctx context.Context, caller inventory.Caller, id uuid.UUID, change inventory.ApplicationChange) (inventory.Application, error)
	DeleteApplication(ctx context.Context, caller inventory.Caller, id uuid.UUID) error

	ListComponents(ctx context.Context, filter inventory.ComponentFilter, page inventory.Page) (inventory.ComponentPage, error)
	GetComponent(ctx context.Context, id uuid.UUID) (inventory.Component, error)
	CreateComponent(ctx context.Context, caller inventory.Caller, in inventory.NewComponent) (inventory.Component, error)
	UpdateComponent(ctx context.Context, caller inventory.Caller, id uuid.UUID, change inventory.ComponentChange) (inventory.Component, error)
	DeleteComponent(ctx context.Context, caller inventory.Caller, id uuid.UUID) error

	ListSources(ctx context.Context, filter inventory.SourceFilter, page inventory.Page) (inventory.SourcePage, error)
	GetSource(ctx context.Context, id uuid.UUID) (inventory.Source, error)
	CreateSource(ctx context.Context, caller inventory.Caller, in inventory.NewSource) (inventory.Source, error)
	UpdateSource(ctx context.Context, caller inventory.Caller, id uuid.UUID, change inventory.SourceChange) (inventory.Source, error)
	DeleteSource(ctx context.Context, caller inventory.Caller, id uuid.UUID) error
}

var _ Inventory = (*inventory.Service)(nil)

// errNoInventory is an inventory operation on a Server built without one.
var errNoInventory = errors.New("httpapi: no inventory service")

// caller is who sent a change: the guard has checked their session and
// role.
func (s *Server) caller(ctx context.Context) (inventory.Caller, error) {
	info := requestFrom(ctx)
	if info.principal == nil || s.inventory == nil {
		return inventory.Caller{}, errNoInventory
	}
	return inventory.Caller{
		Actor:     audit.UserActor(info.principal.User.ID, info.principal.User.Email),
		RequestID: info.client.RequestID,
	}, nil
}

// failure returns the problem an inventory error is answered with, what
// naming the entity type for a 404; ok is false for an error that is not
// the request's fault. A handler answers only the statuses its operation
// declares, and any other as a server error.
func failure(err error, what string) (p openapi.Problem, ok bool) {
	var invalid *inventory.InvalidError
	var conflict *inventory.ConflictError
	var owner *inventory.OwnerError
	switch {
	case errors.As(err, &invalid):
		return problem(http.StatusBadRequest, invalid.Error()), true
	case errors.Is(err, inventory.ErrNotFound):
		return problem(http.StatusNotFound, "no "+what+" has this ID"), true
	case errors.As(err, &conflict):
		return problem(http.StatusConflict, conflict.Detail), true
	case errors.As(err, &owner):
		return problem(http.StatusUnprocessableEntity, owner.Detail), true
	}
	return openapi.Problem{}, false
}

func badRequest(p openapi.Problem) openapi.BadRequestApplicationProblemPlusJSONResponse {
	return openapi.BadRequestApplicationProblemPlusJSONResponse(p)
}

func notFound(p openapi.Problem) openapi.NotFoundApplicationProblemPlusJSONResponse {
	return openapi.NotFoundApplicationProblemPlusJSONResponse(p)
}

func conflict(p openapi.Problem) openapi.ConflictApplicationProblemPlusJSONResponse {
	return openapi.ConflictApplicationProblemPlusJSONResponse(p)
}

func noOwner(p openapi.Problem) openapi.NoOwnerApplicationProblemPlusJSONResponse {
	return openapi.NoOwnerApplicationProblemPlusJSONResponse(p)
}

// page is the page a list's parameters ask for.
func page(limit *int, cursor *string) inventory.Page {
	var p inventory.Page
	if limit != nil {
		p.Limit = *limit
		if p.Limit == 0 {
			p.Limit = -1 // Page reads 0 as the default; ?limit=0 is out of range
		}
	}
	if cursor != nil {
		p.Cursor = *cursor
	}
	return p
}

// nextCursor is a page's next_cursor: absent on the last page.
func nextCursor(c string) *string {
	if c == "" {
		return nil
	}
	return &c
}

func deref[T ~string](p *T) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

func optional[T ~string](p *T) *string {
	if p == nil {
		return nil
	}
	s := string(*p)
	return &s
}

func labelsIn(p *openapi.Labels) map[string]string {
	if p == nil {
		return nil
	}
	return *p
}

func status(p *openapi.Status) string { return deref(p) }

// Environments.

func environmentOut(e inventory.Environment) openapi.Environment {
	return openapi.Environment{
		Id: e.ID, Name: e.Name, Slug: e.Slug, Type: openapi.EnvironmentType(e.Type),
		Description: e.Description, Status: openapi.Status(e.Status), Labels: e.Labels,
		CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
	}
}

// ListEnvironments serves GET /api/v1/environments.
func (s *Server) ListEnvironments(ctx context.Context, req openapi.ListEnvironmentsRequestObject) (openapi.ListEnvironmentsResponseObject, error) {
	if s.inventory == nil {
		return nil, errNoInventory
	}
	envs, err := s.inventory.ListEnvironments(ctx, status(req.Params.Status))
	if p, ok := failure(err, "environment"); ok && p.Status == http.StatusBadRequest {
		return openapi.ListEnvironments400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(p)}, nil
	}
	if err != nil {
		return nil, err
	}
	out := openapi.ListEnvironments200JSONResponse{Items: make([]openapi.Environment, len(envs))}
	for i, e := range envs {
		out.Items[i] = environmentOut(e)
	}
	return out, nil
}

// GetEnvironment serves GET /api/v1/environments/{id}.
func (s *Server) GetEnvironment(ctx context.Context, req openapi.GetEnvironmentRequestObject) (openapi.GetEnvironmentResponseObject, error) {
	if s.inventory == nil {
		return nil, errNoInventory
	}
	e, err := s.inventory.GetEnvironment(ctx, req.Id)
	if p, ok := failure(err, "environment"); ok && p.Status == http.StatusNotFound {
		return openapi.GetEnvironment404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
	}
	if err != nil {
		return nil, err
	}
	return openapi.GetEnvironment200JSONResponse(environmentOut(e)), nil
}

// CreateEnvironment serves POST /api/v1/environments.
func (s *Server) CreateEnvironment(ctx context.Context, req openapi.CreateEnvironmentRequestObject) (openapi.CreateEnvironmentResponseObject, error) {
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	e, err := s.inventory.CreateEnvironment(ctx, caller, inventory.NewEnvironment{
		Name: b.Name, Slug: b.Slug, Type: string(b.Type), Description: deref(b.Description),
		Status: status(b.Status), Labels: labelsIn(b.Labels),
	})
	if p, ok := failure(err, "environment"); ok {
		switch p.Status {
		case http.StatusBadRequest:
			return openapi.CreateEnvironment400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(p)}, nil
		case http.StatusConflict:
			return openapi.CreateEnvironment409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: conflict(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.CreateEnvironment201JSONResponse{
		Body:    environmentOut(e),
		Headers: openapi.CreateEnvironment201ResponseHeaders{Location: fmt.Sprintf("/api/v1/environments/%s", e.ID)},
	}, nil
}

// UpdateEnvironment serves PATCH /api/v1/environments/{id}.
func (s *Server) UpdateEnvironment(ctx context.Context, req openapi.UpdateEnvironmentRequestObject) (openapi.UpdateEnvironmentResponseObject, error) {
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	e, err := s.inventory.UpdateEnvironment(ctx, caller, req.Id, inventory.EnvironmentChange{
		Name: b.Name, Type: optional(b.Type), Description: b.Description, Status: optional(b.Status),
		Labels: (*map[string]string)(b.Labels),
	})
	if p, ok := failure(err, "environment"); ok {
		switch p.Status {
		case http.StatusBadRequest:
			return openapi.UpdateEnvironment400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(p)}, nil
		case http.StatusNotFound:
			return openapi.UpdateEnvironment404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
		case http.StatusConflict:
			return openapi.UpdateEnvironment409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: conflict(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.UpdateEnvironment200JSONResponse(environmentOut(e)), nil
}

// DeleteEnvironment serves DELETE /api/v1/environments/{id}.
func (s *Server) DeleteEnvironment(ctx context.Context, req openapi.DeleteEnvironmentRequestObject) (openapi.DeleteEnvironmentResponseObject, error) {
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	err = s.inventory.DeleteEnvironment(ctx, caller, req.Id)
	if p, ok := failure(err, "environment"); ok {
		switch p.Status {
		case http.StatusNotFound:
			return openapi.DeleteEnvironment404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
		case http.StatusConflict:
			return openapi.DeleteEnvironment409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: conflict(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.DeleteEnvironment204Response{}, nil
}

// Applications.

func applicationOut(a inventory.Application) openapi.Application {
	return openapi.Application{
		Id: a.ID, EnvironmentId: a.EnvironmentID, Name: a.Name, Platform: a.Platform,
		OwnerTeam: a.OwnerTeam, Criticality: openapi.Criticality(a.Criticality),
		Status: openapi.Status(a.Status), CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
}

// ListApplications serves GET /api/v1/applications.
func (s *Server) ListApplications(ctx context.Context, req openapi.ListApplicationsRequestObject) (openapi.ListApplicationsResponseObject, error) {
	if s.inventory == nil {
		return nil, errNoInventory
	}
	filter := inventory.ApplicationFilter{Status: status(req.Params.Status)}
	if req.Params.EnvironmentId != nil {
		filter.EnvironmentID = *req.Params.EnvironmentId
	}
	apps, err := s.inventory.ListApplications(ctx, filter, page(req.Params.Limit, req.Params.Cursor))
	if p, ok := failure(err, "application"); ok && p.Status == http.StatusBadRequest {
		return openapi.ListApplications400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(p)}, nil
	}
	if err != nil {
		return nil, err
	}
	out := openapi.ListApplications200JSONResponse{Items: make([]openapi.Application, len(apps.Items)), NextCursor: nextCursor(apps.NextCursor)}
	for i, a := range apps.Items {
		out.Items[i] = applicationOut(a)
	}
	return out, nil
}

// GetApplication serves GET /api/v1/applications/{id}.
func (s *Server) GetApplication(ctx context.Context, req openapi.GetApplicationRequestObject) (openapi.GetApplicationResponseObject, error) {
	if s.inventory == nil {
		return nil, errNoInventory
	}
	a, err := s.inventory.GetApplication(ctx, req.Id)
	if p, ok := failure(err, "application"); ok && p.Status == http.StatusNotFound {
		return openapi.GetApplication404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
	}
	if err != nil {
		return nil, err
	}
	return openapi.GetApplication200JSONResponse(applicationOut(a)), nil
}

// CreateApplication serves POST /api/v1/applications.
func (s *Server) CreateApplication(ctx context.Context, req openapi.CreateApplicationRequestObject) (openapi.CreateApplicationResponseObject, error) {
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	a, err := s.inventory.CreateApplication(ctx, caller, inventory.NewApplication{
		EnvironmentID: b.EnvironmentId, Name: b.Name, Platform: b.Platform, OwnerTeam: deref(b.OwnerTeam),
		Criticality: deref(b.Criticality), Status: status(b.Status),
	})
	if p, ok := failure(err, "application"); ok {
		switch p.Status {
		case http.StatusBadRequest:
			return openapi.CreateApplication400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(p)}, nil
		case http.StatusConflict:
			return openapi.CreateApplication409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: conflict(p)}, nil
		case http.StatusUnprocessableEntity:
			return openapi.CreateApplication422ApplicationProblemPlusJSONResponse{NoOwnerApplicationProblemPlusJSONResponse: noOwner(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.CreateApplication201JSONResponse{
		Body:    applicationOut(a),
		Headers: openapi.CreateApplication201ResponseHeaders{Location: fmt.Sprintf("/api/v1/applications/%s", a.ID)},
	}, nil
}

// UpdateApplication serves PATCH /api/v1/applications/{id}.
func (s *Server) UpdateApplication(ctx context.Context, req openapi.UpdateApplicationRequestObject) (openapi.UpdateApplicationResponseObject, error) {
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	a, err := s.inventory.UpdateApplication(ctx, caller, req.Id, inventory.ApplicationChange{
		Name: b.Name, Platform: b.Platform, OwnerTeam: b.OwnerTeam, Criticality: optional(b.Criticality),
		Status: optional(b.Status),
	})
	if p, ok := failure(err, "application"); ok {
		switch p.Status {
		case http.StatusBadRequest:
			return openapi.UpdateApplication400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(p)}, nil
		case http.StatusNotFound:
			return openapi.UpdateApplication404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
		case http.StatusConflict:
			return openapi.UpdateApplication409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: conflict(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.UpdateApplication200JSONResponse(applicationOut(a)), nil
}

// DeleteApplication serves DELETE /api/v1/applications/{id}.
func (s *Server) DeleteApplication(ctx context.Context, req openapi.DeleteApplicationRequestObject) (openapi.DeleteApplicationResponseObject, error) {
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	err = s.inventory.DeleteApplication(ctx, caller, req.Id)
	if p, ok := failure(err, "application"); ok {
		switch p.Status {
		case http.StatusNotFound:
			return openapi.DeleteApplication404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
		case http.StatusConflict:
			return openapi.DeleteApplication409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: conflict(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.DeleteApplication204Response{}, nil
}

// Components.

func componentOut(c inventory.Component) openapi.Component {
	return openapi.Component{
		Id: c.ID, ApplicationId: c.ApplicationID, EnvironmentId: c.EnvironmentID, Name: c.Name,
		ComponentType: c.ComponentType, RuntimeName: c.RuntimeName, Labels: c.Labels,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

// ListComponents serves GET /api/v1/components.
func (s *Server) ListComponents(ctx context.Context, req openapi.ListComponentsRequestObject) (openapi.ListComponentsResponseObject, error) {
	if s.inventory == nil {
		return nil, errNoInventory
	}
	var filter inventory.ComponentFilter
	if req.Params.ApplicationId != nil {
		filter.ApplicationID = *req.Params.ApplicationId
	}
	components, err := s.inventory.ListComponents(ctx, filter, page(req.Params.Limit, req.Params.Cursor))
	if p, ok := failure(err, "component"); ok && p.Status == http.StatusBadRequest {
		return openapi.ListComponents400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(p)}, nil
	}
	if err != nil {
		return nil, err
	}
	out := openapi.ListComponents200JSONResponse{Items: make([]openapi.Component, len(components.Items)), NextCursor: nextCursor(components.NextCursor)}
	for i, c := range components.Items {
		out.Items[i] = componentOut(c)
	}
	return out, nil
}

// GetComponent serves GET /api/v1/components/{id}.
func (s *Server) GetComponent(ctx context.Context, req openapi.GetComponentRequestObject) (openapi.GetComponentResponseObject, error) {
	if s.inventory == nil {
		return nil, errNoInventory
	}
	c, err := s.inventory.GetComponent(ctx, req.Id)
	if p, ok := failure(err, "component"); ok && p.Status == http.StatusNotFound {
		return openapi.GetComponent404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
	}
	if err != nil {
		return nil, err
	}
	return openapi.GetComponent200JSONResponse(componentOut(c)), nil
}

// CreateComponent serves POST /api/v1/components.
func (s *Server) CreateComponent(ctx context.Context, req openapi.CreateComponentRequestObject) (openapi.CreateComponentResponseObject, error) {
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	c, err := s.inventory.CreateComponent(ctx, caller, inventory.NewComponent{
		ApplicationID: b.ApplicationId, Name: b.Name, ComponentType: b.ComponentType,
		RuntimeName: deref(b.RuntimeName), Labels: labelsIn(b.Labels),
	})
	if p, ok := failure(err, "component"); ok {
		switch p.Status {
		case http.StatusBadRequest:
			return openapi.CreateComponent400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(p)}, nil
		case http.StatusConflict:
			return openapi.CreateComponent409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: conflict(p)}, nil
		case http.StatusUnprocessableEntity:
			return openapi.CreateComponent422ApplicationProblemPlusJSONResponse{NoOwnerApplicationProblemPlusJSONResponse: noOwner(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.CreateComponent201JSONResponse{
		Body:    componentOut(c),
		Headers: openapi.CreateComponent201ResponseHeaders{Location: fmt.Sprintf("/api/v1/components/%s", c.ID)},
	}, nil
}

// UpdateComponent serves PATCH /api/v1/components/{id}.
func (s *Server) UpdateComponent(ctx context.Context, req openapi.UpdateComponentRequestObject) (openapi.UpdateComponentResponseObject, error) {
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	c, err := s.inventory.UpdateComponent(ctx, caller, req.Id, inventory.ComponentChange{
		Name: b.Name, ComponentType: b.ComponentType, RuntimeName: b.RuntimeName,
		Labels: (*map[string]string)(b.Labels),
	})
	if p, ok := failure(err, "component"); ok {
		switch p.Status {
		case http.StatusBadRequest:
			return openapi.UpdateComponent400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(p)}, nil
		case http.StatusNotFound:
			return openapi.UpdateComponent404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
		case http.StatusConflict:
			return openapi.UpdateComponent409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: conflict(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.UpdateComponent200JSONResponse(componentOut(c)), nil
}

// DeleteComponent serves DELETE /api/v1/components/{id}.
func (s *Server) DeleteComponent(ctx context.Context, req openapi.DeleteComponentRequestObject) (openapi.DeleteComponentResponseObject, error) {
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	err = s.inventory.DeleteComponent(ctx, caller, req.Id)
	if p, ok := failure(err, "component"); ok {
		switch p.Status {
		case http.StatusNotFound:
			return openapi.DeleteComponent404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
		case http.StatusConflict:
			return openapi.DeleteComponent409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: conflict(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.DeleteComponent204Response{}, nil
}
