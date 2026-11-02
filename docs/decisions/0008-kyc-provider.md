# 0008. KYC provider interface and the fake vendor (OD-03's default)

**Date:** 2026-11-02 · **Status:** accepted

## Context
System design §2 asks for "provider-agnostic KYC: one internal interface (start check, receive result, fetch report) with an adapter per vendor", so management can pick or switch the vendor without touching the flow. Which vendor is ROADMAP §4.3's **OD-03**, needed by Phase 2, with the default "fake adapter until chosen; interface fixed". No vendor has been chosen, so task 2.6 builds that default and fixes the interface. Data model §4's `kyc_checks` keeps a summary only ("the full report stays with the provider") and is unique on `(provider, provider_ref)`. The rules for what a result does were agreed on 2 Nov (OQ-07).

## Decision
- **One interface,** `internal/accounts/adapters.KYCProvider`:
  - `Name`;
  - `StartCheck` returns the vendor's reference and its hosted flow URL; we send our check ID as the vendor's reference;
  - `ParseWebhook` verifies the vendor's signature and timestamp before reading anything, and returns a status and a decision-only summary;
  - `FetchReport`.
  A vendor is one adapter. Its own signature scheme lives inside it.
- **Until OD-03 is decided, the provider is `fake`.** `FakeVendorProvider` speaks the protocol of `tools/fakes/kycvendor`, a real HTTP server:
  - webhooks are signed HMAC-SHA256 over `timestamp + "." + body`;
  - the timestamp window is 5 minutes either way;
  - a test drives the decisions.
  Config: `ACCOUNTS_KYC_PROVIDER`, `ACCOUNTS_KYC_FAKE_URL`, `ACCOUNTS_KYC_WEBHOOK_SECRET`. With no URL, KYC is off.
- **Webhooks** arrive at `POST /v1/accounts/kyc/webhooks/{provider}` through the gateway.
  - The spec declares the operation `security: []`, and the gateway skips the session only for operations the spec marks public.
  - No Idempotency-Key is required, at the gateway or in accounts.
  - A webhook names a check by reference, not by account, so the lookup uses a narrow NOLOGIN role (`accounts_kyc_webhook`, `WITH INHERIT FALSE`, used only inside `WithRoleTx`).
- **Results (OQ-07):**
  - A decided check is final; only review → passed/failed/expired moves on (`domain.NextKYCStatus`). Repeated and out-of-order webhooks are therefore no-ops, and webhooks are idempotent without a separate dedup store.
  - A passed individual check promotes a sandbox account to starter in the same transaction (2.5's rules).
  - Nothing on this path demotes: failed and expired are recorded only, and demotion stays with staff and re-verification.
  - An unknown reference is acknowledged and ignored.
- **Personal data:** documents, images and full reports stay with the vendor. `kyc_checks.result` holds the decision summary the vendor sends, and nothing from a webhook body is logged.

## Consequences
- **Choosing a vendor** (OD-03) means writing one adapter, adding its name to `ACCOUNTS_KYC_PROVIDER`, and registering the webhook URL with the vendor. The flow, the tables and the rules don't change.
- **Local development:** KYC needs the fake vendor running and `ACCOUNTS_KYC_*` set. There's no compose service for it yet; tests start it themselves.
- **Deployment:** the service's login role needs `GRANT accounts_kyc_webhook TO <role> WITH INHERIT FALSE` wherever it isn't the role that runs migrations.
- **Still open:** business checks (KYB, 2.7), screening, and the staff decision on checks in review (2.9).
