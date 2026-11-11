# VASPRO API reference

This is the HTTP API of VASPRO v4 as built so far: identity (sign-up, sign-in, two-factor, credentials), accounts (client accounts, members, services, KYC/KYB, documents) the ledger (wallets, token purchases, staff prices and reversals) and payments (M-Pesa top-ups, staff matching and reconciliation, checkout links). The first part explains the conventions every endpoint shares; the second lists every endpoint with its parameters, request body, response and errors; the last part is the full error catalogue.

This file is generated from the OpenAPI specs in `docs/api/` (the source of truth, also usable in Postman, Insomnia or Swagger UI), so it always matches them.

## 1. Base URL

All three APIs sit behind one gateway, on one host:

| Environment | Base URL |
|---|---|
| Dev environment | `https://<API_DOMAIN>`, the API domain you were given |
| On the dev host itself | `http://127.0.0.1:18080` |

Paths are versioned (`/v1/...`). The gateway routes `/v1/auth/*` and `/v1/oauth/*` to identity, `/v1/accounts/*` to accounts and `/v1/ledger/*` to the ledger, and `/v1/payments/*` and the hosted checkout pages under `/pay/` to payments. Clients never talk to a service directly.

## 2. Conventions

- **JSON in and out**, `Content-Type: application/json`, field names in `snake_case`. The exceptions are the document upload (`multipart/form-data`), document downloads (the raw file) and the OAuth token endpoint (form-encoded, RFC 6749).
- **IDs** are UUIDs (version 7, so they sort by creation time). Treat them as opaque strings.
- **Times** are RFC 3339 in UTC, e.g. `2026-11-05T09:30:00Z`.
- **Money** is never a float. Amounts are integers in minor units (cents) with a currency code; prices that need decimals are strings such as `"0.80"`.
- **Countries and currencies** are ISO codes (`KE`, `KES`).
- **Validation:** every request is checked against the spec at the gateway before it reaches a service. Missing required fields, wrong types and bad values get `VAL-4000`.
- **Request size:** 1 MB for JSON, 10 MB per uploaded file. Larger requests get `413`.
- **`X-Request-Id`:** every response carries one. Send your own (any unique string) to trace a request end to end, and quote it when reporting a problem.
- **Lists** are newest or oldest first as each endpoint says. Paged lists return `next_cursor`; pass it back as `cursor` for the next page. It's absent on the last page.

## 3. Authentication

### Dashboard session

The dashboard signs a user in and gets a **session token**:

```
Authorization: Bearer sess_...
```

Send it on every endpoint marked *session token* below. A session lasts 24 hours. Without a valid token those endpoints return `401 AUTH-4010`; send the user back to sign-in.

The response to sign-in also has a `session_id`. That's the session's public handle, used to list and revoke sessions. It is **not** a credential and never goes in the `Authorization` header.

The API sets no cookies. Store the token where your app keeps credentials, and drop it on sign-out or on `AUTH-4010`.

### Two-factor sign-in

If the user has TOTP set up, sign-in returns a `challenge` instead of a session. Ask for their authenticator code (or a recovery code) and send it to `POST /v1/auth/totp-challenges/{challenge}/verification`, which returns the session. A challenge lasts 5 minutes.

Five failed sign-in attempts within 15 minutes lock sign-in for up to 15 minutes (staff can unlock sooner). A locked account gets the same `AUTH-4010` as a wrong password, so the response never says which.

### Step-up

Sensitive actions (enrolling TOTP or a passkey, creating or rotating API clients and SMPP accounts, linking Google, inviting members, approving or withdrawing prices) need the user to have proved a second factor in the **last 5 minutes**. Without it they return `401 AUTH-4012`. Then:

1. With TOTP enrolled: `POST /v1/auth/step-up/totp` with a current code.
2. Otherwise: `POST /v1/auth/step-up/email`, which emails a code, then `POST /v1/auth/step-up/email/confirmation` with that code.
3. Retry the original request with the **same** `Idempotency-Key`.

`AUTH-4012` means "verify again". `AUTH-4010` means "sign in again". Handle them differently.

### Selected account

A user can belong to several client accounts. Accounts endpoints take the account in the path (`/v1/accounts/tenants/{id}/...`). The identity endpoints for API clients and SMPP accounts use the account **selected on the session** instead: call `PUT /v1/auth/sessions/current/tenant` after sign-in, and again when the user switches account. With none selected they return `403 AUTH-4023`.

### Roles and permissions

What a member may do depends on their role in the account: Admin (everything), Developer (credentials), Finance (billing) or Viewer (read only). `GET /v1/accounts/roles` lists the roles and their permissions. A member whose role lacks a permission gets `403 AUTH-4024`. A user who isn't a member gets `404 VAL-4003`, the same as for an account that doesn't exist.

Endpoints under `/staff/` are for VASPRO staff only (`403 AUTH-4025` for everyone else).

### Machine credentials (API clients)

API clients exchange their ID and secret for a 1-hour JWT at `POST /v1/oauth/token` (OAuth2 client credentials). Public keys are at `/.well-known/jwks.json`. No endpoint in this document takes a JWT yet; messaging and payments will.

## 4. Idempotency

**Every write** (`POST`, `PUT`, `PATCH`, `DELETE`) needs an `Idempotency-Key` header. The exceptions are `POST /v1/oauth/token` and the KYC vendor webhook.

```
Idempotency-Key: <a new UUID for each action>
```

- Generate a new key (a UUID) for each user action.
- If a request fails on the network or times out, **retry it with the same key and the same body**. If the first attempt went through, you get its original response back and nothing happens twice.
- The same key with a different body gets `409 VAL-4001`.
- A missing key gets `400 VAL-4000`.

## 5. Errors

Every error is `application/problem+json` (RFC 9457):

```json
{
  "type": "https://docs.vaspro.io/errors/AUTH-4012",
  "title": "Step-up verification required",
  "status": 401,
  "code": "AUTH-4012",
  "detail": "step-up verification is required",
  "retryable": false,
  "request_id": "5c0e7a52-3f1b-4d8e-9a6c-2b7f1e0d4c93"
}
```

| Field | Meaning |
|---|---|
| `code` | The stable error code. **Branch on this**, never on `title` or `detail`. |
| `status` | The HTTP status, repeated. |
| `title` | A short, fixed description of the code. |
| `detail` | Text about this occurrence, for logs and support. It isn't meant for end users and may change. |
| `retryable` | `true` if retrying the same request unchanged can succeed (e.g. rate limits, timeouts). |
| `request_id` | Quote it when reporting a problem. |

Codes are `FAMILY-NNNN`. The families are:

- `AUTH`: authentication and permission
- `KYC`: verification tier
- `VAL`: validation, conflicts, not found
- `BAL`: balances
- `RATE`: rate limits
- `SYS`: server-side

Each endpoint lists the codes it's known to return. **Any endpoint may also return:**

| Code | HTTP | When | What to do |
|---|---|---|---|
| `VAL-4000` | 400 | The request doesn't match the spec, or a field breaks a rule. `detail` says which. | Fix the request. |
| `RATE-4290` | 429 | More than 1,200 requests a minute from one IP address. | Wait for the `Retry-After` header (seconds), then retry. |
| `SYS-9000` | 500 | An unexpected server error. | Retry later with the same `Idempotency-Key`. Report it with the `request_id` if it persists. |
| `SYS-5030` | 503 | A dependency timed out. | Retry with backoff and the same `Idempotency-Key`. |

To avoid revealing who's registered, some endpoints deliberately give the same answer in different cases:

- sign-up and password reset always return `202`;
- a wrong password, an unknown email and a locked account all return `AUTH-4010`;
- "doesn't exist" and "not yours" are both `VAL-4003`.

The OAuth token endpoint is the one exception to problem+json. Following RFC 6749, it returns:

```json
{"error": "invalid_client", "error_description": "..."}
```

## 6. Typical flows

### Sign up and first sign-in

1. `POST /v1/auth/signups` with email and name. A 6-digit code is emailed.
2. `POST /v1/auth/email-verifications` with the email and code.
3. `POST /v1/auth/password-resets` with the email. A code is emailed (the same endpoint sets a first password and resets a forgotten one).
4. `POST /v1/auth/password-reset-confirmations` with the email, the code and the new password.
5. `POST /v1/auth/sessions` with email and password, which returns `session_token` (or a `challenge` once TOTP is set up).

Codes last 15 minutes and allow 5 attempts.

### Onboard a client account

1. `POST /v1/accounts/tenants` with `display_name` and `country_code` (`KE`). The caller becomes its Admin, and the account starts on the **sandbox** tier.
2. `PUT /v1/auth/sessions/current/tenant` to select it.
3. `GET /v1/accounts/tenants/{id}/services` to see the catalogue. `PUT .../services/{code}` selects one; it stays `locked` until the account's tier is high enough.
4. `POST /v1/accounts/tenants/{id}/kyc-checks` starts individual verification. When it passes, the account moves to **starter** and starter services unlock.
5. For a business:
   1. `PUT .../business-profile` to enter the business details;
   2. `POST .../business-profile/owners` to add at least one director;
   3. `POST .../documents` to upload the documents `submitKyb` asks for;
   4. `POST .../kyb-submissions`. Staff review it; a pass moves the account to **business**.
6. Poll `GET /v1/accounts/tenants/{id}` (its `tier`) and `GET .../kyc-checks` (each check's `status` and `reason`) to show progress.

The tiers are sandbox → starter → business → financial. Each service needs a minimum tier, shown as `min_tier` in the catalogue.

### Invite a team member

1. Step up.
2. `POST /v1/accounts/tenants/{id}/invitations` with the email and role. The invitee gets an email.
3. The invitee signs in, or signs up with that address.
4. The invitee calls `POST /v1/accounts/me/memberships/{tenantId}/acceptance`.

`GET /v1/accounts/me/memberships` shows a user's accounts and pending invitations.

## 7. Testing in the dev environment

The dev environment uses fakes instead of real providers:

- **Emails** (verification and step-up codes, invitations) aren't delivered. They're in Mailpit at the mail domain you were given, behind a password.
- **KYC** uses a fake vendor. A check stays `pending` until someone decides it. On the dev host, run `make dev-kyc ID=<check id> STATUS=passed` (or `failed`, `review`, `expired`); the account updates within a second.
- **Staff access:** a user is made VASPRO staff on the dev host with `make dev-staff EMAIL=<email> BY=<your name>`.
- **Sign in with Google** is off unless it's been configured; its endpoints then return `VAL-4003`.
- **Passkeys** work only from the origins configured on the server.
- **CORS** is likewise limited to the configured origins (by default `http://localhost:3000` and `http://localhost:5173`). Ask the backend team to add yours.

The data is test data and may be reset at any time.
