// Verifies the signature on a VASPRO webhook. Node 18+, no dependencies:
// copy this file into your project.
//
//   // Express: keep the raw bytes, before any JSON parsing.
//   app.post('/webhooks/vaspro', express.raw({ type: 'application/json' }), (req, res) => {
//     try {
//       verify({
//         secret: process.env.VASPRO_WEBHOOK_SECRET,
//         timestamp: req.get('Vaspro-Webhook-Timestamp'),
//         signature: req.get('Vaspro-Webhook-Signature'),
//         body: req.body, // a Buffer
//       });
//     } catch (e) {
//       return res.sendStatus(400);
//     }
//     const event = JSON.parse(req.body);
//     res.sendStatus(204);
//   });
'use strict';

const crypto = require('node:crypto');

/** How far the timestamp may be from your clock, either way, in seconds. */
const TOLERANCE_SECONDS = 300;

class WebhookVerificationError extends Error {
  /** @param {'bad_timestamp'|'bad_signature'} code */
  constructor(code) {
    super(`VASPRO webhook: ${code}`);
    this.name = 'WebhookVerificationError';
    this.code = code;
  }
}

/**
 * secret is the endpoint's secret exactly as shown (whsec_…, used whole as
 * the HMAC key); timestamp and signature are the Vaspro-Webhook-Timestamp
 * and -Signature headers; body is the raw request body (a Buffer; a string
 * is taken as UTF-8). now is in Unix seconds and defaults to the clock.
 * Throws WebhookVerificationError.
 */
function verify({ secret, timestamp, signature, body, now = Math.floor(Date.now() / 1000) }) {
  if (typeof timestamp !== 'string' || !/^[0-9]+$/.test(timestamp)) {
    throw new WebhookVerificationError('bad_timestamp');
  }
  if (Math.abs(now - Number(timestamp)) > TOLERANCE_SECONDS) {
    throw new WebhookVerificationError('bad_timestamp');
  }

  const expected = Buffer.from(
    crypto
      .createHmac('sha256', secret)
      .update(`${timestamp}.`)
      .update(Buffer.isBuffer(body) ? body : Buffer.from(body, 'utf8'))
      .digest('hex'),
  );

  // During a secret rotation there are two values, space-separated.
  // Values with other schemes are for later versions; skip them.
  for (const value of String(signature ?? '').split(' ')) {
    if (!value.startsWith('v1=')) continue;
    const candidate = Buffer.from(value.slice(3).toLowerCase(), 'utf8');
    // timingSafeEqual throws on different lengths.
    if (candidate.length === expected.length && crypto.timingSafeEqual(candidate, expected)) {
      return;
    }
  }
  throw new WebhookVerificationError('bad_signature');
}

module.exports = { verify, WebhookVerificationError, TOLERANCE_SECONDS };
