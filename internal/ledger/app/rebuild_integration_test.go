//go:build integration

package app_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

var runDate = time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)

func rebuild(t *testing.T, h harness, date time.Time) app.RebuildSummary {
	t.Helper()
	sum, err := h.svc.RebuildBalances(context.Background(), date)
	if err != nil {
		t.Fatalf("RebuildBalances: %v", err)
	}
	return sum
}

// rebuildLedger is a ledger with every kind of row: the worked example's
// top-up, purchase, hold, capture and settlement, a money hold, and a
// reversal paid back.
func rebuildLedger(t *testing.T, h harness) (tenant uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tenant = uuid.New()
	top := topUp(t, h, tenant, 1_300_000)
	if _, err := h.svc.PurchaseTokens(ctx, purchase(tenant, "sms", "KES", "10000")); err != nil {
		t.Fatal(err)
	}
	ref := uuid.New()
	if _, err := h.svc.PlaceHold(ctx, app.HoldRequest{TenantID: tenant, Account: smsKES, ReferenceType: "campaign", ReferenceID: ref,
		Tokens: tok("400"), ExpiresAt: testNow.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.CaptureHold(ctx, app.CaptureRequest{TenantID: tenant, ReferenceType: "campaign", ReferenceID: ref, CapturedTokens: tok("380")}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SettleUsage(ctx, testNow.Add(app.DefaultSettlementWindow)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.PlaceHold(ctx, app.HoldRequest{TenantID: tenant, Account: mainKES, ReferenceType: "payout", ReferenceID: uuid.New(),
		AmountMinor: 1_000, ExpiresAt: testNow.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	maker, lead := newStaff(h), financeLead(h)
	r := approve(t, h, lead, raise(t, h, maker, reversalOf(tenant, top.TransactionID, domain.ReversalPayback, 300_000)).ID)
	if _, err := h.svc.RecordPayback(ctx, app.PaybackReport{TenantID: tenant, ReversalID: r.ID, Succeeded: true}); err != nil {
		t.Fatal(err)
	}
	return tenant
}

func accountID(t *testing.T, h harness, tenant uuid.UUID, key domain.AccountKey) uuid.UUID {
	t.Helper()
	return balanceOf(t, h, tenant, key).AccountID
}

func checksOf(sum app.RebuildSummary) map[string]int {
	out := map[string]int{}
	for _, d := range sum.Differences {
		out[d.Check]++
	}
	return out
}

// superTx runs fn as the superuser; replica turns triggers off, as only
// someone tampering with the database could.
func superTx(t *testing.T, h harness, replica bool, fn func(tx pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	err := pgx.BeginFunc(ctx, h.super, func(tx pgx.Tx) error {
		if replica {
			if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
				return err
			}
		}
		return fn(tx)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func exec(t *testing.T, h harness, replica bool, sql string, args ...any) {
	t.Helper()
	superTx(t, h, replica, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), sql, args...)
		return err
	})
}

// TestRebuild_CleanLedgerPasses: a ledger built only through the
// service agrees with its journal everywhere.
func TestRebuild_CleanLedgerPasses(t *testing.T) {
	h := newHarness(t)
	rebuildLedger(t, h)
	sum := rebuild(t, h, runDate)
	if !sum.Ran || !sum.Passed || len(sum.Differences) != 0 {
		t.Fatalf("clean ledger: %+v", sum)
	}
	// Main, SMS, M-Pesa clearing, revenue, VAT and reversal clearing; the
	// top-up, purchase, usage, reversal and pay-back.
	if sum.AccountsChecked != 6 || sum.TransactionsChecked != 5 {
		t.Fatalf("checked %d accounts and %d transactions, want 6 and 5", sum.AccountsChecked, sum.TransactionsChecked)
	}
	data := assertEventValid(t, h, "ledger.rebuild_passed", sum.RunID)
	if data["run_date"] != "2026-11-09" {
		t.Fatalf("run_date %v", data["run_date"])
	}
	if got := count(t, h, `SELECT count(*) FROM rebuild_runs WHERE run_date = '2026-11-09' AND status = 'passed'`); got != 1 {
		t.Fatalf("%d passed runs recorded, want 1", got)
	}
}

// TestRebuild_TamperedBalance is the ROADMAP's tamper test: a balance
// row changed directly is found, the P1 event goes out, and the row is
// left as it is (report only, OQ-19).
func TestRebuild_TamperedBalance(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := rebuildLedger(t, h)
	main := accountID(t, h, tenant, mainKES)
	before := balanceOf(t, h, tenant, mainKES).BalanceMinor
	exec(t, h, false, `UPDATE balances SET balance_minor = balance_minor + 1 WHERE account_id = $1`, main)

	sum := rebuild(t, h, runDate)
	if !sum.Ran || sum.Passed || len(sum.Differences) != 1 {
		t.Fatalf("after tampering: %+v", sum)
	}
	d := sum.Differences[0]
	if d.Check != app.CheckBalanceMismatch || *d.AccountID != main || *d.TenantID != tenant || d.Currency != "KES" {
		t.Fatalf("difference %+v", d)
	}
	if d.Expected != itoa(before) || d.Actual != itoa(before+1) {
		t.Fatalf("expected %s actual %s, want %d and %d", d.Expected, d.Actual, before, before+1)
	}
	data := assertEventValid(t, h, "ledger.rebuild_failed", sum.RunID)
	if data["severity"] != "P1" || data["differences_total"] != float64(1) {
		t.Fatalf("event %v", data)
	}
	if got := balanceOf(t, h, tenant, mainKES).BalanceMinor; got != before+1 {
		t.Fatalf("balance %d after the run, want the tampered %d left for finance", got, before+1)
	}
	var status string
	if err := h.super.QueryRow(ctx, `SELECT status FROM rebuild_runs WHERE id = $1`, sum.RunID).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("run status %q, %v", status, err)
	}
}

// TestRebuild_EachCheck: each kind of tampering is reported under its
// own check.
func TestRebuild_EachCheck(t *testing.T) {
	tests := []struct {
		name   string
		tamper func(t *testing.T, h harness, tenant uuid.UUID)
		want   map[string]int
	}{
		{"held money", func(t *testing.T, h harness, tenant uuid.UUID) {
			exec(t, h, false, `UPDATE balances SET held_minor = held_minor + 1 WHERE account_id = $1`, accountID(t, h, tenant, mainKES))
		}, map[string]int{app.CheckHeldMinorMismatch: 1}},
		{"held tokens", func(t *testing.T, h harness, tenant uuid.UUID) {
			exec(t, h, false, `UPDATE balances SET held_tokens = held_tokens + 0.5 WHERE account_id = $1`, accountID(t, h, tenant, smsKES))
		}, map[string]int{app.CheckHeldTokensMismatch: 1}},
		{"a lot's remaining tokens", func(t *testing.T, h harness, tenant uuid.UUID) {
			exec(t, h, false, `UPDATE token_lots SET tokens_remaining = tokens_remaining - 1 WHERE tenant_id = $1`, tenant)
		}, map[string]int{app.CheckLotRemainingMismatch: 1}},
		{"a service wallet's money", func(t *testing.T, h harness, tenant uuid.UUID) {
			exec(t, h, false, `UPDATE balances SET balance_minor = balance_minor - 1 WHERE account_id = $1`, accountID(t, h, tenant, smsKES))
		}, map[string]int{app.CheckBalanceMismatch: 1, app.CheckWalletValueMismatch: 1}},
		{"a deleted balance row", func(t *testing.T, h harness, tenant uuid.UUID) {
			exec(t, h, true, `DELETE FROM balances WHERE account_id = $1`, accountID(t, h, tenant, recvKES))
		}, map[string]int{app.CheckBalanceRowMissing: 1}},
		{"a wallet below its floor", func(t *testing.T, h harness, tenant uuid.UUID) {
			exec(t, h, true, `UPDATE balances SET balance_minor = -5, held_minor = 0 WHERE account_id = $1`, accountID(t, h, tenant, mainKES))
		}, map[string]int{app.CheckBalanceMismatch: 1, app.CheckWalletFloor: 1, app.CheckHeldMinorMismatch: 1}},
		{"an unbalanced transaction with one line", func(t *testing.T, h harness, tenant uuid.UUID) {
			txn := uuid.New()
			superTx(t, h, true, func(tx pgx.Tx) error {
				ctx := context.Background()
				if _, err := tx.Exec(ctx, `INSERT INTO transactions (id, tenant_id, type, idempotency_key, description) VALUES ($1, $2, 'adjustment', 'tamper', 'tamper')`, txn, tenant); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `INSERT INTO entries (transaction_id, account_id, direction, amount_minor, currency) VALUES ($1, $2, 'C', 5, 'KES')`,
					txn, accountID(t, h, tenant, recvKES))
				return err
			})
		}, map[string]int{app.CheckTransactionUnbalanced: 1, app.CheckTransactionThin: 1, app.CheckCurrencyUnbalanced: 1, app.CheckBalanceMismatch: 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tenant := rebuildLedger(t, h)
			// A receivable account with a balance row and no lines.
			if _, err := h.svc.Post(context.Background(), post(tenant, domain.TxnAdjustment, d(recvKES, 1), c(mainKES, 1))); err != nil {
				t.Fatal(err)
			}
			if sum := rebuild(t, h, runDate); !sum.Passed {
				t.Fatalf("before tampering: %+v", sum.Differences)
			}
			tc.tamper(t, h, tenant)
			sum := rebuild(t, h, runDate.AddDate(0, 0, 1))
			if got := checksOf(sum); !equalCounts(got, tc.want) {
				t.Fatalf("checks %v, want %v (%+v)", got, tc.want, sum.Differences)
			}
			assertEventValid(t, h, "ledger.rebuild_failed", sum.RunID)
		})
	}
}

// TestRebuild_EventCapsDifferences: past 100 differences the event
// carries the first 100 and the true total.
func TestRebuild_EventCapsDifferences(t *testing.T) {
	h := newHarness(t)
	exec(t, h, false, `
		INSERT INTO accounts (id, tenant_id, type, currency)
		SELECT gen_random_uuid(), gen_random_uuid(), 'receivable', 'KES' FROM generate_series(1, 120)`)
	exec(t, h, false, `UPDATE balances SET balance_minor = 7`)
	sum := rebuild(t, h, runDate)
	if len(sum.Differences) != 120 {
		t.Fatalf("%d differences, want 120", len(sum.Differences))
	}
	data := assertEventValid(t, h, "ledger.rebuild_failed", sum.RunID)
	if data["differences_total"] != float64(120) || len(data["differences"].([]any)) != 100 {
		t.Fatalf("event total %v with %d listed, want 120 and 100", data["differences_total"], len(data["differences"].([]any)))
	}
}

// TestRebuild_OncePerDate: one run per date across replicas; a run
// that died is taken over after an hour.
func TestRebuild_OncePerDate(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if !rebuild(t, h, runDate).Ran {
		t.Fatal("the first run of the date didn't run")
	}
	if rebuild(t, h, runDate).Ran {
		t.Fatal("the date ran twice")
	}

	t.Run("concurrent replicas", func(t *testing.T) {
		date := runDate.AddDate(0, 0, 1)
		var wg sync.WaitGroup
		ran := make([]bool, 5)
		for i := range ran {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sum, err := h.svc.RebuildBalances(ctx, date)
				if err != nil {
					t.Error(err)
				}
				ran[i] = sum.Ran
			}()
		}
		wg.Wait()
		n := 0
		for _, r := range ran {
			if r {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%d replicas ran the date, want 1", n)
		}
		if got := count(t, h, `SELECT count(*) FROM outbox_events WHERE event_type = 'ledger.rebuild_passed'`); got != 2 {
			t.Fatalf("%d rebuild_passed events for 2 dates, want 2", got)
		}
	})

	t.Run("a run that died is taken over after an hour", func(t *testing.T) {
		stale, fresh := runDate.AddDate(0, 0, 2), runDate.AddDate(0, 0, 3)
		exec(t, h, false, `INSERT INTO rebuild_runs (id, run_date, status, started_at) VALUES ($1, $2, 'running', $3), ($4, $5, 'running', $6)`,
			uuid.New(), stale, h.clk.Now().Add(-61*time.Minute), uuid.New(), fresh, h.clk.Now().Add(-10*time.Minute))
		if sum := rebuild(t, h, stale); !sum.Ran || !sum.Passed {
			t.Fatalf("a stale run wasn't taken over: %+v", sum)
		}
		if rebuild(t, h, fresh).Ran {
			t.Fatal("a run still within its hour was taken over")
		}
	})
}

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
