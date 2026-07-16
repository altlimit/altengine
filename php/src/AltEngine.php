<?php

declare(strict_types=1);

namespace AltEngine;

/**
 * altengine — official PHP SDK for https://www.altengine.net (managed
 * datastore, search, and realtime channels behind one API key).
 *
 *     use AltEngine\AltEngine;
 *     use AltEngine\F;
 *
 *     $ae = new AltEngine(['api_key' => 'ae_...']);   // production api.altengine.net
 *     // $ae = new AltEngine(['dev' => true, 'api_key' => 'dev']); // local `altengine dev`
 *
 *     $db = $ae->datastore('myapp');
 *     $keys = $db->put('todos', [['data' => ['title' => 'ship SDK']]]);
 *
 *     $idx = $ae->search('myapp')->index('products');
 *     $idx->put([['fields' => [F::text('title', 'Blue Shoes'), F::number('price', 59)]]]);
 *
 *     $ch = $ae->channel('myapp');
 *     $tok = $ch->createToken(['room:1']);
 *
 * Options: `api_key` (falls back to the ALTENGINE_API_KEY env var),
 * `dev` (target the local emulator), `base_url` (explicit origin; also via
 * ALTENGINE_URL — resolution: base_url → dev → ALTENGINE_URL → production),
 * `timeout`, `max_attempts`, `client` (a preconfigured Guzzle client).
 *
 * Requests and responses are wire-verbatim arrays — what the REST API
 * documents is exactly what you pass and get back. Retryable failures (429
 * with Retry-After, 502/503/504, network errors) are retried up to 3 times
 * with jittered backoff — except transactions and publishes, which are never
 * auto-retried. Errors throw AltEngineError with errorCode/status/details/
 * retryAfter.
 */
class AltEngine
{
    private Http $http;

    /**
     * @param array{api_key?: string, base_url?: string, dev?: bool, timeout?: float,
     *              max_attempts?: int, client?: \GuzzleHttp\Client} $options
     */
    public function __construct(array $options = [])
    {
        $this->http = new Http($options);
    }

    /** Client for a datastore instance (namespace-bound; default ""). */
    public function datastore(string $instance, string $namespace = ''): Datastore
    {
        return new Datastore($this->http, $instance, $namespace);
    }

    /** Client for a search instance (namespace-bound; default ""). */
    public function search(string $instance, string $namespace = ''): Search
    {
        return new Search($this->http, $instance, $namespace);
    }

    /** Server-side client for a channel instance (tokens, publish, presence). */
    public function channel(string $instance): Channel
    {
        return new Channel($this->http, $instance);
    }
}
