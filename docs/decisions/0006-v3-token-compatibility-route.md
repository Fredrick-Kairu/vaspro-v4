# 0006. v3 API token compatibility route (design only)

**Date:** 2026-10-23 · **Status:** accepted (design only — nothing in this ADR is built or enabled; see Consequences)

## Context

Data model §3's own "From v3" mapping table is explicit that v3's
`user_client_map.api_token`/`access_token` are **not migrated**:
"clients create new credentials." The requirements doc's v3 review
headlines flag why that's the right call on its own merits, not just a
migration shortcut — v3 stores these as a "plaintext static
`api_token`," exactly the shape a v4 credential must never take
(`identity.api_client_secrets.secret_hash` is Argon2id, never the raw
value, task 1.7).

But cutting every v3 integration off the instant a client's data
migrates would break live traffic that hasn't been updated to call
`POST /v1/oauth/token` (task 1.8) yet — and OD-15's own decision is
that migration happens in **client waves with dual running**, not one
global cutover, so there's no single date after which v3 tokens could
simply stop working everywhere at once either.

This is ROADMAP §4.3's **OD-16** — *"Accept old v3 API tokens through
a compatibility route, and for how long?"* — needed by Phase 1 for
design and Phase 18 for enabling it, with a documented default
("Yes, for 90 days after a client's migration") that nothing has
overridden. It's also data model §3's own open point raised by Part 1
("Old v3 API tokens: accept them through a compatibility route during
migration, and if so for how long?"), still unchecked there — this ADR
is that answer, recorded the way every undecided OD gets resolved when
its needed-by phase arrives and no one overrides the default.

Task 1.14 is explicitly **design, not enable**: Phase 18 (dual running,
full v3 cutover) is 17 phases away, there's no real v3 token data to
import yet (same access constraint 1.13's importer ran into — no v3
database or export exists in this environment), and the gateway (task
1.11) has no real protected business endpoint for an old token to call
yet either. Building live code and a migration against a mechanism
that can't be exercised for real would be the same mistake CLAUDE.md
already warns against — designing for a need that doesn't exist yet —
just stretched out over more phases than usual. The deliverable here
is the mechanism design, so Phase 18 implements it against a decision
already made, not one it has to invent under deadline pressure.

## Decision

**Yes — a compatibility route accepts a v3 token for 90 days after
that specific client's own migration, not a single global expiry.**
The mechanism, to be built in Phase 18 against this design:

- **Only a hash is ever imported, never the plaintext token.** v3's
  own tokens are "plaintext static" (no guarantee of the entropy a
  freshly generated v4 secret has), so the import computes
  `platform/crypto.HMACSign` (already built generically for exactly
  this "credential fingerprint" case, 0.11's own doc comment) over
  each token with a dedicated server-side key — never
  `Argon2Hash`/`Argon2Verify`, which assume a human-chosen secret and
  are deliberately slow; this is a machine token looked up on every
  request, so it needs a fast, keyed, one-way fingerprint instead,
  the same reasoning 1.3 gave for hashing recovery codes with plain
  SHA-256 rather than Argon2id — except keyed, since unlike a
  recovery code a v3 token's own randomness isn't something v4 can
  vouch for.
- **A new table, sketched but not created by this task**
  (`identity.v3_token_compat`, columns: `id`, `token_fingerprint`
  (the HMAC above, `UNIQUE`), `api_client_id` (`REFERENCES
  identity.api_clients`), `migrated_at`, `expires_at` —
  `migrated_at + 90 days`, computed once at import and never
  recalculated). Every migrating client already gets a real v4 API
  client/secret pair as part of normal onboarding (task 1.7); this
  table is purely a bridge from an old opaque string to that already-
  real client, not a second credential store. Scopes/environment/IP
  allowlist are read live from the `api_clients` row it points at —
  not snapshotted — so revoking or editing that client immediately
  affects both the real token and the compatibility one.
- **The gateway's existing JWT pipeline (task 1.11) gains a fallback,
  not a second code path for everything.** `requireJWT` already
  rejects anything that doesn't parse as a JWT with `AUTH-4010`; the
  compatibility route changes that one failure path to try the
  fingerprint lookup *before* giving up, not before or instead of the
  real JWT check. A hit resolves to exactly the same `Claims` shape
  (`gateway.Claims`) `requireJWT` already produces, so every
  downstream check (scope, environment, IP allowlist, rate limit)
  stays the one implementation it already is — this route is an
  alternate way to arrive at `Claims`, never a parallel enforcement
  mechanism.
- **The lookup is identity's job, not the gateway's**, same boundary
  reasoning task 1.11 already applied to the JWT's own `ip_allowlist`
  claim: the gateway must never read another service's schema, so it
  asks identity (a new, narrow, SELECT-only endpoint or role, the
  same shape 1.8's `identity_token_exchange` role already
  established for the OAuth2 token endpoint's own cross-tenant
  lookup) rather than querying `v3_token_compat` itself.
- **Every accepted v3 token is its own security event**
  (`v3_token_used`, task 1.12's existing `recordSecurityEvent`
  mechanism — no new infrastructure, just a new `type` value) so a
  client (or VASPRO staff) can see the bridge actually being used,
  and tell when a given client's traffic has fully moved to real v4
  credentials before the 90 days run out.
- **A proposed catalogue code, not an added one**: `AUTH-4017`
  ("legacy credential expired or unrecognized"), parallel to
  `AUTH-4010`'s existing "don't distinguish" reasoning. Left as a
  proposal in this ADR, not added to `docs/errors/catalogue.yaml` —
  a code with nothing returning it yet is dead weight in the
  generated constants, same reasoning this whole task avoids writing
  the table or the route themselves.

## Consequences

- **Nothing changes in code or schema today.** No migration, no
  gateway change, no catalogue code, no test. The next time this
  needs to become real (Phase 18, or earlier if a client wave starts
  sooner than planned) starts from this design, not from OD-16's bare
  one-line default.
- Phase 18's own task list should point back to this ADR rather than
  re-deriving the mechanism — in particular, the per-client
  `migrated_at` anchor (not a single cutover date) and "fingerprint,
  not plaintext" are both load-bearing decisions, not incidental
  details a re-reading of OD-16 alone would surface.
- `identity.v3_token_compat` doesn't exist until Phase 18 actually
  writes its migration against this sketch — and only once real v3
  token data (or an anonymised export of it) is available to import,
  the same access constraint that shaped task 1.13.
- If OD-15's migration-wave plan changes materially before Phase 18
  (e.g. a single global cutover after all), this ADR's "per-client
  `migrated_at`" anchor should be revisited — it's only correct under
  the wave model.
