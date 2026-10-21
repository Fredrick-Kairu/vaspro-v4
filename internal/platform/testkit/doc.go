// Package testkit gives tests the infrastructure .claude/rules/tests.md
// requires — PostgreSQL, NATS, Redis and RabbitMQ through
// testcontainers-go, each started with one call that cleans itself up
// via t.Cleanup — plus a few container-free helpers useful in any test:
// a re-exported fake clock, a golden-file assertion, and a generic
// row-level-security assertion that exercises any tenant-scoped table
// through platform/db.WithTenantTx.
//
// Concrete business-entity factories (tenants, users, wallets, messages
// — ROADMAP §8.4) are deliberately not here: those schemas belong to
// accounts, identity and messaging, none of which exist yet in Phase 0.
// platform holds mechanisms only, never business logic (CLAUDE.md) — each
// service adds its own factories once its own schema and sqlc code land.
package testkit
