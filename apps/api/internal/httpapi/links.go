package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"heartbeat/apps/api/internal/inventory"
	"heartbeat/apps/api/internal/links"
	"heartbeat/apps/api/internal/openapi"
	"heartbeat/internal/config"
)

// LinkConfig is the integrations config links are rendered from:
// *links.Source.
type LinkConfig interface {
	Config() (config.RuntimeConfig, error)
}

var _ LinkConfig = (*links.Source)(nil)

// Telemetry says what an entity's telemetry is labelled with:
// *inventory.Service.
type Telemetry interface {
	ApplicationTelemetry(ctx context.Context, id uuid.UUID) (inventory.Telemetry, error)
	DatabaseTargetTelemetry(ctx context.Context, id uuid.UUID) (inventory.Telemetry, error)
}

var _ Telemetry = (*inventory.Service)(nil)

// errNoTelemetry is a link operation on a Server built without inventory.
var errNoTelemetry = errors.New("httpapi: no telemetry lookup")

// linkConfig is the config to render from; ok false answers 503.
func (s *Server) linkConfig() (config.RuntimeConfig, openapi.Problem, bool) {
	if s.links == nil {
		return config.RuntimeConfig{}, openapi.Problem{}, true
	}
	cfg, err := s.links.Config()
	if err != nil {
		return config.RuntimeConfig{}, problem(http.StatusServiceUnavailable,
			"the API has no valid integrations.yaml to render links from; its logs say why"), false
	}
	return cfg, openapi.Problem{}, true
}

// window is the time range from and to ask for: both or neither, to after
// from.
func window(from, to *time.Time) (links.Window, *openapi.Problem) {
	switch {
	case from == nil && to == nil:
		return links.Window{}, nil
	case from == nil || to == nil:
		p := problem(http.StatusBadRequest, "from and to: give both, or neither for the last hour")
		return links.Window{}, &p
	case !to.After(*from):
		p := problem(http.StatusBadRequest, "to must be after from")
		return links.Window{}, &p
	}
	return links.Window{From: *from, To: *to}, nil
}

func linksOut(in []links.Link) openapi.LinkList {
	out := openapi.LinkList{Items: make([]openapi.Link, len(in))}
	for i, l := range in {
		out.Items[i] = openapi.Link{Name: l.Name, Kind: openapi.LinkKind(l.Kind), Url: l.URL}
		if l.ApplicationName != "" {
			name := l.ApplicationName
			out.Items[i].ApplicationName = &name
		}
	}
	return out
}

// ListDashboards serves GET /api/v1/dashboards.
func (s *Server) ListDashboards(_ context.Context, req openapi.ListDashboardsRequestObject) (openapi.ListDashboardsResponseObject, error) {
	w, bad := window(req.Params.From, req.Params.To)
	if bad != nil {
		return openapi.ListDashboards400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(*bad)}, nil
	}
	cfg, p, ok := s.linkConfig()
	if !ok {
		return openapi.ListDashboards503ApplicationProblemPlusJSONResponse{LinksUnavailableApplicationProblemPlusJSONResponse: openapi.LinksUnavailableApplicationProblemPlusJSONResponse(p)}, nil
	}
	return openapi.ListDashboards200JSONResponse(linksOut(links.Dashboards(cfg, w))), nil
}

// GetApplicationLinks serves GET /api/v1/applications/{id}/links.
func (s *Server) GetApplicationLinks(ctx context.Context, req openapi.GetApplicationLinksRequestObject) (openapi.GetApplicationLinksResponseObject, error) {
	if s.telemetry == nil {
		return nil, errNoTelemetry
	}
	w, bad := window(req.Params.From, req.Params.To)
	if bad != nil {
		return openapi.GetApplicationLinks400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(*bad)}, nil
	}
	t, err := s.telemetry.ApplicationTelemetry(ctx, req.Id)
	if p, ok := failure(err, "application"); ok && p.Status == http.StatusNotFound {
		return openapi.GetApplicationLinks404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
	}
	if err != nil {
		return nil, err
	}
	cfg, p, ok := s.linkConfig()
	if !ok {
		return openapi.GetApplicationLinks503ApplicationProblemPlusJSONResponse{LinksUnavailableApplicationProblemPlusJSONResponse: openapi.LinksUnavailableApplicationProblemPlusJSONResponse(p)}, nil
	}
	scope := links.Scope{Environment: t.EnvironmentSlug, Applications: t.ApplicationNames}
	return openapi.GetApplicationLinks200JSONResponse(linksOut(links.For(cfg, scope, w))), nil
}

// GetDatabaseTargetLinks serves GET /api/v1/database-targets/{id}/links.
func (s *Server) GetDatabaseTargetLinks(ctx context.Context, req openapi.GetDatabaseTargetLinksRequestObject) (openapi.GetDatabaseTargetLinksResponseObject, error) {
	if s.telemetry == nil {
		return nil, errNoTelemetry
	}
	w, bad := window(req.Params.From, req.Params.To)
	if bad != nil {
		return openapi.GetDatabaseTargetLinks400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(*bad)}, nil
	}
	t, err := s.telemetry.DatabaseTargetTelemetry(ctx, req.Id)
	if p, ok := failure(err, "database target"); ok && p.Status == http.StatusNotFound {
		return openapi.GetDatabaseTargetLinks404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
	}
	if err != nil {
		return nil, err
	}
	cfg, p, ok := s.linkConfig()
	if !ok {
		return openapi.GetDatabaseTargetLinks503ApplicationProblemPlusJSONResponse{LinksUnavailableApplicationProblemPlusJSONResponse: openapi.LinksUnavailableApplicationProblemPlusJSONResponse(p)}, nil
	}
	scope := links.Scope{Environment: t.EnvironmentSlug, Target: t.TargetName}
	return openapi.GetDatabaseTargetLinks200JSONResponse(linksOut(links.For(cfg, scope, w))), nil
}
