package httpapi

import (
	"context"
	"fmt"
	"net/http"

	"heartbeat/apps/api/internal/inventory"
	"heartbeat/apps/api/internal/openapi"
)

func sourceOut(src inventory.Source) openapi.TelemetrySource {
	o := src.Observation
	observation := openapi.Observation{Status: openapi.ObservationStatus(o.Status), Collectors: o.Collectors}
	if observation.Collectors == nil {
		observation.Collectors = []string{}
	}
	if !o.CheckedAt.IsZero() {
		at := o.CheckedAt
		observation.CheckedAt = &at
	}
	if o.Detail != "" {
		detail := o.Detail
		observation.Detail = &detail
	}
	return openapi.TelemetrySource{
		Id: src.ID, ApplicationId: src.ApplicationID, EnvironmentId: src.EnvironmentID,
		SourceType: openapi.SourceType(src.SourceType), IngestMode: openapi.IngestMode(src.IngestMode),
		Status: openapi.Status(src.Status), Config: src.Config, Observation: observation,
		CreatedAt: src.CreatedAt, UpdatedAt: src.UpdatedAt,
	}
}

// ListTelemetrySources serves GET /api/v1/telemetry-sources.
func (s *Server) ListTelemetrySources(ctx context.Context, req openapi.ListTelemetrySourcesRequestObject) (openapi.ListTelemetrySourcesResponseObject, error) {
	if s.inventory == nil {
		return nil, errNoInventory
	}
	filter := inventory.SourceFilter{SourceType: deref(req.Params.SourceType), Status: status(req.Params.Status)}
	if req.Params.ApplicationId != nil {
		filter.ApplicationID = *req.Params.ApplicationId
	}
	if req.Params.EnvironmentId != nil {
		filter.EnvironmentID = *req.Params.EnvironmentId
	}
	sources, err := s.inventory.ListSources(ctx, filter, page(req.Params.Limit, req.Params.Cursor))
	if p, ok := failure(err, "telemetry source"); ok && p.Status == http.StatusBadRequest {
		return openapi.ListTelemetrySources400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(p)}, nil
	}
	if err != nil {
		return nil, err
	}
	out := openapi.ListTelemetrySources200JSONResponse{Items: make([]openapi.TelemetrySource, len(sources.Items)), NextCursor: nextCursor(sources.NextCursor)}
	for i, src := range sources.Items {
		out.Items[i] = sourceOut(src)
	}
	return out, nil
}

// GetTelemetrySource serves GET /api/v1/telemetry-sources/{id}.
func (s *Server) GetTelemetrySource(ctx context.Context, req openapi.GetTelemetrySourceRequestObject) (openapi.GetTelemetrySourceResponseObject, error) {
	if s.inventory == nil {
		return nil, errNoInventory
	}
	src, err := s.inventory.GetSource(ctx, req.Id)
	if p, ok := failure(err, "telemetry source"); ok && p.Status == http.StatusNotFound {
		return openapi.GetTelemetrySource404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
	}
	if err != nil {
		return nil, err
	}
	return openapi.GetTelemetrySource200JSONResponse(sourceOut(src)), nil
}

// CreateTelemetrySource serves POST /api/v1/telemetry-sources.
func (s *Server) CreateTelemetrySource(ctx context.Context, req openapi.CreateTelemetrySourceRequestObject) (openapi.CreateTelemetrySourceResponseObject, error) {
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	src, err := s.inventory.CreateSource(ctx, caller, inventory.NewSource{
		ApplicationID: b.ApplicationId, SourceType: string(b.SourceType), IngestMode: string(b.IngestMode),
		Status: status(b.Status), Config: b.Config,
	})
	if p, ok := failure(err, "telemetry source"); ok {
		switch p.Status {
		case http.StatusBadRequest:
			return openapi.CreateTelemetrySource400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(p)}, nil
		case http.StatusConflict:
			return openapi.CreateTelemetrySource409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: conflict(p)}, nil
		case http.StatusUnprocessableEntity:
			return openapi.CreateTelemetrySource422ApplicationProblemPlusJSONResponse{NoOwnerApplicationProblemPlusJSONResponse: noOwner(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.CreateTelemetrySource201JSONResponse{
		Body:    sourceOut(src),
		Headers: openapi.CreateTelemetrySource201ResponseHeaders{Location: fmt.Sprintf("/api/v1/telemetry-sources/%s", src.ID)},
	}, nil
}

// UpdateTelemetrySource serves PATCH /api/v1/telemetry-sources/{id}.
func (s *Server) UpdateTelemetrySource(ctx context.Context, req openapi.UpdateTelemetrySourceRequestObject) (openapi.UpdateTelemetrySourceResponseObject, error) {
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	change := inventory.SourceChange{IngestMode: optional(b.IngestMode), Status: optional(b.Status)}
	if b.Config != nil {
		config := map[string]any(*b.Config)
		change.Config = &config
	}
	src, err := s.inventory.UpdateSource(ctx, caller, req.Id, change)
	if p, ok := failure(err, "telemetry source"); ok {
		switch p.Status {
		case http.StatusBadRequest:
			return openapi.UpdateTelemetrySource400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(p)}, nil
		case http.StatusNotFound:
			return openapi.UpdateTelemetrySource404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
		case http.StatusConflict:
			return openapi.UpdateTelemetrySource409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: conflict(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.UpdateTelemetrySource200JSONResponse(sourceOut(src)), nil
}

// DeleteTelemetrySource serves DELETE /api/v1/telemetry-sources/{id}.
func (s *Server) DeleteTelemetrySource(ctx context.Context, req openapi.DeleteTelemetrySourceRequestObject) (openapi.DeleteTelemetrySourceResponseObject, error) {
	caller, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	err = s.inventory.DeleteSource(ctx, caller, req.Id)
	if p, ok := failure(err, "telemetry source"); ok {
		switch p.Status {
		case http.StatusNotFound:
			return openapi.DeleteTelemetrySource404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
		case http.StatusConflict:
			return openapi.DeleteTelemetrySource409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: conflict(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.DeleteTelemetrySource204Response{}, nil
}
