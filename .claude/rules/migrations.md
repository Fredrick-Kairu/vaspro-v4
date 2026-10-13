---
paths:
  - "migrations/**"
---
# Migration rules
- One folder per schema. File names: goose timestamps with a short description.
- Never edit a migration that is on main. Add a new one.
- Follow docs/design/data-model.md exactly: names, types, constraints, indexes. If you must differ, stop and ask.
- Every client-owned table: tenant_id uuid NOT NULL, ENABLE ROW LEVEL SECURITY, and the tenant_isolation policy.
- Money columns are bigint *_minor plus a currency. Timestamps are timestamptz.
- Every migration has a Down section unless it is irreversible; say so in a comment if it is.
- Add a repository test for every constraint, trigger and policy you create.
