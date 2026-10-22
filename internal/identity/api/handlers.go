package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"

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

func (h *Handlers) CreateStepUpTotpVerification(ctx context.Context, request CreateStepUpTotpVerificationRequestObject) (CreateStepUpTotpVerificationResponseObject, error) {
	body := request.Body
	if err := h.app.VerifyStepUpTOTP(ctx, sessionUserIDFromContext(ctx), sessionIDFromContext(ctx), body.Code); err != nil {
		return nil, toProblem(err)
	}
	status := "stepped_up"
	return CreateStepUpTotpVerification200JSONResponse{Status: &status}, nil
}

func (h *Handlers) CreateStepUpEmail(ctx context.Context, request CreateStepUpEmailRequestObject) (CreateStepUpEmailResponseObject, error) {
	if err := h.app.RequestStepUpEmail(ctx, sessionUserIDFromContext(ctx)); err != nil {
		return nil, toProblem(err)
	}
	message := "Check the account's email for a step-up code."
	return CreateStepUpEmail202JSONResponse{Message: &message}, nil
}

func (h *Handlers) CreateStepUpEmailConfirmation(ctx context.Context, request CreateStepUpEmailConfirmationRequestObject) (CreateStepUpEmailConfirmationResponseObject, error) {
	body := request.Body
	if err := h.app.ConfirmStepUpEmail(ctx, sessionUserIDFromContext(ctx), sessionIDFromContext(ctx), body.Code); err != nil {
		return nil, toProblem(err)
	}
	status := "stepped_up"
	return CreateStepUpEmailConfirmation200JSONResponse{Status: &status}, nil
}

func (h *Handlers) CreatePasskeyRegistration(ctx context.Context, request CreatePasskeyRegistrationRequestObject) (CreatePasskeyRegistrationResponseObject, error) {
	reg, err := h.app.BeginPasskeyRegistration(ctx, sessionUserIDFromContext(ctx))
	if err != nil {
		return nil, toProblem(err)
	}
	options, err := toOptionsMap(reg.Options)
	if err != nil {
		return nil, err
	}
	return CreatePasskeyRegistration200JSONResponse{Session: reg.SessionToken, Options: options}, nil
}

func (h *Handlers) CreatePasskeyRegistrationConfirmation(ctx context.Context, request CreatePasskeyRegistrationConfirmationRequestObject) (CreatePasskeyRegistrationConfirmationResponseObject, error) {
	body := request.Body
	credential, err := json.Marshal(body.Credential)
	if err != nil {
		return nil, toProblem(err)
	}
	name := ""
	if body.Name != nil {
		name = *body.Name
	}
	summary, err := h.app.FinishPasskeyRegistration(ctx, sessionUserIDFromContext(ctx), body.Session, name, credential)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreatePasskeyRegistrationConfirmation200JSONResponse(passkeySummaryResponse(summary)), nil
}

func (h *Handlers) ListPasskeys(ctx context.Context, request ListPasskeysRequestObject) (ListPasskeysResponseObject, error) {
	passkeys, err := h.app.ListPasskeys(ctx, sessionUserIDFromContext(ctx))
	if err != nil {
		return nil, toProblem(err)
	}
	summaries := make([]PasskeySummary, len(passkeys))
	for i, p := range passkeys {
		summaries[i] = passkeySummaryResponse(&p)
	}
	return ListPasskeys200JSONResponse{Passkeys: &summaries}, nil
}

func (h *Handlers) UpdatePasskey(ctx context.Context, request UpdatePasskeyRequestObject) (UpdatePasskeyResponseObject, error) {
	passkeyID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	summary, err := h.app.RenamePasskey(ctx, sessionUserIDFromContext(ctx), passkeyID, request.Body.Name)
	if err != nil {
		return nil, toProblem(err)
	}
	return UpdatePasskey200JSONResponse(passkeySummaryResponse(summary)), nil
}

func (h *Handlers) DeletePasskey(ctx context.Context, request DeletePasskeyRequestObject) (DeletePasskeyResponseObject, error) {
	passkeyID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	if err := h.app.DeletePasskey(ctx, sessionUserIDFromContext(ctx), passkeyID); err != nil {
		return nil, toProblem(err)
	}
	return DeletePasskey204Response{}, nil
}

func (h *Handlers) CreatePasskeyChallenge(ctx context.Context, request CreatePasskeyChallengeRequestObject) (CreatePasskeyChallengeResponseObject, error) {
	challenge, err := h.app.BeginPasskeyLogin(ctx, request.Body.Email)
	if err != nil {
		return nil, toProblem(err)
	}
	options, err := toOptionsMap(challenge.Options)
	if err != nil {
		return nil, err
	}
	return CreatePasskeyChallenge200JSONResponse{Session: challenge.SessionToken, Options: options}, nil
}

func (h *Handlers) CreatePasskeyChallengeVerification(ctx context.Context, request CreatePasskeyChallengeVerificationRequestObject) (CreatePasskeyChallengeVerificationResponseObject, error) {
	credential, err := json.Marshal(request.Body.Credential)
	if err != nil {
		return nil, toProblem(err)
	}
	info := clientInfoFromContext(ctx)
	session, err := h.app.CompletePasskeyLogin(ctx, request.Challenge, credential, info.IP, info.UserAgent)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreatePasskeyChallengeVerification200JSONResponse(signInResult(&app.SignInResult{Session: session})), nil
}

func (h *Handlers) ListSessions(ctx context.Context, request ListSessionsRequestObject) (ListSessionsResponseObject, error) {
	sessions, err := h.app.ListSessions(ctx, sessionUserIDFromContext(ctx), sessionIDFromContext(ctx))
	if err != nil {
		return nil, toProblem(err)
	}
	summaries := make([]SessionSummary, len(sessions))
	for i, s := range sessions {
		summaries[i] = sessionSummaryResponse(&s)
	}
	return ListSessions200JSONResponse{Sessions: &summaries}, nil
}

func (h *Handlers) DeleteSession(ctx context.Context, request DeleteSessionRequestObject) (DeleteSessionResponseObject, error) {
	sessionID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	if err := h.app.RevokeSession(ctx, sessionUserIDFromContext(ctx), sessionID); err != nil {
		return nil, toProblem(err)
	}
	return DeleteSession204Response{}, nil
}

func (h *Handlers) DeleteSessions(ctx context.Context, request DeleteSessionsRequestObject) (DeleteSessionsResponseObject, error) {
	if _, err := h.app.SignOutEverywhere(ctx, sessionUserIDFromContext(ctx)); err != nil {
		return nil, toProblem(err)
	}
	return DeleteSessions204Response{}, nil
}

// sessionSummaryResponse renders an app.SessionSummary as the API's
// SessionSummary shape.
func sessionSummaryResponse(s *app.SessionSummary) SessionSummary {
	id := s.ID.String()
	summary := SessionSummary{
		Id:         &id,
		CreatedAt:  &s.CreatedAt,
		LastSeenAt: &s.LastSeenAt,
		ExpiresAt:  &s.ExpiresAt,
		Current:    &s.Current,
	}
	if s.IP != "" {
		summary.Ip = &s.IP
	}
	if s.UserAgent != "" {
		summary.UserAgent = &s.UserAgent
	}
	return summary
}

// toOptionsMap re-marshals v (a go-webauthn protocol.CredentialCreation
// or protocol.CredentialAssertion) into the generic
// map[string]interface{} shape PasskeyCeremonyOptions.Options carries —
// the wire format is the library's own JSON encoding, not ours to
// model field by field.
func toOptionsMap(v any) (map[string]interface{}, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// passkeySummaryResponse renders an app.PasskeySummary as the API's
// PasskeySummary shape.
func passkeySummaryResponse(s *app.PasskeySummary) PasskeySummary {
	id := s.ID.String()
	summary := PasskeySummary{Id: &id, CreatedAt: &s.CreatedAt}
	if s.Name != "" {
		summary.Name = &s.Name
	}
	if s.LastUsedAt != nil {
		summary.LastUsedAt = s.LastUsedAt
	}
	return summary
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
	case errors.Is(err, app.ErrNotFound):
		return problem.New(problem.VAL4003, "not found")
	default:
		return err
	}
}
