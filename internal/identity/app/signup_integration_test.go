//go:build integration

package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/cache"
	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/email"
)

// testTOTPKeyID is the LocalKMS key ID newTestService's Envelope
// encrypts TOTP secrets under — matches identity.go's own
// totpKMSKeyID in spirit, kept independent so this test doesn't import
// the identity package (internal/<a> mustn't import internal/<b>, and
// a test importing its own service's root package back is a pointless
// coupling anyway).
const testTOTPKeyID = "test-totp"

// testSigningKeysKeyID is the LocalKMS key ID newTestService's
// Envelope seals JWT signing keys' private material under (task
// 1.8) — matches identity.go's own signingKeysKMSKeyID in spirit,
// same independence reasoning as testTOTPKeyID above.
const testSigningKeysKeyID = "test-signing-keys"

// testRPID/testRPOrigin fix the WebAuthn relying party every passkey
// test (1.4) registers and signs in against — passkey_integration_test.go's
// virtualwebauthn.RelyingParty must use these exact same values.
const (
	testRPID     = "localhost"
	testRPOrigin = "https://localhost"
)

func randomKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generating random key: %v", err)
	}
	return key
}

var sixDigitCode = regexp.MustCompile(`^[0-9]{6}$`)

func newTestService(t *testing.T) (*app.Service, *vasdb.Pool, *email.Sender, *testkit.Clock, *cache.Cache) {
	t.Helper()
	return newTestServiceWithOIDC(t, nil)
}

// newTestServiceWithOIDC is newTestService with Sign in with Google
// wired to provider (task 1.16); nil leaves Google off.
func newTestServiceWithOIDC(t *testing.T, provider adapters.OIDCProvider) (*app.Service, *vasdb.Pool, *email.Sender, *testkit.Clock, *cache.Cache) {
	t.Helper()
	ctx := context.Background()

	c := testkit.StartRedis(t)

	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/identity"))
	pool, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=identity,public")
	if err != nil {
		t.Fatalf("failed to connect with search_path=identity,public: %v", err)
	}
	t.Cleanup(pool.Close)

	sender := &email.Sender{}
	// Anchored at real wall time, not an arbitrary fixed date: expires_at
	// is computed Go-side from this clock, but checked against
	// Postgres's own real now() (GetPendingVerificationTokenByTarget's
	// "expires_at > now()") — a fake clock frozen at an arbitrary past
	// or future date would make every token look pre-expired to the
	// database, which already happened once while writing this test.
	clk := testkit.NewClock(time.Now().UTC())

	kms, err := vascrypto.NewLocalKMS(map[string][]byte{
		testTOTPKeyID:        randomKey(t),
		testSigningKeysKeyID: randomKey(t),
	})
	if err != nil {
		t.Fatalf("building LocalKMS: %v", err)
	}

	wa, err := webauthn.New(&webauthn.Config{
		RPID:          testRPID,
		RPDisplayName: "VASPRO Test",
		RPOrigins:     []string{testRPOrigin},
	})
	if err != nil {
		t.Fatalf("building webauthn config: %v", err)
	}

	svc := app.New(app.Deps{
		Pool:             pool,
		Email:            sender,
		Clock:            clk,
		Envelope:         vascrypto.NewEnvelope(kms),
		TOTPKeyID:        testTOTPKeyID,
		SigningKeysKeyID: testSigningKeysKeyID,
		ChallengeKey:     randomKey(t),
		WebAuthn:         wa,
		Cache:            c,
		OIDC:             provider,
	})
	return svc, pool, sender, clk, c
}

func outboxEventTypes(t *testing.T, ctx context.Context, pool *vasdb.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, "SELECT event_type FROM outbox_events ORDER BY occurred_at")
	if err != nil {
		t.Fatalf("querying outbox_events: %v", err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var et string
		if err := rows.Scan(&et); err != nil {
			t.Fatalf("scanning outbox_events: %v", err)
		}
		types = append(types, et)
	}
	return types
}

// assertEventPayloadValid fetches the most recent outbox_events row with
// the given event_type, unmarshals it as an Envelope, and validates its
// Data field against docs/events/<eventType>.schema.json — the §8.3
// quality gate ("Every event published has a JSON Schema... a test
// validates published payloads against it").
func assertEventPayloadValid(t *testing.T, ctx context.Context, pool *vasdb.Pool, eventType string) {
	t.Helper()

	var payload []byte
	err := pool.QueryRow(ctx,
		"SELECT payload FROM outbox_events WHERE event_type = $1 ORDER BY occurred_at DESC LIMIT 1",
		eventType,
	).Scan(&payload)
	if err != nil {
		t.Fatalf("fetching outbox row for %s: %v", eventType, err)
	}

	var env platformevents.Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("unmarshaling envelope for %s: %v", eventType, err)
	}

	schemaPath := "../../../docs/events/" + eventType + ".schema.json"
	schemaBytes, err := os.ReadFile(schemaPath) //nolint:gosec // schemaPath is built from a fixed test-controlled event type, not external input
	if err != nil {
		t.Fatalf("reading %s: %v", schemaPath, err)
	}
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		t.Fatalf("parsing %s: %v", schemaPath, err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(eventType+".schema.json", schemaDoc); err != nil {
		t.Fatalf("loading schema %s: %v", schemaPath, err)
	}
	schema, err := compiler.Compile(eventType + ".schema.json")
	if err != nil {
		t.Fatalf("compiling schema %s: %v", schemaPath, err)
	}

	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(env.Data))
	if err != nil {
		t.Fatalf("parsing %s event data: %v", eventType, err)
	}
	if err := schema.Validate(instance); err != nil {
		t.Fatalf("%s event payload %s failed schema validation: %v", eventType, env.Data, err)
	}
}

func TestSignUp_NewEmail_CreatesUserAndSendsCode(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, _, _ := newTestService(t)

	if err := svc.SignUp(ctx, "Ada@Example.com", "Ada Lovelace", nil); err != nil {
		t.Fatalf("SignUp: %v", err)
	}

	q := store.New(pool)
	user, err := q.GetUserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if user.EmailVerifiedAt.Valid {
		t.Fatal("expected a freshly signed-up user to be unverified")
	}

	sent, ok := sender.Last()
	if !ok {
		t.Fatal("expected a verification email to have been sent")
	}
	if sent.To != "ada@example.com" {
		t.Fatalf("sent to %q, want %q", sent.To, "ada@example.com")
	}
	if !sixDigitCode.MatchString(sent.Code) {
		t.Fatalf("code %q is not 6 digits", sent.Code)
	}

	if types := outboxEventTypes(t, ctx, pool); len(types) != 1 || types[0] != "user.registered" {
		t.Fatalf("outbox event types = %v, want exactly [user.registered]", types)
	}
	assertEventPayloadValid(t, ctx, pool, "user.registered")
}

func TestSignUp_RepeatOnPendingEmail_ReissuesCodeWithoutRepublishing(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, _, _ := newTestService(t)

	if err := svc.SignUp(ctx, "ada@example.com", "Ada Lovelace", nil); err != nil {
		t.Fatalf("first SignUp: %v", err)
	}
	first, _ := sender.Last()

	if err := svc.SignUp(ctx, "ada@example.com", "Ada Lovelace", nil); err != nil {
		t.Fatalf("second SignUp: %v", err)
	}
	second, _ := sender.Last()

	if len(sender.All()) != 2 {
		t.Fatalf("expected 2 emails sent, got %d", len(sender.All()))
	}
	if first.Code == second.Code {
		t.Fatal("expected the reissued code to differ from the first")
	}

	// Still only one user.registered event — a resend isn't a second
	// registration.
	if types := outboxEventTypes(t, ctx, pool); len(types) != 1 {
		t.Fatalf("outbox event types = %v, want exactly 1 (user.registered)", types)
	}

	// The old code must no longer verify, only the reissued one.
	if err := svc.VerifyEmail(ctx, "ada@example.com", first.Code); err == nil {
		t.Fatal("expected the superseded code to no longer verify")
	}
	if err := svc.VerifyEmail(ctx, "ada@example.com", second.Code); err != nil {
		t.Fatalf("expected the reissued code to verify: %v", err)
	}
}

func TestSignUp_AlreadyVerifiedEmail_SendsNothing(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)

	if err := svc.SignUp(ctx, "ada@example.com", "Ada Lovelace", nil); err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	sent, _ := sender.Last()
	if err := svc.VerifyEmail(ctx, "ada@example.com", sent.Code); err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}

	countBefore := len(sender.All())
	if err := svc.SignUp(ctx, "ada@example.com", "Ada Lovelace", nil); err != nil {
		t.Fatalf("second SignUp on a verified email: %v", err)
	}
	if got := len(sender.All()); got != countBefore {
		t.Fatalf("expected no new email for an already-verified address, got %d sends (was %d)", got, countBefore)
	}
}

func TestVerifyEmail_HappyPath_MarksVerifiedAndPublishes(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, _, _ := newTestService(t)

	if err := svc.SignUp(ctx, "ada@example.com", "Ada Lovelace", nil); err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	sent, _ := sender.Last()

	if err := svc.VerifyEmail(ctx, "ada@example.com", sent.Code); err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}

	q := store.New(pool)
	user, err := q.GetUserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if !user.EmailVerifiedAt.Valid {
		t.Fatal("expected email_verified_at to be set")
	}

	assertEventPayloadValid(t, ctx, pool, "user.email_verified")
}

func TestVerifyEmail_WrongCode_IncrementsAttemptsWithoutVerifying(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, _, _ := newTestService(t)

	if err := svc.SignUp(ctx, "ada@example.com", "Ada Lovelace", nil); err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	sent, _ := sender.Last()
	wrongCode := "000000"
	if wrongCode == sent.Code {
		wrongCode = "111111"
	}

	err := svc.VerifyEmail(ctx, "ada@example.com", wrongCode)
	if !errors.Is(err, app.ErrInvalidOrExpiredCode) {
		t.Fatalf("got error %v, want app.ErrInvalidOrExpiredCode", err)
	}

	q := store.New(pool)
	user, err := q.GetUserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if user.EmailVerifiedAt.Valid {
		t.Fatal("expected the user to remain unverified after a wrong code")
	}
}

func TestVerifyEmail_TooManyAttempts_InvalidatesEvenTheCorrectCode(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)

	if err := svc.SignUp(ctx, "ada@example.com", "Ada Lovelace", nil); err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	sent, _ := sender.Last()
	wrongCode := "000000"
	if wrongCode == sent.Code {
		wrongCode = "111111"
	}

	for range 5 {
		_ = svc.VerifyEmail(ctx, "ada@example.com", wrongCode)
	}

	if err := svc.VerifyEmail(ctx, "ada@example.com", sent.Code); !errors.Is(err, app.ErrInvalidOrExpiredCode) {
		t.Fatalf("got error %v, want app.ErrInvalidOrExpiredCode (attempts exhausted)", err)
	}
}
