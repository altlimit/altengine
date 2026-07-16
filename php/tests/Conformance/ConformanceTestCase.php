<?php

declare(strict_types=1);

namespace AltEngine\Tests\Conformance;

use AltEngine\AltEngine;
use PHPUnit\Framework\TestCase;

abstract class ConformanceTestCase extends TestCase
{
    protected static function client(): AltEngine
    {
        $url = getenv('ALTENGINE_TEST_URL');
        if ($url === false || $url === '') {
            self::fail('conformance bootstrap did not run (ALTENGINE_TEST_URL unset)');
        }
        return new AltEngine([
            'base_url' => $url,
            'api_key' => getenv('ALTENGINE_TEST_KEY') ?: 'conformance-dev-key',
        ]);
    }

    protected static function destructiveOk(): bool
    {
        return getenv('ALTENGINE_CONFORMANCE_URL') === false
            || getenv('ALTENGINE_CONFORMANCE_DESTRUCTIVE') === '1';
    }

    /** Random suffix so suites never collide across runs or languages. */
    protected static function uniq(string $prefix): string
    {
        return sprintf('%s-%x%04x', $prefix, (int) (microtime(true) * 1000), random_int(0, 0xFFFF));
    }

    protected static function fixture(string $name): array
    {
        $path = dirname(__DIR__, 3) . "/conformance/fixtures/{$name}";
        return json_decode(file_get_contents($path), true);
    }
}
