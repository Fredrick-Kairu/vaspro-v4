# 0010. Ledger balances: kept by trigger for client accounts only

**Date:** 2026-11-04 · **Status:** accepted

## Context
Data model §5 says wallet balances are "a copy of the journal, updated in the same transaction", and that a nightly job rebuilds every balance from the entries (task 3.8). It doesn't say who updates the copy, which way a balance counts, or whether VASPRO's own accounts (revenue, tax payable, provider clearing, suspense, reversal clearing) keep one. Those accounts appear in almost every client posting: every usage settlement credits revenue and tax payable. A balance row per VASPRO account would be one row locked by every client's settlement at once. Decided by the user on 4 Nov (OQ-13) for tasks 3.1–3.2.

## Decision
**Who updates:** an `AFTER INSERT` trigger on `ledger.entries` updates `ledger.balances` in the same transaction, for client-owned accounts only. No code path can post without moving the balance.
- The trigger is `SECURITY DEFINER`. The service's role (`ledger_service`) may write only `held_minor`, `version` and `updated_at` on `balances`, and can't insert balance rows.
- A client account's balance row is created by a trigger when the account is created.

**Direction:** balances are kept on each account's normal side, so they read positive in normal use.
- Debit-normal: receivable, provider clearing.
- Credit-normal: every other type.
- The rule's source is `internal/ledger/domain/accounttype.go`. The triggers use a SQL copy (`ledger.normal_side`), and a store test keeps the two equal.

**VASPRO's own accounts** (tenant_id NULL) have no balance row and no per-posting update. Their balance is the sum of their entries, computed on demand and by the nightly rebuild.

**Lock order:** a posting inserts its entries in `account_id` order, so two postings touching the same wallets lock their balance rows in the same order and can't deadlock (task 3.3 follows this).

**The nightly rebuild** (task 3.8) checks every account:
- client accounts against their balance rows;
- VASPRO's accounts by recomputing them and checking every transaction balances.

**Wallet floor:** main, service and bonus wallets keep `balance_minor >= 0` and `held_minor <= balance_minor`. A deferred constraint trigger checks this at commit. VASPRO's accounts, receivables and payables may go below zero.

## Consequences
- This deviates from data model §5's wording, which implies every account has a balance row. §5 now carries a note.
- No hot row in VASPRO's accounts, so throughput isn't capped by one revenue or tax row.
- Reading a VASPRO account's balance costs a sum over its entries (indexed by `entries_account`). The finance views that need it (Phase 9 and the console) should use a periodic snapshot rather than summing on every request. That snapshot is a later task, not part of this decision.
- `held_minor` is changed only by the hold, capture and release use cases (3.3).
- Post-paid clients (tier 3, credit limits) would need an exception to the wallet floor. It isn't designed yet; it comes with a credit-limit design.
- The nightly rebuild is built (task 3.8, OQ-19). It reports and never corrects: a difference stays in place as evidence for finance, and a fix is a posting made by staff.
- If EXPLAIN shows the `EXISTS` RLS checks on `entries`/`balances` are slow, adding a `tenant_id` column to them is the fallback (OQ-13).
