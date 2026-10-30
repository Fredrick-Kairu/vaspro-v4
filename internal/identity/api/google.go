package api

import (
	"context"
	"encoding/json"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
)

// Sign in with Google (task 1.16, D-22).

func (h *Handlers) CreateGoogleAuthorization(ctx context.Context, request CreateGoogleAuthorizationRequestObject) (CreateGoogleAuthorizationResponseObject, error) {
	auth, err := h.app.StartGoogleSignIn(ctx)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateGoogleAuthorization200JSONResponse{AuthorizationUrl: auth.URL, State: auth.State}, nil
}

func (h *Handlers) CreateGoogleSession(ctx context.Context, request CreateGoogleSessionRequestObject) (CreateGoogleSessionResponseObject, error) {
	info := clientInfoFromContext(ctx)
	result, err := h.app.CompleteGoogleSignIn(ctx, request.Body.Code, request.Body.State, info.IP)
	if err != nil {
		return nil, toProblem(err)
	}
	resp := CreateGoogleSession200JSONResponse{}
	if result.Challenge != "" {
		resp.Challenge = &result.Challenge
		factors := make([]GoogleSignInResultSecondFactors, len(result.SecondFactors))
		for i, f := range result.SecondFactors {
			factors[i] = GoogleSignInResultSecondFactors(f)
		}
		resp.SecondFactors = &factors
	}
	if result.EnrollmentToken != "" {
		resp.EnrollmentToken = &result.EnrollmentToken
	}
	return resp, nil
}

func (h *Handlers) CreateGooglePasskeyChallenge(ctx context.Context, request CreateGooglePasskeyChallengeRequestObject) (CreateGooglePasskeyChallengeResponseObject, error) {
	challenge, err := h.app.BeginGooglePasskeySecondFactor(ctx, request.Body.Challenge)
	if err != nil {
		return nil, toProblem(err)
	}
	options, err := toOptionsMap(challenge.Options)
	if err != nil {
		return nil, err
	}
	return CreateGooglePasskeyChallenge200JSONResponse{Session: challenge.SessionToken, Options: options}, nil
}

func (h *Handlers) CreateGooglePasskeyChallengeVerification(ctx context.Context, request CreateGooglePasskeyChallengeVerificationRequestObject) (CreateGooglePasskeyChallengeVerificationResponseObject, error) {
	body := request.Body
	credential, err := json.Marshal(body.Credential)
	if err != nil {
		return nil, toProblem(err)
	}
	info := clientInfoFromContext(ctx)
	session, err := h.app.CompleteGooglePasskeySecondFactor(ctx, body.Challenge, body.Session, credential, info.IP, info.UserAgent)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateGooglePasskeyChallengeVerification200JSONResponse(signInResult(&app.SignInResult{Session: session})), nil
}

func (h *Handlers) CreateGoogleTotpEnrollment(ctx context.Context, request CreateGoogleTotpEnrollmentRequestObject) (CreateGoogleTotpEnrollmentResponseObject, error) {
	enrollment, err := h.app.EnrollGoogleTOTP(ctx, request.Body.EnrollmentToken)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateGoogleTotpEnrollment200JSONResponse{Url: &enrollment.URL, RecoveryCodes: &enrollment.RecoveryCodes}, nil
}

func (h *Handlers) CreateGoogleTotpEnrollmentConfirmation(ctx context.Context, request CreateGoogleTotpEnrollmentConfirmationRequestObject) (CreateGoogleTotpEnrollmentConfirmationResponseObject, error) {
	info := clientInfoFromContext(ctx)
	session, err := h.app.ConfirmGoogleTOTPEnrollment(ctx, request.Body.EnrollmentToken, request.Body.Code, info.IP, info.UserAgent)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateGoogleTotpEnrollmentConfirmation200JSONResponse(signInResult(&app.SignInResult{Session: session})), nil
}

func (h *Handlers) CreateGooglePasskeyEnrollment(ctx context.Context, request CreateGooglePasskeyEnrollmentRequestObject) (CreateGooglePasskeyEnrollmentResponseObject, error) {
	reg, err := h.app.BeginGooglePasskeyEnrollment(ctx, request.Body.EnrollmentToken)
	if err != nil {
		return nil, toProblem(err)
	}
	options, err := toOptionsMap(reg.Options)
	if err != nil {
		return nil, err
	}
	return CreateGooglePasskeyEnrollment200JSONResponse{Session: reg.SessionToken, Options: options}, nil
}

func (h *Handlers) CreateGooglePasskeyEnrollmentConfirmation(ctx context.Context, request CreateGooglePasskeyEnrollmentConfirmationRequestObject) (CreateGooglePasskeyEnrollmentConfirmationResponseObject, error) {
	body := request.Body
	credential, err := json.Marshal(body.Credential)
	if err != nil {
		return nil, toProblem(err)
	}
	name := ""
	if body.Name != nil {
		name = *body.Name
	}
	info := clientInfoFromContext(ctx)
	session, err := h.app.FinishGooglePasskeyEnrollment(ctx, body.EnrollmentToken, body.Session, name, credential, info.IP, info.UserAgent)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateGooglePasskeyEnrollmentConfirmation200JSONResponse(signInResult(&app.SignInResult{Session: session})), nil
}

func (h *Handlers) CreateGoogleLinkAuthorization(ctx context.Context, request CreateGoogleLinkAuthorizationRequestObject) (CreateGoogleLinkAuthorizationResponseObject, error) {
	auth, err := h.app.StartGoogleLink(ctx, sessionUserIDFromContext(ctx))
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateGoogleLinkAuthorization200JSONResponse{AuthorizationUrl: auth.URL, State: auth.State}, nil
}

func (h *Handlers) CreateGoogleLink(ctx context.Context, request CreateGoogleLinkRequestObject) (CreateGoogleLinkResponseObject, error) {
	info := clientInfoFromContext(ctx)
	linked, err := h.app.CompleteGoogleLink(ctx, sessionUserIDFromContext(ctx), request.Body.Code, request.Body.State, info.IP)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateGoogleLink200JSONResponse(linkedIdentityResponse(linked)), nil
}

func (h *Handlers) GetGoogleLink(ctx context.Context, request GetGoogleLinkRequestObject) (GetGoogleLinkResponseObject, error) {
	linked, err := h.app.GetGoogleLink(ctx, sessionUserIDFromContext(ctx))
	if err != nil {
		return nil, toProblem(err)
	}
	return GetGoogleLink200JSONResponse(linkedIdentityResponse(linked)), nil
}

func (h *Handlers) DeleteGoogleLink(ctx context.Context, request DeleteGoogleLinkRequestObject) (DeleteGoogleLinkResponseObject, error) {
	info := clientInfoFromContext(ctx)
	if err := h.app.UnlinkGoogle(ctx, sessionUserIDFromContext(ctx), info.IP); err != nil {
		return nil, toProblem(err)
	}
	return DeleteGoogleLink204Response{}, nil
}

func linkedIdentityResponse(li *app.LinkedIdentity) LinkedIdentity {
	provider := LinkedIdentityProvider(li.Provider)
	resp := LinkedIdentity{Provider: &provider, Email: &li.Email, LinkedAt: &li.LinkedAt, LastUsedAt: li.LastUsedAt}
	if li.HostedDomain != "" {
		resp.HostedDomain = &li.HostedDomain
	}
	return resp
}
