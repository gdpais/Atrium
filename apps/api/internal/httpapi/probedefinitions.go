package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/openapi"
	"heartbeat/apps/api/internal/probedefs"
)

// ProbeDefinitions reads and changes probe definitions: *probedefs.Service.
type ProbeDefinitions interface {
	List(ctx context.Context) ([]probedefs.Definition, error)
	Get(ctx context.Context, id uuid.UUID) (probedefs.Definition, error)
	Versions(ctx context.Context, id uuid.UUID) ([]probedefs.Definition, error)
	Create(ctx context.Context, def probedefs.NewDefinition, actor audit.Actor, requestID string) (probedefs.Definition, error)
	AddVersion(ctx context.Context, from uuid.UUID, content probedefs.Content, actor audit.Actor, requestID string) (probedefs.Definition, bool, error)
	SetEnabled(ctx context.Context, id uuid.UUID, enabled bool, actor audit.Actor, requestID string) (probedefs.Definition, error)
}

var _ ProbeDefinitions = (*probedefs.Service)(nil)

// errNoProbeDefinitions is a probe definition operation on a Server built
// without them.
var errNoProbeDefinitions = errors.New("httpapi: no probe definitions service")

// probeDefinitionsPath is where the probe definitions are served.
const probeDefinitionsPath = "/api/v1/probe-definitions"

// ListProbeDefinitions serves GET /api/v1/probe-definitions.
func (s *Server) ListProbeDefinitions(ctx context.Context, _ openapi.ListProbeDefinitionsRequestObject) (openapi.ListProbeDefinitionsResponseObject, error) {
	if s.probeDefinitions == nil {
		return nil, errNoProbeDefinitions
	}
	defs, err := s.probeDefinitions.List(ctx)
	if err != nil {
		return nil, err
	}
	return openapi.ListProbeDefinitions200JSONResponse(probeDefinitionList(defs)), nil
}

// CreateProbeDefinition serves POST /api/v1/probe-definitions.
func (s *Server) CreateProbeDefinition(ctx context.Context, req openapi.CreateProbeDefinitionRequestObject) (openapi.CreateProbeDefinitionResponseObject, error) {
	actor, requestID, err := s.probeDefinitionChange(ctx)
	if err != nil {
		return nil, err
	}
	body := req.Body
	c, detail := content(body.Overrides, body.QueryTemplate, body.Description, body.DefaultIntervalSeconds, body.TimeoutMs)
	if detail != "" {
		return openapi.CreateProbeDefinition400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: openapi.BadRequestApplicationProblemPlusJSONResponse(problem(http.StatusBadRequest, detail)),
		}, nil
	}
	def, err := s.probeDefinitions.Create(ctx, probedefs.NewDefinition{Engine: string(body.Engine), Name: body.Name, Content: c}, actor, requestID)
	var invalid *probedefs.InvalidError
	var conflict *probedefs.ConflictError
	switch {
	case errors.As(err, &invalid):
		return openapi.CreateProbeDefinition400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: openapi.BadRequestApplicationProblemPlusJSONResponse(problem(http.StatusBadRequest, invalid.Error())),
		}, nil
	case errors.As(err, &conflict):
		return openapi.CreateProbeDefinition409ApplicationProblemPlusJSONResponse{
			ConflictApplicationProblemPlusJSONResponse: openapi.ConflictApplicationProblemPlusJSONResponse(problem(http.StatusConflict, conflict.Error())),
		}, nil
	case err != nil:
		return nil, err
	}
	return openapi.CreateProbeDefinition201JSONResponse{
		Body:    probeDefinition(def),
		Headers: openapi.CreateProbeDefinition201ResponseHeaders{Location: probeDefinitionsPath + "/" + def.ID.String()},
	}, nil
}

// GetProbeDefinition serves GET /api/v1/probe-definitions/{id}.
func (s *Server) GetProbeDefinition(ctx context.Context, req openapi.GetProbeDefinitionRequestObject) (openapi.GetProbeDefinitionResponseObject, error) {
	if s.probeDefinitions == nil {
		return nil, errNoProbeDefinitions
	}
	def, err := s.probeDefinitions.Get(ctx, req.Id)
	switch {
	case errors.Is(err, probedefs.ErrNotFound):
		return openapi.GetProbeDefinition404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: notFound(),
		}, nil
	case err != nil:
		return nil, err
	}
	return openapi.GetProbeDefinition200JSONResponse(probeDefinition(def)), nil
}

// UpdateProbeDefinition serves PATCH /api/v1/probe-definitions/{id}: enable
// or disable a custom definition.
func (s *Server) UpdateProbeDefinition(ctx context.Context, req openapi.UpdateProbeDefinitionRequestObject) (openapi.UpdateProbeDefinitionResponseObject, error) {
	actor, requestID, err := s.probeDefinitionChange(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body.Enabled == nil {
		return openapi.UpdateProbeDefinition400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: openapi.BadRequestApplicationProblemPlusJSONResponse(problem(http.StatusBadRequest, "nothing to change: set enabled")),
		}, nil
	}
	def, err := s.probeDefinitions.SetEnabled(ctx, req.Id, *req.Body.Enabled, actor, requestID)
	var conflict *probedefs.ConflictError
	switch {
	case errors.Is(err, probedefs.ErrNotFound):
		return openapi.UpdateProbeDefinition404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: notFound(),
		}, nil
	case errors.As(err, &conflict):
		return openapi.UpdateProbeDefinition409ApplicationProblemPlusJSONResponse{
			ConflictApplicationProblemPlusJSONResponse: openapi.ConflictApplicationProblemPlusJSONResponse(problem(http.StatusConflict, conflict.Error())),
		}, nil
	case err != nil:
		return nil, err
	}
	return openapi.UpdateProbeDefinition200JSONResponse(probeDefinition(def)), nil
}

// ListProbeDefinitionVersions serves GET
// /api/v1/probe-definitions/{id}/versions.
func (s *Server) ListProbeDefinitionVersions(ctx context.Context, req openapi.ListProbeDefinitionVersionsRequestObject) (openapi.ListProbeDefinitionVersionsResponseObject, error) {
	if s.probeDefinitions == nil {
		return nil, errNoProbeDefinitions
	}
	defs, err := s.probeDefinitions.Versions(ctx, req.Id)
	switch {
	case errors.Is(err, probedefs.ErrNotFound):
		return openapi.ListProbeDefinitionVersions404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: notFound(),
		}, nil
	case err != nil:
		return nil, err
	}
	return openapi.ListProbeDefinitionVersions200JSONResponse(probeDefinitionList(defs)), nil
}

// CreateProbeDefinitionVersion serves POST
// /api/v1/probe-definitions/{id}/versions: change a custom definition.
func (s *Server) CreateProbeDefinitionVersion(ctx context.Context, req openapi.CreateProbeDefinitionVersionRequestObject) (openapi.CreateProbeDefinitionVersionResponseObject, error) {
	actor, requestID, err := s.probeDefinitionChange(ctx)
	if err != nil {
		return nil, err
	}
	body := req.Body
	c, detail := content(body.Overrides, body.QueryTemplate, body.Description, body.DefaultIntervalSeconds, body.TimeoutMs)
	if detail != "" {
		return openapi.CreateProbeDefinitionVersion400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: openapi.BadRequestApplicationProblemPlusJSONResponse(problem(http.StatusBadRequest, detail)),
		}, nil
	}
	def, added, err := s.probeDefinitions.AddVersion(ctx, req.Id, c, actor, requestID)
	var invalid *probedefs.InvalidError
	var conflict *probedefs.ConflictError
	switch {
	case errors.As(err, &invalid):
		return openapi.CreateProbeDefinitionVersion400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: openapi.BadRequestApplicationProblemPlusJSONResponse(problem(http.StatusBadRequest, invalid.Error())),
		}, nil
	case errors.Is(err, probedefs.ErrNotFound):
		return openapi.CreateProbeDefinitionVersion404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: notFound(),
		}, nil
	case errors.As(err, &conflict):
		return openapi.CreateProbeDefinitionVersion409ApplicationProblemPlusJSONResponse{
			ConflictApplicationProblemPlusJSONResponse: openapi.ConflictApplicationProblemPlusJSONResponse(problem(http.StatusConflict, conflict.Error())),
		}, nil
	case err != nil:
		return nil, err
	case !added:
		return openapi.CreateProbeDefinitionVersion200JSONResponse(probeDefinition(def)), nil
	}
	return openapi.CreateProbeDefinitionVersion201JSONResponse{
		Body:    probeDefinition(def),
		Headers: openapi.CreateProbeDefinitionVersion201ResponseHeaders{Location: probeDefinitionsPath + "/" + def.ID.String()},
	}, nil
}

// probeDefinitionChange returns who makes a change to a probe definition,
// and the request's ID, for its audit record.
func (s *Server) probeDefinitionChange(ctx context.Context) (audit.Actor, string, error) {
	info := requestFrom(ctx)
	if s.probeDefinitions == nil {
		return audit.Actor{}, "", errNoProbeDefinitions
	}
	if info.principal == nil {
		return audit.Actor{}, "", errNoAuth
	}
	return audit.UserActor(info.principal.User.ID, info.principal.User.Email), info.client.RequestID, nil
}

// content is a request's content of a custom definition's version, or the
// detail of a 400 problem: a field left out is zero, which takes its
// default, so an explicit zero, which the spec does not allow either, is
// refused here.
func content(overrides, query string, description *string, interval, timeout *int) (probedefs.Content, string) {
	c := probedefs.Content{Overrides: overrides, QueryTemplate: query}
	if description != nil {
		c.Description = *description
	}
	if interval != nil {
		if *interval == 0 {
			return probedefs.Content{}, fmt.Sprintf("default_interval_seconds 0: want %d to %d, or leave it out for %d",
				probedefs.MinIntervalSeconds, probedefs.MaxIntervalSeconds, probedefs.DefaultIntervalSeconds)
		}
		c.DefaultIntervalSeconds = *interval
	}
	if timeout != nil {
		if *timeout == 0 {
			return probedefs.Content{}, fmt.Sprintf("timeout_ms 0: want 1 to %d, or leave it out for the collector's default", probedefs.MaxTimeoutMS)
		}
		c.TimeoutMS = *timeout
	}
	return c, ""
}

func notFound() openapi.NotFoundApplicationProblemPlusJSONResponse {
	return openapi.NotFoundApplicationProblemPlusJSONResponse(problem(http.StatusNotFound, "no probe definition has this ID"))
}

func probeDefinitionList(defs []probedefs.Definition) openapi.ProbeDefinitionList {
	items := make([]openapi.ProbeDefinition, len(defs))
	for i, def := range defs {
		items[i] = probeDefinition(def)
	}
	return openapi.ProbeDefinitionList{Items: items}
}

// probeDefinition is def as the API shows it.
func probeDefinition(def probedefs.Definition) openapi.ProbeDefinition {
	out := openapi.ProbeDefinition{
		Id:                     def.ID,
		Engine:                 openapi.ProbeEngine(def.Engine),
		Name:                   def.Name,
		Version:                def.Version,
		Builtin:                def.Builtin,
		Category:               def.Category,
		Description:            def.Description,
		DefaultIntervalSeconds: def.DefaultIntervalSeconds,
		Enabled:                def.Enabled,
		Latest:                 def.Latest,
		CreatedAt:              def.CreatedAt,
		UpdatedAt:              def.UpdatedAt,
	}
	if def.Overrides != "" {
		out.Overrides = &def.Overrides
	}
	if def.TimeoutMS != 0 {
		out.TimeoutMs = &def.TimeoutMS
	}
	if def.QueryTemplate != "" {
		out.QueryTemplate = &def.QueryTemplate
	}
	return out
}
