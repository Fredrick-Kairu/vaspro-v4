# Open questions

Questions found during the build that the design doesn't answer. Each one gets an ID, the date, who raised it, the task it blocks, and the answer once known (then turn it into an ADR if it changes the design).

| ID | Date | Question | Blocks | Answer |
|---|---|---|---|---|
| OQ-01 | 2026-10-21 | `identity.api_client_secrets` (data model §3) has no `tenant_id` column, unlike every other client-owned table in the schema — it's reached only through `api_client_id` → `identity.api_clients.tenant_id`, which does have RLS. Is the missing `tenant_id` intentional (join-scoped access is enough, since nothing should ever query `api_client_secrets` directly without going through its parent), or a gap that should get its own `tenant_id` + RLS policy? | Task 1.7 (API clients: create, rotate, revoke) | — |
