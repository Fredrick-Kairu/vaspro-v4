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
