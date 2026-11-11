package app

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/store"
)

// SyncRails makes payments.rails mirror the country packs (task 4.3,
// data model §7: "global; mirrors the country pack"). A rail no pack
// lists any more is retired, never deleted, because rail accounts
// reference it. One code in two packs is refused, so startup stops.
func (s *Service) SyncRails(ctx context.Context) error {
	type packRail struct {
		country string
		params  store.UpsertRailParams
	}
	var rails []packRail
	seen := map[string]string{}
	for country, p := range s.packs {
		for _, r := range p.Rails {
			if other, ok := seen[r.Code]; ok {
				return fmt.Errorf("%w: rail %s is in both the %s and %s packs", ErrValidation, r.Code, other, country)
			}
			seen[r.Code] = country
			rails = append(rails, packRail{country: country, params: store.UpsertRailParams{
				Code: r.Code, CountryCode: pgtype.Text{String: country, Valid: true},
				Kind: r.Kind, Capabilities: r.Capabilities, Status: r.Status,
			}})
		}
	}
	codes := make([]string, 0, len(rails))
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		for _, r := range rails {
			if _, err := q.UpsertRail(ctx, r.params); err != nil {
				return fmt.Errorf("app: syncing rail %s: %w", r.params.Code, err)
			}
			codes = append(codes, r.params.Code)
		}
		if _, err := q.RetireRailsNotIn(ctx, codes); err != nil {
			return fmt.Errorf("app: retiring rails no pack lists: %w", err)
		}
		return nil
	})
}
