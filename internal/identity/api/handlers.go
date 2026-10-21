package api

import (
	"context"
	"errors"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
)

// Handlers implements StrictServerInterface over app.Service. Every
// method returns (nil, err) on failure — the strict handler never tries
// to use the typed Response objects when err is non-nil (it calls
// StrictHTTPServerOptions.ResponseErrorHandlerFunc instead, wired in
// Routes to translate *problem.Problem into application/problem+json).
type Handlers struct {
	app *app.Service
}

// NewHandlers builds a Handlers over a.
func NewHandlers(a *app.Service) *Handlers {
	return &Handlers{app: a}
}

func (h *Handlers) CreateSignup(ctx context.Context, request CreateSignupRequestObject) (CreateSignupResponseObject, error) {
	body := request.Body
	if err := h.app.SignUp(ctx, body.Email, body.FullName, body.PhoneE164); err != nil {
		return nil, toProblem(err)
	}
	message := "If that address can be registered, a verification code was sent to it."
	return CreateSignup202JSONResponse{Message: &message}, nil
}

func (h *Handlers) CreateEmailVerification(ctx context.Context, request CreateEmailVerificationRequestObject) (CreateEmailVerificationResponseObject, error) {
	body := request.Body
	if err := h.app.VerifyEmail(ctx, body.Email, body.Code); err != nil {
		return nil, toProblem(err)
	}
	status := "verified"
	return CreateEmailVerification200JSONResponse{Status: &status}, nil
}

// toProblem maps app/'s plain-error sentinels to a *problem.Problem;
// anything else is passed through unchanged for respondProblem's
// SYS-9000 fallback (routes.go).
func toProblem(err error) error {
	switch {
	case errors.Is(err, app.ErrValidation):
		return problem.New(problem.VAL4000, err.Error())
	case errors.Is(err, app.ErrInvalidOrExpiredCode):
		return problem.New(problem.VAL4002, "the code is invalid or has expired")
	default:
		return err
	}
}
