# 0013. v3 contacts importer (design only)

**Date:** 2026-11-17 · **Status:** accepted (design only; build it when a v3 export or an anonymised v3 copy exists)

## Context
ROADMAP task 6.6 asks for a v3 importer of contact groups and contacts. Data model §10's "From v3" table names the sources only loosely:

| v3 | v4 | Notes |
|---|---|---|
| Contact lists (`list_ids` in `VASPRO_BULK_COMPOSE`) | `groups` | One group per list |
| Contacts in those lists | `contacts` with `source = 'v3'`, and `group_members` | Numbers normalised; invalid ones flagged, not imported |
| Opt-outs or blacklists, if v3 keeps them | `consents` withdrawn (`source = 'v3'`) or `suppressions` | To find in the export |

What is known of v3:
- campaigns to contact groups go on `VASPRO_BULK_COMPOSE` with a `campaign_id` and `list_ids`;
- that queue's receiver expands the lists from v3's contact tables (requirements, system design §5.2);
- v4 will expand recipients itself (6.5's `ExpandAudience`).

**Not known:**
- the contact tables' names and columns;
- whether v3 keeps opt-outs or a blacklist, and how they were recorded;
- whether contacts carry fields beyond the number and names.

There is no v3 export or anonymised copy here (OQ-02), and nothing may connect to v3 (CLAUDE.md). Your answer of 17 Nov (OQ-26, question 11): design it now, build it with the export, as ADRs 0009 and 0011 did. The details still to confirm against the export are OQ-27.

## Decision
**A `migrate-v3 contacts` subcommand.** It plans one wave of clients in a streaming pass, prints the plan in a dry run, and writes it otherwise. It runs after ADR 0009's `accounts-clients` for the same wave, because it needs each client's tenant. It can run again before cut-over to pick up v3 changes.

### Inputs
- **The v3 export, one CSV file per table**, as in ADRs 0009 and 0011, with parsing kept to one file. The names and columns below are assumptions, to confirm (OQ-27):
  - `contact_lists`: `id`, `client_id`, `name`, `created`;
  - `contacts`: `id`, `list_id`, `client_id`, `msisdn`, `first_name`, `last_name`, `created`, `updated`, plus any other columns;
  - `opt_outs` (if v3 has one): `client_id`, `msisdn`, `created`, and whatever says how it came about (a STOP reply, the client's own entry).
- **`accounts.v3_client_map`** (ADR 0009) turns a v3 client into its tenant. The importer reads it through accounts' app, never the table (CLAUDE.md: no reading another service's schema).
- **An operator file for extra columns**, `contact_fields.csv`: v3 column → attribute key, label and type.
  - A mapped column becomes an attribute definition (at most 50 per account, §10) and its values are checked by type, as an import's are.
  - An unmapped column is dropped and counted in the report; nothing is guessed.
- **Flags:** `--clients` (the wave, as ADR 0009), `--dry-run`, `--opt-outs=consent|suppression` (OQ-27, default below).

### Lists → groups
- One group per v3 list, named as in v3.
- Two lists of one client whose names differ only in case: the second becomes `<name> (v3 <id>)`, flagged, because names are unique ignoring case (§10).
- An empty name becomes `v3 list <id>`, flagged.
- **A new `contacts.v3_list_map (tenant_id, v3_list_id, group_id)`**, written in the group's transaction, makes a re-run find the group rather than create another. It's created by the build's migration, like ADR 0009's `v3_client_map`.

### Contacts
- **Numbers** are read with the client's pack (KE: v3 is Kenya-only) by `domain.NormalizePhone`, as imports do. A number that isn't valid is flagged with its reason and v3 ID, and not imported.
- **One contact per number per client** (§10):
  - a number in several lists, or several times in one, becomes one contact that is a member of each group;
  - its fields merge as an import's do: rows are taken in v3's `updated` order (then `created`, then `id`), a later non-empty value wins, and an empty one leaves the earlier.
  - A number already in v4 (added by API or import since the client started using v4) merges the same way, so a re-run is repeatable.
- **Written through contacts' app** in batches of 1,000, by an in-process method sharing the import worker's batch path (`ImportContacts` and `AddContactsToGroupByPhone`). There is no file upload, scan or `imports` row: the tool runs on the operator's machine.
  - `source = 'v3'` and `created_by` NULL; the migration's CHECKs already allow both.
  - Each batch is one tenant transaction.
  - 6.2 measured 1,000,000 rows through that path in 20 seconds.

### Opt-outs
- **Default:** a v3 opt-out becomes a **withdrawn promotional SMS consent** with `source = 'v3'`, `actor = 'client'` (the CHECK allows no `recorded_by` for v3), and evidence `v3 opt-out <date>`.
- **Why not `actor = 'subject'`:** that's the person's own choice, which only the person can undo. The CHECK ties it to a keyword or the preference centre, and v3's record may not say who opted out.
- **The cost:** the client can grant over it. That change is in the consent history with the client's user, and the dry run lists how many opt-outs each client has.
- **If OQ-27 finds v3 records STOP replies as such,** those become `actor = 'subject'`, `source = 'keyword'` instead (the person's choice, which sticks), with the CHECK unchanged.
- **`--opt-outs=suppression`** puts them on the client's suppression list instead (reason `other`, note `v3`), for a blacklist that the client kept itself.
- **Not imported as consent:** grants. v3 has no record of how consent was obtained, and KE is opt-out (OQ-26).

### Events
**None.** `contact_import.finished` describes a client's own upload. `consent.withdrawn` for every historical opt-out would flood clients' webhook endpoints and ClickHouse with old news. The dry-run and run reports stand in for them.

### The dry run and the report
Per client, and totals:
- lists read and groups to create (renamed ones flagged);
- contact rows read, distinct numbers, numbers merged across lists, numbers already in v4;
- invalid numbers by reason;
- opt-outs, and which way they'd go;
- columns dropped;
- per list, its v3 row count beside its group's member count.

The last line is the exit gate's "the v3 contacts import dry run matches v3 counts": each group's members plus the list's invalid and duplicate rows must equal the list's v3 count.

After a real run, the same report is read back from v4 (group sizes, consents with `source = 'v3'`) and must match the dry run.

### Personal data
- The export holds phone numbers and names. It stays on the operator's machine, is never committed, and is deleted after the wave.
- The tool logs counts and v3 IDs, never a number or name. Flags in the report name a row by its v3 ID and the reason, never its values (as import errors do).

### Cut-over
- **Per wave:**
  - until a client's wave, v3's `VASPRO_BULK_COMPOSE` keeps serving its campaigns;
  - after it, the client's campaigns are expanded by v4 (`ExpandAudience`, Phase 7).
- **A re-run just before cut-over** brings in v3's last changes. It's repeatable: groups through `v3_list_map`, contacts by number, memberships and opt-outs skip what's already there.
- **Contacts deleted in v3 between runs stay in v4;** the report lists numbers in v4 with `source = 'v3'` that the latest export no longer has, for the operator.

## Consequences
- 6.6 builds nothing. ROADMAP 6.6 is `[>]` and the exit gate's v3 dry-run box waits for the build, as Phases 2 and 3's do.
- The build adds:
  - `v3_list_map` (a migration);
  - an in-process batch method in contacts' app, sharing the import worker's path;
  - the subcommand and its CSV parsing in `tools/migrate-v3`;
  - tests against fixture CSVs with test numbers only.
- **To confirm with the export (OQ-27):**
  - the tables and columns;
  - whether v3 keeps opt-outs, and whether it records how each happened;
  - which extra columns clients rely on;
  - whether lists can belong to sub-accounts (a v3 reseller's clients), which ADR 0009's map already covers if they're keyed by client.
