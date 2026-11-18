"""Verifies the signature on a VASPRO webhook. Python 3.8+, standard
library only: copy this file into your project.

    # Flask: the raw bytes, before any JSON parsing.
    body = request.get_data()
    try:
        verify(
            secret,
            request.headers.get("Vaspro-Webhook-Timestamp", ""),
            request.headers.get("Vaspro-Webhook-Signature", ""),
            body,
        )
    except WebhookVerificationError:
        abort(400)
"""

import hashlib
import hmac
import re
import time

#: How far the timestamp may be from your clock, either way, in seconds.
TOLERANCE_SECONDS = 300

# ASCII digits only: str.isdigit() and int() also accept other scripts' digits.
_DIGITS = re.compile(r"[0-9]+")


class WebhookVerificationError(Exception):
    """The request is not a genuine, fresh VASPRO webhook."""


class InvalidTimestamp(WebhookVerificationError):
    """The timestamp is missing, malformed, or too far from now."""


class InvalidSignature(WebhookVerificationError):
    """No v1 signature matches the body under the secret."""


def verify(secret, timestamp, signature, body, now=None):
    """Raise WebhookVerificationError unless the request is genuine.

    secret is the endpoint's secret exactly as shown (whsec_..., used whole
    as the HMAC key); timestamp and signature are the
    Vaspro-Webhook-Timestamp and -Signature headers; body is the raw request
    body (bytes; a str is taken as UTF-8). now is in Unix seconds and
    defaults to the clock.
    """
    if not isinstance(timestamp, str) or not _DIGITS.fullmatch(timestamp):
        raise InvalidTimestamp()
    if now is None:
        now = int(time.time())
    if abs(now - int(timestamp)) > TOLERANCE_SECONDS:
        raise InvalidTimestamp()

    if isinstance(body, str):
        body = body.encode("utf-8")
    expected = hmac.new(
        secret.encode("utf-8"), timestamp.encode("ascii") + b"." + body, hashlib.sha256
    ).hexdigest().encode("ascii")

    # During a secret rotation there are two values, space-separated.
    # Values with other schemes are for later versions; skip them.
    for value in (signature or "").split(" "):
        if value.startswith("v1=") and hmac.compare_digest(
            value[3:].lower().encode("utf-8"), expected
        ):
            return
    raise InvalidSignature()
