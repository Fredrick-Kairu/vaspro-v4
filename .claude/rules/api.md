---
paths:
  - "docs/api/**"
  - "internal/*/api/**"
---
# API rules
- OpenAPI 3.1 in docs/api/ is the source of truth; regenerate code with make gen, never edit generated files.
- Every write operation requires an Idempotency-Key header.
- Errors are application/problem+json with a code from docs/errors/catalogue.yaml.
- Every operation has a realistic example using reserved test numbers; examples run in e2e.
- Paths are versioned (/v1/...); breaking changes need a new version and an ADR.
- Pagination is cursor-based; amounts are minor units with a currency.
