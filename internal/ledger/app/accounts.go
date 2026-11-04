package app

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/store"
)

// errSystemAccountsMissing: a posting names a VASPRO account that isn't
// open yet. A tenant's transaction can't open one (RLS), so the use
// case opens it as ledger_system and runs again.
var errSystemAccountsMissing = errors.New("app: VASPRO accounts to open")

// inOwnerTx runs fn in the tenant's transaction, or as ledger_system
// for a system request (tenant nil).
func (s *Service) inOwnerTx(ctx context.Context, tenant *uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if tenant == nil {
		return s.pool.WithRoleTx(ctx, systemRole, fn)
	}
	return s.pool.WithTenantTx(ctx, *tenant, fn)
}

// withSystemAccounts runs fn, and when it reports VASPRO accounts
// missing, opens them and runs it once more.
func (s *Service) withSystemAccounts(ctx context.Context, keys []domain.AccountKey, fn func() error) error {
	err := fn()
	if !errors.Is(err, errSystemAccountsMissing) {
		return err
	}
	if err := s.openSystemAccounts(ctx, keys); err != nil {
		return err
	}
	return fn()
}

// openSystemAccounts opens VASPRO's accounts among keys, in
// ledger_system's own transaction. Opening is idempotent.
func (s *Service) openSystemAccounts(ctx context.Context, keys []domain.AccountKey) error {
	return s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		for _, k := range keys {
			if domain.IsClientOwned(k.Type) {
				continue
			}
			id, err := ids.New()
			if err != nil {
				return err
			}
			if err := q.OpenAccount(ctx, store.OpenAccountParams{
				ID: id, Type: store.LedgerAccountType(k.Type), ServiceCode: text(k.ServiceCode),
				Rail: text(k.Rail), Currency: k.Currency,
			}); err != nil {
				return fmt.Errorf("app: opening VASPRO's %s account: %w", k.Type, err)
			}
		}
		return nil
	})
}

// resolveAccounts finds every account the keys name: the tenant's own,
// opened on first use in its transaction, and VASPRO's, which must
// already be open (errSystemAccountsMissing otherwise).
func resolveAccounts(ctx context.Context, q *store.Queries, tenant *uuid.UUID, keys []domain.AccountKey) (map[domain.AccountKey]store.LedgerAccount, error) {
	accts := make(map[domain.AccountKey]store.LedgerAccount, len(keys))
	for _, k := range keys {
		if _, done := accts[k]; done {
			continue
		}
		var (
			a   store.LedgerAccount
			err error
		)
		if domain.IsClientOwned(k.Type) {
			a, err = resolveClientAccount(ctx, q, *tenant, k)
		} else {
			a, err = q.FindSystemAccount(ctx, store.FindSystemAccountParams{
				Type: store.LedgerAccountType(k.Type), Currency: k.Currency,
				ServiceCode: text(k.ServiceCode), Rail: text(k.Rail),
			})
			if errors.Is(err, pgx.ErrNoRows) {
				err = errSystemAccountsMissing
			}
		}
		if err != nil {
			return nil, err
		}
		accts[k] = a
	}
	return accts, nil
}

// resolveClientAccount finds the tenant's account by key, opening it if
// it isn't there. A concurrent opener's row wins the ON CONFLICT and is
// read back.
func resolveClientAccount(ctx context.Context, q *store.Queries, tenant uuid.UUID, k domain.AccountKey) (store.LedgerAccount, error) {
	find := store.FindClientAccountParams{
		TenantID: pgUUID(&tenant), Type: store.LedgerAccountType(k.Type), Currency: k.Currency,
		ServiceCode: text(k.ServiceCode), Rail: text(k.Rail),
	}
	a, err := q.FindClientAccount(ctx, find)
	if !errors.Is(err, pgx.ErrNoRows) {
		return a, err
	}
	id, err := ids.New()
	if err != nil {
		return a, err
	}
	if err := q.OpenAccount(ctx, store.OpenAccountParams{
		ID: id, TenantID: pgUUID(&tenant), Type: store.LedgerAccountType(k.Type),
		ServiceCode: text(k.ServiceCode), Rail: text(k.Rail), Currency: k.Currency,
	}); err != nil {
		return a, fmt.Errorf("app: opening the %s account: %w", k.Type, err)
	}
	return q.FindClientAccount(ctx, find)
}

// lockBalances locks the client accounts' balance rows in account_id
// order (ADR 0010) and returns them by account.
func lockBalances(ctx context.Context, q *store.Queries, accounts []uuid.UUID) (map[uuid.UUID]store.LockBalancesRow, error) {
	if len(accounts) == 0 {
		return nil, nil // a system posting: VASPRO's accounts have no balance rows (ADR 0010)
	}
	sorted := slices.Clone(accounts)
	slices.SortFunc(sorted, func(a, b uuid.UUID) int { return compareUUID(a, b) })
	sorted = slices.Compact(sorted)
	rows, err := q.LockBalances(ctx, sorted)
	if err != nil {
		return nil, fmt.Errorf("app: locking balances: %w", err)
	}
	out := make(map[uuid.UUID]store.LockBalancesRow, len(rows))
	for _, r := range rows {
		out[r.AccountID] = r
	}
	if len(out) != len(sorted) {
		return nil, fmt.Errorf("app: %d of %d balance rows found", len(out), len(sorted))
	}
	return out, nil
}

func keyOf(typ store.LedgerAccountType, service, rail pgtype.Text, currency string) domain.AccountKey {
	return domain.AccountKey{Type: domain.AccountType(typ), ServiceCode: service.String, Rail: rail.String, Currency: currency}
}

func accountKey(a store.LedgerAccount) domain.AccountKey {
	return keyOf(a.Type, a.ServiceCode, a.Rail, a.Currency)
}

func compareUUID(a, b uuid.UUID) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func pgUUID(u *uuid.UUID) pgtype.UUID {
	if u == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *u, Valid: true}
}

// uniqueViolationOn reports whether err is a unique violation of the
// named constraint.
func uniqueViolationOn(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
