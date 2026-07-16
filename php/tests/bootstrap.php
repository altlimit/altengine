<?php

/**
 * Conformance harness: builds the emulator once and spawns it on a free port
 * for the run. When ALTENGINE_CONFORMANCE_URL is set, the suite targets that
 * instead and no emulator is spawned.
 */

declare(strict_types=1);

require __DIR__ . '/../vendor/autoload.php';

$external = getenv('ALTENGINE_CONFORMANCE_URL');
if ($external !== false && $external !== '') {
    putenv("ALTENGINE_TEST_URL={$external}");
    putenv('ALTENGINE_TEST_KEY=' . (getenv('ALTENGINE_CONFORMANCE_KEY') ?: ''));
    return;
}

$repoRoot = dirname(__DIR__, 2);
$bin = sys_get_temp_dir() . '/altengine-conformance-php';

exec(
    sprintf('cd %s && go build -o %s ./cmd/altengine 2>&1', escapeshellarg("{$repoRoot}/cli"), escapeshellarg($bin)),
    $output,
    $status,
);
if ($status !== 0) {
    fwrite(STDERR, "building emulator failed:\n" . implode("\n", $output) . "\n");
    exit(1);
}

// Find a free port by binding to :0 and releasing it.
$sock = stream_socket_server('tcp://127.0.0.1:0', $errno, $errstr);
$port = (int) explode(':', stream_socket_get_name($sock, false))[1];
fclose($sock);

$proc = proc_open(
    [$bin, 'dev', '--memory', '--port', (string) $port],
    [1 => ['file', '/dev/null', 'w'], 2 => ['file', '/dev/null', 'w']],
    $pipes,
);
if (!is_resource($proc)) {
    fwrite(STDERR, "starting emulator failed\n");
    exit(1);
}

$base = "http://127.0.0.1:{$port}";
$deadline = microtime(true) + 15;
$ctx = stream_context_create(['http' => ['timeout' => 1, 'ignore_errors' => true]]);
while (true) {
    if (@file_get_contents("{$base}/healthz", false, $ctx) !== false) {
        break;
    }
    if (microtime(true) > $deadline) {
        proc_terminate($proc, 9);
        fwrite(STDERR, "emulator did not become healthy in 15s\n");
        exit(1);
    }
    usleep(100_000);
}

putenv("ALTENGINE_TEST_URL={$base}");
putenv('ALTENGINE_TEST_KEY=conformance-dev-key');

register_shutdown_function(static function () use ($proc): void {
    proc_terminate($proc);
});
