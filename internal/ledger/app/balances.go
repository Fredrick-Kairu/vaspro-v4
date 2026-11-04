package app

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/store"
)

// GetBalances lists the tenant's accounts with their balances, or, for
// tenant nil, VASPRO's accounts with balances summed from the journal
// (ADR 0010), read as ledger_system.
func (s *Service) GetBalances(ctx context.Context, tenant *uuid.UUID) ([]Balance, error) {
	var out []Balance
	err := s.inOwnerTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if tenant == nil {
			rows, err := q.ListSystemAccountBalances(ctx)
			if err != nil {
				return err
			}
			for _, r := range rows {
				out = append(out, Balance{
					AccountID: r.ID, Account: keyOf(r.Type, r.ServiceCode, r.Rail, r.Currency), Status: r.Status,
					BalanceMinor: r.BalanceMinor, AvailableMinor: r.BalanceMinor,
				})
			}
			return nil
		}
		rows, err := q.ListClientBalances(ctx, pgUUID(tenant))
		if err != nil {
			return err
		}
		for _, r := range rows {
			out = append(out, Balance{
				AccountID: r.ID, Account: keyOf(r.Type, r.ServiceCode, r.Rail, r.Currency), Status: r.Status,
				BalanceMinor: r.BalanceMinor, HeldMinor: r.HeldMinor, AvailableMinor: r.BalanceMinor - r.HeldMinor,
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("app: reading balances: %w", err)
	}
	return out, nil
}
