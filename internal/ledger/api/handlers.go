package api

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

// Handlers implements StrictServerInterface over app.Service.
type Handlers struct {
	app *app.Service
}

// NewHandlers builds Handlers.
func NewHandlers(a *app.Service) *Handlers { return &Handlers{app: a} }

var _ StrictServerInterface = (*Handlers)(nil)

func (h *Handlers) CreateTokenPrice(ctx context.Context, request CreateTokenPriceRequestObject) (CreateTokenPriceResponseObject, error) {
	b := request.Body
	p := domain.NewPrice{
		ServiceCode: b.ServiceCode, Currency: b.Currency, MinorPerToken: b.MinorPerToken,
		From: b.EffectiveFrom, To: b.EffectiveTo,
	}
	if b.TenantId != nil {
		tenant, err := uuid.Parse(*b.TenantId)
		if err != nil {
			return nil, problem.New(problem.VAL4000, "tenant_id must be a UUID")
		}
		p.TenantID = &tenant
	}
	if b.MinTokens != nil {
		p.MinTokens = *b.MinTokens
	}
	if b.DealReference != nil {
		p.DealReference = *b.DealReference
	}
	price, err := h.app.CreateTokenPrice(ctx, callerFromContext(ctx), p)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateTokenPrice201JSONResponse(tokenPriceResponse(price)), nil
}

func (h *Handlers) ListTokenPrices(ctx context.Context, request ListTokenPricesRequestObject) (ListTokenPricesResponseObject, error) {
	params := request.Params
	var f app.PriceFilter
	if params.TenantId != nil {
		tenant, err := uuid.Parse(*params.TenantId)
		if err != nil {
			return nil, problem.New(problem.VAL4000, "tenant_id must be a UUID")
		}
		f.TenantID = &tenant
	}
	if params.ServiceCode != nil {
		f.ServiceCode = *params.ServiceCode
	}
	if params.Status != nil {
		f.Status = string(*params.Status)
	}
	if params.Cursor != nil {
		f.Cursor = *params.Cursor
	}
	if params.Limit != nil {
		f.Limit = *params.Limit
	}
	page, err := h.app.ListTokenPrices(ctx, callerFromContext(ctx), f)
	if err != nil {
		return nil, toProblem(err)
	}
	resp := ListTokenPrices200JSONResponse{Prices: make([]TokenPrice, len(page.Prices))}
	for i, p := range page.Prices {
		resp.Prices[i] = tokenPriceResponse(p)
	}
	if page.NextCursor != "" {
		resp.NextCursor = &page.NextCursor
	}
	return resp, nil
}

func (h *Handlers) ApproveTokenPrice(ctx context.Context, request ApproveTokenPriceRequestObject) (ApproveTokenPriceResponseObject, error) {
	price, err := h.decide(ctx, request.PriceId, h.app.ApproveTokenPrice)
	if err != nil {
		return nil, err
	}
	return ApproveTokenPrice200JSONResponse(tokenPriceResponse(price)), nil
}

func (h *Handlers) WithdrawTokenPrice(ctx context.Context, request WithdrawTokenPriceRequestObject) (WithdrawTokenPriceResponseObject, error) {
	price, err := h.decide(ctx, request.PriceId, h.app.WithdrawTokenPrice)
	if err != nil {
		return nil, err
	}
	return WithdrawTokenPrice200JSONResponse(tokenPriceResponse(price)), nil
}

// decide runs an approval or withdrawal. A price ID that isn't a UUID
// names no price: uuid.Nil goes on, so the staff and step-up checks
// still come first and only then is the price not found.
func (h *Handlers) decide(ctx context.Context, priceID string,
	act func(ctx context.Context, staffID, sessionID, priceID uuid.UUID) (app.TokenPrice, error)) (app.TokenPrice, error) {
	id, err := uuid.Parse(priceID)
	if err != nil {
		id = uuid.Nil
	}
	price, err := act(ctx, callerFromContext(ctx), sessionFromContext(ctx), id)
	if err != nil {
		return app.TokenPrice{}, toProblem(err)
	}
	return price, nil
}

// toProblem maps the app's errors to catalogue codes; anything else
// passes through to respondProblem as SYS-9000.
func toProblem(err error) error {
	switch {
	case errors.Is(err, app.ErrNotStaff):
		return problem.New(problem.AUTH4025, "only VASPRO staff can do this")
	case errors.Is(err, app.ErrStepUpRequired):
		return problem.New(problem.AUTH4012, "verify again (step-up) before changing a price")
	case errors.Is(err, app.ErrValidation):
		return problem.New(problem.VAL4000, err.Error())
	case errors.Is(err, app.ErrNotFound):
		return problem.New(problem.VAL4003, "no such price")
	case errors.Is(err, app.ErrMakerCannotApprove):
		return problem.New(problem.VAL4013, "another staff member must approve a price you entered")
	case errors.Is(err, app.ErrPriceDecided):
		return problem.New(problem.VAL4014, err.Error())
	case errors.Is(err, app.ErrPriceOverlap):
		return problem.New(problem.VAL4012, "an active price already covers this client, service, currency and tier then")
	}
	return err
}

func tokenPriceResponse(p app.TokenPrice) TokenPrice {
	out := TokenPrice{
		Id: p.ID.String(), ServiceCode: p.ServiceCode, Currency: p.Currency,
		MinTokens: p.MinTokens.String(), MinorPerToken: p.MinorPerToken.String(),
		Status: TokenPriceStatus(p.Status), EffectiveFrom: p.From, EffectiveTo: p.To, CreatedBy: p.CreatedBy.String(),
	}
	if p.TenantID != nil {
		s := p.TenantID.String()
		out.TenantId = &s
	}
	if p.DealReference != "" {
		out.DealReference = &p.DealReference
	}
	if p.ApprovedBy != nil {
		s := p.ApprovedBy.String()
		out.ApprovedBy = &s
	}
	return out
}
