package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

// webauthnUser adapts a store.IdentityUser plus its passkeys to
// webauthn.User. Kept in app/, not domain/, since it depends on store
// types (go.md: domain/ has no imports of store/).
type webauthnUser struct {
	user     store.IdentityUser
	passkeys []store.IdentityUserPasskey
}

func (u webauthnUser) WebAuthnID() []byte          { return u.user.ID[:] }
func (u webauthnUser) WebAuthnName() string        { return u.user.Email }
func (u webauthnUser) WebAuthnDisplayName() string { return u.user.FullName }

func (u webauthnUser) WebAuthnCredentials() []webauthn.Credential {
	creds := make([]webauthn.Credential, len(u.passkeys))
	for i, p := range u.passkeys {
		creds[i] = webauthn.Credential{
			ID:                p.CredentialID,
			PublicKey:         p.PublicKey,
			AttestationType:   p.AttestationType,
			AttestationFormat: p.AttestationFormat,
			Transport:         stringsToTransports(p.Transports),
			Flags:             webauthn.CredentialFlagsFromMsgpByte(byte(p.Flags)), //nolint:gosec // flags is smallint storing a single protocol octet (0-255), always written by us as int16(credential.Flags.MsgpByte())
			Authenticator: webauthn.Authenticator{
				SignCount:    uint32(p.SignCount), //nolint:gosec // sign_count is a WebAuthn authenticator counter, always written by us from a uint32, never negative or near 2^32
				CloneWarning: p.CloneWarning,
			},
		}
	}
	return creds
}

func stringsToTransports(ss []string) []protocol.AuthenticatorTransport {
	out := make([]protocol.AuthenticatorTransport, len(ss))
	for i, s := range ss {
		out[i] = protocol.AuthenticatorTransport(s)
	}
	return out
}

func transportsToStrings(ts []protocol.AuthenticatorTransport) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = string(t)
	}
	return out
}

// loadWebAuthnUser loads userID and its passkeys together — every
// Begin/Finish call needs both.
func (s *Service) loadWebAuthnUser(ctx context.Context, q *store.Queries, userID uuid.UUID) (*webauthnUser, error) {
	user, err := q.GetUserByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("app: looking up user: %w", err)
	}
	passkeys, err := q.ListPasskeysByUserID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("app: listing passkeys: %w", err)
	}
	return &webauthnUser{user: user, passkeys: passkeys}, nil
}

// issueWebAuthnSession signs a *webauthn.SessionData the same
// stateless way issueChallenge signs a totpChallenge — go-webauthn's
// own package doc endorses exactly this ("store it... in a signed,
// opaque session cookie"), and SessionData already carries its own
// Expires field the library checks on Finish, so no second expiry
// needs adding here.
func (s *Service) issueWebAuthnSession(data *webauthn.SessionData) (string, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("app: marshaling webauthn session: %w", err)
	}
	return signPayload(s.challengeKey, payload), nil
}

// errWebAuthnSessionInvalid is verifyWebAuthnSession's low-level
// sentinel; callers map it to whichever higher-level error fits their
// context (ErrValidation for an authenticated management action,
// ErrBadCredentials for a sign-in attempt).
var errWebAuthnSessionInvalid = errors.New("app: webauthn session token invalid")

func (s *Service) verifyWebAuthnSession(token string) (*webauthn.SessionData, error) {
	payload, ok := verifyPayload(s.challengeKey, token)
	if !ok {
		return nil, errWebAuthnSessionInvalid
	}
	var data webauthn.SessionData
	if err := json.Unmarshal(payload, &data); err != nil {
		return nil, errWebAuthnSessionInvalid
	}
	return &data, nil
}

// PasskeyRegistration is BeginPasskeyRegistration's result.
type PasskeyRegistration struct {
	SessionToken string
	Options      *protocol.CredentialCreation
}

// PasskeySummary is a passkey as shown to its owner — never the
// credential's public key or any other verification material.
type PasskeySummary struct {
	ID         uuid.UUID
	Name       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

func passkeySummary(p store.IdentityUserPasskey) PasskeySummary {
	summary := PasskeySummary{ID: p.ID, CreatedAt: p.CreatedAt}
	if p.Name.Valid {
		summary.Name = p.Name.String
	}
	if p.LastUsedAt.Valid {
		t := p.LastUsedAt.Time
		summary.LastUsedAt = &t
	}
	return summary
}

// BeginPasskeyRegistration starts a WebAuthn registration ceremony for
// userID, excluding any passkeys they've already registered so the
// authenticator doesn't offer to re-register one.
func (s *Service) BeginPasskeyRegistration(ctx context.Context, userID uuid.UUID) (*PasskeyRegistration, error) {
	user, err := s.loadWebAuthnUser(ctx, store.New(s.pool), userID)
	if err != nil {
		return nil, err
	}

	creation, session, err := s.webauthn.BeginRegistration(user,
		webauthn.WithExclusions(webauthn.Credentials(user.WebAuthnCredentials()).CredentialDescriptors()),
	)
	if err != nil {
		return nil, fmt.Errorf("app: beginning passkey registration: %w", err)
	}

	token, err := s.issueWebAuthnSession(session)
	if err != nil {
		return nil, err
	}
	return &PasskeyRegistration{SessionToken: token, Options: creation}, nil
}

// FinishPasskeyRegistration verifies rawCredential (the
// navigator.credentials.create() response) against sessionToken and
// stores the new passkey. A tampered or expired sessionToken, or a
// credential that fails verification, returns ErrValidation.
func (s *Service) FinishPasskeyRegistration(ctx context.Context, userID uuid.UUID, sessionToken, name string, rawCredential []byte) (*PasskeySummary, error) {
	session, err := s.verifyWebAuthnSession(sessionToken)
	if err != nil {
		return nil, fmt.Errorf("%w: webauthn session invalid or expired", ErrValidation)
	}

	parsedResponse, err := protocol.ParseCredentialCreationResponseBytes(rawCredential)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}

	var summary *PasskeySummary

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)

		user, err := s.loadWebAuthnUser(ctx, q, userID)
		if err != nil {
			return err
		}

		credential, err := s.webauthn.CreateCredential(user, *session, parsedResponse)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrValidation, err)
		}

		id, err := ids.New()
		if err != nil {
			return fmt.Errorf("app: generating passkey id: %w", err)
		}

		row, err := q.CreatePasskey(ctx, store.CreatePasskeyParams{
			ID:                id,
			UserID:            userID,
			CredentialID:      credential.ID,
			PublicKey:         credential.PublicKey,
			Name:              pgtype.Text{String: name, Valid: name != ""},
			Flags:             int16(credential.Flags.MsgpByte()),
			AttestationType:   credential.AttestationType,
			AttestationFormat: credential.AttestationFormat,
			Transports:        transportsToStrings(credential.Transport),
		})
		if err != nil {
			return fmt.Errorf("app: storing passkey: %w", err)
		}

		s2 := passkeySummary(row)
		summary = &s2
		return nil
	})
	if err != nil {
		return nil, err
	}
	return summary, nil
}

// ListPasskeys returns userID's passkeys, oldest first.
func (s *Service) ListPasskeys(ctx context.Context, userID uuid.UUID) ([]PasskeySummary, error) {
	rows, err := store.New(s.pool).ListPasskeysByUserID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("app: listing passkeys: %w", err)
	}
	summaries := make([]PasskeySummary, len(rows))
	for i, row := range rows {
		summaries[i] = passkeySummary(row)
	}
	return summaries, nil
}

// RenamePasskey renames passkeyID, which must belong to userID.
// Neither "doesn't exist" nor "exists but isn't yours" is
// distinguished — both return ErrNotFound.
func (s *Service) RenamePasskey(ctx context.Context, userID, passkeyID uuid.UUID, name string) (*PasskeySummary, error) {
	row, err := store.New(s.pool).RenamePasskey(ctx, store.RenamePasskeyParams{
		ID:     passkeyID,
		UserID: userID,
		Name:   pgtype.Text{String: name, Valid: name != ""},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("app: renaming passkey: %w", err)
	}
	summary := passkeySummary(row)
	return &summary, nil
}

// DeletePasskey deletes passkeyID, which must belong to userID.
func (s *Service) DeletePasskey(ctx context.Context, userID, passkeyID uuid.UUID) error {
	n, err := store.New(s.pool).DeletePasskey(ctx, store.DeletePasskeyParams{ID: passkeyID, UserID: userID})
	if err != nil {
		return fmt.Errorf("app: deleting passkey: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// PasskeyChallenge is BeginPasskeyLogin's result.
type PasskeyChallenge struct {
	SessionToken string
	Options      *protocol.CredentialAssertion
}

// fakeWebAuthnUser stands in for an unknown email or an account with
// no registered passkeys, so BeginPasskeyLogin can return a
// structurally identical challenge either way — the same
// anti-enumeration principle 1.2/1.3 applied to sign-up, password
// reset and sign-in. webauthn.BeginLogin errors outright on zero
// credentials, so a real empty-credential list isn't an option here.
type fakeWebAuthnUser struct {
	id         []byte
	credential []byte
}

func (u fakeWebAuthnUser) WebAuthnID() []byte          { return u.id }
func (u fakeWebAuthnUser) WebAuthnName() string        { return "" }
func (u fakeWebAuthnUser) WebAuthnDisplayName() string { return "" }
func (u fakeWebAuthnUser) WebAuthnCredentials() []webauthn.Credential {
	return []webauthn.Credential{{ID: u.credential}}
}

func newFakeWebAuthnUser() (*fakeWebAuthnUser, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, fmt.Errorf("app: generating fake webauthn user id: %w", err)
	}
	credential := make([]byte, 32)
	if _, err := rand.Read(credential); err != nil {
		return nil, fmt.Errorf("app: generating fake webauthn credential id: %w", err)
	}
	return &fakeWebAuthnUser{id: id, credential: credential}, nil
}

// BeginPasskeyLogin starts a WebAuthn sign-in ceremony for rawEmail.
// Always returns a structurally valid challenge, whether or not the
// email exists or has any registered passkey.
func (s *Service) BeginPasskeyLogin(ctx context.Context, rawEmail string) (*PasskeyChallenge, error) {
	email, err := domain.NormalizeEmail(rawEmail)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}

	q := store.New(s.pool)

	var loginUser webauthn.User
	user, err := q.GetUserByEmail(ctx, email)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		loginUser, err = newFakeWebAuthnUser()
		if err != nil {
			return nil, err
		}
	case err != nil:
		return nil, fmt.Errorf("app: looking up user: %w", err)
	default:
		passkeys, err := q.ListPasskeysByUserID(ctx, user.ID)
		if err != nil {
			return nil, fmt.Errorf("app: listing passkeys: %w", err)
		}
		if len(passkeys) == 0 {
			loginUser, err = newFakeWebAuthnUser()
			if err != nil {
				return nil, err
			}
		} else {
			loginUser = webauthnUser{user: user, passkeys: passkeys}
		}
	}

	assertion, session, err := s.webauthn.BeginLogin(loginUser)
	if err != nil {
		return nil, fmt.Errorf("app: beginning passkey login: %w", err)
	}

	token, err := s.issueWebAuthnSession(session)
	if err != nil {
		return nil, err
	}
	return &PasskeyChallenge{SessionToken: token, Options: assertion}, nil
}

// CompletePasskeyLogin finishes a sign-in BeginPasskeyLogin started.
// An unknown/fake challenge, a wrong or unrecognized credential, a
// locked-out account, and a cloned-authenticator signal (a sign
// counter that goes backward) all return the same ErrBadCredentials.
func (s *Service) CompletePasskeyLogin(ctx context.Context, challengeToken string, rawCredential []byte, ip net.IP, userAgent string) (*Session, error) {
	session, err := s.verifyWebAuthnSession(challengeToken)
	if err != nil {
		return nil, ErrBadCredentials
	}

	userID, err := uuid.FromBytes(session.UserID)
	if err != nil {
		return nil, ErrBadCredentials
	}

	parsedResponse, err := protocol.ParseCredentialRequestResponseBytes(rawCredential)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}

	var (
		result        *Session
		failed        bool
		sendAlertTo   string
		sendAlertName string
		sendAlert     bool
	)

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)

		dbUser, err := q.GetUserByID(ctx, userID)
		if errors.Is(err, pgx.ErrNoRows) {
			// A forged or fake-challenge userID — nothing real to lock
			// out or record a failure against.
			failed = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("app: looking up user: %w", err)
		}

		locked, err := s.tooManyRecentFailures(ctx, q, userID)
		if err != nil {
			return err
		}
		if locked {
			failed = true
			return nil
		}

		passkeys, err := q.ListPasskeysByUserID(ctx, userID)
		if err != nil {
			return fmt.Errorf("app: listing passkeys: %w", err)
		}
		user := webauthnUser{user: dbUser, passkeys: passkeys}

		credential, err := s.webauthn.ValidateLogin(user, *session, parsedResponse)
		if err != nil {
			if err := s.recordSignInFailure(ctx, q, userID, ip); err != nil {
				return err
			}
			failed = true
			return nil
		}

		if credential.Authenticator.CloneWarning {
			if err := s.recordSignInFailure(ctx, q, userID, ip); err != nil {
				return err
			}
			failed = true
			return nil
		}

		if err := q.UpdatePasskeySignCount(ctx, store.UpdatePasskeySignCountParams{
			CredentialID: credential.ID,
			SignCount:    int64(credential.Authenticator.SignCount),
			Flags:        int16(credential.Flags.MsgpByte()),
			CloneWarning: credential.Authenticator.CloneWarning,
		}); err != nil {
			return fmt.Errorf("app: updating passkey sign count: %w", err)
		}

		var alertNewIP bool
		result, alertNewIP, err = s.createSession(ctx, q, userID, ip, userAgent)
		if err != nil {
			return err
		}
		if alertNewIP {
			sendAlertTo, sendAlertName, sendAlert = dbUser.Email, dbUser.FullName, true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if failed {
		return nil, ErrBadCredentials
	}
	if sendAlert {
		s.sendLoginAlert(ctx, sendAlertTo, sendAlertName, ip)
	}
	return result, nil
}
