---
paths:
  - "**/*.go"
---
# Go rules
- Errors: wrap with %w and context; return platform/problem errors with a catalogue code at API boundaries. Never panic in request paths.
- context.Context is the first parameter of anything that does I/O.
- Money: platform/money types only. Tokens: shopspring/decimal. No float64 for either.
- Time: platform/clock only; never time.Now() directly.
- IDs: platform/ids (UUIDv7). Public IDs use the service's prefix.
- Logging: slog with request_id, tenant_id and trace_id. Never log secrets, tokens, full phone numbers or message bodies.
- Database: every client-owned query runs inside db.WithTenantTx. No string-built SQL; sqlc only.
- Events: publish only through platform/outbox in the same transaction as the change.
- Adapters live behind interfaces; tests use the fakes in tools/fakes.
- Keep functions small; domain/ has no imports of store/, adapters/ or net/http.
