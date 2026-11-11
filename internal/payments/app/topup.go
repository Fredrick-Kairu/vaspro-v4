package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/store"
)

// maxNumberAttempts bounds the retries when a fresh account number is
// already taken: with 32⁶ numbers per paybill that's vanishingly rare,
// so running out means something is wrong, not unlucky.
const maxNumberAttempts = 5

// TopupDetails is where and how an account tops up its main wallet.
type TopupDetails struct {
	Rail          string
	PaybillNumber string
	PaybillName   string
	AccountNumber string
	Currency      string
}

// requireTenant asks accounts whether user may work in tenant with
// perm, and learns its country and currency (task 4.3). No access is
// ErrNotFound, no permission ErrPermissionDenied; accounts being down
// fails closed.
func (s *Service) requireTenant(ctx context.Context, user, tenant uuid.UUID, perm string) (adapters.TenantInfo, error) {
	if s.accounts == nil {
		return adapters.TenantInfo{}, errors.New("app: no accounts adapter for access checks")
	}
	info, err := s.accounts.TenantAccess(ctx, user, tenant, perm)
	switch {
	case errors.Is(err, adapters.ErrNoTenantAccess):
		return adapters.TenantInfo{}, fmt.Errorf("%w: no account %s", ErrNotFound, tenant)
	case errors.Is(err, adapters.ErrPermissionDenied):
		return adapters.TenantInfo{}, fmt.Errorf("%w: %s", ErrPermissionDenied, perm)
	case err != nil:
		return adapters.TenantInfo{}, fmt.Errorf("app: checking account access: %w", err)
	}
	return info, nil
}

// TopupDetails is VASPRO's paybill for the account's country and the
// account's own number on it (task 4.3, OQ-22). The number is issued
// the first time it's asked for and never changes. Only a verified
// (active) paybill is offered: ErrTopupUnavailable otherwise.
func (s *Service) TopupDetails(ctx context.Context, user, tenant uuid.UUID) (TopupDetails, error) {
	info, err := s.requireTenant(ctx, user, tenant, PermBillingRead)
	if err != nil {
		return TopupDetails{}, err
	}
	pack, ok := s.packs[info.CountryCode]
	if !ok {
		return TopupDetails{}, fmt.Errorf("%w: no pack for %s", ErrTopupUnavailable, info.CountryCode)
	}
	rail, ok := pack.TopupRail()
	if !ok {
		return TopupDetails{}, fmt.Errorf("%w: %s has no top-up rail", ErrTopupUnavailable, info.CountryCode)
	}

	var out TopupDetails
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		paybill, err := q.GetActiveTopupRailAccount(ctx, rail.Code)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no verified paybill on %s", ErrTopupUnavailable, rail.Code)
		}
		if err != nil {
			return fmt.Errorf("app: loading VASPRO's paybill: %w", err)
		}
		number, err := s.ensureTopupAccount(ctx, q, tenant, paybill.ID)
		if err != nil {
			return err
		}
		out = TopupDetails{
			Rail: rail.Code, PaybillNumber: paybill.Number, PaybillName: paybill.DisplayName,
			AccountNumber: number, Currency: info.Currency,
		}
		return nil
	})
	return out, err
}

// ensureTopupAccount returns tenant's number on paybill, issuing one if
// it has none. A concurrent first call that wins the insert is read
// back; a number another tenant already has is replaced by a fresh one.
func (s *Service) ensureTopupAccount(ctx context.Context, q *store.Queries, tenant, paybill uuid.UUID) (string, error) {
	key := store.GetTopupAccountForTenantParams{TenantID: tenant, RailAccountID: paybill}
	for range maxNumberAttempts {
		existing, err := q.GetTopupAccountForTenant(ctx, key)
		if err == nil {
			return existing.AccountNumber, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("app: loading the account number: %w", err)
		}
		number, err := domain.NewAccountNumber(s.rand)
		if err != nil {
			return "", err
		}
		id, err := ids.New()
		if err != nil {
			return "", err
		}
		row, err := q.InsertTopupAccount(ctx, store.InsertTopupAccountParams{ID: id, TenantID: tenant, RailAccountID: paybill, AccountNumber: number})
		if err == nil {
			return row.AccountNumber, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("app: issuing an account number: %w", err)
		}
		// Nothing inserted: another call issued this tenant's number
		// (read it next time round), or the number was taken.
	}
	return "", fmt.Errorf("app: no free account number after %d attempts", maxNumberAttempts)
}
