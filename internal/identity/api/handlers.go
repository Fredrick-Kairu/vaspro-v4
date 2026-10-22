package api

import (
	"context"
	"errors"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
)

func ptr[T any](v T) *T { return &v }

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

func (h *Handlers) CreatePasswordReset(ctx context.Context, request CreatePasswordResetRequestObject) (CreatePasswordResetResponseObject, error) {
	body := request.Body
	if err := h.app.RequestPasswordReset(ctx, body.Email); err != nil {
		return nil, toProblem(err)
	}
	message := "If that address can be registered, a password code was sent to it."
	return CreatePasswordReset202JSONResponse{Message: &message}, nil
}

func (h *Handlers) CreatePasswordResetConfirmation(ctx context.Context, request CreatePasswordResetConfirmationRequestObject) (CreatePasswordResetConfirmationResponseObject, error) {
	body := request.Body
	if err := h.app.ConfirmPasswordReset(ctx, body.Email, body.Code, body.NewPassword); err != nil {
		return nil, toProblem(err)
	}
	status := "password_set"
	return CreatePasswordResetConfirmation200JSONResponse{Status: &status}, nil
}

func (h *Handlers) CreateSession(ctx context.Context, request CreateSessionRequestObject) (CreateSessionResponseObject, error) {
	body := request.Body
	info := clientInfoFromContext(ctx)
	result, err := h.app.SignIn(ctx, body.Email, body.Password, info.IP, info.UserAgent)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateSession200JSONResponse(signInResult(result)), nil
}

func (h *Handlers) CreateTotpChallengeVerification(ctx context.Context, request CreateTotpChallengeVerificationRequestObject) (CreateTotpChallengeVerificationResponseObject, error) {
	body := request.Body
	info := clientInfoFromContext(ctx)
	session, err := h.app.CompleteTOTPChallenge(ctx, request.Challenge, body.Code, info.IP, info.UserAgent)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateTotpChallengeVerification200JSONResponse(signInResult(&app.SignInResult{Session: session})), nil
}

func (h *Handlers) CreateTotpEnrollment(ctx context.Context, request CreateTotpEnrollmentRequestObject) (CreateTotpEnrollmentResponseObject, error) {
	enrollment, err := h.app.EnrollTOTP(ctx, sessionUserIDFromContext(ctx))
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateTotpEnrollment200JSONResponse{
		Url:           &enrollment.URL,
		RecoveryCodes: &enrollment.RecoveryCodes,
	}, nil
}

func (h *Handlers) CreateTotpEnrollmentConfirmation(ctx context.Context, request CreateTotpEnrollmentConfirmationRequestObject) (CreateTotpEnrollmentConfirmationResponseObject, error) {
	body := request.Body
	if err := h.app.ConfirmTOTPEnrollment(ctx, sessionUserIDFromContext(ctx), body.Code); err != nil {
		return nil, toProblem(err)
	}
	status := "confirmed"
	return CreateTotpEnrollmentConfirmation200JSONResponse{Status: &status}, nil
}

// signInResult renders an app.SignInResult as the API's SignInResult
// shape: Challenge set, or SessionId+ExpiresAt set — never both.
func signInResult(r *app.SignInResult) SignInResult {
	if r.Challenge != "" {
		return SignInResult{Challenge: &r.Challenge}
	}
	return SignInResult{
		SessionId: ptr(r.Session.ID.String()),
		ExpiresAt: &r.Session.ExpiresAt,
	}
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
	case errors.Is(err, app.ErrBadCredentials):
		return problem.New(problem.AUTH4010, "invalid email or password")
	case errors.Is(err, app.ErrTwoFactorInvalid):
		return problem.New(problem.AUTH4011, "the two-factor code is invalid or has expired")
	default:
		return err
	}
}
