package api

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/app"
)

// Handlers implements StrictServerInterface over app.Service.
type Handlers struct {
	app *app.Service
}

// NewHandlers builds Handlers.
func NewHandlers(a *app.Service) *Handlers { return &Handlers{app: a} }

var _ StrictServerInterface = (*Handlers)(nil)

func (h *Handlers) GetTopupDetails(ctx context.Context, request GetTopupDetailsRequestObject) (GetTopupDetailsResponseObject, error) {
	d, err := h.app.TopupDetails(ctx, callerFromContext(ctx), parseOrNil(request.TenantId))
	if err != nil {
		return nil, toProblem(err)
	}
	return GetTopupDetails200JSONResponse{
		Rail: d.Rail, PaybillNumber: d.PaybillNumber, PaybillName: d.PaybillName,
		AccountNumber: d.AccountNumber, Currency: d.Currency,
	}, nil
}

func (h *Handlers) CreateTopup(ctx context.Context, request CreateTopupRequestObject) (CreateTopupResponseObject, error) {
	top, err := h.app.StartTopup(ctx, callerFromContext(ctx), parseOrNil(request.TenantId), request.Params.IdempotencyKey,
		request.Body.AmountMinor, request.Body.PayerPhone)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateTopup201JSONResponse(topupResponse(top)), nil
}

func (h *Handlers) GetTopup(ctx context.Context, request GetTopupRequestObject) (GetTopupResponseObject, error) {
	top, err := h.app.GetTopup(ctx, callerFromContext(ctx), parseOrNil(request.TenantId), parseOrNil(request.TopupId))
	if err != nil {
		return nil, toProblem(err)
	}
	return GetTopup200JSONResponse(topupResponse(top)), nil
}

func (h *Handlers) ReceiveStkCallback(ctx context.Context, request ReceiveStkCallbackRequestObject) (ReceiveStkCallbackResponseObject, error) {
	cr, ok := callbackFromContext(ctx)
	if !ok {
		return nil, errors.New("api: the callback body wasn't kept (keepCallbackBody isn't wired)")
	}
	if err := h.app.HandleSTKCallback(ctx, parseOrNil(request.RailAccountId), request.Token, cr.clientIP, cr.body); err != nil {
		return nil, toProblem(err)
	}
	return ReceiveStkCallback200JSONResponse{"ResultCode": 0, "ResultDesc": "Accepted"}, nil
}

func topupResponse(t app.Topup) Topup {
	out := Topup{
		Id: t.ID.String(), Status: TopupStatus(t.Status), AmountMinor: t.AmountMinor, Currency: t.Currency,
		PayerPhone: t.PayerPhone, AccountReference: t.AccountReference, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt,
		CompletedAt: t.CompletedAt,
	}
	if t.Receipt != "" {
		out.Receipt = &t.Receipt
	}
	if t.ErrorCode != "" {
		out.ErrorCode = &t.ErrorCode
	}
	return out
}

// toProblem maps the app's errors to catalogue codes; anything else
// passes through to respondProblem as SYS-9000.
func toProblem(err error) error {
	switch {
	case errors.Is(err, app.ErrNotFound):
		return problem.New(problem.VAL4003, "not found")
	case errors.Is(err, app.ErrPermissionDenied):
		return problem.New(problem.AUTH4024, "your role in this account doesn't allow this")
	case errors.Is(err, app.ErrNotStaff):
		return problem.New(problem.AUTH4025, "only VASPRO staff can do this")
	case errors.Is(err, app.ErrTopupUnavailable):
		return problem.New(problem.PAY6507, "top-ups aren't available for this account right now")
	case errors.Is(err, app.ErrBadCallback):
		return problem.New(problem.AUTH4013, "the callback isn't from the rail")
	case errors.Is(err, app.ErrIdempotencyConflict):
		return problem.New(problem.VAL4001, err.Error())
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
