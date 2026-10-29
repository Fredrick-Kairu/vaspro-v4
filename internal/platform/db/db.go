// Package db wraps a pgx connection pool with VASPRO's tenant-isolation
// pattern: every client-owned query runs inside WithTenantTx, which sets
// app.tenant_id for that transaction so each table's row-level security
// policy (data model §1) can enforce it.
package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool is a pgx connection pool.
type Pool struct {
	*pgxpool.Pool
}

// NewPool parses dsn and opens a pool against it.
func NewPool(ctx context.Context, dsn string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: parsing dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: connecting: %w", err)
	}
	return &Pool{pool}, nil
}

// WithTenantTx runs fn inside a transaction with app.tenant_id set to
// tenantID for that transaction only, via set_config(..., true) rather
// than string-built SQL (.claude/rules/go.md) — the same effect as
// `SET LOCAL app.tenant_id = tenantID`, which Postgres's SET statement
// can't take as a query parameter. Commits if fn returns nil, otherwise
// rolls back and returns fn's error.
func (p *Pool) WithTenantTx(ctx context.Context, tenantID uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed; rollback failing after a commit isn't actionable here

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String()); err != nil {
		return fmt.Errorf("db: setting app.tenant_id: %w", err)
	}

	if err := fn(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}

// WithRoleTx runs fn inside a transaction with the current role
// switched to role for that transaction only (SET LOCAL ROLE), reverting
// automatically at commit or rollback. Unlike WithTenantTx's
// set_config, Postgres's SET statement can't take ROLE as a bind
// parameter at all — role must always be a hardcoded Go constant at
// the call site, never request data, so pgx.Identifier's quoting below
// is defensive, not a substitute for that rule. The connecting role
// must already have been GRANTed membership in role (a migration's
// job, not this function's) or SET LOCAL ROLE fails.
func (p *Pool) WithRoleTx(ctx context.Context, role string, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed; rollback failing after a commit isn't actionable here

	stmt := "SET LOCAL ROLE " + pgx.Identifier{role}.Sanitize()
	if _, err := tx.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("db: setting role: %w", err)
	}

	if err := fn(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}
