<?php

declare(strict_types=1);

namespace AltEngine;

use GuzzleHttp\Client as GuzzleClient;
use GuzzleHttp\Exception\GuzzleException;

/**
 * Minimal JSON-over-HTTP transport shared by all service clients.
 *
 * @internal
 */
final class Http
{
    /** Production API origin used when no override is given. */
    public const DEFAULT_BASE_URL = 'https://api.altengine.net';
    /** Where `altengine dev` (the local emulator) listens by default. */
    public const DEV_BASE_URL = 'http://127.0.0.1:9191';

    private const MAX_ATTEMPTS = 3;
    private const BASE_DELAY = 0.25;
    private const MAX_DELAY = 4.0;

    private string $baseUrl;
    private ?string $apiKey;
    private GuzzleClient $client;
    private int $maxAttempts;

    /**
     * @param array{api_key?: string, base_url?: string, dev?: bool, timeout?: float,
     *              max_attempts?: int, client?: GuzzleClient} $options
     */
    public function __construct(array $options = [])
    {
        $baseUrl = $options['base_url']
            ?? (($options['dev'] ?? false) ? self::DEV_BASE_URL : null)
            ?? (getenv('ALTENGINE_URL') ?: null)
            ?? self::DEFAULT_BASE_URL;
        $this->baseUrl = rtrim($baseUrl, '/');
        $this->apiKey = $options['api_key'] ?? (getenv('ALTENGINE_API_KEY') ?: null);
        $this->maxAttempts = $options['max_attempts'] ?? self::MAX_ATTEMPTS;
        $this->client = $options['client'] ?? new GuzzleClient([
            'timeout' => $options['timeout'] ?? 30.0,
            'http_errors' => false,
        ]);
    }

    /**
     * @param array<string, mixed>|null $query
     * @param array<string, string>|null $headers
     */
    public function request(
        string $method,
        string $path,
        ?array $query = null,
        mixed $body = null,
        ?array $headers = null,
        bool $retry = true,
    ): mixed {
        $attempts = $retry ? $this->maxAttempts : 1;
        $attempt = 0;
        while (true) {
            $attempt++;
            try {
                return $this->once($method, $path, $query, $body, $headers);
            } catch (AltEngineError $err) {
                if (!$err->retryable() || $attempt >= $attempts) {
                    throw $err;
                }
                $this->backoff($attempt, $err->retryAfter);
            } catch (AltEngineNetworkError $err) {
                if ($attempt >= $attempts) {
                    throw $err;
                }
                $this->backoff($attempt, null);
            }
        }
    }

    private function backoff(int $attempt, ?float $retryAfter): void
    {
        $delay = min(self::BASE_DELAY * (2 ** ($attempt - 1)), self::MAX_DELAY);
        if ($retryAfter !== null && $retryAfter > $delay) {
            $delay = $retryAfter;
        }
        // Full jitter keeps concurrent retries from stampeding in unison.
        usleep((int) ($delay * (0.5 + mt_rand() / mt_getrandmax() * 0.5) * 1_000_000));
    }

    /**
     * @param array<string, mixed>|null $query
     * @param array<string, string>|null $headers
     */
    private function once(string $method, string $path, ?array $query, mixed $body, ?array $headers): mixed
    {
        $hdrs = $headers ?? [];
        if ($this->apiKey !== null) {
            $hdrs['authorization'] = 'Bearer ' . $this->apiKey;
        }
        $options = ['headers' => $hdrs];
        if ($query !== null) {
            $options['query'] = array_map(
                static fn ($v) => is_bool($v) ? ($v ? 'true' : 'false') : (string) $v,
                array_filter($query, static fn ($v) => $v !== null),
            );
        }
        if ($body !== null) {
            $options['json'] = $body;
        }

        try {
            $res = $this->client->request($method, $this->baseUrl . $path, $options);
        } catch (GuzzleException $exc) {
            throw new AltEngineNetworkError("request failed: {$method} {$path}: {$exc->getMessage()}", 0, $exc);
        }

        $status = $res->getStatusCode();
        $raw = (string) $res->getBody();
        if ($status < 200 || $status > 299) {
            $code = 'INTERNAL';
            $message = "HTTP {$status}";
            $details = null;
            $parsed = json_decode($raw, true);
            if (is_array($parsed) && isset($parsed['error']) && is_array($parsed['error'])) {
                $code = $parsed['error']['code'] ?? $code;
                $message = $parsed['error']['message'] ?? $message;
                $details = $parsed['error']['details'] ?? null;
            }
            $ra = $res->getHeaderLine('retry-after');
            $retryAfter = is_numeric($ra) ? (float) $ra : null;
            throw new AltEngineError($code, $message, $status, $details, $retryAfter);
        }
        if ($status === 204 || $raw === '') {
            return null;
        }
        return json_decode($raw, true);
    }

    /** Encode one path segment (instance/index/collection/key names). */
    public static function seg(string|int $value): string
    {
        return rawurlencode((string) $value);
    }

    /**
     * Encode a namespace path segment. The empty (default) namespace can't ride
     * a URL path — routers collapse the resulting "//" — so it travels as the
     * reserved sentinel `_default` (the server maps it back to "").
     */
    public static function nsSeg(string $namespace): string
    {
        return rawurlencode($namespace === '' ? '_default' : $namespace);
    }
}
