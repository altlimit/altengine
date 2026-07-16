<?php

declare(strict_types=1);

namespace AltEngine\Tests\Conformance;

use AltEngine\Channel;

/**
 * HTTP-only channel coverage (token minting + publish). WebSocket lifecycle
 * scenarios are covered by the JS/Go/Python suites — this SDK does not open
 * WebSockets (mint a token here and subscribe from the client app).
 */
final class ChannelTest extends ConformanceTestCase
{
    private static Channel $ch;

    public static function setUpBeforeClass(): void
    {
        self::$ch = self::client()->channel(self::uniq('sdk-conf'));
    }

    public function testTokenMint(): void
    {
        $tok = self::$ch->createToken(['room:1', 'room:2'], ['ttl_seconds' => 600]);
        $this->assertCount(3, explode('.', $tok['token']));
        $this->assertSame(['room:1', 'room:2'], $tok['channels']);
        $this->assertStringContainsString('/subscribe', $tok['ws_url']);
        $this->assertGreaterThan(time(), $tok['expires_at']);
    }

    public function testHttpPublishDeliveredCount(): void
    {
        $this->assertSame(0, self::$ch->publish('room:empty', ['hello' => 1]));
    }
}
