# 0007. Dashboard sessions authenticate with a separate token, not the session id

**Date:** 2026-10-25 · **Status:** accepted

## Context

Since task 1.3, a dashboard session's bearer credential was its own row id:
`Authorization: Bearer <session_id>`. Task 1.5 then made that same id the
public handle for session management: `GET /v1/auth/sessions` returns every
active session's `id`, and `DELETE /v1/auth/sessions/{id}` takes one in its URL.
Together that meant:

- anyone holding one of a user's sessions could list the user's other sessions
  and then use them, which defeats revoking a single stolen session;
- a bearer credential travelled in a URL path, so it ended up in access logs.
  The gateway's audit line logged the raw path (task 1.11). The Phase 1 exit
  gate requires that no token appears in any log line;
- the credential was a UUIDv7, which is partly a timestamp, rather than a
  full-entropy secret.

This was found while preparing the Phase 1 exit gate. Data model §3's
`identity.sessions` has no column for a separate token.

`.claude/rules/api.md` says a breaking change needs a new API version and an
ADR. Requiring `session_token` instead of `session_id` as the bearer is
breaking for any existing client of `/v1/auth/*`.

## Decision

A session gets its own bearer token, separate from its id. The change stays in
`/v1` rather than starting a `/v2`.

- Sign-in (password, TOTP challenge or passkey) returns `session_token`
  alongside `session_id`. The token is `sess_` followed by 32 random bytes,
  base64url-encoded (`domain.GenerateSessionToken`). It is returned once and
  stored only as SHA-256 in a new nullable, unique `identity.sessions.token_hash`
  column (migration `20261025090000_add_session_token_hash.sql`).
- Every `sessionBearer` operation authenticates with
  `Authorization: Bearer <session_token>`. A session id is never accepted as a
  bearer. The Redis session cache is keyed by the token hash.
- `session_id` stays the public handle that list and revoke use.
- Plain SHA-256 is enough for the hash: the token is random, the same reasoning
  task 1.3 applied to recovery codes.

There's no `/v2` because the API has no consumer yet. No dashboard frontend
exists (track F0), and the only clients of these routes are this repository's
own tests, which moved in the same change. Keeping a `/v1` that accepts
session ids as bearers would keep the vulnerability alive.

## Consequences

- Rows created before the migration have no `token_hash`, so they never
  authenticate and those users sign in again. No existing data is rewritten.
- The data model now differs from data model §3 by one column. It should be
  folded into data-model.md the next time that document is revised.
- The gateway's audit line now logs the OpenAPI route template, never the raw
  path. That also covers the TOTP and passkey sign-in challenge tokens, which
  travel in their URL paths by design.
- Once the API has real consumers, a change like this one needs a new version,
  as `.claude/rules/api.md` says. This ADR's "no consumers yet" reasoning
  stops applying when the first frontend or SDK release ships.
