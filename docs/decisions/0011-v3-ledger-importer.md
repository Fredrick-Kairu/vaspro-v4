# 0011. v3 ledger importer (design only)

**Date:** 2026-11-09 · **Status:** accepted (design only; build it when a v3 export or an anonymised v3 copy exists)

## Context
ROADMAP task 3.10 asks for two things:
- opening balances, bonuses and commissions as one migration transaction per client;
- v3 transaction history as read-only migration records.

Data model §5's "From v3" table names the sources:

| v3 table or column | v4 |
|---|---|
| `client_balance.balance` | main wallet, as one `migration` transaction per client |
| `client_balance.bonus`, `client_app_bonus` | bonus account |
| `client_balance.subscription_balance` | `revenue_share_payable` ("to confirm") |
| `user_balance.commissions` | `commission_payable` |
| `transactions` (reference types 1 to 30) | imported read-only, with the v3 reference kept; balances checked against v3 totals before cut-over |
| `client_services.unit_cost`, `discount` | `token_prices` with `tenant_id` set: the client's deal from cut-over |

Three things are left open by the design:
- **The other side of the posting.** A migration posting credits client accounts but names no account to debit.
- **Bonus.** System design §4.2 says v3 bonus balances "become bonus tokens per the client's plan"; data model §5 says a bonus account "usable on any service".
- **History.** ROADMAP says "read-only migration records"; data model §5 says `migration` transactions with entries. Entries for every v3 transaction would count each shilling twice alongside the opening balance.

There is no v3 export or anonymised copy here (OQ-02), and nothing may connect to v3 (CLAUDE.md). The questions are recorded as OQ-21 and were answered on 9 Nov:
- design the importer now and build it with the export, as ADR 0009 did for 2.11;
- debit a new VASPRO account, `migration_clearing`;
- bonus is money in the bonus account;
- history goes in its own read-only table, never the journal.

## Decision
**A `migrate-v3 ledger` subcommand.** It plans every client's import in memory, prints the plan in a dry run, and writes it otherwise, one transaction per client. It runs after ADR 0009's `accounts-clients` for the same wave, because it needs each client's tenant.

### Inputs
- **The v3 export, one file per table**, CSV as in ADR 0009 and 1.13, with parsing kept to one file. The columns below are assumptions, to confirm against the real export before building:
  - `client_balance`: `client_id`, `balance`, `bonus`, `subscription_balance`;
  - `client_app_bonus`: `client_id`, `bonus` (summed per client);
  - `user_balance`: `user_id`, `commissions`;
  - `user_client_map`: `user_id`, `client_id` (ADR 0009's file);
  - `transactions`: `id`, `client_id`, `reference_type`, `amount`, `created`, `description` (and whatever v3 uses for direction);
  - `client_services`: `client_id`, `service`, `unit_cost`, `discount`.
- **`accounts.v3_client_map`** (ADR 0009) turns a v3 client into its tenant. The importer reads it through accounts' app, never the table (CLAUDE.md: no reading another service's schema).
- **Flags:**
  - `--clients` (the wave, as ADR 0009);
  - `--by <staff email>` and `--approved-by <staff email>`: two different staff members, for the deals;
  - `--include-subscription` (off until finance confirms what `subscription_balance` is);
  - `--dry-run`.

### Amounts
v3 amounts are read as decimal strings and converted to minor units of the client's currency. v3 is Kenya-only, so that's KES with 2 places. A value with more places than the currency has is **flagged, never rounded**. Floats are never used (CLAUDE.md).

### The opening posting
One `migration` transaction per client, through the ledger's app (internal, like the token postings):
- **Key** `v3-opening:<v3 client id>`; reference type `v3_client`, with the description naming the v3 client.
- **Credits:**
  - the main wallet: `balance`;
  - the bonus account: `bonus` + Σ `client_app_bonus`;
  - commission payable: the client's commissions (see below);
  - revenue share payable: `subscription_balance`, only with `--include-subscription`.
- **Debit:** migration clearing, the total.
- **A negative v3 balance** becomes a receivable debit instead of a main-wallet credit (the wallet floor forbids a negative wallet), and is flagged for finance.
- **Zero amounts** get no line; a client with nothing is skipped and counted.
- **VAT:** none moves. A balance is the client's own money; VAT is charged when it's used (data model §5).

**`migration_clearing`** is a new VASPRO account type, one per currency, with normal side D:
- added to `ledger.account_type`, `ledger.normal_side` and `domain/accounttype.go` (the enum test catches a missing side);
- its balance is what v3 owed its clients at cut-over;
- finance reconciles it with v3's bank and float balances and clears it with staff adjustment postings. A non-zero balance after reconciliation is the gap to explain.

### Commissions
`user_balance` is per user, but `commission_payable` belongs to a tenant. A user with commissions is credited on the commission payable of the one imported **reseller (or agent) client** they're a member of, through `user_client_map`. A user with commissions who belongs to none, or to more than one, is flagged and not posted. This rule is a guess at v3's meaning and must be confirmed with v3's owners when building.

### Client prices
For each client, `client_services`' SMS `unit_cost − discount` becomes that client's **deal** from cut-over:
- a `token_prices` row with `tenant_id`, `min_tokens` 0, `deal_reference` `v3:<client id>`;
- entered by `--by` and approved by `--approved-by`, so maker-checker (OQ-15, the database's CHECK) holds;
- the price is converted to minor units per token with up to 6 places, and more is flagged;
- other services wait for their own phases' pricing.

### History
v3's `transactions` go into a new table, not the journal:

```sql
CREATE TABLE ledger.v3_history (
  id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id          uuid NOT NULL,
  v3_transaction_id  text NOT NULL,
  reference_type     integer NOT NULL,          -- v3's 1 to 30, kept as v3 had it
  direction          char(1) NOT NULL CHECK (direction IN ('D', 'C')),
  amount_minor       bigint NOT NULL CHECK (amount_minor >= 0),
  currency           char(3) NOT NULL REFERENCES ledger.currencies (code),
  occurred_at        timestamptz NOT NULL,
  description        text NOT NULL,
  imported_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, v3_transaction_id)
);
```

- **RLS:** the standard `tenant_isolation` policy.
- **Insert-only:** the same append-only trigger as the journal, and no UPDATE or DELETE grant.
- **Never counted:** it isn't in balances, statements' running totals or the nightly rebuild (3.8).
- **Shown to the client** through a read route next to the statement (`GET /v1/ledger/tenants/{id}/v3-history`), when it's built.
- **Reference labels:** v3's reference types keep their number. A label table for the dashboard waits for v3's list.

### Idempotency
- **Opening posting:** the ledger's key. A re-run with the same amounts is a replay. A re-run where v3's balance has changed since is refused (VAL-4001) and flagged, so a late v3 movement is never silently overwritten; finance adjusts it.
- **History:** `ON CONFLICT (tenant_id, v3_transaction_id) DO NOTHING`.
- **Deals:** an existing pending or active price with the same `deal_reference` is skipped.

### Dry run and checks against v3 totals
The dry run and the real run share the planning step. The report gives:
- **Per client:** v3 balance, bonus and commissions next to the lines to be posted.
- **Per currency:** Σ v3 balances, and the migration clearing total it will produce.
- **Counts:** clients posted, skipped (nothing to post, or already imported), history rows, and deals.
- **Every flag:**

| Flag | Effect |
|---|---|
| Client not in `v3_client_map` (accounts not imported) | Nothing imported for it |
| More decimal places than the currency allows | That amount not imported |
| Negative balance | Posted to the receivable; listed for finance |
| Commissions with no single reseller or agent client | Not posted |
| `subscription_balance` without `--include-subscription` | Not posted; listed |
| Balance changed since an earlier import | Refused; listed for adjustment |
| No SMS row in `client_services`, or a price outside 6 places | No deal; the list price applies |
| History row whose client isn't imported | Skipped |

After a real run the tool:
- reads the clients' balances back and compares them with v3's;
- runs the nightly rebuild's checks (3.8's `RebuildBalances` logic, as a one-off run for the date) and requires a pass.

### Cut-over
Per wave, as ADR 0009 and ROADMAP §13 describe:
1. the wave's clients are frozen in v3;
2. accounts are imported (ADR 0009);
3. then this importer runs, as a dry run and then for real;
4. the checks pass;
5. the clients move.

Migration clearing is reconciled by finance after each wave.

### Not in this importer
- invoices, `invoice_status`, `mpesa_transactions` and `manual_payments` as money movements: billing and payments (Parts 2 and 13). Their v3 rows still appear in `v3_history` when they're in `transactions`.
- `alert_threshold`: monitoring (Part 4).
- v3 API tokens: ADR 0006.
- token lots: v3 has no tokens; clients buy them in v4 from their main wallet.

## Consequences
- 3.10 is designed, not built; Phase 3's exit gate notes it, as Phase 2's does for 2.11.
- **Building it needs:**
  - the real columns;
  - one ledger migration: the `migration_clearing` enum value and normal side, and `v3_history` with RLS, the append-only trigger and grants;
  - a ledger app `ImportV3Ledger(plan)` posting through `postInTx` as an internal posting (`ValidatePosting` allows `migration` with client and VASPRO lines);
  - accounts' app read of `v3_client_map`;
  - the subcommand.
- **Tests when built:**
  - planning without a database (every flag, decimal places, a negative balance, commissions);
  - the opening posting with each line, and a replay;
  - a changed balance refused;
  - history insert-only and RLS;
  - deals pending then approved by a second staff member;
  - the rebuild passing after a run.
- **Data model §5's two descriptions are reconciled:** bonus is money; history is records, not entries.
