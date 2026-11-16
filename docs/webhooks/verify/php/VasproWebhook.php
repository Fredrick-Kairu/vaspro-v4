<?php
// Verifies the signature on a VASPRO webhook. PHP 8.1+, no extensions
// beyond the defaults: copy this file into your project.
//
//   $body = file_get_contents('php://input'); // the raw bytes, before json_decode
//   try {
//       VasproWebhook::verify(
//           $secret,
//           $_SERVER['HTTP_VASPRO_WEBHOOK_TIMESTAMP'] ?? '',
//           $_SERVER['HTTP_VASPRO_WEBHOOK_SIGNATURE'] ?? '',
//           $body,
//       );
//   } catch (VasproWebhookException $e) {
//       http_response_code(400);
//       exit;
//   }

declare(strict_types=1);

final class VasproWebhookException extends RuntimeException
{
    /** @param string $reason 'bad_timestamp' or 'bad_signature' */
    public function __construct(public readonly string $reason)
    {
        parent::__construct("VASPRO webhook: {$reason}");
    }
}

final class VasproWebhook
{
    /** How far the timestamp may be from your clock, either way, in seconds. */
    public const TOLERANCE = 300;

    /**
     * $secret is the endpoint's secret exactly as shown (whsec_…, used whole
     * as the HMAC key); $timestamp and $signature are the
     * Vaspro-Webhook-Timestamp and -Signature headers; $body is the raw
     * request body. $now defaults to time().
     *
     * @throws VasproWebhookException
     */
    public static function verify(string $secret, string $timestamp, string $signature, string $body, ?int $now = null): void
    {
        // D: '$' must not match before a trailing newline.
        if (preg_match('/^[0-9]+$/D', $timestamp) !== 1) {
            throw new VasproWebhookException('bad_timestamp');
        }
        $now ??= time();
        // A timestamp too long for an int becomes PHP_INT_MAX: far outside the window.
        if (abs($now - (int) $timestamp) > self::TOLERANCE) {
            throw new VasproWebhookException('bad_timestamp');
        }

        $expected = hash_hmac('sha256', $timestamp . '.' . $body, $secret);

        // During a secret rotation there are two values, space-separated.
        // Values with other schemes are for later versions; skip them.
        foreach (explode(' ', $signature) as $value) {
            if (str_starts_with($value, 'v1=') && hash_equals($expected, strtolower(substr($value, 3)))) {
                return;
            }
        }
        throw new VasproWebhookException('bad_signature');
    }
}
