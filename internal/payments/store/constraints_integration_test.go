//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

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

func runConstraintCases(t *testing.T, db paymentsDB, cases []constraintCase) {
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

func TestConstraints(t *testing.T) {
	db := newPaymentsDB(t)
	pb := seedPaybill(t, db)
	tenantA, tenantB := uuid.New(), uuid.New()
	cpbRow := clientPaybill(tenantA, "700200")
	mustInsert(t, db.super, "rail_accounts", cpbRow)
	cpb := cpbRow["id"].(uuid.UUID)

	t.Run("rails", func(t *testing.T) {
		base := with(mpesaRail(), row{"code": "airtel_money_ke"})
		runConstraintCases(t, db, []constraintCase{
			{name: "a rail with known capabilities", table: "rails", r: base},
			{name: "an unknown capability is refused", table: "rails", r: with(base, row{"capabilities": []string{"stk", "teleport"}}), code: checkViolation},
			{name: "an unknown kind is refused", table: "rails", r: with(base, row{"kind": "pigeon"}), code: checkViolation},
			{name: "an unknown status is refused", table: "rails", r: with(base, row{"status": "maybe"}), code: checkViolation},
			{name: "the same code twice is refused", table: "rails", r: mpesaRail(), code: uniqueViolation},
		})
	})

	t.Run("rail_accounts", func(t *testing.T) {
		other := with(vasproPaybill(), row{"number": "600101"})
		runConstraintCases(t, db, []constraintCase{
			{name: "VASPRO's top-up paybill", table: "rail_accounts", r: other},
			{name: "a client's collections paybill", table: "rail_accounts", r: clientPaybill(tenantB, "700201")},
			{name: "a client account can't take VASPRO's top-ups", table: "rail_accounts", r: with(clientPaybill(tenantB, "700202"), row{"uses": []string{"collections", "vaspro_topups"}}), code: checkViolation},
			{name: "uses can't be empty", table: "rail_accounts", r: with(other, row{"uses": []string{}}), code: checkViolation},
			{name: "an unknown use is refused", table: "rail_accounts", r: with(other, row{"uses": []string{"vaspro_topups", "gambling"}}), code: checkViolation},
			{name: "active needs a verification", table: "rail_accounts", r: with(other, row{"verified_at": drop}), code: checkViolation},
			{name: "pending needs none", table: "rail_accounts", r: with(other, row{"verified_at": drop, "status": "pending_verification"})},
			{name: "an unknown account type is refused", table: "rail_accounts", r: with(other, row{"account_type": "jar"}), code: checkViolation},
			{name: "an unknown status is refused", table: "rail_accounts", r: with(other, row{"status": "asleep"}), code: checkViolation},
			{name: "the same number on a rail twice is refused", table: "rail_accounts", r: with(vasproPaybill(), row{}), code: uniqueViolation},
			{name: "an unknown rail is refused", table: "rail_accounts", r: with(other, row{"rail_code": "nope"}), code: fkViolation},
		})
	})

	t.Run("rail_credentials", func(t *testing.T) {
		active := credential(pb, nil)
		runConstraintCases(t, db, []constraintCase{
			{name: "an active credential", table: "rail_credentials", r: active},
			{name: "retired needs retired_at", table: "rail_credentials", r: with(active, row{"status": "retired"}), code: checkViolation},
			{name: "active can't have retired_at", table: "rail_credentials", r: with(active, row{"retired_at": now}), code: checkViolation},
			{name: "a retired one", table: "rail_credentials", r: with(active, row{"status": "retired", "retired_at": now})},
			{name: "a second active one of a kind is refused", before: []tableRow{{"rail_credentials", credential(pb, nil)}}, table: "rail_credentials", r: active, code: uniqueViolation},
			{name: "another kind while one is active", before: []tableRow{{"rail_credentials", credential(pb, nil)}}, table: "rail_credentials", r: with(active, row{"kind": "passkey"})},
			{name: "a new active one after retiring", before: []tableRow{{"rail_credentials", with(credential(pb, nil), row{"status": "retired", "retired_at": now})}}, table: "rail_credentials", r: active},
			{name: "an unknown kind is refused", table: "rail_credentials", r: with(active, row{"kind": "password123"}), code: checkViolation},
		})
	})

	t.Run("payment_intents", func(t *testing.T) {
		base := intent(tenantA, pb)
		dupKey := intent(tenantA, pb)
		withRequest := with(intent(tenantA, pb), row{"rail_request_id": "ws_CO_1"})
		succeeded := with(base, row{"status": "succeeded", "rail_receipt": "NLJ7RT61SV", "completed_at": now})
		runConstraintCases(t, db, []constraintCase{
			{name: "a pending STK top-up", table: "payment_intents", r: base},
			{name: "a zero amount is refused", table: "payment_intents", r: with(base, row{"amount_minor": int64(0)}), code: checkViolation},
			{name: "an STK push needs a phone", table: "payment_intents", r: with(base, row{"payer_phone_e164": drop}), code: checkViolation},
			{name: "a card payment needs none", table: "payment_intents", r: with(base, row{"payer_phone_e164": drop, "method": "card"})},
			{name: "succeeded with a receipt", table: "payment_intents", r: succeeded},
			{name: "succeeded needs a receipt", table: "payment_intents", r: with(succeeded, row{"rail_receipt": drop}), code: checkViolation},
			{name: "failed needs an error code", table: "payment_intents", r: with(base, row{"status": "failed", "completed_at": now}), code: checkViolation},
			{name: "failed with an error code", table: "payment_intents", r: with(base, row{"status": "failed", "completed_at": now, "error_code": "PAY-6503", "result_code": "1032"})},
			{name: "a final status needs completed_at", table: "payment_intents", r: with(succeeded, row{"completed_at": drop}), code: checkViolation},
			{name: "pending can't be completed", table: "payment_intents", r: with(base, row{"completed_at": now}), code: checkViolation},
			{name: "expired is final", table: "payment_intents", r: with(base, row{"status": "expired", "completed_at": now, "error_code": "PAY-6504"})},
			{name: "it must expire after it's created", table: "payment_intents", r: with(base, row{"created_at": now, "expires_at": now}), code: checkViolation},
			{name: "an invoice payment needs its invoice", table: "payment_intents", r: with(base, row{"purpose": "invoice"}), code: checkViolation},
			{name: "an invoice payment with its invoice", table: "payment_intents", r: with(base, row{"purpose": "invoice", "invoice_id": uuid.New()})},
			{name: "a payment link payment needs its link", table: "payment_intents", r: with(base, row{"purpose": "payment_link"}), code: checkViolation},
			{name: "a lower-case currency is refused", table: "payment_intents", r: with(base, row{"currency": "kes"}), code: checkViolation},
			{name: "a tenant's key twice is refused", before: []tableRow{{"payment_intents", dupKey}}, table: "payment_intents", r: with(base, row{"idempotency_key": dupKey["idempotency_key"]}), code: uniqueViolation},
			{name: "another tenant may reuse a key", before: []tableRow{{"payment_intents", dupKey}}, table: "payment_intents", r: with(intent(tenantB, pb), row{"idempotency_key": dupKey["idempotency_key"]})},
			{name: "a rail request ID twice on an account is refused", before: []tableRow{{"payment_intents", withRequest}}, table: "payment_intents", r: with(base, row{"rail_request_id": "ws_CO_1"}), code: uniqueViolation},
			{name: "a receipt twice on an account is refused", before: []tableRow{{"payment_intents", succeeded}}, table: "payment_intents", r: with(intent(tenantA, pb), row{"status": "succeeded", "rail_receipt": "NLJ7RT61SV", "completed_at": now}), code: uniqueViolation},
		})
	})

	t.Run("collection_notices", func(t *testing.T) {
		susp := suspenseNotice(pb, "R000000001")
		runConstraintCases(t, db, []constraintCase{
			{name: "a payment in suspense", table: "collection_notices", r: susp},
			{name: "a matched top-up", table: "collection_notices", r: matchedNotice(pb, tenantA, "R000000002")},
			{name: "a client's own collection", table: "collection_notices", r: with(suspenseNotice(cpb, "R000000003"), row{"tenant_id": tenantA, "match_status": "client_owned"})},
			{name: "the same receipt twice on an account is refused", before: []tableRow{{"collection_notices", suspenseNotice(pb, "R000000001")}}, table: "collection_notices", r: susp, code: uniqueViolation},
			{name: "the same receipt on another account", before: []tableRow{{"collection_notices", suspenseNotice(pb, "R000000001")}}, table: "collection_notices", r: with(suspenseNotice(cpb, "R000000001"), row{"tenant_id": tenantA, "match_status": "client_owned"})},
			{name: "a zero amount is refused", table: "collection_notices", r: with(susp, row{"amount_minor": int64(0)}), code: checkViolation},
			{name: "matched needs a tenant", table: "collection_notices", r: with(susp, row{"match_status": "matched"}), code: checkViolation},
			{name: "suspense can't name a matched tenant", table: "collection_notices", r: with(susp, row{"matched_tenant_id": tenantA}), code: checkViolation},
			{name: "suspense can't have an owner", table: "collection_notices", r: with(susp, row{"tenant_id": tenantA}), code: checkViolation},
			{name: "client_owned needs an owner", table: "collection_notices", r: with(susp, row{"match_status": "client_owned"}), code: checkViolation},
			{name: "client_owned isn't matched to anyone", table: "collection_notices", r: with(susp, row{"match_status": "client_owned", "tenant_id": tenantA, "matched_tenant_id": tenantA}), code: checkViolation},
			{name: "an owned notice is never matched", table: "collection_notices", r: with(matchedNotice(pb, tenantA, "R000000004"), row{"tenant_id": tenantA}), code: checkViolation},
			{name: "an unknown match status is refused", table: "collection_notices", r: with(susp, row{"match_status": "guessed"}), code: checkViolation},
			{name: "raw is required", table: "collection_notices", r: with(susp, row{"raw": drop}), code: notNull},
		})
	})

	t.Run("statements", func(t *testing.T) {
		imp := statementImport(pb, nil)
		impID := imp["id"].(uuid.UUID)
		mustInsert(t, db.super, "statement_imports", imp)
		line := statementLine(impID, "R000000001")
		runConstraintCases(t, db, []constraintCase{
			{name: "the same period twice is refused", table: "statement_imports", r: with(imp, row{"id": uuid.New()}), code: uniqueViolation},
			{name: "the period must end after it starts", table: "statement_imports", r: with(statementImport(pb, nil), row{"period_end": now.Add(-48 * time.Hour)}), code: checkViolation},
			{name: "an unknown source is refused", table: "statement_imports", r: with(statementImport(pb, nil), row{"period_start": now.Add(-72 * time.Hour), "source": "fax"}), code: checkViolation},
			{name: "an unmatched line", table: "statement_lines", r: line},
			{name: "a matched line", table: "statement_lines", r: with(line, row{"match_status": "matched", "matched_type": "collection_notice", "matched_id": uuid.New()})},
			{name: "matched needs what it matched", table: "statement_lines", r: with(line, row{"match_status": "matched"}), code: checkViolation},
			{name: "unmatched names nothing", table: "statement_lines", r: with(line, row{"matched_type": "payout", "matched_id": uuid.New()}), code: checkViolation},
			{name: "an unknown matched type is refused", table: "statement_lines", r: with(line, row{"match_status": "matched", "matched_type": "lottery", "matched_id": uuid.New()}), code: checkViolation},
			{name: "a negative amount is refused", table: "statement_lines", r: with(line, row{"amount_minor": int64(-1)}), code: checkViolation},
			{name: "negative charges are refused", table: "statement_lines", r: with(line, row{"charges_minor": int64(-1)}), code: checkViolation},
			{name: "an unknown direction is refused", table: "statement_lines", r: with(line, row{"direction": "X"}), code: checkViolation},
		})
	})

	t.Run("recon_exceptions", func(t *testing.T) {
		open := reconException(pb, nil)
		resolved := with(open, row{"status": "resolved", "resolution": "matched by hand", "resolved_by": uuid.New(), "resolved_at": now})
		runConstraintCases(t, db, []constraintCase{
			{name: "an open exception", table: "recon_exceptions", r: open},
			{name: "a ledger mismatch", table: "recon_exceptions", r: with(open, row{"kind": "ledger_mismatch"})},
			{name: "an unknown kind is refused", table: "recon_exceptions", r: with(open, row{"kind": "vibes"}), code: checkViolation},
			{name: "a resolved one", table: "recon_exceptions", r: resolved},
			{name: "written off", table: "recon_exceptions", r: with(resolved, row{"status": "written_off"})},
			{name: "resolved needs who, when and how", table: "recon_exceptions", r: with(resolved, row{"resolved_by": drop}), code: checkViolation},
			{name: "resolved needs a resolution", table: "recon_exceptions", r: with(resolved, row{"resolution": drop}), code: checkViolation},
			{name: "open has no resolution", table: "recon_exceptions", r: with(open, row{"resolved_at": now}), code: checkViolation},
		})
	})

	t.Run("topup_accounts", func(t *testing.T) {
		base := topupAccount(tenantA, pb, "ABC23456")
		runConstraintCases(t, db, []constraintCase{
			{name: "an account number", table: "topup_accounts", r: base},
			{name: "lower case is refused", table: "topup_accounts", r: with(base, row{"account_number": "abc23456"}), code: checkViolation},
			{name: "a zero is refused", table: "topup_accounts", r: with(base, row{"account_number": "ABC23450"}), code: checkViolation},
			{name: "a one is refused", table: "topup_accounts", r: with(base, row{"account_number": "ABC23451"}), code: checkViolation},
			{name: "an O is refused", table: "topup_accounts", r: with(base, row{"account_number": "ABC2345O"}), code: checkViolation},
			{name: "an I is refused", table: "topup_accounts", r: with(base, row{"account_number": "ABC2345I"}), code: checkViolation},
			{name: "seven characters are refused", table: "topup_accounts", r: with(base, row{"account_number": "ABC2345"}), code: checkViolation},
			{name: "a second number for a tenant is refused", before: []tableRow{{"topup_accounts", topupAccount(tenantA, pb, "XYZ23456")}}, table: "topup_accounts", r: base, code: uniqueViolation},
			{name: "a number taken by another tenant is refused", before: []tableRow{{"topup_accounts", topupAccount(tenantB, pb, "ABC23456")}}, table: "topup_accounts", r: base, code: uniqueViolation},
		})
	})

	t.Run("stk_callbacks", func(t *testing.T) {
		cb := stkCallback(pb, "ws_CO_1")
		runConstraintCases(t, db, []constraintCase{
			{name: "a parked callback", table: "stk_callbacks", r: cb},
			{name: "an applied callback", table: "stk_callbacks", r: with(cb, row{"payment_intent_id": uuid.New(), "applied_at": now})},
			{name: "applied needs its intent", table: "stk_callbacks", r: with(cb, row{"applied_at": now}), code: checkViolation},
			{name: "an intent needs applied_at", table: "stk_callbacks", r: with(cb, row{"payment_intent_id": uuid.New()}), code: checkViolation},
			{name: "the same checkout twice is refused", before: []tableRow{{"stk_callbacks", stkCallback(pb, "ws_CO_1")}}, table: "stk_callbacks", r: cb, code: uniqueViolation},
		})
	})
}
