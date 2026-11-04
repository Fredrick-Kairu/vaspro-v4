package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

// Sign in with Google (task 1.16, D-22). Google proves the first
// factor only: a session always needs VASPRO's own TOTP or passkey
// next, and Google never satisfies step-up (it never sets step_up_at).
// Accounts are matched on Google's sub, never on email, and are never
// linked silently.

// ErrExternalSignInFailed covers every failed Google flow alike: an
// unknown, reused or mismatched state, a failed code exchange, and an
// ID token with a bad signature, wrong issuer or audience, past expiry
// or someone else's nonce; also an unverified Google email at sign-up.
// api/ maps it to AUTH-4018.
var ErrExternalSignInFailed = errors.New("app: external sign-in failed")

// ErrSSORequired means the email's domain has an enabled SSO connection
// (identity.sso_connections), so Google sign-in is refused. api/ maps
// it to AUTH-4019.
var ErrSSORequired = errors.New("app: this domain signs in with single sign-on")

// ErrAccountExistsLinkRequired means a Google account not yet linked to
// anyone reported the email of an existing user. VASPRO never links
// silently: that user must sign in the usual way and link Google from
// security settings. api/ maps it to AUTH-4020.
var ErrAccountExistsLinkRequired = errors.New("app: account exists; sign in and link Google")

// ErrIdentityLinkedElsewhere means the Google account is already linked
// to a different user. api/ maps it to AUTH-4021.
var ErrIdentityLinkedElsewhere = errors.New("app: Google account linked to another user")

// ErrOnlySignInMethod means unlinking Google would leave the user no
// way to sign in (no password and no passkey). api/ maps it to
// AUTH-4022.
var ErrOnlySignInMethod = errors.New("app: Google is the only way to sign in")

const (
	providerGoogle = "google"

	googleFlowTTL       = 10 * time.Minute
	googleEnrollmentTTL = 15 * time.Minute

	googleIntentSignIn = "sign_in"
	googleIntentLink   = "link"

	// The values GoogleSignInResult.SecondFactors lists.
	secondFactorTOTP    = "totp"
	secondFactorPasskey = "passkey"

	ssoDomainLookupRole = "identity_sso_domain_lookup"
)

// googleFlow is what StartGoogle* keeps in Redis under the flow's
// state, until the callback takes it (once).
type googleFlow struct {
	Intent   string    `json:"intent"`
	Nonce    string    `json:"nonce"`
	Verifier string    `json:"verifier"`
	UserID   uuid.UUID `json:"user_id,omitempty"`
}

func googleFlowKey(state string) string { return "identity:google_flow:" + state }

// GoogleAuthorization is where to send the browser, and the state the
// dashboard must check comes back unchanged.
type GoogleAuthorization struct {
	URL   string
	State string
}

// GoogleSignInResult is CompleteGoogleSignIn's outcome: never a
// session. Either Challenge is set (finish with TOTP through
// CompleteTOTPChallenge, or a passkey through the Google passkey
// second factor), or EnrollmentToken is (the user has no second factor
// yet and must enrol one before any session exists).
type GoogleSignInResult struct {
	Challenge       string
	SecondFactors   []string
	EnrollmentToken string
}

// LinkedIdentity is a linked Google account as shown to its owner.
type LinkedIdentity struct {
	Provider     string
	Email        string
	HostedDomain string
	LinkedAt     time.Time
	LastUsedAt   *time.Time
}

// randomToken returns 32 random bytes, base64url: 43 characters, also
// a valid PKCE verifier (RFC 7636 §4.1).
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("app: generating random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// StartGoogleSignIn begins a Google sign-in (or sign-up).
func (s *Service) StartGoogleSignIn(ctx context.Context) (*GoogleAuthorization, error) {
	return s.startGoogleFlow(ctx, googleFlow{Intent: googleIntentSignIn})
}

// StartGoogleLink begins linking a Google account to userID, who is
// signed in and has stepped up (api/ enforces both).
func (s *Service) StartGoogleLink(ctx context.Context, userID uuid.UUID) (*GoogleAuthorization, error) {
	return s.startGoogleFlow(ctx, googleFlow{Intent: googleIntentLink, UserID: userID})
}

func (s *Service) startGoogleFlow(ctx context.Context, flow googleFlow) (*GoogleAuthorization, error) {
	if s.oidc == nil {
		return nil, ErrNotFound
	}
	state, err := randomToken()
	if err != nil {
		return nil, err
	}
	if flow.Nonce, err = randomToken(); err != nil {
		return nil, err
	}
	if flow.Verifier, err = randomToken(); err != nil {
		return nil, err
	}
	authURL, err := s.oidc.AuthCodeURL(ctx, state, flow.Nonce, flow.Verifier)
	if err != nil {
		return nil, fmt.Errorf("app: building Google authorization URL: %w", err)
	}
	payload, err := json.Marshal(flow)
	if err != nil {
		return nil, fmt.Errorf("app: encoding Google flow: %w", err)
	}
	if err := s.cache.Set(ctx, googleFlowKey(state), payload, googleFlowTTL).Err(); err != nil {
		return nil, fmt.Errorf("app: storing Google flow: %w", err)
	}
	return &GoogleAuthorization{URL: authURL, State: state}, nil
}

// takeGoogleFlow removes and returns the flow stored under state. A
// state is usable once: GETDEL means a replayed callback finds nothing.
func (s *Service) takeGoogleFlow(ctx context.Context, state, intent string) (*googleFlow, error) {
	if state == "" {
		return nil, ErrExternalSignInFailed
	}
	raw, err := s.cache.GetDel(ctx, googleFlowKey(state)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrExternalSignInFailed
	}
	if err != nil {
		return nil, fmt.Errorf("app: reading Google flow: %w", err)
	}
	var flow googleFlow
	if err := json.Unmarshal(raw, &flow); err != nil || flow.Intent != intent {
		return nil, ErrExternalSignInFailed
	}
	return &flow, nil
}

// exchangeGoogleCode finishes the provider side of a flow: code
// exchange with the PKCE verifier, ID token verification, nonce check.
func (s *Service) exchangeGoogleCode(ctx context.Context, flow *googleFlow, code string) (sub, email string, emailVerified bool, name, hostedDomain string, err error) {
	claims, err := s.oidc.Exchange(ctx, code, flow.Verifier, flow.Nonce)
	if err != nil {
		return "", "", false, "", "", fmt.Errorf("%w: %v", ErrExternalSignInFailed, err)
	}
	if claims.Subject == "" {
		return "", "", false, "", "", fmt.Errorf("%w: ID token has no sub", ErrExternalSignInFailed)
	}
	email, err = domain.NormalizeEmail(claims.Email)
	if err != nil {
		return "", "", false, "", "", fmt.Errorf("%w: ID token email: %v", ErrExternalSignInFailed, err)
	}
	return claims.Subject, email, claims.EmailVerified, claims.Name, claims.HostedDomain, nil
}

// ssoRequired reports whether email's domain has an enabled SSO
// connection. sso_connections has FORCE ROW LEVEL SECURITY, so the
// cross-tenant lookup runs as the narrow identity_sso_domain_lookup
// role (migration 20261030120000).
func (s *Service) ssoRequired(ctx context.Context, email string) (bool, error) {
	_, emailDomain, ok := strings.Cut(email, "@")
	if !ok || emailDomain == "" {
		return false, nil
	}
	found := false
	err := s.pool.WithRoleTx(ctx, ssoDomainLookupRole, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.New(tx).GetEnabledSSOConnectionByDomain(ctx, emailDomain)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("app: looking up SSO domain: %w", err)
	}
	return found, nil
}

// secondFactors lists the second factors userID can use now.
func secondFactors(ctx context.Context, q *store.Queries, userID uuid.UUID) ([]string, error) {
	var factors []string
	totpRow, err := q.GetUserTOTP(ctx, userID)
	switch {
	case err == nil:
		if totpRow.ConfirmedAt.Valid {
			factors = append(factors, secondFactorTOTP)
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("app: looking up TOTP enrollment: %w", err)
	}
	passkeys, err := q.ListPasskeysByUserID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("app: listing passkeys: %w", err)
	}
	if len(passkeys) > 0 {
		factors = append(factors, secondFactorPasskey)
	}
	return factors, nil
}

// CompleteGoogleSignIn handles the provider's redirect back (code and
// state). It never returns a session: see GoogleSignInResult.
func (s *Service) CompleteGoogleSignIn(ctx context.Context, code, state string, ip net.IP) (*GoogleSignInResult, error) {
	if s.oidc == nil {
		return nil, ErrNotFound
	}
	flow, err := s.takeGoogleFlow(ctx, state, googleIntentSignIn)
	if err != nil {
		return nil, err
	}
	sub, email, emailVerified, name, hostedDomain, err := s.exchangeGoogleCode(ctx, flow, code)
	if err != nil {
		return nil, err
	}

	if blocked, err := s.ssoRequired(ctx, email); err != nil {
		return nil, err
	} else if blocked {
		return nil, ErrSSORequired
	}

	q := store.New(s.pool)
	linked, err := q.GetUserIdentityBySubject(ctx, store.GetUserIdentityBySubjectParams{Provider: providerGoogle, Subject: sub})
	switch {
	case err == nil:
		return s.continueLinkedGoogleSignIn(ctx, linked)
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("app: looking up Google identity: %w", err)
	}

	if _, err := q.GetUserByEmail(ctx, email); err == nil {
		return nil, ErrAccountExistsLinkRequired
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("app: looking up user: %w", err)
	}
	if !emailVerified {
		return nil, fmt.Errorf("%w: Google email not verified", ErrExternalSignInFailed)
	}
	userID, err := s.signUpWithGoogle(ctx, sub, email, name, hostedDomain, ip)
	if err != nil {
		return nil, err
	}
	token, err := s.issueGoogleEnrollment(userID)
	if err != nil {
		return nil, err
	}
	return &GoogleSignInResult{EnrollmentToken: token}, nil
}

// continueLinkedGoogleSignIn handles a Google account already linked to
// a user: a second-factor challenge, or an enrolment token when the
// user has no second factor yet.
func (s *Service) continueLinkedGoogleSignIn(ctx context.Context, linked store.IdentityUserIdentity) (*GoogleSignInResult, error) {
	q := store.New(s.pool)
	user, err := q.GetUserByID(ctx, linked.UserID)
	if err != nil {
		return nil, fmt.Errorf("app: looking up user: %w", err)
	}
	// The user's own email may differ from Google's; its domain's SSO
	// rule applies too.
	if blocked, err := s.ssoRequired(ctx, user.Email); err != nil {
		return nil, err
	} else if blocked {
		return nil, ErrSSORequired
	}

	var result *GoogleSignInResult
	locked := false
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		if locked, err = s.tooManyRecentFailures(ctx, q, user.ID); err != nil || locked {
			return err
		}
		if err := q.TouchUserIdentity(ctx, linked.ID); err != nil {
			return fmt.Errorf("app: touching Google identity: %w", err)
		}
		factors, err := secondFactors(ctx, q, user.ID)
		if err != nil {
			return err
		}
		if len(factors) == 0 {
			token, err := s.issueGoogleEnrollment(user.ID)
			if err != nil {
				return err
			}
			result = &GoogleSignInResult{EnrollmentToken: token}
			return nil
		}
		challenge, err := s.issueChallenge(user.ID, authMethodGoogle)
		if err != nil {
			return err
		}
		result = &GoogleSignInResult{Challenge: challenge, SecondFactors: factors}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if locked {
		return nil, ErrBadCredentials
	}
	return result, nil
}

// signUpWithGoogle creates a verified user with no password and links
// the Google account, publishing the same events as an email sign-up
// followed by verification.
func (s *Service) signUpWithGoogle(ctx context.Context, sub, email, name, hostedDomain string, ip net.IP) (uuid.UUID, error) {
	fullName := strings.TrimSpace(name)
	if fullName == "" {
		fullName, _, _ = strings.Cut(email, "@")
	}
	userID, err := ids.New()
	if err != nil {
		return uuid.Nil, fmt.Errorf("app: generating user id: %w", err)
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.CreateGoogleUser(ctx, store.CreateGoogleUserParams{ID: userID, Email: email, FullName: fullName}); err != nil {
			return err
		}
		if err := linkGoogleIdentity(ctx, q, userID, sub, email, hostedDomain); err != nil {
			return err
		}
		if err := recordSecurityEvent(ctx, q, userID, "identity_linked", ip, map[string]any{"provider": providerGoogle, "via": "sign_up"}); err != nil {
			return err
		}
		for _, eventType := range []string{"user.registered", "user.email_verified"} {
			eventID, err := ids.New()
			if err != nil {
				return fmt.Errorf("app: generating event id: %w", err)
			}
			env, err := platformevents.New(eventID, eventType, 1, uuid.Nil, s.clk.Now(), "", map[string]any{
				"user_id": userID,
				"email":   email,
			})
			if err != nil {
				return fmt.Errorf("app: building %s event: %w", eventType, err)
			}
			if err := outbox.Write(ctx, tx, "user", userID, env); err != nil {
				return err
			}
		}
		return nil
	})
	if isUniqueViolation(err) {
		// A concurrent sign-up claimed the email or the Google account
		// first.
		return uuid.Nil, ErrAccountExistsLinkRequired
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("app: signing up with Google: %w", err)
	}
	return userID, nil
}

func linkGoogleIdentity(ctx context.Context, q *store.Queries, userID uuid.UUID, sub, email, hostedDomain string) error {
	id, err := ids.New()
	if err != nil {
		return fmt.Errorf("app: generating identity id: %w", err)
	}
	_, err = q.CreateUserIdentity(ctx, store.CreateUserIdentityParams{
		ID:           id,
		UserID:       userID,
		Provider:     providerGoogle,
		Subject:      sub,
		EmailAtLink:  email,
		HostedDomain: pgtype.Text{String: hostedDomain, Valid: hostedDomain != ""},
	})
	return err
}

// googleEnrollment is the payload of an enrolment token: proof that the
// holder just signed in with Google as UserID, who has no second factor
// yet.
type googleEnrollment struct {
	UserID    uuid.UUID `json:"user_id"`
	Purpose   string    `json:"purpose"`
	ExpiresAt time.Time `json:"expires_at"`
}

const googleEnrollmentPurpose = "google_enrollment"

func (s *Service) issueGoogleEnrollment(userID uuid.UUID) (string, error) {
	payload, err := json.Marshal(googleEnrollment{UserID: userID, Purpose: googleEnrollmentPurpose, ExpiresAt: s.clk.Now().Add(googleEnrollmentTTL)})
	if err != nil {
		return "", fmt.Errorf("app: building enrolment token: %w", err)
	}
	return signPayload(s.challengeKey, payload), nil
}

// verifyGoogleEnrollment checks the token. The purpose field keeps a
// sign-in challenge (signed with the same key) from passing as one.
func (s *Service) verifyGoogleEnrollment(token string) (uuid.UUID, error) {
	payload, ok := verifyPayload(s.challengeKey, token)
	if !ok {
		return uuid.Nil, ErrTwoFactorInvalid
	}
	var e googleEnrollment
	if err := json.Unmarshal(payload, &e); err != nil || e.Purpose != googleEnrollmentPurpose || e.UserID == uuid.Nil {
		return uuid.Nil, ErrTwoFactorInvalid
	}
	if s.clk.Now().After(e.ExpiresAt) {
		return uuid.Nil, ErrTwoFactorInvalid
	}
	return e.UserID, nil
}

// enrollmentUser verifies an enrolment token and that its user still
// has no second factor, so the token can never reset one.
func (s *Service) enrollmentUser(ctx context.Context, q *store.Queries, token string) (uuid.UUID, error) {
	userID, err := s.verifyGoogleEnrollment(token)
	if err != nil {
		return uuid.Nil, err
	}
	factors, err := secondFactors(ctx, q, userID)
	if err != nil {
		return uuid.Nil, err
	}
	if len(factors) > 0 {
		return uuid.Nil, ErrTwoFactorInvalid
	}
	return userID, nil
}

// EnrollGoogleTOTP starts TOTP enrolment for a Google sign-up.
func (s *Service) EnrollGoogleTOTP(ctx context.Context, enrollmentToken string) (*TOTPEnrollment, error) {
	userID, err := s.enrollmentUser(ctx, store.New(s.pool), enrollmentToken)
	if err != nil {
		return nil, err
	}
	return s.EnrollTOTP(ctx, userID)
}

// ConfirmGoogleTOTPEnrollment confirms the TOTP enrolment and creates
// the user's first session (auth_method google). A wrong code counts
// toward the sign-in lockout like any failed second factor.
func (s *Service) ConfirmGoogleTOTPEnrollment(ctx context.Context, enrollmentToken, code string, ip net.IP, userAgent string) (*Session, error) {
	code = strings.TrimSpace(code)
	var (
		session *Session
		failed  bool
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		userID, err := s.enrollmentUser(ctx, q, enrollmentToken)
		if errors.Is(err, ErrTwoFactorInvalid) {
			failed = true
			return nil
		}
		if err != nil {
			return err
		}
		locked, err := s.tooManyRecentFailures(ctx, q, userID)
		if err != nil {
			return err
		}
		if locked {
			failed = true
			return nil
		}
		confirmed, err := s.confirmTOTPEnrollment(ctx, q, userID, code)
		if err != nil {
			return err
		}
		if !confirmed {
			failed = true
			return s.recordSignInFailure(ctx, q, userID, ip)
		}
		session, _, err = s.createSession(ctx, q, userID, ip, userAgent, authMethodGoogle)
		return err
	})
	if err != nil {
		return nil, err
	}
	if failed {
		return nil, ErrTwoFactorInvalid
	}
	return session, nil
}

// BeginGooglePasskeyEnrollment starts passkey enrolment for a Google
// sign-up.
func (s *Service) BeginGooglePasskeyEnrollment(ctx context.Context, enrollmentToken string) (*PasskeyRegistration, error) {
	userID, err := s.enrollmentUser(ctx, store.New(s.pool), enrollmentToken)
	if err != nil {
		return nil, err
	}
	return s.BeginPasskeyRegistration(ctx, userID)
}

// FinishGooglePasskeyEnrollment stores the passkey and creates the
// user's first session (auth_method google).
func (s *Service) FinishGooglePasskeyEnrollment(ctx context.Context, enrollmentToken, ceremonyToken, name string, rawCredential []byte, ip net.IP, userAgent string) (*Session, error) {
	ceremony, err := s.verifyWebAuthnSession(ceremonyToken)
	if err != nil {
		return nil, ErrTwoFactorInvalid
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(rawCredential)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	var session *Session
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		userID, err := s.enrollmentUser(ctx, q, enrollmentToken)
		if err != nil {
			return err
		}
		if !bytes.Equal(ceremony.UserID, userID[:]) {
			return ErrTwoFactorInvalid
		}
		if _, err := s.storeNewPasskey(ctx, q, userID, ceremony, name, parsed); err != nil {
			return err
		}
		session, _, err = s.createSession(ctx, q, userID, ip, userAgent, authMethodGoogle)
		return err
	})
	if err != nil {
		return nil, err
	}
	return session, nil
}

// BeginGooglePasskeySecondFactor starts a passkey assertion as the
// second factor after Google, for the challenge CompleteGoogleSignIn
// returned.
func (s *Service) BeginGooglePasskeySecondFactor(ctx context.Context, challenge string) (*PasskeyChallenge, error) {
	userID, method, err := s.verifyChallenge(challenge)
	if err != nil {
		return nil, err
	}
	if method != authMethodGoogle {
		return nil, ErrTwoFactorInvalid
	}
	q := store.New(s.pool)
	user, err := q.GetUserByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("app: looking up user: %w", err)
	}
	passkeys, err := q.ListPasskeysByUserID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("app: listing passkeys: %w", err)
	}
	if len(passkeys) == 0 {
		return nil, ErrTwoFactorInvalid
	}
	assertion, ceremony, err := s.webauthn.BeginLogin(webauthnUser{user: user, passkeys: passkeys})
	if err != nil {
		return nil, fmt.Errorf("app: beginning passkey assertion: %w", err)
	}
	token, err := s.issueWebAuthnSession(ceremony)
	if err != nil {
		return nil, err
	}
	return &PasskeyChallenge{SessionToken: token, Options: assertion}, nil
}

// CompleteGooglePasskeySecondFactor checks the passkey assertion for
// the Google challenge and creates the session (auth_method google).
// Failures return ErrBadCredentials, as a passkey sign-in does.
func (s *Service) CompleteGooglePasskeySecondFactor(ctx context.Context, challenge, ceremonyToken string, rawCredential []byte, ip net.IP, userAgent string) (*Session, error) {
	userID, method, err := s.verifyChallenge(challenge)
	if err != nil {
		return nil, err
	}
	if method != authMethodGoogle {
		return nil, ErrTwoFactorInvalid
	}
	ceremony, err := s.verifyWebAuthnSession(ceremonyToken)
	if err != nil || !bytes.Equal(ceremony.UserID, userID[:]) {
		return nil, ErrBadCredentials
	}
	return s.finishPasskeyLogin(ctx, ceremony, userID, rawCredential, ip, userAgent, authMethodGoogle)
}

// CompleteGoogleLink links the Google account from a flow userID
// started. The flow must have been started by the same user, which
// stops an attacker's link flow being completed in a victim's session.
func (s *Service) CompleteGoogleLink(ctx context.Context, userID uuid.UUID, code, state string, ip net.IP) (*LinkedIdentity, error) {
	if s.oidc == nil {
		return nil, ErrNotFound
	}
	flow, err := s.takeGoogleFlow(ctx, state, googleIntentLink)
	if err != nil {
		return nil, err
	}
	if flow.UserID != userID {
		return nil, ErrExternalSignInFailed
	}
	sub, email, _, _, hostedDomain, err := s.exchangeGoogleCode(ctx, flow, code)
	if err != nil {
		return nil, err
	}
	if blocked, err := s.ssoRequired(ctx, email); err != nil {
		return nil, err
	} else if blocked {
		return nil, ErrSSORequired
	}

	var linked store.IdentityUserIdentity
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		existing, err := q.GetUserIdentityBySubject(ctx, store.GetUserIdentityBySubjectParams{Provider: providerGoogle, Subject: sub})
		switch {
		case err == nil && existing.UserID == userID:
			return fmt.Errorf("%w: this Google account is already linked", ErrValidation)
		case err == nil:
			return ErrIdentityLinkedElsewhere
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("app: looking up Google identity: %w", err)
		}
		if _, err := q.GetUserIdentityByUser(ctx, store.GetUserIdentityByUserParams{UserID: userID, Provider: providerGoogle}); err == nil {
			return fmt.Errorf("%w: a Google account is already linked; unlink it first", ErrValidation)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("app: looking up linked identity: %w", err)
		}
		if err := linkGoogleIdentity(ctx, q, userID, sub, email, hostedDomain); err != nil {
			if isUniqueViolation(err) {
				return ErrIdentityLinkedElsewhere
			}
			return fmt.Errorf("app: linking Google identity: %w", err)
		}
		if err := recordSecurityEvent(ctx, q, userID, "identity_linked", ip, map[string]any{"provider": providerGoogle}); err != nil {
			return err
		}
		linked, err = q.GetUserIdentityByUser(ctx, store.GetUserIdentityByUserParams{UserID: userID, Provider: providerGoogle})
		return err
	})
	if err != nil {
		return nil, err
	}
	li := linkedIdentity(linked)
	return &li, nil
}

func linkedIdentity(row store.IdentityUserIdentity) LinkedIdentity {
	li := LinkedIdentity{
		Provider:     row.Provider,
		Email:        row.EmailAtLink,
		HostedDomain: row.HostedDomain.String,
		LinkedAt:     row.LinkedAt,
	}
	if row.LastUsedAt.Valid {
		t := row.LastUsedAt.Time
		li.LastUsedAt = &t
	}
	return li
}

// GetGoogleLink returns userID's linked Google account, or ErrNotFound.
func (s *Service) GetGoogleLink(ctx context.Context, userID uuid.UUID) (*LinkedIdentity, error) {
	row, err := store.New(s.pool).GetUserIdentityByUser(ctx, store.GetUserIdentityByUserParams{UserID: userID, Provider: providerGoogle})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("app: looking up linked identity: %w", err)
	}
	li := linkedIdentity(row)
	return &li, nil
}

// UnlinkGoogle removes userID's Google link, unless Google is their
// only way to sign in (no password and no passkey).
func (s *Service) UnlinkGoogle(ctx context.Context, userID uuid.UUID, ip net.IP) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.GetUserIdentityByUser(ctx, store.GetUserIdentityByUserParams{UserID: userID, Provider: providerGoogle}); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("app: looking up linked identity: %w", err)
		}
		user, err := q.GetUserByID(ctx, userID)
		if err != nil {
			return fmt.Errorf("app: looking up user: %w", err)
		}
		passkeys, err := q.ListPasskeysByUserID(ctx, userID)
		if err != nil {
			return fmt.Errorf("app: listing passkeys: %w", err)
		}
		if !user.PasswordHash.Valid && len(passkeys) == 0 {
			return ErrOnlySignInMethod
		}
		if _, err := q.DeleteUserIdentity(ctx, store.DeleteUserIdentityParams{UserID: userID, Provider: providerGoogle}); err != nil {
			return fmt.Errorf("app: unlinking Google identity: %w", err)
		}
		return recordSecurityEvent(ctx, q, userID, "identity_unlinked", ip, map[string]any{"provider": providerGoogle})
	})
}
