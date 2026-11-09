<!-- Exported on 2026-10-12 from "VASPRO v4 System Design: Core Flows" (https://claude.ai/code/artifact/e41284c4-aa32-45a8-ba2e-484b736454a4). The live doc is the source of truth until this file is committed; after that, change design here first and mirror it back. Diagrams appear as '[embedded content: ...]' placeholders: open the live doc to see them. -->

# VASPRO v4 System Design: Core Flows

Oct 9, 2026 · @Fred Kairu

v4 runs as Go services behind one API gateway. Every country rule lives in a country pack, and every channel and payment rail sits behind an adapter. The flows below define the core behaviour for review, in the order they depend on each other. Scope follows the requirements doc: Kenya first, then Tanzania and Uganda, with everything except Voice and IVR.

**Revision 2 (9 Oct 2026), after the team design review.** Nine changes, each landing in the sections below:

| # | Change | Where it lands |
| --- | --- | --- |
| 1 | Email replaces phone number as the unique sign-in identity, so anyone in any country can sign up | Sections 2 and 3 |
| 2 | Reversals of payments already received | Section 4.3 (new flow) |
| 3 | Auto-invoicing for recurring charges, and invoice emails with a Pay now button opening a hosted checkout | Section 11 (new flow) |
| 4 | Client-set limits, such as a maximum number of SMS, on any service that supports them | Section 5.1 and the `LIM-` error family |
| 5 | Main wallet plus service token wallets: money is spent only after tokens are bought for a service | Section 4.2 (new flow) and the ledger model |
| 6 | Several USSD codes and shortcodes per client, each billed monthly | Section 11 (billable assets) |
| 7 | Applications raise an ops ticket; staff source available codes from the MNO, the client picks one, staff apply, the client pays within a set window | Section 7 (redrawn) |
| 8 | Shortcode MT messaging: bulk, replies to MO, and premium | Section 12 (new flow) |
| 9 | Payments as a full service: collections and disbursements | Section 13 (two new flows) and the architecture |

**Decisions of 12 Oct:** tokens are worth the same as money today, with the token price held in configuration so it can change later; VAT is charged on use, not on top-up or token purchase; and VASPRO does not hold client collections. These are applied in sections 4 and 13 and in the data model.

**Decision of 12 Oct (SMS engine):** v4 does not rebuild SMS sending. The existing Go SMS engine stays as it is, and v4 hands SMS to it the way v3 does today (section 5.2). The engine is assumed to do only what v3 shows it doing, and only SMS. v4 chooses each message's route from the country pack, and builds its own adapters for WhatsApp, Telegram, email, RCS and premium MT through the operator SDP. Sections 1, 5, 10 and 12 reflect this.

**Applied with the same decision (payouts):** VASPRO "only drives the paybill integration to trigger payments and disbursements", so payouts leave from the client's own B2C or B2B shortcode with the client's own rail credentials, as v3 does today. VASPRO holds no payout float; it charges a fee per transaction from the client's payments token wallet. Airtime, data and bills are the exception: VASPRO buys them from its own providers, so they still draw on the airtime wallet. Sections 4.2, 10 and 13 reflect this.

## 1. Target architecture

All traffic enters through one gateway, and all state changes leave through one event bus. Only country packs and adapters differ between Kenya, Tanzania and Uganda.

&#91;embedded content: Target architecture · gateway, 14 core services, event bus, adapters\]

Services own their data and talk through the event bus. Webhooks, analytics, journeys and monitoring all read the same events, so a client's webhook, the dashboard timeline and a report can never disagree.

**Proposed building blocks** (for review)

| Concern | Proposed choice | Reason |
| --- | --- | --- |
| Services | Go; REST + JSON outside, gRPC between services | Project mandate; typed internal contracts |
| API contract | OpenAPI 3.1 as the source of truth | SDKs, docs and the API explorer are generated from it |
| Transactional data | PostgreSQL, one schema per service | The ledger needs strict transactions |
| Fast state | Redis | USSD sessions, rate limits, idempotency keys |
| Event bus | NATS JetStream, or Kafka if volume demands | Durable events, replay for webhooks |
| Message events and analytics | ClickHouse | Billions of DLR rows, fast reports |
| Documents | Encrypted S3-compatible object storage | KYC files and application documents |
| Secrets | Vault or a cloud KMS | Ends hardcoded credentials |
| SMS delivery | Existing Go SMS engine, unchanged; v4 hands off over the RabbitMQ queues and gateway calls v3 uses | Proven in production; v4 only picks the route and reads delivery reports (decided 12 Oct) |
| Other channels | v4 adapters for WhatsApp, Telegram, email, RCS and the operator SDP (premium MT) | The SMS engine handles SMS only |
| Observability | OpenTelemetry, Prometheus, Grafana | One `request_id` traced end to end |

**Country packs at launch.** Each pack is versioned data; adding a country later means a new pack and adapters, with no core changes.

| Pack | Currency | Operators | Mobile money rails | Regulator and data law | Rules the pack enforces |
| --- | --- | --- | --- | --- | --- |
| Kenya (+254) | KES | Safaricom, Airtel, Telkom | M-Pesa, Airtel Money | CA; Data Protection Act 2019 (ODPC) | Promotional SMS 8am–8pm; STOP on every promotional message |
| Tanzania (+255) | TZS | Vodacom, Airtel, Yas (ex-Tigo), Halotel, TTCL | M-Pesa, Airtel Money, Yas (ex-Tigo Pesa), HaloPesa | TCRA; Personal Data Protection Act 2022 | Sender IDs pre-registered with TCRA (about 3–4 weeks); STOP and SIMAMA opt-out keywords |
| Uganda (+256) | UGX | MTN, Airtel | MTN MoMo, Airtel Money | UCC; Data Protection and Privacy Act 2019 | Sender IDs registered with each network (about 2 weeks); A2P SMS cannot receive replies |

Tanzania and Uganda rules are from [Sent](https://www.sent.dm/resources/tanzania-sms-guide) and [Telerivet](https://www.telerivet.com/blog/uganda-sms-compliance-ucc-dppa); the Yas rebrand is from [The Citizen](https://www.thecitizen.co.tz/tanzania/business/tigo-tanzania-rebrands-to-yas-in-shift-towards-pan-african-ambitions-4836754). All should be confirmed with local counsel before launch.

## 2. Signup, service selection and KYC tiering

Every signup lands in the sandbox at once. Verification is asked for only when a chosen service needs it, and it stops at the lowest tier that unlocks everything the client picked.

&#91;embedded content: Signup and KYC tiering · 4 tiers, 3 checks, 1 review queue\]

Shaded boxes are account tiers; the others are steps and checks. Each check returns pass, review or fail. A pass promotes the account, review goes to the staff queue, and a fail lets the client retry with new documents.

**Behaviour**

- **Account states:** `unverified_contact` → `sandbox` → `starter` → `business` → `financial`, plus `suspended` and `archived`. Each change is an event (`account.tier_changed`) with the reason and reviewer.
- **Service catalogue:** each service declares its minimum tier per country. The dashboard shows chosen services only; a locked service shows the tier it needs and a "Start verification" button.
- **Tier 1 caps are configuration:** daily messages, daily spend and unique recipients per day, set per country pack and overridable per client by staff.
- **Provider-agnostic KYC:** one internal interface (start check, receive result, fetch report) with an adapter per vendor. Management can pick or switch the vendor without touching the flow.
- **Documents are verified once:** KYB documents are stored encrypted, and sender ID, shortcode and USSD applications reuse them (section 7).
- **Re-verification** runs on director or ownership changes, document expiry, or a fraud signal.
- **Hygiene:** sandbox accounts with no activity for 60 days are archived; reports count signups, verified and active clients separately.

**Identity is the email address (revision 2).** A user is identified by one verified email, stored lower-cased and unique across VASPRO. Phone numbers are optional profile data in any country's format, so sign-ups from outside East Africa work without a local number.

- **One person, many accounts:** the same email can belong to several client accounts (an agency managing many brands), each with its own role.
- **Recovery and alerts** go to the email first. A phone is used for SMS fallback only once it has been verified.
- **Changing the email** needs step-up MFA and a confirmation sent to both the old and new address.
- **v3 migration:** v3 users are keyed by phone (`profile.msisdn`). Migrated users without a verified email are asked to add and confirm one at first v4 sign-in.

## 3. API authentication

People prove who they are with a passkey, or with a password or Google account plus an authenticator app; machines exchange a hashed secret for a token that expires within an hour. The gateway checks every call before any service sees it.

&#91;embedded content: API authentication · 5 parties, 4 phases\]

Solid arrows are requests and dashed arrows are replies. The highlighted box is the single place where access is enforced.

**Behaviour**

- **API client record:** name, environment (sandbox or live), scopes, IP allowlist, expiry date, spend cap, and up to two active secrets. Secrets are hashed with Argon2id and shown once.
- **Token claims:** tenant, client, environment, scopes, KYC tier and expiry. Tokens are signed with rotating keys published as a JWKS, so partners can verify them offline.
- **High-value calls** (payouts, B2B, wallet transfers) also carry an HMAC-SHA256 signature over method, path, body and timestamp, valid for 5 minutes. mTLS is an option for enterprise clients.
- **SMPP clients** keep binding to the existing SMPP front end, which passes each submit to v4 the way it calls v3's `/smpp/api/create` today. v4 checks the system ID against a hashed password and the IP allowlist, then sends the message through the same orchestrator as REST.
- **Dashboard sign-in:** email plus passkey; email, password and TOTP; or Sign in with Google followed by TOTP or a passkey. Email is the unique identity, so no local phone number is needed. Step-up MFA is required to create or reveal credentials, change callback URLs, approve payouts, add users, or edit sender IDs.
- **Sign in with Google** (decided 30 Oct) uses OpenID Connect: the authorization code flow with PKCE, `state` and `nonce`, with the ID token checked against Google's published keys and our client ID. Accounts are matched on Google's `sub`, never on email, and are never linked silently: an existing user signs in the usual way first, then links Google. Google never replaces the second factor or step-up. A domain with company SSO is sent to its own identity provider instead. Stored in `identity.user_identities`.
- **Audit:** every credential event (created, used from a new IP, rotated, revoked) is logged and shown to the tenant admin.
- **Errors raised here:** `AUTH-1001` bad credentials, `AUTH-1003` token expired, `AUTH-1004` scope missing, `AUTH-1005` IP not allowed, `AUTH-1010` step-up needed, `KYC-2001` tier too low, `RATE-8001` rate limit.

## 4. Wallets, service tokens, top-ups and reversals

Each client has one main wallet for money and a token wallet per service. Money enters the main wallet through top-ups (4.1), becomes spendable only when the client buys tokens for a service (4.2), and leaves again only through a controlled reversal (4.3).

### 4.1 Top-ups

A top-up only changes a balance through one ledger posting, after the provider has confirmed it. Money that cannot be matched waits in a suspense account instead of being guessed onto a wallet.

&#91;embedded content: Wallet top-up · 3 triggers, 1 decision, suspense and reconciliation\]

Paybill and bank transfers skip the intent step: the unique account number on the payment identifies the client. The dashed link shows reconciliation reading the ledger, not posting to it.

### 4.2 Service tokens

Money in the main wallet cannot be spent by any service. The client first buys tokens for a service, and each service draws only from its own token wallet.

&#91;embedded content: Main wallet and service token wallets · 5 example services\]

The shaded boxes are service token wallets; the five shown are examples, and every billable service gets one. Messaging services hold units, while airtime holds a money float in the wallet's currency. Payouts are paid from the client's own M-Pesa or bank balance (section 13), so only VASPRO's fee for them comes from a token wallet.

- **Buying tokens** is a ledger transfer from the main wallet to the service wallet at the price list in force. The price per token is locked at purchase, so later price changes do not affect tokens already bought. Volume tiers can give a lower price per token.
- **Consumption per message** comes from the country pack. For example, one SMS segment to a Kenyan network might use 1 token and an international one more. The exact rates are a pricing decision.
- **Auto-buy** is opt-in per service: when tokens drop below the client's level, the rule buys a set amount from the main wallet. Without it, a service stops at zero tokens, as the team asked.
- **Holds** (section 5) now reserve tokens in the service wallet, not money in the main wallet.
- **v3 migration:** v3 balances move into each client's main wallet; v3 bonus balances become bonus tokens per the client's plan. *Superseded 9 Nov 2026 (OQ-21, ADR 0011): v3 bonus moves as money into the client's bonus account.*

### 4.3 Reversals

When money a client paid has to go back, VASPRO posts the opposite entries, linked to the original, and pays back through the rail. Nothing in the ledger is ever edited or deleted.

&#91;embedded content: Reversals · 4 triggers, 1 decision, maker-checker, pay-back and credit note\]

For chargebacks and operator-initiated reversals the rail has already moved the money, so the pay-back step is skipped and only the ledger entries and credit note are posted.

- **Unspent check:** if the money has already been turned into tokens or spent, unused tokens are clawed back first. Anything left becomes a recorded debt, and staff decide whether to proceed.
- **Maker-checker:** one staff member raises the reversal and a different one approves it, with step-up MFA. Amounts above a threshold need a finance lead.
- **Pay-back routes:** M-Pesa's reversal API inside its time limit, otherwise a B2C payment to the original payer; card refunds through the card processor; bank transfers back to the source account.
- **Pay-back failures** stay in a `reversal_pending` state with automatic retries. The money sits in a reversal clearing account until paid out, so it is never lost or double-paid.
- **Tax:** a credit note cancels the VAT on the original invoice; in Kenya it is issued through eTIMS.
- **Audit:** every reversal shows who raised it, who approved it, the reason and the evidence, and emits a `payment.reversed` event.

### 4.4 Ledger model and rules

**Ledger model**

| Account | Holds | Moves when |
| --- | --- | --- |
| Main wallet, one per currency | Money the client paid in | Top-ups credit it; token purchases, invoices and reversals debit it |
| Service token wallets (SMS, WhatsApp, USSD, airtime, payments and others) | Tokens or float bought for one service | Purchases credit them; that service's charges debit them |
| Hold, per service wallet | Tokens reserved for queued sends, scheduled campaigns and the fees on pending payouts | Reserved on accept; captured on success or released on failure |
| Bonus | Promo tokens and migrated v3 bonus | Spent before bought tokens, per the client's plan |
| Receivables | Unpaid invoices (section 11) | Cleared when the invoice is paid |
| Provider clearing | Money a rail owes VASPRO | Credited on top-up; cleared by settlement |
| Suspense | Unmatched incoming money | Cleared when matched or refunded |
| Reversal clearing | Money approved for return but not yet paid out | Cleared when the pay-back succeeds |
| Tax payable | VAT per country pack | Posted with each invoice, reversed by credit notes |

There is no collection wallet (decided 12 Oct): client collections settle into the client's own paybill and never enter the ledger. Premium MT revenue share owed to clients sits in a revenue share payable account. The full ledger schema is in [VASPRO v4 Data Model](https://claude.ai/code/artifact/2210198c-313f-4a2d-9692-931aa5dac3c6), section 5.

**Behaviour**

- **Every posting is a balanced transaction** with a unique key, so a repeated callback can never credit twice.
- **VASPRO computes tax** from the country pack. The v3 practice of accepting a client-supplied VAT amount goes away.
- **Auto top-up** raises an intent against a saved card or a standing M-Pesa authorisation when the balance drops below the client's threshold.
- **Post-paid clients** (tier 3) get a credit limit in place of a positive balance; the same postings apply, and a monthly statement closes the period.
- **Refunds** (failed sends, rejected applications) post back automatically to the service token wallet they were taken from, with the reason code.

## 5. Message send lifecycle

One orchestrator handles every outbound message, whatever the channel or entry point. Bad requests stop before any money moves. Accepted messages hold funds, and the final outcome decides whether the hold is captured or refunded.

&#91;embedded content: Message send lifecycle · 10 steps, route retry loop, channel failover\]

For SMS, "Hand off" passes the message and its route to the existing SMS engine (5.2), which does the sending. WhatsApp, Telegram, Messenger, email and RCS follow the same steps, with v4's own adapter in the engine's place. Bulk campaigns enter one message at a time after a single up-front balance check.

**Status model**

| Status | Meaning | Final | Charged |
| --- | --- | --- | --- |
| `accepted` | Passed checks, funds held | No | Held |
| `queued` | Waiting for a send window or throttle slot | No | Held |
| `sent` | Handed to the SMS engine, or accepted by the channel | No | Held |
| `delivered` | Handset confirmed | Yes | Yes |
| `failed` | Final failure with a `DLR-` or `RTE-` reason | Yes | Only if the reason is billable |
| `expired` | No DLR within the validity period | Yes | Per country pack rule |
| `rejected` | Stopped before sending | Yes | No |

**Behaviour**

- **Idempotency:** a repeated `Idempotency-Key` returns the original `message_id`, never a second send.
- **Send windows:** promotional messages outside the pack's hours are `queued` until the window opens, unless the client chose rejection instead.
- **Routing:** v4 chooses, the engine sends. Each pack lists the routes per operator with their hand-off, SMSC and priority (5.2). Route health (hand-off errors, DLR latency and outcomes) is scored live, and a failing route can be paused so traffic moves to the pack's next route.
- **Throttling** per client enforces Tier 1 caps and client limits. Pacing towards the operators stays inside the SMS engine.
- **Two-way:** replies and keywords arrive through the same adapters and become `message.received` events for the inbox, surveys and journeys. Uganda has no A2P inbound, so its pack disables SMS replies.

### 5.1 Client limits

Clients can cap any metered service, so a runaway campaign, an integration bug or a leaked key cannot use more than they chose. Limits are checked in the compliance step, before any tokens are held.

| Service | Limits a client can set | Can be scoped to |
| --- | --- | --- |
| SMS, WhatsApp, Telegram, email | Maximum messages per day, per month and per campaign; maximum recipients per campaign; maximum token spend per day | Account, API key, campaign, sender ID |
| USSD | Maximum sessions per day | Code |
| Airtime and data rewards | Maximum per recipient per day; maximum total per day | Account, API key |
| Payouts | Maximum per transaction, per recipient per day and in total per day; approval needed above an amount | Account, API key, user |
| Verify (OTP) | Maximum attempts per number per hour; allowed countries | Account |
| Surveys | Maximum responses or incentive spend per survey | Survey |

- **Hard or soft:** a hard limit stops further requests with a `LIM-` error; a soft limit only warns. Every limit has a warning level (80% by default) that sends a client alert through the alerting engine (section 10).
- **Campaigns are checked up front.** If a campaign would cross a limit, the client chooses to send up to the limit or cancel, before anything is queued.
- **Periods follow the client's time zone,** and counters reset at the period boundary.
- **Changing a limit** needs the account admin role and step-up MFA, takes effect immediately, and is audited.
- **VASPRO's own limits come first.** Tier 1 caps and risk limits set by staff sit above client limits, and the lower of the two always wins.

### 5.2 SMS engine interface (from v3)

SMS sending stays in the existing Go SMS engine, unchanged (decided 12 Oct). Its code is not part of this review, so the contract below is read from how v3 calls the engine and the gateways behind it. v4 assumes the engine does exactly this, for SMS only.

| Concern | v4 | Existing SMS engine |
| --- | --- | --- |
| Checks and money | Validation, consent, client limits, token hold and settlement | None |
| Route | Chooses the route from the country pack and sends it with each message | Uses the route it is given |
| Sending | Hands each message off on a queue or gateway call (below) | Delivers it to the operator |
| Delivery reports | Receives them, maps them to a status and `DLR-` reason, settles | Returns them to the callback URL sent with the message |
| Other channels | Own adapters for WhatsApp, Telegram, email, RCS and premium MT through the operator SDP | None |

**How v3 hands off today**

| Hand-off | Used for | Main fields |
| --- | --- | --- |
| RabbitMQ queues `VASPRO_QUICK_1` to `_7` (direct exchange, persistent JSON) | Campaign and scheduled sends; `_6` carries Ghana and `_7` one network | `outbox_id` (correlator), `campaign_id`, `recipients`, `short_code`, `message`, `message_pages`, `network`, `gateway`, `alert_type`, `dlr_url` |
| `VASPRO_OUTBOX_API`, `VASPRO_NESTED`, `VASPRO_OUTBOXTEST_1` to `_4` | API sends, nested bulk, bulk to uploaded numbers | The same message fields, or a campaign with its list of numbers |
| `VASPRO_BULK_COMPOSE` | Contact-group campaigns | `campaign_id` and `list_ids`; the receiver expands the lists from v3's contact tables |
| `TZ_SMS`, `TZ_SMS_VERIFICATION` on the Tanzania RabbitMQ server | Tanzania traffic | `msisdn`, `message` |
| Kannel `sendsms` (HTTP GET) | Single sends to a named SMSC | `from`, `to`, `text`, `smsc`, `account` (correlator), `priority`, `validity`, `dlr-url` |
| MNO apps gateway (HTTP GET) | Airtel, Telkom, Equitel and international routes | `msisdn`, `short_code`, `message`, `smsc`, `correlator`, `priority`, `alert_type`, `dlr_url` |
| Safaricom bulk API (HTTP POST with a token) | Safaricom sends | `packageId`, `oa` (sender), `msisdn` list, `message`, `uniqueId` (correlator), `actionResponseURL` |

**What comes back**

| Return | Carries | v3 today |
| --- | --- | --- |
| Kannel-style DLR callback (`/v3/callback/create`, `/v3/SMS/Report/kannel/dlr`) | `type`, `dlrvalue` or `stat:` text, the correlator, the gateway's message ID | Stores the DLR (2001 or 2002 for delivered); a rejected blast message is resubmitted up to 5 times |
| Safaricom bulk DLR | Posted to `actionResponseURL` | Lands on a separate DLR service, not in v3 |
| Submit records on `VASPRO_DLR` and `VASPRO_SAF_DLR` | Status 2000 (submitted) per send | Published by v3 for reporting |

**Rules for v4**

- **Same contract.** v4's SMS adapter publishes and calls exactly these hand-offs, so the engine and gateways do not change, and traffic can move from v3 to v4 one queue at a time.
- **Route in the pack.** Each operator's entry in the country pack names the hand-off (queue or gateway), the SMSC, the Safaricom package and sender mapping, and the priority. This replaces v3's hardcoded maps (SMSC per network, bulk sender settings, numbered queues).
- **v4 expands campaigns.** v4 resolves recipients from its own contacts service and hands off per message or per batch of numbers, instead of using `VASPRO_BULK_COMPOSE`, whose receiver reads v3's contact tables.
- **Correlator.** Every hand-off carries v4's message ID (or a short form if a gateway limits length) as `outbox_id`, `account`, `correlator` or `uniqueId`, so each DLR finds its message.
- **DLR intake.** v4 accepts each return format above, ignores duplicates by correlator and status, and maps raw values to `DLR-` reasons through the pack's tables. v3's numeric statuses are mapped once.
- **Retries are v4's call.** After a rejected or failed hand-off, v4 may hand off again on the pack's next route, up to the pack's limit (v3 uses 5 for blasts). The engine is not assumed to fail over.
- **Monitoring from the outside.** v4 cannot see the engine's binds, so section 10 watches hand-off errors, the depth of the engine queues, and DLR latency and outcomes per route.
- **Credentials.** RabbitMQ and gateway credentials move from v3's config files into the secrets store and are rotated.

## 6. USSD self-service

Clients provision, build, test and publish USSD services themselves. Only a dedicated code needs an operator, and even that runs as a tracked application.

### Provisioning

&#91;embedded content: USSD provisioning · 3 code types, 1 shared build-and-publish path\]

All three code types converge on one build, test and publish path, so a client can start on a shared channel and move to a dedicated code later without rebuilding. Channel numbers in the example are illustrations; real parent codes per network are still to be confirmed.

### A live session

&#91;embedded content: Live USSD session · 4 parties\]

With the no-code builder, VASPRO's menu engine plays the "Client app" role, so no client server is needed.

**Behaviour**

- **Menu builder nodes:** menu, free-text input with validation, API call, M-Pesa or mobile-money checkout, survey question, airtime or data reward, and end. Flows are versioned and can be rolled back.
- **Response budget:** each network in the country pack sets how long the client app has to answer. If the callback misses it, the gateway tries the failover URL, then ends with the app's default message.
- **Session state** lives in Redis with a timeout matching the network, so the client's server can stay stateless.
- **Billing:** a per-session price per network, charged when the session ends. With an empty wallet, sessions end with the client's "service unavailable" text and the client is alerted.
- **Analytics:** sessions, completion rate, drop-off per menu node, timeouts, and a split by network.
- **Lifecycle:** each code is a billable asset invoiced monthly (section 11), suspension after a grace period for non-payment, and release back to the pool.
- **Fixes from v3:** the create-code bug and the empty Activate and Activities actions are replaced by this flow.

## 7. Sender ID, shortcode and dedicated USSD applications

Every application for a sender ID, shortcode or dedicated USSD code raises a ticket for VASPRO's operations team. Staff get the available codes from the MNO, the client picks one, staff apply, and the client pays only after approval, within a set window. This replaces the earlier draft, where the fee was taken on submission.

&#91;embedded content: Applications · 6 main states, 4 side states, ops ticket behind each\]

The dashed path is for sender IDs: the client proposes the name, so there is no list to choose from. Red states are endings that need the client to act.

| State | Who acts | What happens | Client is told |
| --- | --- | --- | --- |
| Submitted | Client | Applies in the dashboard or by API; an ops ticket opens in the staff console with the KYB documents attached | Ticket reference, by email and dashboard |
| Sourcing options | Staff | Ask the MNO which codes are free (shortcodes and USSD only); SLA timer runs | On change |
| Client chooses | Client | Picks one code from the list in the dashboard, with each option's price, or asks for more options | Options list by email and dashboard |
| Applying to the MNO | Staff | File the application for the chosen code and record the MNO's reference | On change, per network |
| Approved, awaiting payment | Client | An invoice with a Pay now link is issued (section 11); the code is held for the payment window | Invoice email, then reminders |
| Active | System | Payment received: code mapped on VASPRO routes; monthly billing starts | Yes |
| Rejected | MNO or staff | Reason recorded; the client can reapply | Yes, with the reason |
| Lapsed | System | Window passed without payment; the code is released back to the MNO | Yes |
| Suspended, Expired | Staff or system | Misuse or an unpaid monthly invoice; or not renewed | Yes, with the fix |

**Also covered**

- **The ops ticket is the workspace.** Each application is a ticket in the staff console, in the same inbox as support, with an assignee, SLA, notes and the MNO's reference. Moving the ticket moves the client-facing state, so the two never disagree.
- **Several networks per application:** each network is tracked separately, so a code can be active on Safaricom while still pending on Airtel.
- **Payment window:** its length is set per country pack. Reminders go out before it ends, and once it lapses the reserved code is released.
- **Shared-shortcode keywords** skip this flow: a Tier 2 client claims a free keyword and it is active at once.
- **Sandbox** applications run through every state automatically, so clients can test the flow and its webhooks.
- **API parity:** create, choose an option, withdraw and query are all available by API, with an `application.status_changed` event.

## 8. Error handling

Each code family has one home in the request path. Anything caught before acceptance returns an error at once and costs nothing; anything after acceptance becomes an event with a reason code.

&#91;embedded content: Error handling · where each code family is raised, 2 ways it reaches the client\]

**Synchronous error body** (every service, every endpoint):

```json
{
  "type": "https://docs.vaspro.io/errors/BAL-4001",
  "title": "Insufficient balance",
  "status": 402,
  "code": "BAL-4001",
  "detail": "KES wallet holds 120.00; this send needs 450.00.",
  "retryable": false,
  "request_id": "req_01J9ZQ4M2K7X"
}
```

The docs domain and IDs are placeholders. Stack traces and internal messages never leave the service; they go to logs under the same `request_id`.

**Families added in revision 2**

| Family | Covers | Examples |
| --- | --- | --- |
| `LIM-` | Client-set limits (section 5.1) | `LIM-4501` daily message limit reached; `LIM-4510` payout above the per-transaction limit |
| `PAY-` | Payment rail outcomes for collections and payouts (section 13) | `PAY-6501` recipient wallet not registered; `PAY-6502` rail timeout, status being checked |
| `BAL-` (changed) | Now refers to the service token wallet, not the main wallet | `BAL-4002` no tokens left for this service; buy tokens or turn on auto-buy |

**Delivery reasons, normalised** (examples; the full catalogue is drafted in detailed design):

| Raw operator outcome | VASPRO status | Reason code | Retry | Charged |
| --- | --- | --- | --- | --- |
| Delivered to handset | `delivered` | — | — | Yes |
| Absent subscriber (phone off) | `failed` | `DLR-7003` | Later, within validity | No |
| Unknown or inactive number | `failed` | `DLR-7001` | No | No |
| Blocked by operator content filter | `failed` | `DLR-7020` | No | No |
| No DLR within validity period | `expired` | `DLR-7010` | No | Per pack rule |
| Sender ID not registered on this network | `rejected` | `RTE-6020` | No | No |

**Behaviour**

- **Raw codes are kept** next to the normalised ones, for audit and operator disputes.
- **Mapping tables live in the country pack** per operator and route, so a new operator's codes are data, not code.
- **Alerting:** error rate per route, operator and client is watched live. A spike opens an incident, can pause a route, and posts to the public status page.
- **Docs:** every code has a page with its cause, retry advice and fix, linked from the `type` field.

## 9. Developer portal, sandbox and integrations

Every feature can be built and tested end to end before a client pays or passes KYB. Going live changes the credential, not the code.

&#91;embedded content: Developer portal and sandbox · 2 environments, 1 go-live gate\]

Sandbox runs the real gateway and services against simulated adapters. So what passes in sandbox, including error codes, webhooks and ledger postings, behaves the same live.

**Sandbox test values** (proposal)

| Test input | Sandbox result |
| --- | --- |
| Reserved test number ending `01` | `delivered` after 2 seconds |
| Reserved test number ending `02` | `failed`, `DLR-7003` absent subscriber |
| Reserved test number ending `03` | `failed`, `DLR-7001` unknown number |
| Reserved test number ending `04` | `expired`, no DLR |
| Top-up amount ending `.00` | payment succeeds |
| Top-up amount ending `.01` | payment fails (insufficient funds) |
| Top-up amount ending `.02` | no callback, so reconciliation picks it up |

Each country pack publishes its own reserved test numbers in local format.

**Integrations catalogue**

| Integration | What it does | Built on |
| --- | --- | --- |
| Shopify, WooCommerce | Order, shipping and abandoned-cart messages over SMS or WhatsApp | Webhooks in, message API out |
| HubSpot, Salesforce, Zoho | Send from the CRM; log messages and replies on the contact | OAuth app + events |
| Google Sheets | Bulk sends and survey exports from a sheet | OAuth app |
| Zapier | VASPRO triggers (message received, payment completed) and actions (send, top up) | Public API |
| MCP server | Lets AI agents send, run surveys and check balances, with scoped credentials | Public API |

- **OAuth apps:** third parties register an app, request scopes and act for a client who consents, which is the basis for a partner ecosystem.
- **Every connector has a sandbox mode** that uses the client's sandbox credential.
- **Docs are generated** from the OpenAPI spec, so examples, SDKs and the explorer never drift from the API. A changelog and a versioning policy (at least 12 months' notice before a breaking change) are published.

## 10. Monitoring and alerting

A dedicated core service watches every event and metric, spots patterns that clients would feel, and hands support one incident with its evidence attached. Today v3 only emails clients about low balances; outages are found when clients call.

&#91;embedded content: Monitoring and alerting · 3 sources, 2 decisions, escalation loop\]

The loop on the left runs continuously. Client-defined rules share the same counters but notify the client, not VASPRO support. The highlighted box is the report every notification carries.

**Default rules** (starting points, tuned per route after a few weeks of live traffic)

| Signal | Scope | Triggers when | Severity |
| --- | --- | --- | --- |
| Routing errors | Operator route | `RTE-` rate above 5% of submissions for 5 min, with at least 200 submissions | P2; P1 if every route for that operator is affected |
| Delivery rate drop | Channel + country + operator | Delivered % more than 20 points below the same hour over the last 7 days | P2 |
| DLR silence | Operator route | No delivery reports for 10 min while sends continue | P1 |
| Payment callback silence | Payment rail + country | No callbacks for 15 min during business hours while intents are created | P1 |
| USSD callback failures | Client app | Callback timeouts above 10% of sessions for 5 min | P3 to support; client alerted |
| Credential abuse | API key | 50+ `AUTH-` failures in 1 min | P2; key locked automatically |
| Spam or pumping pattern | Client | `CMP-` rejections or traffic to unusual number ranges well above the client's normal | P2; client throttled |
| Suspense growth | Payments | Unmatched payments above a set amount or age | P2 |
| Ledger imbalance | Ledger | Any transaction that does not balance | P1 |
| Platform health | Service, SMS engine hand-off, queue | Service down, hand-offs to the SMS engine failing, or queue backlog growing for 5 min | P1 or P2 |

**Who gets what**

| Severity | Channels | Escalation |
| --- | --- | --- |
| P1 Critical | Page on-call by phone call and SMS through a route independent of VASPRO, plus Slack and email | Next on-call after 10 min unacknowledged, then the engineering lead |
| P2 Major | Slack and email to support; on-call during business hours | After 30 min unacknowledged |
| P3 Minor | Support ticket; the client is notified where relevant | Ticket queue SLA |
| Info | Daily digest email | — |

**What a notification contains**

```
[P2] SMS · Kenya · Safaricom bind 2 · Routing errors high
Started 14:02 EAT · open 12 min · owner: unassigned
What:      RTE-6010 rate 18.4% (normal 0.4%)
Volume:    2,391 failed of 12,980 submitted in the last 10 min
Breakdown: RTE-6010 no response from bind (91%), RTE-6020 (9%)
Clients:   top affected clients with counts
Samples:   5 message IDs, each linked to its timeline and raw operator response
Done:      bind 2 paused; traffic moved to bind 1 and the aggregator route;
           212 non-billable failures refunded
Links:     live graph · logs filtered to this route · runbook for RTE-6010
Actions:   Acknowledge · Escalate · Resolve · Post to status page
```

The figures are illustrative.

**Behaviour**

- **Rates, not counts:** every rule compares a rate or a baseline and needs a minimum volume, so quiet hours and small clients do not cause false alarms.
- **One incident per problem:** repeat triggers for the same rule and scope update the open incident and its thread instead of opening new ones.
- **Flapping and maintenance:** a signal must stay healthy for a set time (15 min by default) before auto-resolving. Staff can set maintenance windows that silence a route during announced operator work.
- **Independent paging:** P1 pages never travel over VASPRO's own SMS routes. They use a separate provider or an on-call tool, so an outage cannot silence its own alarm.
- **Auto-actions with limits:** pausing a failing route, locking an abused key and throttling a spike happen automatically. Suspending a client or posting a country-wide status notice waits for a person to approve.
- **Client alerts:** clients set their own rules in the dashboard (delivery rate, failing webhooks or USSD callbacks, sender ID rejections, daily spend, low balance). They receive them by email, SMS or webhook.
- **Incident report on close:** duration, messages and clients affected, refunds issued and actions taken, generated automatically as the start of the review.
- **Rules are data:** staff edit rules and thresholds in an admin screen, and every change is audited.
- **Building blocks:** Prometheus and Alertmanager for infrastructure metrics; a Go rules service reading the event bus for business signals; ClickHouse for history and baselines.

**Rules added in revision 2:** balance on a client's payout shortcode below the client's level; payout failure rate per rail; a spike in reversal requests; collection callbacks stopping; approved codes nearing the end of their payment window; token wallets that will run out before a scheduled campaign.

## 11. Recurring billing and invoices

Every recurring charge becomes a line on one invoice per client per cycle: each USSD code, shortcode or sender ID rental, plans, and scheduled top-ups. The invoice is paid automatically if the client turned on auto-pay; otherwise it arrives by email with a Pay now button that opens a hosted checkout.

&#91;embedded content: Recurring billing · 3 sources, auto-pay or Pay now, dunning loop\]

If auto-pay fails (empty wallet, expired card), the invoice falls back to the Pay now email, and dunning applies as usual.

**What gets billed**

| Item | Billed | Notes |
| --- | --- | --- |
| Dedicated USSD code | Monthly, per code per network | A client can hold many codes; each is its own invoice line |
| Shared USSD channel or extension range | Monthly | Per channel or range |
| Dedicated shortcode | Monthly, per shortcode per network | Same as USSD codes |
| Sender ID | Monthly, where the network charges a maintenance fee | Uganda charges one per network, for example |
| Subscription plans | Monthly or yearly | Bundles, support plans |
| Recurring top-ups | A fixed amount each cycle | The paid invoice credits the main wallet |
| One-off charges | Once | Setup fees, approved applications (section 7), custom work raised by staff |

**Behaviour**

- **Billable assets register:** every code, shortcode and sender ID a client holds is a record with its monthly fee, network, start date and status. The billing run reads it, so new and released assets are billed correctly without manual work.
- **Billing day and proration:** each client has a billing day; an asset added mid-cycle is charged pro rata in its first invoice (proposal).
- **Pay now link:** signed and tied to one invoice. It opens a checkout that needs no login and shows the invoice lines, tax and due date. The link stops working once the invoice is paid or voided.
- **Auto-pay order:** main wallet first, then the saved card or M-Pesa standing order.
- **Dunning:** reminders before and after the due date, then a grace period set per country pack. After that only the unpaid asset is suspended, not the whole account, and it comes back as soon as the invoice is paid.
- **Where paid money goes:** an asset or plan invoice clears receivables; a recurring top-up invoice credits the main wallet.
- **Documents:** invoices and receipts as PDF; eTIMS in Kenya; corrections by credit note (4.3).
- **Events:** `invoice.issued`, `invoice.paid`, `invoice.overdue`, `asset.suspended`, `asset.reactivated`.

## 12. Shortcode MO and MT messaging

MO (mobile originated) is a subscriber's message to a client's shortcode; MT (mobile terminated) is a message from the shortcode to the subscriber. v3 already handles MO (keywords, inbox, subscriptions) and sends on-demand MT through Safaricom's SDP using a `link_id` and offer code. v4 keeps MO as in section 5, where it arrives as a `message.received` event, and gives MT one flow with three kinds.

&#91;embedded content: Shortcode MO and MT · 3 MT kinds, operator charging, revenue share\]

Bulk MT leaves this flow and runs through the standard send lifecycle with the shortcode as sender, so it reaches the SMS engine like any other SMS. Replies and premium MT go to the operator's subscription delivery platform (SDP) through v4's own SDP adapter, not the SMS engine; the SDP charges the subscriber where the service is premium.

| MT kind | Who pays | Needs | Route |
| --- | --- | --- | --- |
| Bulk MT | The client, in SMS tokens | Shortcode active for bulk on that network | Standard send lifecycle (section 5) |
| Reply MT | The client's tokens on a standard two-way shortcode; the subscriber on a premium on-demand service | The MO's linkId, used inside the operator's time window | Operator SDP for premium; standard routes otherwise |
| Premium MT (subscription content) | The subscriber, charged by the operator | An active subscription with a recorded opt-in that stated the price | Operator SDP with offer code and tariff |

**Behaviour**

- **Subscription lifecycle:** opt-in by MO keyword, USSD or web, with a confirmation that states the price and how to stop. STOP ends it. Operator subscription notifications (activation, renewal, deactivation) keep the subscriber list in step, as v3's subscription sync does today.
- **Charging outcomes:** charged means the content counts as delivered; insufficient airtime means no charge and a retry on the schedule the operator allows; failed means no charge. Each outcome is an event (`mt.charged`, `mt.charge_failed`) with a reason code.
- **No subscription, no send:** a premium MT for a number without an active subscription is rejected with a `CMP-` code before reaching the operator.
- **Revenue share:** the operator settles subscriber charges to VASPRO. Each client's share is recorded as a revenue share payable and paid out per its contract, with a monthly statement per shortcode and offer code.
- **Country rules:** linkId windows, premium price caps and opt-in wording come from each country pack per operator.
- **MO stays as built:** keyword routing to the client's webhook, the inbox, surveys and subscriptions.

## 13. Payments: collections and disbursements

VASPRO moves money for clients in both directions: it collects from their customers and pays out on their behalf. Both directions share the rails, the ledger, the controls and reconciliation. What differs is whose money it is and which wallet it touches.

### 13.1 Service map

&#91;embedded content: Payments service map · 4 collection and 4 disbursement capabilities\]

Collections settle directly into the client's own paybill or till, and payouts leave from the client's own B2C or B2B shortcode using the client's own rail credentials. VASPRO triggers both and relays the notifications but never holds the money (decided 12 Oct). VASPRO earns a fee per transaction, taken from the client's payments token wallet. Airtime, data and bills are the exception: VASPRO buys them from its providers, so they draw on the client's airtime wallet.

| Capability | What the client does | In v3 today |
| --- | --- | --- |
| STK push checkout | Calls the API to prompt a customer's phone to pay | `checkout/mpesa` |
| Paybill and till (C2B) | Registers their own paybill or till; VASPRO receives the confirmations and forwards them | `payment/paybill/register` |
| Payment links and invoices | Creates a link or invoice for their own customer; reuses the hosted checkout from section 11 | New |
| Cards and bank transfers | Customers pay by card or bank through an aggregator | DPO cards |
| B2C payouts | Single or bulk payouts to mobile wallets | `payouts/b2c` |
| B2B payments | Pays suppliers' paybills and tills | `payment/b2b` |
| Bank transfers | Pays bank accounts | New |
| Airtime, data and bills | Buys airtime, bundles or KPLC tokens for recipients | `rewards` |

### 13.2 Payout flow

&#91;embedded content: Payout flow · checks, approval, hold, send, settle or release\]

Approval is needed for every bulk file and for single payouts above the client's approval limit (section 5.1). The hold covers VASPRO's fee in payments tokens; the payout amount itself is checked against the balance of the client's own shortcode. Airtime, data and bill payments follow the same steps but hold their full value in the airtime wallet.

**Behaviour**

- **VASPRO never holds client collections (decided 12 Oct).** Collections settle straight into the client's own paybill or till, and VASPRO only drives the integration and relays notifications. This keeps VASPRO out of holding client money, which in Kenya would likely need Central Bank authorisation as a payment service provider.
- **No double payouts:** each payout carries an idempotency key. If the rail times out, the payout stays pending and a status query runs before any retry.
- **Bulk files** are validated row by row, and the client sees every problem before approving. The approved batch then runs with a result per row and a downloadable report.
- **Recipient checks:** where the rail returns the registered name, it is compared with the expected name and mismatches are flagged. Saved beneficiaries can be allowlisted.
- **Fees** per rail and country come from the pack, are shown before approval, and are captured with the payout.
- **Callbacks:** every collection and payout emits a signed webhook (`payment.received`, `payout.completed`, `payout.failed`) carrying the rail's reference.
- **Reconciliation** matches each rail's statement against VASPRO's records daily, and gaps go to the ops queue. Section 10 alerts on a low balance on the client's payout shortcode and on rising failure rates.
- **Errors** use the `PAY-` family for rail outcomes, plus `VAL-`, `LIM-` and `BAL-` for checks before sending.

## Open points from these flows

I made these design choices to draw the flows; confirm or change them in your review. Decisions still open from the [requirements doc](https://claude.ai/code/artifact/91f2fbc8-54d8-4131-a47a-cf10c8f83f85) are listed last.

- [ ] **Event bus:** NATS JetStream to start, or Kafka from day one?
- [ ] **SMS engine:** confirm with its owners the correlator format and length each gateway accepts (v3 sends a numeric ID; v4 IDs are UUIDv7), whether single sends keep v3's direct gateway calls or all go through the engine queues, how v4 receives Safaricom bulk DLRs (today they reach a separate service), and that the engine's queue consumers can carry contact-group campaigns once v4 stops using `VASPRO_BULK_COMPOSE`.
- [ ] **Service tiers:** is Tier 1 enough for surveys and a shared USSD channel, as drawn, or should those need Tier 2?
- [ ] **Promotional sends outside hours:** queue until the window opens (as drawn), or reject by default?
- [ ] **Application fees:** full refund on rejection (as drawn), or keep a review fee?
- [ ] **Expired messages:** charge or refund when no DLR arrives, per country?
- [ ] **Empty wallet on USSD:** end sessions with the client's unavailable message (as drawn), or allow a small negative balance for Tier 2 and above?
- [ ] **Sandbox test values:** accept the proposed test numbers and amounts?
- [ ] **Still open from the requirements doc:** KYC vendor, shared USSD parent codes, hosting and residency, v3 migration, pricing model.
- [ ] **On-call paging:** an on-call tool (PagerDuty, Opsgenie) or a second SMS provider for P1 pages, and who is on the rota?
- [ ] **Alert thresholds:** accept the default rules in section 10 as starting values?

**Raised by revision 2**

- [x] **Token units:** are tokens message units with a consumption rate per route (as drawn), or a money balance per service?
- [ ] **Unused tokens:** can they be moved back to the main wallet or refunded, and do they expire?
- [x] **Tax point:** is VAT charged on the top-up, on the token purchase, or on use? This decides when invoices are issued.
- [x] **Reversals:** the amount above which a finance lead must approve, and who holds that role. *Answered 9 Nov 2026 (OQ-18): a per-currency threshold in the country pack, KES 2,000 in Kenya; finance leads are staff flagged by the ops CLI.*
- [ ] **Payment window** for approved codes, per country, and whether any deposit is taken at application.
- [ ] **Billing day and proration** policy for assets added mid-cycle.
- [x] **Holding client money:** collections straight to the client's own paybill, or through VASPRO accounts with sweeps? The second option needs legal advice on payment service provider licensing.
- [ ] **Premium MT revenue share:** contract terms and settlement timing per operator.
- [ ] **v3 users without an email address:** confirm the rule that they add one at first v4 sign-in.

Once these are settled, the next stage is detailed design: the data model per service, the OpenAPI spec for the core APIs, the event catalogue, the full error-code catalogue, and a build plan for Phase 0.

## Sources

- VASPRO v3 code (`vaspro-api-v3-2`) and the VASPRO Next requirements doc
- [Sent: Tanzania SMS guide](https://www.sent.dm/resources/tanzania-sms-guide)
- [Telerivet: Uganda SMS compliance](https://www.telerivet.com/blog/uganda-sms-compliance-ucc-dppa)
- [The Citizen: Tigo Tanzania rebrands to Yas](https://www.thecitizen.co.tz/tanzania/business/tigo-tanzania-rebrands-to-yas-in-shift-towards-pan-african-ambitions-4836754)
- [Clickatell: USSD code types](https://clickatell.com/help-center/ussd/what-is-the-difference-between-a-dedicated-semi-dedicated-and-shared-ussd-service-code)
- [Twilio error dictionary](https://www.twilio.com/docs/api/errors) (model for the error catalogue)
