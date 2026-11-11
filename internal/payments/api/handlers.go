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

func (h *Handlers) ReceiveC2bValidation(ctx context.Context, request ReceiveC2bValidationRequestObject) (ReceiveC2bValidationResponseObject, error) {
	cr, ok := callbackFromContext(ctx)
	if !ok {
		return nil, errors.New("api: the callback body wasn't kept (keepCallbackBody isn't wired)")
	}
	accept, code, err := h.app.HandleC2BValidation(ctx, parseOrNil(request.RailAccountId), request.Token, cr.clientIP, cr.body)
	if err != nil {
		return nil, toProblem(err)
	}
	if !accept {
		return ReceiveC2bValidation200JSONResponse{"ResultCode": code, "ResultDesc": "Rejected"}, nil
	}
	return ReceiveC2bValidation200JSONResponse{"ResultCode": "0", "ResultDesc": "Accepted"}, nil
}

func (h *Handlers) ReceiveC2bConfirmation(ctx context.Context, request ReceiveC2bConfirmationRequestObject) (ReceiveC2bConfirmationResponseObject, error) {
	cr, ok := callbackFromContext(ctx)
	if !ok {
		return nil, errors.New("api: the callback body wasn't kept (keepCallbackBody isn't wired)")
	}
	if err := h.app.HandleC2BConfirmation(ctx, parseOrNil(request.RailAccountId), request.Token, cr.clientIP, cr.body); err != nil {
		return nil, toProblem(err)
	}
	return ReceiveC2bConfirmation200JSONResponse{"ResultCode": "0", "ResultDesc": "Accepted"}, nil
}

func (h *Handlers) ListSuspense(ctx context.Context, request ListSuspenseRequestObject) (ListSuspenseResponseObject, error) {
	cursor, limit := "", 0
	if request.Params.Cursor != nil {
		cursor = *request.Params.Cursor
	}
	if request.Params.Limit != nil {
		limit = *request.Params.Limit
	}
	notices, next, err := h.app.ListSuspense(ctx, callerFromContext(ctx), cursor, limit)
	if err != nil {
		return nil, toProblem(err)
	}
	out := ListSuspense200JSONResponse{Notices: make([]SuspenseNotice, len(notices))}
	for i, n := range notices {
		out.Notices[i] = noticeResponse(n)
	}
	if next != "" {
		out.NextCursor = &next
	}
	return out, nil
}

func (h *Handlers) MatchCollectionNotice(ctx context.Context, request MatchCollectionNoticeRequestObject) (MatchCollectionNoticeResponseObject, error) {
	n, err := h.app.MatchNotice(ctx, callerFromContext(ctx), sessionFromContext(ctx), parseOrNil(request.NoticeId),
		request.Body.AccountNumber, request.Body.Reason)
	if err != nil {
		return nil, toProblem(err)
	}
	return MatchCollectionNotice200JSONResponse(noticeResponse(n)), nil
}

func noticeResponse(n app.Notice) SuspenseNotice {
	out := SuspenseNotice{
		Id: n.ID.String(), Receipt: n.Receipt, AmountMinor: n.AmountMinor, Currency: n.Currency,
		PaidAt: n.PaidAt, Status: SuspenseNoticeStatus(n.Status),
		BillRef: nonEmpty(n.BillRef), PayerName: nonEmpty(n.PayerName), PayerPhone: nonEmpty(n.PayerPhone),
	}
	if n.TenantID != nil {
		id := n.TenantID.String()
		out.TenantId = &id
	}
	return out
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
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
	case errors.Is(err, app.ErrStepUpRequired):
		return problem.New(problem.AUTH4012, "verify again (step-up) first")
	case errors.Is(err, app.ErrNoticeDecided):
		return problem.New(problem.VAL4014, "the payment was already matched")
	case errors.Is(err, app.ErrSuspenseNotPosted):
		return problem.New(problem.PAY6507, "the payment isn't in suspense in the ledger yet; try again shortly")
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
