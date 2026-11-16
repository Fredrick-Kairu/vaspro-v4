<?php
// Checks the PHP snippet against VASPRO's golden vectors
// (test/contract runs it in the official php image).

declare(strict_types=1);

require __DIR__ . '/VasproWebhook.php';

$file = json_decode(file_get_contents($argv[1]), true, 512, JSON_THROW_ON_ERROR);
$vectors = $file['vectors'];
$passed = 0;
foreach ($vectors as $v) {
    $body = base64_decode($v['body_base64'], true);
    try {
        VasproWebhook::verify($v['secret'], $v['timestamp'], $v['signature'], $body, $v['now']);
        $got = 'ok';
    } catch (VasproWebhookException $e) {
        $got = $e->reason;
    } catch (Throwable $e) {
        $got = 'error: ' . $e->getMessage();
    }
    if ($got !== $v['expect']) {
        echo "FAIL {$v['name']}: got {$got}, want {$v['expect']}\n";
        continue;
    }
    $passed++;
}
$total = count($vectors);
echo "php: {$passed}/{$total} vectors ok\n";
exit($passed === $total ? 0 : 1);
