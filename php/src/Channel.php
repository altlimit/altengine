<?php

declare(strict_types=1);

namespace AltEngine;

/**
 * Server-side client for one channel instance (API-key auth): mint subscriber
 * tokens, publish over HTTP, and read presence. Browser/edge WebSocket
 * subscribers should use `@altengine/sdk/channel` (JS) with a token minted
 * here — this SDK does not open WebSockets.
 */
class Channel
{
    private string $base;

    public function __construct(
        private readonly Http $http,
        public readonly string $instance,
    ) {
        $this->base = '/v1/channel/' . Http::seg($instance);
    }

    /**
     * Mint a subscriber token (JWT) for the given channels (≤100 names, each
     * ≤200 bytes). Options: ttl_seconds (default 3600, max 14400), publish
     * ("http"|"ws"|"all"|true — requires a write grant), presence_id (≤128
     * bytes, bound into the token server-side). Returns
     * {token, expires_at, channels, publish, ws_url, ...}.
     *
     * @param list<string> $channels
     */
    public function createToken(array $channels, array $options = []): array
    {
        return $this->http->request('POST', "{$this->base}/tokens", body: ['channels' => $channels, ...$options]);
    }

    /**
     * Publish a message (framed ≤32 KiB) to a channel; returns the number of
     * subscribers reached. NOT retried automatically (a retry double-delivers).
     */
    public function publish(string $channel, mixed $data): int
    {
        $res = $this->http->request(
            'POST',
            "{$this->base}/publish",
            body: ['channel' => $channel, 'data' => $data],
            retry: false,
        );
        return $res['delivered'];
    }

    /** Presence roster for a channel (requires the instance's presence flag). */
    public function presence(string $channel): array
    {
        return $this->http->request('GET', "{$this->base}/presence", ['channel' => $channel]);
    }
}
