# 0012. Uploaded files are temporary

**Date:** 2026-11-16 · **Status:** accepted

## Context
Phase 6 brings the first client file uploads that feed processing: contact imports (task 6.2). Campaign recipient files (7.11) and payout files (13.4) follow. Until now the only uploads were KYB documents (task 2.8), which are records kept with the account, sent through the gateway in the request body and capped at 10 MB.

Import files are different. They can be large (proposed cap 100 MB and 1,000,000 rows), they hold personal data (phone numbers, names, attributes), and once their rows are in the database the file adds nothing but risk. The Data Protection Act requires keeping personal data no longer than needed. The design had no rule for how long such files live, where they're stored, or how they reach VASPRO.

The decision was made on 16 Nov 2026 and is recorded as D-23 in ROADMAP §4.2 and as data model §8 ("Uploaded files and retention").

## Decision
**An uploaded file is a temporary input: the rows taken from it are the record, and the file is deleted soon after processing ends.**

- **Ownership.** Each service keeps its own upload table in its own schema (`contacts.imports` now; campaign uploads and `payments.payout_batches` later). The shared mechanics live in `platform/uploads`: signed upload and download links, the quarantine-to-final move, scanning, encryption and the sweeper. `platform/uploads` holds no business rules and no table.
- **Path.** Files never pass through the gateway. The client gets a signed upload link (valid 15 minutes) to a quarantine prefix in object storage and uploads straight to it, then tells the API the upload is complete.
- **Protection, as for KYB documents.** The service checks the type from the content, not the name; checks the size; scans with ClamAV; takes the SHA-256; encrypts the file with envelope encryption under its final key; and deletes the quarantine copy. An infected file fails and is deleted at once.
- **Keys.** Object keys are built from the retention class, the tenant and the upload's ID (`uploads/<class>/<tenant>/<id>`), never from anything the client sends. The client's file name is stored only to show back.
- **Processing** streams the file in batches and never loads it whole.
- **Retention classes:**
  - `transient`: contact imports, campaign recipient files and the error reports made from them. Deleted 2 days after a final status.
  - `financial`: payout files and their result reports. Deleted 30 days after a final status.
  - Both periods are configurable.
- **Two separate states:** `status` says how far processing has got; `delete_after` and `deleted_at` say whether the file still exists. `delete_after` is set at creation (the upload link's end plus 2 days, so an upload that never happens still expires) and reset at the final status. A sweeper in each service deletes the files of every row past `delete_after`, then sets `deleted_at`. It is safe to repeat. The row, its counts and its per-row errors stay.
- **After deletion,** a download returns a clear "file deleted" error.
- **Backstops:** a 1-day expiry rule on the quarantine prefix and, where the store supports it, on each class's prefix. Backups of upload prefixes are kept no longer than the files (task 17.3).

**Not changed:** KYB documents keep their own table and rules (data model §4, task 2.8): they're records, not inputs. Paybill statement imports (4.6) send their CSV in the request body, parse it in memory and keep no file.

**OD-23, recorded with its default** (ROADMAP §4.3, needed by Phase 17): how long to keep KYB documents and other account records after an account closes, and rejected or replaced documents. Until decided:
- kept while the account is active;
- rejected or replaced documents deleted 30 days after;
- closed accounts decided with counsel.

## Consequences
- **New platform package** `platform/uploads` (task 6.2a), built before contact imports (6.2). Contacts' `imports` and `import_errors` follow data model §8 exactly. Payments adds the same file columns to `payout_batches` in Phase 13.
- **Object storage must be reachable by clients** for signed links. That needs a public endpoint, CORS for the dashboard's origin, and links that are short-lived and bound to one key. The service signs them with its storage credentials (S3 presigned URLs; SeaweedFS locally and on dev), so the credentials never leave the service. Deployment notes go with 6.2a.
- **Large files can't be sealed in memory.** `platform/crypto.Envelope` seals a whole payload at once, so encrypting files up to 100 MB without loading them needs a streaming form of envelope encryption, designed in 6.2a.
- **Expiry backstops are uncertain.** Whether SeaweedFS supports bucket expiry rules is still to confirm (data model §8's open points); until then the sweeper alone deletes files.
- **Clients lose access to originals.** After 2 days (30 for payouts) a client can't download the original file or the error report, only the row's counts and errors. The dashboard shows the deletion date (frontend track F3).
- **Retention becomes testable.** Every service with uploads gets fake-clock tests: deleted at `delete_after`, never while processing, and safe to repeat. ROADMAP Phase 6's tests and exit gate name them.
