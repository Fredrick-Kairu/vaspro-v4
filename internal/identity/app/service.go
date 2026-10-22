package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

// ErrValidation wraps every input-shape rejection (bad email, empty
// name, bad phone, too-short password) — api/ maps it to VAL-4000.
var ErrValidation = errors.New("app: validation failed")

// ErrInvalidOrExpiredCode covers a wrong email-verification or
// password-reset code, an expired or already-used token, and attempts
// exhausted alike — deliberately not distinguished. api/ maps it to
// VAL-4002.
var ErrInvalidOrExpiredCode = errors.New("app: verification code invalid or expired")

// ErrBadCredentials covers an unknown email, a wrong password, and a
// locked-out account alike — deliberately not distinguished: telling a
// caller specifically "this account is locked" would confirm the
// account exists. api/ maps it to AUTH-4010.
var ErrBadCredentials = errors.New("app: invalid credentials")

// ErrTwoFactorInvalid covers a wrong TOTP code, a wrong or already-used
// recovery code, and an expired or malformed challenge token alike —
// same "don't distinguish" principle. api/ maps it to AUTH-4011.
var ErrTwoFactorInvalid = errors.New("app: two-factor code invalid or expired")

// ErrNotFound covers "doesn't exist" and "exists but isn't the
// caller's" alike — same anti-enumeration principle as
// ErrBadCredentials/ErrTwoFactorInvalid. api/ maps it to VAL-4003.
var ErrNotFound = errors.New("app: resource not found")

const (
	verificationTTL         = 15 * time.Minute
	maxVerificationAttempts = 5

	// maxSignInFailures/lockoutWindow drive sign-in lockout as a rolling
	// window over identity.security_events, not a persisted "locked"
	// state — see Service.tooManyRecentFailures.
	maxSignInFailures = 5
	lockoutWindow     = 15 * time.Minute

	sessionTTL   = 24 * time.Hour
	challengeTTL = 5 * time.Minute

	minPasswordLength = 8
)

// Service holds identity's use cases. Its tables (users,
// verification_tokens, sessions, user_totp, security_events,
// outbox_events) have no tenant_id at all — identity is global, not a
// tenant resource (1.1's own finding) — so every transaction here is a
// plain pgx.BeginFunc, never pool.WithTenantTx.
type Service struct {
	pool  *db.Pool
	email adapters.EmailSender
	clk   clock.Clock

	// envelope/totpKeyID encrypt TOTP secrets at rest
	// (user_totp.secret_ciphertext, data model §3's own comment: "KMS
	// key"). envelope is nil-safe to construct against LocalKMS in
	// development; a real Cloud KMS/Vault implementation is still
	// undecided (0.11's own note).
	envelope  *vascrypto.Envelope
	totpKeyID string

	// challengeKey signs every short-lived, stateless challenge token
	// this service issues — the TOTP sign-in challenge
	// (issueChallenge/verifyChallenge) and the WebAuthn ceremony
	// session token (issueWebAuthnSession/verifyWebAuthnSession).
	// Not the same key as envelope's: different purpose, shouldn't
	// share material.
	challengeKey []byte

	// webauthn is nil-safe to construct once per process (1.4): its
	// Config fixes the relying party ID/display name/origins for
	// every registration and sign-in ceremony.
	webauthn *webauthn.WebAuthn
}

// Deps is everything New needs. Grouped into a struct rather than
// growing New's positional parameter list indefinitely as each task
// adds its own dependencies on top of the last.
type Deps struct {
	Pool         *db.Pool
	Email        adapters.EmailSender
	Clock        clock.Clock
	Envelope     *vascrypto.Envelope
	TOTPKeyID    string
	ChallengeKey []byte
	WebAuthn     *webauthn.WebAuthn
}

// New builds a Service.
func New(d Deps) *Service {
	return &Service{
		pool:         d.Pool,
		email:        d.Email,
		clk:          d.Clock,
		envelope:     d.Envelope,
		totpKeyID:    d.TOTPKeyID,
		challengeKey: d.ChallengeKey,
		webauthn:     d.WebAuthn,
	}
}

// Session is what a successful sign-in (or completed TOTP challenge)
// produces.
type Session struct {
	ID        uuid.UUID
	ExpiresAt time.Time
}

// hashVerificationCode hashes target+purpose+code together, not the
// bare code: verification_tokens.token_hash is globally UNIQUE (data
// model §3), and a 6-digit code only has ~1M possibilities — hashing it
// alone risks a collision across two different users' codes.
func hashVerificationCode(target string, purpose store.IdentityTokenPurpose, code string) []byte {
	sum := sha256.Sum256([]byte(target + ":" + string(purpose) + ":" + code))
	return sum[:]
}

// hashRecoveryCode hashes a recovery code with plain SHA-256, not
// Argon2id: these are random and high-entropy already (unlike a
// password), the same reasoning 1.2 applied to verification codes.
// recovery_code_hashes is text[] (data model §3), so the hash is stored
// hex-encoded, not raw bytes.
func hashRecoveryCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// toNetipAddr converts a net.IP (as the HTTP layer hands in from
// r.RemoteAddr) to the *netip.Addr the generated sessions/
// security_events columns want, or nil if ip is nil.
func toNetipAddr(ip net.IP) *netip.Addr {
	if ip == nil {
		return nil
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return nil
	}
	return &addr
}

// tooManyRecentFailures reports whether userID has maxSignInFailures or
// more sign_in_failed security events within the last lockoutWindow —
// the rolling-window lockout check shared by password and TOTP/recovery
// verification. "Unlocks by time" for free: once a failure ages out of
// the window, the count (and the lockout) drops on its own.
func (s *Service) tooManyRecentFailures(ctx context.Context, q *store.Queries, userID uuid.UUID) (bool, error) {
	since := s.clk.Now().Add(-lockoutWindow)
	count, err := q.CountRecentSignInFailures(ctx, store.CountRecentSignInFailuresParams{
		UserID:     pgtype.UUID{Bytes: userID, Valid: true},
		OccurredAt: since,
	})
	if err != nil {
		return false, fmt.Errorf("app: counting recent sign-in failures: %w", err)
	}
	return count >= maxSignInFailures, nil
}

// recordSignInFailure writes a sign_in_failed security event for
// userID — called from inside the same transaction as the failed
// attempt, so the write survives even though the caller's own
// business-level outcome ("invalid") is decided after the transaction
// commits, not by returning an error from inside it (pgx.BeginFunc
// rolls back on ANY non-nil return — found the hard way in 1.2).
func (s *Service) recordSignInFailure(ctx context.Context, q *store.Queries, userID uuid.UUID, ip net.IP) error {
	id, err := ids.New()
	if err != nil {
		return fmt.Errorf("app: generating security event id: %w", err)
	}
	_, err = q.CreateSecurityEvent(ctx, store.CreateSecurityEventParams{
		ID:       id,
		UserID:   pgtype.UUID{Bytes: userID, Valid: true},
		TenantID: pgtype.UUID{},
		Type:     "sign_in_failed",
		Ip:       toNetipAddr(ip),
		Detail:   []byte("{}"),
	})
	if err != nil {
		return fmt.Errorf("app: recording sign-in failure: %w", err)
	}
	return nil
}

// createSession inserts a new identity.sessions row for userID. tenant_id
// stays unset — there's no tenant/account selection flow yet (accounts
// is Phase 2) — and Redis caching plus list/revoke/sign-out-everywhere
// are task 1.5's job, built on top of this row, not replacing it.
func (s *Service) createSession(ctx context.Context, q *store.Queries, userID uuid.UUID, ip net.IP, userAgent string) (*Session, error) {
	id, err := ids.New()
	if err != nil {
		return nil, fmt.Errorf("app: generating session id: %w", err)
	}
	expiresAt := s.clk.Now().Add(sessionTTL)
	row, err := q.CreateSession(ctx, store.CreateSessionParams{
		ID:        id,
		UserID:    userID,
		TenantID:  pgtype.UUID{},
		Ip:        toNetipAddr(ip),
		UserAgent: pgtype.Text{String: userAgent, Valid: userAgent != ""},
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return nil, fmt.Errorf("app: creating session: %w", err)
	}
	return &Session{ID: row.ID, ExpiresAt: row.ExpiresAt}, nil
}

// totpChallenge is the payload inside a sign-in challenge token.
type totpChallenge struct {
	UserID    uuid.UUID `json:"user_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// signPayload returns a short-lived, stateless, HMAC-signed token
// wrapping payload — no new table: a signed opaque value needs
// nothing persisted, unlike the "pending sign-ins" table a stateful
// design would need (and that isn't in the data model). Format:
// base64url(payload) + "." + base64url(HMAC-SHA256 of the payload
// half). Shared by every challenge-shaped token this service issues
// (the TOTP sign-in challenge, the WebAuthn ceremony session).
func signPayload(key, payload []byte) string {
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	sig := vascrypto.HMACSign(key, []byte(payloadB64))
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)
	return payloadB64 + "." + sigB64
}

// verifyPayload reverses signPayload, checking the signature before
// returning anything decoded from it. ok is false for a malformed or
// tampered token; it does not check expiry, since that's each
// payload's own shape to decode and compare.
func verifyPayload(key []byte, token string) (payload []byte, ok bool) {
	payloadB64, sigB64, cut := strings.Cut(token, ".")
	if !cut {
		return nil, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return nil, false
	}
	if !vascrypto.HMACVerify(key, []byte(payloadB64), sig) {
		return nil, false
	}
	payload, err = base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return nil, false
	}
	return payload, true
}

// issueChallenge returns a short-lived, stateless, HMAC-signed token
// proving the caller already supplied a correct password for userID.
func (s *Service) issueChallenge(userID uuid.UUID) (string, error) {
	c := totpChallenge{UserID: userID, ExpiresAt: s.clk.Now().Add(challengeTTL)}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("app: building challenge: %w", err)
	}
	return signPayload(s.challengeKey, payload), nil
}

// verifyChallenge reverses issueChallenge and rejects an expired
// challenge.
func (s *Service) verifyChallenge(token string) (uuid.UUID, error) {
	payload, ok := verifyPayload(s.challengeKey, token)
	if !ok {
		return uuid.Nil, ErrTwoFactorInvalid
	}
	var c totpChallenge
	if err := json.Unmarshal(payload, &c); err != nil {
		return uuid.Nil, ErrTwoFactorInvalid
	}
	if s.clk.Now().After(c.ExpiresAt) {
		return uuid.Nil, ErrTwoFactorInvalid
	}
	return c.UserID, nil
}
