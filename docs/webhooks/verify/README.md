# Verifying VASPRO webhooks

Every webhook VASPRO sends is a `POST` with a JSON body and three headers:

| Header | Value |
|---|---|
| `Vaspro-Webhook-Id` | The event's ID. Retries and replays keep it, so use it to ignore duplicates. |
| `Vaspro-Webhook-Timestamp` | When this attempt was signed, in Unix seconds. |
| `Vaspro-Webhook-Signature` | `v1=` and the lower-case hex HMAC-SHA256 of the timestamp, a full stop and the body, keyed by your endpoint's secret. During a secret rotation there are two values separated by a space, the new secret's first. |

To verify a request:

1. Take the **raw body bytes** as received. Don't parse the JSON and re-serialise it: any change to spacing or key order breaks the signature.
2. Use the secret **exactly as shown** when you created or rotated the endpoint, `whsec_` prefix included, as the HMAC key. Don't base64-decode it.
3. Refuse a timestamp that isn't plain digits or is more than 5 minutes from your clock. This stops an old request being replayed.
4. Compute `hex(HMAC-SHA256(secret, timestamp + "." + body))` and compare it, in constant time, with each `v1=` value in the header. Accept if any matches. Ignore values with other schemes, which a later version may add.

Then answer with any 2xx within 10 seconds, and do slow work afterwards. Anything else is retried (10 attempts over about 3 days).

## Snippets

Each snippet is one file using only the standard library. Copy it into your project.

| Language | File | Call |
|---|---|---|
| Go 1.22+ | [`go/vasprowebhook/verify.go`](go/vasprowebhook/verify.go) | `vasprowebhook.Verify(secret, timestamp, signature, body, time.Now())` returns `ErrBadTimestamp` or `ErrBadSignature` |
| PHP 8.1+ | [`php/VasproWebhook.php`](php/VasproWebhook.php) | `VasproWebhook::verify($secret, $timestamp, $signature, $body)` throws `VasproWebhookException` (`->reason`) |
| Node 18+ | [`node/vaspro-webhook.js`](node/vaspro-webhook.js) | `verify({ secret, timestamp, signature, body })` throws `WebhookVerificationError` (`.code`) |
| Python 3.8+ | [`python/vaspro_webhook.py`](python/vaspro_webhook.py) | `verify(secret, timestamp, signature, body)` raises `InvalidTimestamp` or `InvalidSignature` |

Each file's header comment shows it in a request handler.

## How they're tested

`testdata/vectors.json` holds signed requests made by VASPRO's own signer, each with its expected outcome. They include rotations, every kind of tampering, the edges of the 5-minute window, malformed timestamps, other schemes, and non-ASCII and CRLF bodies. The secrets in it are made up.

`make test-contract` regenerates the vectors in memory, checks the file still matches, then runs each language's `check_vectors` in its official Docker image. Every snippet must give every vector's expected outcome.
