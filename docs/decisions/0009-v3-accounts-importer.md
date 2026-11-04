# 0009. v3 accounts importer (design only)

**Date:** 2026-11-04 · **Status:** accepted (design only; build it when a v3 export or an anonymised v3 copy exists)

## Context
ROADMAP task 2.11 asks for v3 importers for clients, user-client links, permissions to roles, and resellers to parent tenants. Phase 2's exit gate wants a dry run on an anonymised v3 copy whose counts match v3.

Data model §4's "From v3" table names the tables:

| v3 table | v4 |
|---|---|
| `clients` (`client_name`, `client_email`, `country_id`) | `accounts.tenants` |
| `user_client_map` (`permission_acl`) | `accounts.memberships` |
| `client_resellers`, `client_reseller_accounts` | `tenants.type`, `parent_tenant_id` |

It doesn't give their other columns, how the data is exported, the `permission_acl` values, or the tier an imported client gets. No v3 export or anonymised copy exists in this environment (OQ-02), and nothing may connect to v3 (CLAUDE.md).

The questions are recorded as OQ-12 and were answered on 4 Nov:
- build nothing until a real export exists, and design the importer now;
- every imported client goes to Starter;
- `permission_acl` becomes a built-in role through an operator mapping file;
- v3 users are linked to v4 users by email, through identity.

This follows ADR 0006's shape (designed in Phase 1, built when its input exists). An importer written against guessed columns would be rewritten on first contact with the real export, so this ADR is the deliverable.

## Decision
**A `migrate-v3 accounts-clients` subcommand.** It plans the whole import in memory and then writes it, with a dry run that prints the plan and writes nothing. It is built when a v3 export or anonymised copy is available.

### Inputs
- **The v3 export, one file per table.** CSV is assumed, following 1.13's pattern: the parsing stays in one file (like `tools/migrate-v3/csv.go`), so another format is a contained change.
  - `clients`: `id`, `client_name`, `client_email`, `country_id`.
  - `user_client_map`: `user_id`, `client_id`, `permission_acl`.
  - `client_resellers`: `id` and the reseller's `client_id`.
  - `client_reseller_accounts`: `reseller_id` and the member `client_id`.
  - Only the data model's named columns are known. The ID and joining columns above are assumptions, to confirm against the real export before building.
- **1.13's `users.csv`** (`id`, `email_address`), to turn a v3 `user_id` into an email.
- **Operator files:**
  - `acl_roles.csv` (`permission_acl,role`) maps each v3 ACL value to a built-in role (Admin, Developer, Finance, Viewer). Only built-in roles are allowed (OQ-05).
  - `countries.csv` (`country_id,country_code`) maps v3's country IDs to ISO codes. v3 is Kenya-only, so this is expected to be a single `KE` row.
- **Flags:**
  - `--by <staff email>`: the staff member recorded on every tier change;
  - `--clients <id,...>`: the wave, a subset of v3 client IDs (ROADMAP §13);
  - `--dry-run`.
- **Environment:** `IDENTITY_DATABASE_URL` and `ACCOUNTS_DATABASE_URL`.

### Linking users
The tool calls identity's app to turn each email into a v4 user ID: a new read-only `UserIDsByEmail`, normalising emails as sign-up does. Nothing is added to identity's tables.
- A v3 user with no v4 user (not imported by 1.13, or flagged there) is flagged, and that membership isn't imported.
- `--by` must be staff (`IsStaff`); otherwise the run stops before planning.

`tools/` isn't covered by the per-service depguard rules, so the tool may call both services' `app` packages, as `tools/migrate-v3` already calls identity's.

### Mapping
Accounts' app gains a new `ImportV3Clients(ctx, plan)`.
- **Each client** becomes a direct tenant:
  - `display_name` = `client_name`;
  - country from `countries.csv`;
  - currency, time zone and region from that country's pack (2.10);
  - `billing_email` = `client_email` when it's a usable address.
- **Tier:** the tenant is inserted in the sandbox (2.5's trigger), then moved to **Starter** in the same transaction through `changeTierTx`, with reason `v3 import` and `ChangedBy` = the `--by` staff member. That satisfies the evidence rule: staff may promote any number of tiers. `tenant.tier_changed` is published. Business and above come later through KYB or staff (2.7/2.9). No v3 KYC value is trusted (data model §4).
- **Resellers:** a reseller's tenant gets `type = reseller` in its import transaction. `tenant.type_changed` is published with reason `v3 import`.
  - **This is a deliberate exception to 2.2b's rule** that only an active Financial account becomes a reseller: v3 resellers already exist and come in at Starter. Re-check this exception when building.
- **Sub-accounts:** a reseller's clients become sub-accounts, inserted in the reseller's transaction as `CreateSubAccount` does. They take the reseller's country, currency, time zone and region (OQ-04; 2.10's trigger enforces the region). Their tier follows the same Starter rule.
- **Memberships:** each `user_client_map` row becomes an **active** membership. The role comes from `acl_roles.csv`; nobody re-accepts an invitation. `membership.joined` is published.
- **Order:** resellers, then direct clients, then sub-accounts, then memberships. Each tenant is one transaction with its memberships and events.

### Idempotency
A new table, `accounts.v3_client_map (v3_client_id text PRIMARY KEY, tenant_id uuid NOT NULL UNIQUE REFERENCES accounts.tenants, imported_at timestamptz NOT NULL DEFAULT now())`, is written in each tenant's transaction.
- A client already in the map is skipped and its memberships are re-checked, so a wave can be re-run, and cutover can find each client's tenant (ROADMAP §13).
- The table has no `tenant_id`-scoped reads by clients. It needs a narrow role, or the importer's own login, to read across accounts; decide when building, following 2.3a's pattern.

### Dry run and report
The dry run and the real run share the planning step. The dry run stops before writing.

The report gives counts: clients, resellers, sub-accounts, memberships per role, skipped (already imported) and flagged. It also lists every flagged record with its v3 ID and reason:

| Flag | Effect |
|---|---|
| No `country_id`, or one not in `countries.csv`, or a country with no pack | Client not imported |
| No usable `client_email` | Imported with no billing email |
| v3 user not found in v4 | Membership not imported |
| `permission_acl` not in `acl_roles.csv` | Membership not imported |
| An account left with no Admin | Imported; listed for follow-up |
| Reseller link to a client not in the export | Link ignored |
| A sub-account whose reseller isn't imported or in this wave | Not imported until its reseller is |

Those counts, compared with v3's own, are the exit gate's check.

### Not in this importer
- `client_services`: the selection and client prices. They go with ledger task 3.10, since prices become token prices.
- `client_billingmode`: billing, Part 2.
- `clients_leads` and `client_referral`: Part 4.
- v3 API tokens: ADR 0006.
- Sign-up `national_id` and `date_of_birth`: not trusted, so KYC runs again.

## Consequences
- Phase 2's exit-gate box for the dry run stays open until an export exists and this is built. Phase 2 can still go to `main` with it open, recorded under Blockers.
- Building it needs:
  - the real column names;
  - one migration (`v3_client_map`, plus a role if needed);
  - `identity/app.UserIDsByEmail`;
  - `accounts/app.ImportV3Clients` and its import-only reseller path;
  - the subcommand;
  - tests: planning without a database (every flag), an idempotent re-run, Starter with `tier_changes`, and a reseller with its sub-accounts.
- Imported clients can send on Starter services as soon as they're imported. Business services wait for KYB.
