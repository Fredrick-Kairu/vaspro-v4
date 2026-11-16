package api

import (
	"context"
	"encoding/base64"
	"errors"
	"unicode/utf8"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/app"
)

// Handlers implements StrictServerInterface over app.Service.
type Handlers struct {
	app *app.Service
}

// NewHandlers builds Handlers.
func NewHandlers(a *app.Service) *Handlers { return &Handlers{app: a} }

var _ StrictServerInterface = (*Handlers)(nil)

func (h *Handlers) ListWebhookEndpoints(ctx context.Context, request ListWebhookEndpointsRequestObject) (ListWebhookEndpointsResponseObject, error) {
	eps, err := h.app.ListEndpoints(ctx, callerFromContext(ctx), parseOrNil(request.TenantId))
	if err != nil {
		return nil, toProblem(err)
	}
	out := ListWebhookEndpoints200JSONResponse{Endpoints: make([]Endpoint, len(eps))}
	for i, ep := range eps {
		out.Endpoints[i] = endpointResponse(ep)
	}
	return out, nil
}

func (h *Handlers) CreateWebhookEndpoint(ctx context.Context, request CreateWebhookEndpointRequestObject) (CreateWebhookEndpointResponseObject, error) {
	b := request.Body
	in := app.NewEndpoint{URL: b.Url, Events: b.Events}
	if b.Description != nil {
		in.Description = *b.Description
	}
	created, err := h.app.CreateEndpoint(ctx, callerFromContext(ctx), sessionFromContext(ctx), parseOrNil(request.TenantId), in)
	if err != nil {
		return nil, toProblem(err)
	}
	ep := endpointResponse(created.Endpoint)
	secret := created.Secret
	return CreateWebhookEndpoint201JSONResponse{
		Id: ep.Id, Url: ep.Url, Description: ep.Description, Status: EndpointWithSecretStatus(ep.Status), Events: ep.Events,
		VerifiedAt: ep.VerifiedAt, FailingSince: ep.FailingSince, DisabledAt: ep.DisabledAt,
		DisabledReason: (*EndpointWithSecretDisabledReason)(ep.DisabledReason),
		CreatedAt:      ep.CreatedAt, UpdatedAt: ep.UpdatedAt, Secret: &secret,
	}, nil
}

func (h *Handlers) GetWebhookEndpoint(ctx context.Context, request GetWebhookEndpointRequestObject) (GetWebhookEndpointResponseObject, error) {
	ep, err := h.app.GetEndpoint(ctx, callerFromContext(ctx), parseOrNil(request.TenantId), parseOrNil(request.EndpointId))
	if err != nil {
		return nil, toProblem(err)
	}
	return GetWebhookEndpoint200JSONResponse(endpointResponse(ep)), nil
}

func (h *Handlers) UpdateWebhookEndpoint(ctx context.Context, request UpdateWebhookEndpointRequestObject) (UpdateWebhookEndpointResponseObject, error) {
	b := request.Body
	ch := app.EndpointChanges{URL: b.Url, Description: b.Description}
	if b.Events != nil {
		ch.Events = *b.Events
	}
	ep, err := h.app.UpdateEndpoint(ctx, callerFromContext(ctx), sessionFromContext(ctx), parseOrNil(request.TenantId), parseOrNil(request.EndpointId), ch)
	if err != nil {
		return nil, toProblem(err)
	}
	return UpdateWebhookEndpoint200JSONResponse(endpointResponse(ep)), nil
}

func (h *Handlers) DeleteWebhookEndpoint(ctx context.Context, request DeleteWebhookEndpointRequestObject) (DeleteWebhookEndpointResponseObject, error) {
	if err := h.app.DeleteEndpoint(ctx, callerFromContext(ctx), parseOrNil(request.TenantId), parseOrNil(request.EndpointId)); err != nil {
		return nil, toProblem(err)
	}
	return DeleteWebhookEndpoint204Response{}, nil
}

func (h *Handlers) VerifyWebhookEndpoint(ctx context.Context, request VerifyWebhookEndpointRequestObject) (VerifyWebhookEndpointResponseObject, error) {
	ep, err := h.app.VerifyEndpoint(ctx, callerFromContext(ctx), parseOrNil(request.TenantId), parseOrNil(request.EndpointId))
	if err != nil {
		return nil, toProblem(err)
	}
	return VerifyWebhookEndpoint200JSONResponse(endpointResponse(ep)), nil
}

func (h *Handlers) RotateWebhookEndpointSecret(ctx context.Context, request RotateWebhookEndpointSecretRequestObject) (RotateWebhookEndpointSecretResponseObject, error) {
	rot, err := h.app.RotateEndpointSecret(ctx, callerFromContext(ctx), sessionFromContext(ctx), parseOrNil(request.TenantId), parseOrNil(request.EndpointId))
	if err != nil {
		return nil, toProblem(err)
	}
	secret := rot.Secret
	return RotateWebhookEndpointSecret201JSONResponse{EndpointId: request.EndpointId, Secret: &secret, PreviousValidUntil: rot.PreviousValidUntil}, nil
}

func (h *Handlers) DisableWebhookEndpoint(ctx context.Context, request DisableWebhookEndpointRequestObject) (DisableWebhookEndpointResponseObject, error) {
	ep, err := h.app.DisableEndpoint(ctx, callerFromContext(ctx), parseOrNil(request.TenantId), parseOrNil(request.EndpointId))
	if err != nil {
		return nil, toProblem(err)
	}
	return DisableWebhookEndpoint200JSONResponse(endpointResponse(ep)), nil
}

func (h *Handlers) SendWebhookTestEvent(ctx context.Context, request SendWebhookTestEventRequestObject) (SendWebhookTestEventResponseObject, error) {
	d, err := h.app.SendTestEvent(ctx, callerFromContext(ctx), parseOrNil(request.TenantId), parseOrNil(request.EndpointId))
	if err != nil {
		return nil, toProblem(err)
	}
	return SendWebhookTestEvent202JSONResponse(deliveryResponse(d)), nil
}

func (h *Handlers) ListWebhookDeliveries(ctx context.Context, request ListWebhookDeliveriesRequestObject) (ListWebhookDeliveriesResponseObject, error) {
	p := request.Params
	var f app.DeliveryFilter
	if p.EndpointId != nil {
		id, err := uuid.Parse(*p.EndpointId)
		if err != nil {
			return nil, problem.New(problem.VAL4000, "endpoint_id must be an endpoint's ID")
		}
		f.EndpointID = &id
	}
	if p.EventId != nil {
		id, err := uuid.Parse(*p.EventId)
		if err != nil {
			return nil, problem.New(problem.VAL4000, "event_id must be an event's ID")
		}
		f.EventID = &id
	}
	if p.Status != nil {
		f.Status = string(*p.Status)
	}
	if p.EventType != nil {
		f.EventType = *p.EventType
	}
	cursor, limit := "", 0
	if p.Cursor != nil {
		cursor = *p.Cursor
	}
	if p.Limit != nil {
		limit = *p.Limit
	}
	ds, next, err := h.app.ListDeliveries(ctx, callerFromContext(ctx), parseOrNil(request.TenantId), f, cursor, limit)
	if err != nil {
		return nil, toProblem(err)
	}
	out := ListWebhookDeliveries200JSONResponse{Deliveries: make([]Delivery, len(ds))}
	for i, d := range ds {
		out.Deliveries[i] = deliveryResponse(d)
	}
	if next != "" {
		out.NextCursor = &next
	}
	return out, nil
}

func (h *Handlers) GetWebhookDelivery(ctx context.Context, request GetWebhookDeliveryRequestObject) (GetWebhookDeliveryResponseObject, error) {
	d, err := h.app.GetDelivery(ctx, callerFromContext(ctx), parseOrNil(request.TenantId), parseOrNil(request.DeliveryId))
	if err != nil {
		return nil, toProblem(err)
	}
	base := deliveryResponse(d.Delivery)
	out := GetWebhookDelivery200JSONResponse{
		Id: base.Id, EndpointId: base.EndpointId, EventId: base.EventId, EventType: base.EventType, OccurredAt: base.OccurredAt,
		Status: DeliveryDetailStatus(base.Status), DeadReason: (*DeliveryDetailDeadReason)(base.DeadReason),
		Attempts: base.Attempts, NextAttemptAt: base.NextAttemptAt, ReplayOf: base.ReplayOf,
		CreatedAt: base.CreatedAt, CompletedAt: base.CompletedAt, Body: string(d.Body),
		AttemptLog: make([]DeliveryAttempt, len(d.AttemptLog)),
	}
	for i, a := range d.AttemptLog {
		out.AttemptLog[i] = attemptResponse(a)
	}
	return out, nil
}

func (h *Handlers) ReplayWebhookDelivery(ctx context.Context, request ReplayWebhookDeliveryRequestObject) (ReplayWebhookDeliveryResponseObject, error) {
	d, err := h.app.ReplayDelivery(ctx, callerFromContext(ctx), parseOrNil(request.TenantId), parseOrNil(request.DeliveryId))
	if err != nil {
		return nil, toProblem(err)
	}
	return ReplayWebhookDelivery202JSONResponse(deliveryResponse(d)), nil
}

func (h *Handlers) GetDailyActivity(ctx context.Context, request GetDailyActivityRequestObject) (GetDailyActivityResponseObject, error) {
	rep, err := h.app.DailyActivity(ctx, callerFromContext(ctx), parseOrNil(request.TenantId),
		request.Params.From.String(), request.Params.To.String())
	if err != nil {
		return nil, toProblem(err)
	}
	out := GetDailyActivity200JSONResponse{TimeZone: rep.TimeZone, Days: make([]DailyActivity, len(rep.Days))}
	for i, d := range rep.Days {
		out.Days[i] = DailyActivity{
			Date: openapi_types.Date{Time: d.Day}, Currency: d.Currency, CreditedMinor: d.CreditedMinor, Topups: d.Topups,
			TokenPurchasesMinor: d.TokenPurchasesMinor, UsageMinor: d.UsageMinor, UsageEvents: d.UsageEvents,
		}
	}
	return out, nil
}

func endpointResponse(ep app.Endpoint) Endpoint {
	out := Endpoint{
		Id: ep.ID.String(), Url: ep.URL, Status: EndpointStatus(ep.Status), Events: ep.Events,
		VerifiedAt: ep.VerifiedAt, FailingSince: ep.FailingSince, DisabledAt: ep.DisabledAt,
		CreatedAt: ep.CreatedAt, UpdatedAt: ep.UpdatedAt, Description: nonEmpty(ep.Description),
	}
	if out.Events == nil {
		out.Events = []string{}
	}
	if ep.DisabledReason != "" {
		r := EndpointDisabledReason(ep.DisabledReason)
		out.DisabledReason = &r
	}
	return out
}

func deliveryResponse(d app.Delivery) Delivery {
	out := Delivery{
		Id: d.ID.String(), EndpointId: d.EndpointID.String(), EventId: d.EventID.String(), EventType: d.EventType,
		OccurredAt: d.OccurredAt, Status: DeliveryStatus(d.Status), Attempts: d.Attempts,
		NextAttemptAt: d.NextAttemptAt, CreatedAt: d.CreatedAt, CompletedAt: d.CompletedAt,
	}
	if d.DeadReason != "" {
		r := DeliveryDeadReason(d.DeadReason)
		out.DeadReason = &r
	}
	if d.ReplayOf != nil {
		s := d.ReplayOf.String()
		out.ReplayOf = &s
	}
	return out
}

func attemptResponse(a app.Attempt) DeliveryAttempt {
	out := DeliveryAttempt{
		Attempt: a.Attempt, StartedAt: a.StartedAt, DurationMs: a.DurationMs, Outcome: DeliveryAttemptOutcome(a.Outcome),
		RequestHeaders: a.RequestHeaders, ResponseStatus: a.ResponseStatus, ResponseTruncated: a.ResponseTruncated,
		Error: nonEmpty(a.Error),
	}
	if a.ResponseHeaders != nil {
		out.ResponseHeaders = &a.ResponseHeaders
	}
	if a.ResponseBody != nil {
		body, enc := string(a.ResponseBody), DeliveryAttemptResponseBodyEncoding("utf8")
		if !utf8.Valid(a.ResponseBody) {
			body, enc = base64.StdEncoding.EncodeToString(a.ResponseBody), "base64"
		}
		out.ResponseBody, out.ResponseBodyEncoding = &body, &enc
	}
	return out
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// toProblem maps the app's errors to catalogue codes; anything else
// passes through to respondProblem as SYS-9000.
func toProblem(err error) error {
	var vf *app.VerificationFailure
	switch {
	case errors.Is(err, app.ErrNotFound):
		return problem.New(problem.VAL4003, "not found")
	case errors.Is(err, app.ErrPermissionDenied):
		return problem.New(problem.AUTH4024, "your role in this account doesn't allow this")
	case errors.Is(err, app.ErrStepUpRequired):
		return problem.New(problem.AUTH4012, "verify again (step-up) first")
	case errors.Is(err, app.ErrURLNotAllowed):
		return problem.New(problem.VAL4019, err.Error())
	case errors.As(err, &vf):
		return problem.New(problem.VAL4018, vf.Reason)
	case errors.Is(err, app.ErrValidation):
		return problem.New(problem.VAL4000, err.Error())
	}
	return err
}

// parseOrNil reads a path ID; one that isn't a UUID names nothing, and
// uuid.Nil goes on so the access check answers it as not found.
func parseOrNil(s string) uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil
	}
	return id
}
