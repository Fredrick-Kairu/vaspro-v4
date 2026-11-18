//go:build integration

package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type tableRow struct {
	table string
	r     row
}

// constraintCase inserts before, then r into table, in a rolled-back
// owner transaction, and expects SQLSTATE code ("" for success).
type constraintCase struct {
	name   string
	before []tableRow
	table  string
	r      row
	code   string
}

const (
	checkViolation  = "23514"
	uniqueViolation = "23505"
	notNull         = "23502"
	fkViolation     = "23503"
)

func runConstraintCases(t *testing.T, db webhooksDB, cases []constraintCase) {
	t.Helper()
	ctx := context.Background()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tx, err := db.super.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			for _, b := range c.before {
				mustInsert(t, tx, b.table, b.r)
			}
			wantCode(t, insert(ctx, tx, c.table, c.r), c.code)
		})
	}
}

func TestConstraints_Endpoints(t *testing.T) {
	db := newWebhooksDB(t)
	tenant := uuid.New()
	disabled := row{"status": "disabled", "disabled_at": now, "disabled_reason": "failing", "verified_at": now}
	runConstraintCases(t, db, []constraintCase{
		{name: "pending, unverified", table: "endpoints", r: endpoint(tenant)},
		{name: "active once verified", table: "endpoints", r: activeEndpoint(tenant)},
		{name: "active needs verified_at", table: "endpoints", r: with(endpoint(tenant), row{"status": "active"}), code: checkViolation},
		{name: "an unknown status", table: "endpoints", r: with(endpoint(tenant), row{"status": "paused"}), code: checkViolation},
		{name: "disabled with a time and reason", table: "endpoints", r: with(endpoint(tenant), disabled)},
		{name: "disabled by the client", table: "endpoints", r: with(endpoint(tenant), with(disabled, row{"disabled_reason": "client"}))},
		{name: "disabled without a time", table: "endpoints", r: with(endpoint(tenant), with(disabled, row{"disabled_at": drop})), code: checkViolation},
		{name: "disabled without a reason", table: "endpoints", r: with(endpoint(tenant), with(disabled, row{"disabled_reason": drop})), code: checkViolation},
		{name: "a disabled time while active", table: "endpoints", r: with(activeEndpoint(tenant), row{"disabled_at": now}), code: checkViolation},
		{name: "a reason while active", table: "endpoints", r: with(activeEndpoint(tenant), row{"disabled_reason": "failing"}), code: checkViolation},
		{name: "an unknown reason", table: "endpoints", r: with(endpoint(tenant), with(disabled, row{"disabled_reason": "staff"})), code: checkViolation},
		{name: "plain http (allowed only in development, by the app)", table: "endpoints", r: with(endpoint(tenant), row{"url": "http://localhost:8080/hook"})},
		{name: "not a URL", table: "endpoints", r: with(endpoint(tenant), row{"url": "hooks.example.com"}), code: checkViolation},
		{name: "another scheme", table: "endpoints", r: with(endpoint(tenant), row{"url": "ftp://hooks.example.com/"}), code: checkViolation},
		{name: "a URL over 2048 characters", table: "endpoints", r: with(endpoint(tenant), row{"url": "https://hooks.example.com/" + strings.Repeat("a", 2048)}), code: checkViolation},
		{name: "no tenant", table: "endpoints", r: with(endpoint(tenant), row{"tenant_id": drop}), code: notNull},
		{name: "no maker", table: "endpoints", r: with(endpoint(tenant), row{"created_by": drop}), code: notNull},
	})
}

func TestConstraints_SecretsAndSubscriptions(t *testing.T) {
	db := newWebhooksDB(t)
	tenant := uuid.New()
	e := seedEndpoint(t, db, tenant)
	current := secret(e, tenant)
	runConstraintCases(t, db, []constraintCase{
		{name: "a current secret", table: "endpoint_secrets", r: current},
		{name: "a second current secret", before: []tableRow{{"endpoint_secrets", current}}, table: "endpoint_secrets", r: secret(e, tenant), code: uniqueViolation},
		{name: "a new secret while the old one expires", before: []tableRow{{"endpoint_secrets", with(current, row{"expires_at": now})}}, table: "endpoint_secrets", r: secret(e, tenant)},
		{name: "a secret for an unknown endpoint", table: "endpoint_secrets", r: secret(uuid.New(), tenant), code: fkViolation},
		{name: "a secret with no ciphertext", table: "endpoint_secrets", r: with(current, row{"ciphertext": drop}), code: notNull},
		{name: "a secret with no fingerprint", table: "endpoint_secrets", r: with(current, row{"fingerprint": drop}), code: notNull},
		{name: "every event", table: "subscriptions", r: subscription(e, tenant, "*")},
		{name: "one event type", table: "subscriptions", r: subscription(e, tenant, "payment_intent.succeeded")},
		{name: "the same type twice", before: []tableRow{{"subscriptions", subscription(e, tenant, "wallet.credited")}}, table: "subscriptions", r: subscription(e, tenant, "wallet.credited"), code: uniqueViolation},
		{name: "a malformed type", table: "subscriptions", r: subscription(e, tenant, "Wallet Credited"), code: checkViolation},
		{name: "a type without a family", table: "subscriptions", r: subscription(e, tenant, "credited"), code: checkViolation},
		{name: "a partial wildcard", table: "subscriptions", r: subscription(e, tenant, "wallet.*"), code: checkViolation},
		{name: "a subscription for an unknown endpoint", table: "subscriptions", r: subscription(uuid.New(), tenant, "*"), code: fkViolation},
	})
}

func TestConstraints_Deliveries(t *testing.T) {
	db := newWebhooksDB(t)
	tenant := uuid.New()
	e := seedEndpoint(t, db, tenant)
	first := delivery(e, tenant)
	done := row{"status": "succeeded", "next_attempt_at": nil, "completed_at": now, "attempts": 1}
	dead := row{"status": "dead", "next_attempt_at": nil, "completed_at": now, "attempts": 10, "dead_reason": "attempts_exhausted"}
	replay := func(of row) row {
		return with(delivery(e, tenant), row{"event_id": of["event_id"], "replay_of": of["id"], "replayed_by": uuid.New()})
	}
	runConstraintCases(t, db, []constraintCase{
		{name: "pending", table: "deliveries", r: first},
		{name: "pending and leased", table: "deliveries", r: with(first, row{"leased_until": now})},
		{name: "pending without a next attempt", table: "deliveries", r: with(first, row{"next_attempt_at": nil}), code: checkViolation},
		{name: "pending with a completion time", table: "deliveries", r: with(first, row{"completed_at": now}), code: checkViolation},
		{name: "succeeded", table: "deliveries", r: with(first, done)},
		{name: "succeeded without an attempt", table: "deliveries", r: with(first, with(done, row{"attempts": 0})), code: checkViolation},
		{name: "succeeded with a next attempt", table: "deliveries", r: with(first, with(done, row{"next_attempt_at": now})), code: checkViolation},
		{name: "succeeded without a completion time", table: "deliveries", r: with(first, with(done, row{"completed_at": nil})), code: checkViolation},
		{name: "succeeded but still leased", table: "deliveries", r: with(first, with(done, row{"leased_until": now})), code: checkViolation},
		{name: "dead with a reason", table: "deliveries", r: with(first, dead)},
		{name: "dead with its endpoint disabled", table: "deliveries", r: with(first, with(dead, row{"dead_reason": "endpoint_disabled"}))},
		{name: "dead without a reason", table: "deliveries", r: with(first, with(dead, row{"dead_reason": nil})), code: checkViolation},
		{name: "a reason while pending", table: "deliveries", r: with(first, row{"dead_reason": "attempts_exhausted"}), code: checkViolation},
		{name: "an unknown reason", table: "deliveries", r: with(first, with(dead, row{"dead_reason": "gave_up"})), code: checkViolation},
		{name: "an unknown status", table: "deliveries", r: with(first, row{"status": "failed"}), code: checkViolation},
		{name: "negative attempts", table: "deliveries", r: with(first, row{"attempts": -1}), code: checkViolation},
		{name: "a malformed event type", table: "deliveries", r: with(first, row{"event_type": "*"}), code: checkViolation},
		{name: "an event delivered twice to one endpoint", before: []tableRow{{"deliveries", first}}, table: "deliveries", r: with(delivery(e, tenant), row{"event_id": first["event_id"]}), code: uniqueViolation},
		{name: "the same event to another endpoint", before: []tableRow{{"deliveries", first}, {"endpoints", with(activeEndpoint(tenant), row{"id": uuid.MustParse("01900000-0000-7000-8000-000000000001")})}},
			table: "deliveries", r: with(delivery(uuid.MustParse("01900000-0000-7000-8000-000000000001"), tenant), row{"event_id": first["event_id"]})},
		{name: "a replay of a delivered event", before: []tableRow{{"deliveries", first}}, table: "deliveries", r: replay(first)},
		{name: "two replays of one delivery", before: []tableRow{{"deliveries", first}, {"deliveries", replay(first)}}, table: "deliveries", r: replay(first)},
		{name: "a replay without who asked", before: []tableRow{{"deliveries", first}}, table: "deliveries", r: with(replay(first), row{"replayed_by": drop}), code: checkViolation},
		{name: "who asked without a replay", table: "deliveries", r: with(first, row{"replayed_by": uuid.New()}), code: checkViolation},
		{name: "a replay of an unknown delivery", table: "deliveries", r: with(first, row{"replay_of": uuid.New(), "replayed_by": uuid.New()}), code: fkViolation},
		{name: "a delivery to an unknown endpoint", table: "deliveries", r: delivery(uuid.New(), tenant), code: fkViolation},
		{name: "no body", table: "deliveries", r: with(first, row{"body": drop}), code: notNull},
	})
}

func TestConstraints_DeliveryAttempts(t *testing.T) {
	db := newWebhooksDB(t)
	tenant := uuid.New()
	d := seedDelivery(t, db, seedEndpoint(t, db, tenant), tenant)
	failed := attempt(d, tenant, 1)
	noResponse := func(outcome string) row {
		return with(failed, row{"outcome": outcome, "response_status": drop, "error": "dial tcp: i/o timeout"})
	}
	runConstraintCases(t, db, []constraintCase{
		{name: "a 500", table: "delivery_attempts", r: failed},
		{name: "a 200", table: "delivery_attempts", r: with(failed, row{"outcome": "succeeded", "response_status": 200})},
		{name: "a 204", table: "delivery_attempts", r: with(failed, row{"outcome": "succeeded", "response_status": 204})},
		{name: "a 301 is a failure", table: "delivery_attempts", r: with(failed, row{"response_status": 301})},
		{name: "success with a 500", table: "delivery_attempts", r: with(failed, row{"outcome": "succeeded"}), code: checkViolation},
		{name: "a failure with a 200", table: "delivery_attempts", r: with(failed, row{"response_status": 200}), code: checkViolation},
		{name: "an HTTP failure with no status", table: "delivery_attempts", r: with(failed, row{"response_status": drop}), code: checkViolation},
		{name: "a timeout", table: "delivery_attempts", r: noResponse("timeout")},
		{name: "a connection failure", table: "delivery_attempts", r: noResponse("connect")},
		{name: "a TLS failure", table: "delivery_attempts", r: noResponse("tls")},
		{name: "a refused URL", table: "delivery_attempts", r: noResponse("refused_url")},
		{name: "a timeout with a status", table: "delivery_attempts", r: with(noResponse("timeout"), row{"response_status": 504}), code: checkViolation},
		{name: "a timeout without an error", table: "delivery_attempts", r: with(noResponse("timeout"), row{"error": drop}), code: checkViolation},
		{name: "an HTTP failure with an error", table: "delivery_attempts", r: with(failed, row{"error": "boom"}), code: checkViolation},
		{name: "an unknown outcome", table: "delivery_attempts", r: with(failed, row{"outcome": "redirect"}), code: checkViolation},
		{name: "a body of 4 KB", table: "delivery_attempts", r: with(failed, row{"response_body": make([]byte, 4096), "response_truncated": true})},
		{name: "a body over 4 KB", table: "delivery_attempts", r: with(failed, row{"response_body": make([]byte, 4097)}), code: checkViolation},
		{name: "truncated with no body", table: "delivery_attempts", r: with(failed, row{"response_truncated": true}), code: checkViolation},
		{name: "attempt 0", table: "delivery_attempts", r: with(failed, row{"attempt": 0}), code: checkViolation},
		{name: "a negative duration", table: "delivery_attempts", r: with(failed, row{"duration_ms": -1}), code: checkViolation},
		{name: "one attempt number twice", before: []tableRow{{"delivery_attempts", failed}}, table: "delivery_attempts", r: attempt(d, tenant, 1), code: uniqueViolation},
		{name: "the next attempt", before: []tableRow{{"delivery_attempts", failed}}, table: "delivery_attempts", r: attempt(d, tenant, 2)},
		{name: "an attempt for an unknown delivery", table: "delivery_attempts", r: attempt(uuid.New(), tenant, 1), code: fkViolation},
	})
}
