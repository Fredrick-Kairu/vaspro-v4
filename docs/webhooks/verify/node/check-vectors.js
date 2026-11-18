// Checks the Node snippet against VASPRO's golden vectors
// (test/contract runs it in the official node image).
'use strict';

const fs = require('node:fs');
const { verify, WebhookVerificationError } = require('./vaspro-webhook');

const { vectors } = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
let passed = 0;
for (const v of vectors) {
  const body = Buffer.from(v.body_base64, 'base64');
  let got;
  try {
    verify({ secret: v.secret, timestamp: v.timestamp, signature: v.signature, body, now: v.now });
    got = 'ok';
  } catch (e) {
    got = e instanceof WebhookVerificationError ? e.code : `error: ${e.message}`;
  }
  if (got !== v.expect) {
    console.log(`FAIL ${v.name}: got ${got}, want ${v.expect}`);
    continue;
  }
  passed++;
}
console.log(`node: ${passed}/${vectors.length} vectors ok`);
process.exit(passed === vectors.length ? 0 : 1);
