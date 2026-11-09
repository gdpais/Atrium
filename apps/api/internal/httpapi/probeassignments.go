package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"atrium/apps/api/internal/audit"
	"atrium/apps/api/internal/inventory"
	"atrium/apps/api/internal/openapi"
)

// ProbeAssignments reads, changes and renders probe assignments:
// *inventory.Service.
type ProbeAssignments interface {
	ListProbeAssignments(ctx context.Context, filter inventory.ProbeAssignmentFilter, page inventory.Page) (inventory.ProbeAssignmentPage, error)
	GetProbeAssignment(ctx context.Context, id uuid.UUID) (inventory.ProbeAssignment, error)
	CreateProbeAssignment(ctx context.Context, caller inventory.Caller, in inventory.NewProbeAssignment) (inventory.ProbeAssignment, error)
	UpdateProbeAssignment(ctx context.Context, caller inventory.Caller, id uuid.UUID, change inventory.ProbeAssignmentChange) (inventory.ProbeAssignment, error)
	ApproveProbeAssignment(ctx context.Context, caller inventory.Caller, id uuid.UUID) (inventory.ProbeAssignment, error)
	RejectProbeAssignment(ctx context.Context, caller inventory.Caller, id uuid.UUID, reason string) (inventory.ProbeAssignment, error)
	DeleteProbeAssignment(ctx context.Context, caller inventory.Caller, id uuid.UUID) error
	RenderProbeAssignments(ctx context.Context) ([]byte, error)
}

var _ ProbeAssignments = (*inventory.Service)(nil)

// errNoProbeAssignments is a probe assignment operation on a Server built
// without them.
var errNoProbeAssignments = errors.New("httpapi: no probe assignments service")

// assignmentCaller is who sent a change to an assignment: the guard has
// checked their session and role.
func (s *Server) assignmentCaller(ctx context.Context) (inventory.Caller, error) {
	if s.probeAssignments == nil {
		return inventory.Caller{}, errNoProbeAssignments
	}
	info := requestFrom(ctx)
	if info.principal == nil {
		return inventory.Caller{}, errNoProbeAssignments
	}
	return inventory.Caller{
		Actor:     audit.UserActor(info.principal.User.ID, info.principal.User.Email),
		RequestID: info.client.RequestID,
	}, nil
}

func probeAssignmentOut(a inventory.ProbeAssignment) openapi.ProbeAssignment {
	out := openapi.ProbeAssignment{
		Id: a.ID, DatabaseTargetId: a.DatabaseTargetID, ProbeDefinitionId: a.ProbeDefinitionID,
		IntervalSeconds: a.IntervalSeconds, Enabled: a.Enabled, State: openapi.AssignmentState(a.State),
		StateDetail: a.StateDetail, CollectorId: a.CollectorID, ApprovedAt: a.ApprovedAt,
		ApprovedBy: a.ApprovedBy, CreatedBy: a.CreatedBy, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
	if a.TimeoutMS != 0 {
		out.TimeoutMs = &a.TimeoutMS
	}
	if a.MaxRows != 0 {
		out.MaxRows = &a.MaxRows
	}
	if a.PullRequestURL != "" {
		out.PullRequestUrl = &a.PullRequestURL
	}
	return out
}

// ListProbeAssignments serves GET /api/v1/probe-assignments.
func (s *Server) ListProbeAssignments(ctx context.Context, req openapi.ListProbeAssignmentsRequestObject) (openapi.ListProbeAssignmentsResponseObject, error) {
	if s.probeAssignments == nil {
		return nil, errNoProbeAssignments
	}
	var filter inventory.ProbeAssignmentFilter
	if req.Params.DatabaseTargetId != nil {
		filter.DatabaseTargetID = *req.Params.DatabaseTargetId
	}
	if req.Params.State != nil {
		filter.State = string(*req.Params.State)
	}
	assignments, err := s.probeAssignments.ListProbeAssignments(ctx, filter, page(req.Params.Limit, req.Params.Cursor))
	if p, ok := failure(err, "probe assignment"); ok && p.Status == http.StatusBadRequest {
		return openapi.ListProbeAssignments400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(p)}, nil
	}
	if err != nil {
		return nil, err
	}
	out := openapi.ListProbeAssignments200JSONResponse{Items: make([]openapi.ProbeAssignment, len(assignments.Items)), NextCursor: nextCursor(assignments.NextCursor)}
	for i, a := range assignments.Items {
		out.Items[i] = probeAssignmentOut(a)
	}
	return out, nil
}

// GetProbeAssignment serves GET /api/v1/probe-assignments/{id}.
func (s *Server) GetProbeAssignment(ctx context.Context, req openapi.GetProbeAssignmentRequestObject) (openapi.GetProbeAssignmentResponseObject, error) {
	if s.probeAssignments == nil {
		return nil, errNoProbeAssignments
	}
	a, err := s.probeAssignments.GetProbeAssignment(ctx, req.Id)
	if p, ok := failure(err, "probe assignment"); ok && p.Status == http.StatusNotFound {
		return openapi.GetProbeAssignment404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
	}
	if err != nil {
		return nil, err
	}
	return openapi.GetProbeAssignment200JSONResponse(probeAssignmentOut(a)), nil
}

// CreateProbeAssignment serves POST /api/v1/probe-assignments.
func (s *Server) CreateProbeAssignment(ctx context.Context, req openapi.CreateProbeAssignmentRequestObject) (openapi.CreateProbeAssignmentResponseObject, error) {
	caller, err := s.assignmentCaller(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	in := inventory.NewProbeAssignment{
		DatabaseTargetID: b.DatabaseTargetId, ProbeDefinitionID: b.ProbeDefinitionId,
		Enabled: b.Enabled,
	}
	if b.IntervalSeconds != nil {
		in.IntervalSeconds = *b.IntervalSeconds
	}
	if b.TimeoutMs != nil {
		in.TimeoutMS = *b.TimeoutMs
	}
	if b.MaxRows != nil {
		in.MaxRows = *b.MaxRows
	}
	a, err := s.probeAssignments.CreateProbeAssignment(ctx, caller, in)
	if p, ok := failure(err, "probe assignment"); ok {
		switch p.Status {
		case http.StatusBadRequest:
			return openapi.CreateProbeAssignment400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(p)}, nil
		case http.StatusConflict:
			return openapi.CreateProbeAssignment409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: conflict(p)}, nil
		case http.StatusUnprocessableEntity:
			return openapi.CreateProbeAssignment422ApplicationProblemPlusJSONResponse{NoOwnerApplicationProblemPlusJSONResponse: noOwner(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.CreateProbeAssignment201JSONResponse{
		Body:    probeAssignmentOut(a),
		Headers: openapi.CreateProbeAssignment201ResponseHeaders{Location: fmt.Sprintf("/api/v1/probe-assignments/%s", a.ID)},
	}, nil
}

// UpdateProbeAssignment serves PATCH /api/v1/probe-assignments/{id}.
func (s *Server) UpdateProbeAssignment(ctx context.Context, req openapi.UpdateProbeAssignmentRequestObject) (openapi.UpdateProbeAssignmentResponseObject, error) {
	caller, err := s.assignmentCaller(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	change := inventory.ProbeAssignmentChange{
		ProbeDefinitionID: b.ProbeDefinitionId, IntervalSeconds: b.IntervalSeconds,
		TimeoutMS: b.TimeoutMs, MaxRows: b.MaxRows, Enabled: b.Enabled,
	}
	a, err := s.probeAssignments.UpdateProbeAssignment(ctx, caller, req.Id, change)
	if p, ok := failure(err, "probe assignment"); ok {
		switch p.Status {
		case http.StatusBadRequest:
			return openapi.UpdateProbeAssignment400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(p)}, nil
		case http.StatusNotFound:
			return openapi.UpdateProbeAssignment404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
		case http.StatusConflict:
			return openapi.UpdateProbeAssignment409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: conflict(p)}, nil
		case http.StatusUnprocessableEntity:
			return openapi.UpdateProbeAssignment422ApplicationProblemPlusJSONResponse{NoOwnerApplicationProblemPlusJSONResponse: noOwner(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.UpdateProbeAssignment200JSONResponse(probeAssignmentOut(a)), nil
}

// ApproveProbeAssignment serves POST /api/v1/probe-assignments/{id}/approve.
func (s *Server) ApproveProbeAssignment(ctx context.Context, req openapi.ApproveProbeAssignmentRequestObject) (openapi.ApproveProbeAssignmentResponseObject, error) {
	caller, err := s.assignmentCaller(ctx)
	if err != nil {
		return nil, err
	}
	a, err := s.probeAssignments.ApproveProbeAssignment(ctx, caller, req.Id)
	if p, ok := failure(err, "probe assignment"); ok {
		switch p.Status {
		case http.StatusNotFound:
			return openapi.ApproveProbeAssignment404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
		case http.StatusConflict:
			return openapi.ApproveProbeAssignment409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: conflict(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.ApproveProbeAssignment200JSONResponse(probeAssignmentOut(a)), nil
}

// RejectProbeAssignment serves POST /api/v1/probe-assignments/{id}/reject.
func (s *Server) RejectProbeAssignment(ctx context.Context, req openapi.RejectProbeAssignmentRequestObject) (openapi.RejectProbeAssignmentResponseObject, error) {
	caller, err := s.assignmentCaller(ctx)
	if err != nil {
		return nil, err
	}
	a, err := s.probeAssignments.RejectProbeAssignment(ctx, caller, req.Id, deref(req.Body.Reason))
	if p, ok := failure(err, "probe assignment"); ok {
		switch p.Status {
		case http.StatusBadRequest:
			return openapi.RejectProbeAssignment400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(p)}, nil
		case http.StatusNotFound:
			return openapi.RejectProbeAssignment404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
		case http.StatusConflict:
			return openapi.RejectProbeAssignment409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: conflict(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.RejectProbeAssignment200JSONResponse(probeAssignmentOut(a)), nil
}

// DeleteProbeAssignment serves DELETE /api/v1/probe-assignments/{id}.
func (s *Server) DeleteProbeAssignment(ctx context.Context, req openapi.DeleteProbeAssignmentRequestObject) (openapi.DeleteProbeAssignmentResponseObject, error) {
	caller, err := s.assignmentCaller(ctx)
	if err != nil {
		return nil, err
	}
	err = s.probeAssignments.DeleteProbeAssignment(ctx, caller, req.Id)
	if p, ok := failure(err, "probe assignment"); ok {
		switch p.Status {
		case http.StatusNotFound:
			return openapi.DeleteProbeAssignment404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(p)}, nil
		case http.StatusConflict:
			return openapi.DeleteProbeAssignment409ApplicationProblemPlusJSONResponse{ConflictApplicationProblemPlusJSONResponse: conflict(p)}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return openapi.DeleteProbeAssignment204Response{}, nil
}

// GetRenderedProbeAssignments serves GET /api/v1/probe-assignments/rendered.
func (s *Server) GetRenderedProbeAssignments(ctx context.Context, _ openapi.GetRenderedProbeAssignmentsRequestObject) (openapi.GetRenderedProbeAssignmentsResponseObject, error) {
	if s.probeAssignments == nil {
		return nil, errNoProbeAssignments
	}
	file, err := s.probeAssignments.RenderProbeAssignments(ctx)
	if err != nil {
		return nil, err
	}
	return openapi.GetRenderedProbeAssignments200ApplicationyamlResponse{Body: bytes.NewReader(file), ContentLength: int64(len(file))}, nil
}
