package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"

	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/store"
)

// auditRole reads every client's journal and its copies (migration
// 20261106170000).
const auditRole = "ledger_audit"

// maxEventDifferences bounds the differences one rebuild_failed event
// lists; it always carries the true total.
const maxEventDifferences = 100

// The nightly rebuild's checks (task 3.8, OQ-19).
const (
	CheckTransactionUnbalanced = "transaction_unbalanced"
	CheckTransactionThin       = "transaction_thin"
	CheckCurrencyUnbalanced    = "currency_unbalanced"
	CheckBalanceMismatch       = "balance_mismatch"
	CheckBalanceRowMissing     = "balance_row_missing"
	CheckHeldMinorMismatch     = "held_minor_mismatch"
	CheckHeldTokensMismatch    = "held_tokens_mismatch" //nolint:gosec // a check name: "tokens" are service units, not credentials
	CheckLotRemainingMismatch  = "lot_remaining_mismatch"
	CheckWalletValueMismatch   = "wallet_value_mismatch"
	CheckWalletFloor           = "wallet_floor"
)

// Difference is one thing the rebuild found that doesn't agree with the
// journal. Expected is what the journal says; Actual is what the copy
// holds.
type Difference struct {
	Check         string
	TenantID      *uuid.UUID
	AccountID     *uuid.UUID
	TransactionID *uuid.UUID
	LotID         *uuid.UUID
	Currency      string
	Expected      string
	Actual        string
}

// RebuildSummary is one run's outcome. Ran is false when another
// replica has the date.
type RebuildSummary struct {
	Ran                 bool
	RunID               uuid.UUID
	RunDate             time.Time
	Passed              bool
	AccountsChecked     int64
	TransactionsChecked int64
	Differences         []Difference
}

// RebuildBalances is the nightly proof of correctness (data model §5,
// ADR 0010, task 3.8): it rebuilds every balance and copy from the
// journal and compares. It only reports (OQ-19): a difference is a bug
// or tampering, so nothing is changed, and a fix is a posting staff
// make. One run per date across replicas: the one that claims the
// date's row runs it, and a run that died is taken over after an hour.
// It publishes ledger.rebuild_failed (P1) or ledger.rebuild_passed. A
// check that can't run is an error, never a pass.
func (s *Service) RebuildBalances(ctx context.Context, runDate time.Time) (RebuildSummary, error) {
	runDate = time.Date(runDate.Year(), runDate.Month(), runDate.Day(), 0, 0, 0, 0, time.UTC)
	sum := RebuildSummary{RunDate: runDate}
	id, err := ids.New()
	if err != nil {
		return sum, err
	}
	started := s.clk.Now()
	var run store.LedgerRebuildRun
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		run, err = store.New(tx).ClaimRebuildRun(ctx, store.ClaimRebuildRunParams{
			ID: id, RunDate: pgtype.Date{Time: runDate, Valid: true}, StartedAt: started,
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return sum, nil // another replica has the date
	}
	if err != nil {
		return sum, fmt.Errorf("app: claiming the rebuild for %s: %w", runDate.Format(time.DateOnly), err)
	}
	sum.Ran, sum.RunID = true, run.ID

	err = s.pool.WithRoleTx(ctx, auditRole, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		scope, err := q.CountRebuildScope(ctx)
		if err != nil {
			return err
		}
		sum.AccountsChecked, sum.TransactionsChecked = scope.Accounts, scope.Transactions
		sum.Differences, err = runChecks(ctx, q)
		return err
	})
	if err != nil {
		return sum, fmt.Errorf("app: rebuild checks for %s: %w", runDate.Format(time.DateOnly), err)
	}
	sum.Passed = len(sum.Differences) == 0
	if err := s.finishRebuild(ctx, run, &sum); err != nil {
		return sum, err
	}
	if !sum.Passed {
		s.log.ErrorContext(ctx, "ledger: P1 the nightly rebuild found differences from the journal",
			"run_date", runDate.Format(time.DateOnly), "differences", len(sum.Differences))
	}
	return sum, nil
}

func (s *Service) finishRebuild(ctx context.Context, run store.LedgerRebuildRun, sum *RebuildSummary) error {
	status, eventType := "passed", "ledger.rebuild_passed"
	if !sum.Passed {
		status, eventType = "failed", "ledger.rebuild_failed"
	}
	now := s.clk.Now()
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := store.New(tx).FinishRebuildRun(ctx, store.FinishRebuildRunParams{
			ID: run.ID, StartedAt: run.StartedAt, Status: status, FinishedAt: pgtype.Timestamptz{Time: now, Valid: true},
			AccountsChecked:     pgtype.Int8{Int64: sum.AccountsChecked, Valid: true},
			TransactionsChecked: pgtype.Int8{Int64: sum.TransactionsChecked, Valid: true},
			Differences:         pgtype.Int4{Int32: int32(min(len(sum.Differences), 1<<31-1)), Valid: true}, //nolint:gosec // bounded just above
		})
		if errors.Is(err, pgx.ErrNoRows) {
			sum.Ran = false // a replica that took the run over finished it first
			return nil
		}
		if err != nil {
			return fmt.Errorf("app: finishing the rebuild: %w", err)
		}
		data := map[string]any{
			"run_id": run.ID, "run_date": sum.RunDate.Format(time.DateOnly),
			"accounts_checked": sum.AccountsChecked, "transactions_checked": sum.TransactionsChecked,
		}
		if !sum.Passed {
			listed := sum.Differences[:min(len(sum.Differences), maxEventDifferences)]
			out := make([]map[string]any, len(listed))
			for i, d := range listed {
				out[i] = differenceData(d)
			}
			data["severity"], data["differences_total"], data["differences"] = "P1", len(sum.Differences), out
		}
		eventID, err := ids.New()
		if err != nil {
			return err
		}
		env, err := platformevents.New(eventID, eventType, 1, uuid.Nil, now, "", data)
		if err != nil {
			return fmt.Errorf("app: building %s: %w", eventType, err)
		}
		return outbox.Write(ctx, tx, "rebuild_run", run.ID, env)
	})
}

// runChecks runs every check (each one statement, so each reads one
// consistent snapshot) and gathers what they find.
func runChecks(ctx context.Context, q *store.Queries) ([]Difference, error) {
	var out []Difference
	add := func(d Difference) { out = append(out, d) }

	txns, err := q.RebuildUnbalancedTransactions(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range txns {
		add(Difference{Check: CheckTransactionUnbalanced, TenantID: optionalID(r.TenantID), TransactionID: &r.TransactionID,
			Currency: r.Currency, Expected: "debits = credits", Actual: fmt.Sprintf("debits %d, credits %d", r.Debits, r.Credits)})
	}
	thin, err := q.RebuildThinTransactions(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range thin {
		add(Difference{Check: CheckTransactionThin, TenantID: optionalID(r.TenantID), TransactionID: &r.TransactionID,
			Expected: "at least 2 lines", Actual: strconv.FormatInt(r.Lines, 10) + " lines"})
	}
	curs, err := q.RebuildUnbalancedCurrencies(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range curs {
		add(Difference{Check: CheckCurrencyUnbalanced, Currency: r.Currency,
			Expected: "debits = credits", Actual: fmt.Sprintf("debits %d, credits %d", r.Debits, r.Credits)})
	}
	bals, err := q.RebuildBalanceMismatches(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range bals {
		add(accountDifference(CheckBalanceMismatch, r.AccountID, r.TenantID, r.Currency, minor(r.Expected), minor(r.Actual)))
	}
	missing, err := q.RebuildMissingBalanceRows(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range missing {
		add(accountDifference(CheckBalanceRowMissing, r.AccountID, r.TenantID, r.Currency, "a balance row", "none"))
	}
	heldMinor, err := q.RebuildHeldMinorMismatches(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range heldMinor {
		add(accountDifference(CheckHeldMinorMismatch, r.AccountID, r.TenantID, r.Currency, minor(r.Expected), minor(r.Actual)))
	}
	heldTokens, err := q.RebuildHeldTokensMismatches(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range heldTokens {
		add(accountDifference(CheckHeldTokensMismatch, r.AccountID, r.TenantID, r.Currency, tokens(r.Expected), tokens(r.Actual)))
	}
	lots, err := q.RebuildLotRemainingMismatches(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range lots {
		d := accountDifference(CheckLotRemainingMismatch, r.AccountID, pgUUID(&r.TenantID), "", tokens(r.Expected), tokens(r.Actual))
		d.LotID = &r.LotID
		add(d)
	}
	values, err := q.RebuildWalletValueMismatches(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range values {
		add(accountDifference(CheckWalletValueMismatch, r.AccountID, r.TenantID, r.Currency, minor(r.Expected), minor(r.Actual)))
	}
	floors, err := q.RebuildWalletFloorBreaches(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range floors {
		add(accountDifference(CheckWalletFloor, r.AccountID, r.TenantID, r.Currency,
			"balance >= 0, held <= balance, held tokens <= lots' remaining",
			fmt.Sprintf("balance %d, held %d, held tokens %s, lots' remaining %s", r.BalanceMinor, r.HeldMinor, tokens(r.HeldTokens), tokens(r.LotsRemaining))))
	}
	return out, nil
}

func accountDifference(check string, account uuid.UUID, tenant pgtype.UUID, currency, expected, actual string) Difference {
	return Difference{Check: check, TenantID: optionalID(tenant), AccountID: &account, Currency: currency, Expected: expected, Actual: actual}
}

func minor(n int64) string { return strconv.FormatInt(n, 10) }

// tokens is a numeric's text in its shortest form ("1.500000" → "1.5").
func tokens(s string) string {
	d, err := decimal.NewFromString(s)
	if err != nil {
		return s
	}
	return d.String()
}

func differenceData(d Difference) map[string]any {
	out := map[string]any{"check": d.Check, "expected": d.Expected, "actual": d.Actual}
	for k, v := range map[string]*uuid.UUID{"tenant_id": d.TenantID, "account_id": d.AccountID, "transaction_id": d.TransactionID, "lot_id": d.LotID} {
		if v != nil {
			out[k] = *v
		}
	}
	if d.Currency != "" {
		out["currency"] = d.Currency
	}
	return out
}
