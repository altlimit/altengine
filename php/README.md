# altengine PHP SDK

Official PHP SDK for [altengine](https://www.altengine.net) — managed
**datastore**, **search**, and realtime **channels** behind one API key.
PHP ≥ 8.1, Guzzle transport.

```bash
composer require altlimit/altengine
```

## Setup

```php
use AltEngine\AltEngine;

$ae = new AltEngine(['api_key' => 'ae_...']);   // production: https://api.altengine.net
```

Everything is overridable, nothing is required:

- `api_key` — falls back to the `ALTENGINE_API_KEY` env var
- `dev => true` — target the [local emulator](https://github.com/altlimit/altengine)
  at `http://127.0.0.1:9191` (`altengine dev`)
- `base_url` — explicit origin; also settable via the `ALTENGINE_URL` env var
  (resolution: `base_url` → `dev` → `ALTENGINE_URL` → production)

Requests and responses are **wire-verbatim arrays** — what the REST API
documents is exactly what you pass and get back. Retryable failures (429 with
`Retry-After`, 502/503/504, network errors) are retried up to 3 times with
jittered backoff — except transactions and publishes, which are never
auto-retried. Errors throw `AltEngineError` with `errorCode`, `status`,
`details`, and `retryAfter`.

## Datastore

```php
$db = $ae->datastore('myapp', 'prod');   // instance, namespace (default "")

$keys = $db->put('todos', [['data' => ['title' => 'ship SDK', 'done' => false]]]);
$doc = $db->get('todos', $keys[0]);      // null when missing
$docs = $db->get('todos', $keys);        // array in → array out, null for missing
$db->delete('todos', $keys);             // missing keys are no-ops

// Queries (queryAll pages cursors for you)
$page = $db->query('todos', [
    'where' => [['field' => 'done', 'op' => '=', 'value' => false]],
    'order' => [['field' => 'priority', 'dir' => 'desc']],
    'limit' => 100,
]);
foreach ($db->queryAll('todos', ['where' => [['field' => 'owner', 'op' => '=', 'value' => 'ana']]]) as $doc) {
    // ...
}

// Aggregates
$res = $db->aggregate('todos', [
    'group' => ['owner'],
    'metrics' => [['fn' => 'count', 'as' => 'n'], ['fn' => 'sum', 'field' => 'priority', 'as' => 'total']],
]);

// Transactions (atomic within a namespace; a failed check throws a 409)
$db->transaction([
    ['op' => 'check', 'collection' => 'users', 'key' => 'u1', 'exists' => true],
    ['op' => 'mutate', 'collection' => 'counters', 'key' => 'todos', 'increment' => ['total' => 1], 'upsert' => true],
]);

// Indexes
$db->createIndex('todos', ['owner', 'created:desc']);
```

## Search

```php
use AltEngine\F;

$idx = $ae->search('myapp')->index('products');

$idx->put([[
    'id' => 'p1',
    'fields' => [
        F::text('title', 'Blue Suede Shoes'),
        F::atom('sku', 'BS-001'),
        F::number('price', 59),
        F::date('added', new DateTimeImmutable()),
        F::geo('store', 37.77, -122.41),
    ],
    'facets' => [F::atomFacet('category', 'shoes'), F::numberFacet('price', 59)],
]]);

$res = $idx->search('shoes price<100', [
    'facets' => ['category'],
    'snippet' => ['fields' => ['title']],
    'sort' => [['expr' => 'price']],
    'limit' => 20,
]);

foreach ($idx->searchAll('shoes') as $hit) {   // cursor-paged
    // ...
}
```

## Channels

Server side — mint subscriber tokens, publish, read presence:

```php
$ch = $ae->channel('myapp');
$tok = $ch->createToken(['room:1'], ['ttl_seconds' => 3600, 'presence_id' => 'u42']);
$delivered = $ch->publish('room:1', ['hello' => 'world']);
$roster = $ch->presence('room:1');
```

This SDK does not open WebSockets — hand `['token' => ..., 'ws_url' => ...]`
to your web/mobile client and subscribe there (e.g. with
[`@altengine/sdk/channel`](https://www.npmjs.com/package/@altengine/sdk)).

## Local development

Run the [altengine emulator](https://github.com/altlimit/altengine) and
construct the client with `'dev' => true` (or set `ALTENGINE_URL`) — any
non-empty API key works:

```bash
alt install altlimit/altengine && altengine dev
```

The conformance suite runs against a spawned emulator:

```bash
composer install && composer test
```

Get an API key and instances for production at [www.altengine.net](https://www.altengine.net).
