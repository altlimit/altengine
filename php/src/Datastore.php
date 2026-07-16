<?php

declare(strict_types=1);

namespace AltEngine;

/**
 * Client for one datastore instance, bound to a namespace (default "").
 * All payloads are wire-verbatim arrays.
 */
class Datastore
{
    private string $base;
    private string $ns;

    public function __construct(
        private readonly Http $http,
        public readonly string $instance,
        public readonly string $namespace = '',
    ) {
        $this->base = '/v1/datastore/' . Http::seg($instance);
        $this->ns = $this->base . '/namespaces/' . Http::seg($namespace);
    }

    /** Same instance, different namespace. */
    public function withNamespace(string $namespace): self
    {
        return new self($this->http, $this->instance, $namespace);
    }

    /**
     * Upsert up to 500 documents (['key' => ..., 'data' => [...]]; omit 'key'
     * for auto-id). Returns their keys in order.
     *
     * @param list<array{key?: string|int, data: mixed}> $documents
     * @return list<string>
     */
    public function put(string $collection, array $documents): array
    {
        $res = $this->http->request(
            'POST',
            "{$this->ns}/collections/" . Http::seg($collection) . '/documents',
            body: ['documents' => $documents],
        );
        return $res['keys'];
    }

    /**
     * Fetch one document (null when missing), or — passed a list of keys — up
     * to 500 documents in one round trip, order-preserving with null
     * placeholders for missing keys (App Engine db.get semantics).
     *
     * @param string|int|list<string|int> $key
     */
    public function get(string $collection, string|int|array $key): ?array
    {
        if (is_array($key)) {
            $res = $this->http->request(
                'POST',
                "{$this->ns}/collections/" . Http::seg($collection) . '/documents/get',
                body: ['keys' => $key],
            );
            $byKey = [];
            foreach ($res['documents'] as $doc) {
                $byKey[$doc['key']] = $doc;
            }
            return array_map(static fn ($k) => $byKey[(string) $k] ?? null, $key);
        }
        try {
            $res = $this->http->request(
                'GET',
                "{$this->ns}/collections/" . Http::seg($collection) . '/documents/' . Http::seg($key),
            );
            return $res['document'];
        } catch (AltEngineError $err) {
            if ($err->status === 404) {
                return null;
            }
            throw $err;
        }
    }

    /**
     * Delete up to 500 documents by key; missing keys are no-ops. Returns the
     * number actually deleted.
     *
     * @param list<string|int> $keys
     */
    public function delete(string $collection, array $keys): int
    {
        $res = $this->http->request(
            'POST',
            "{$this->ns}/collections/" . Http::seg($collection) . '/documents/delete',
            body: ['keys' => $keys],
        );
        return $res['deleted'];
    }

    /**
     * Run one page of an index-served query. $request is the wire request:
     * where, order, limit (default 25, max 500), cursor, keys_only, join.
     */
    public function query(string $collection, array $request = []): array
    {
        return $this->http->request(
            'POST',
            "{$this->ns}/collections/" . Http::seg($collection) . '/query',
            body: (object) $request,
        );
    }

    /**
     * Iterate every matching document across pages (cursor handled for you).
     *
     * @return \Generator<int, array>
     */
    public function queryAll(string $collection, array $request = []): \Generator
    {
        unset($request['cursor']);
        $cursor = null;
        do {
            $page = $this->query($collection, $cursor === null ? $request : [...$request, 'cursor' => $cursor]);
            foreach ($page['documents'] ?? [] as $doc) {
                yield $doc;
            }
            $cursor = $page['cursor'] ?? null;
        } while ($cursor);
    }

    /**
     * Grouped metrics over an index-served filter: metrics (count/sum/avg/
     * min/max), group, where, order, limit.
     */
    public function aggregate(string $collection, array $request): array
    {
        return $this->http->request(
            'POST',
            "{$this->ns}/collections/" . Http::seg($collection) . '/aggregate',
            body: (object) $request,
        );
    }

    /**
     * Apply up to 500 operations (op: put/delete/mutate/check) atomically
     * within this namespace. NOT retried automatically (increments would
     * double-apply); a failed check throws a 409 AltEngineError. Returns the
     * per-op resulting key, null for delete/check ops.
     *
     * @param list<array> $operations
     * @return list<?string>
     */
    public function transaction(array $operations): array
    {
        $res = $this->http->request(
            'POST',
            "{$this->ns}/transaction",
            body: ['operations' => $operations],
            retry: false,
        );
        return $res['keys'];
    }

    // --- indexes ---

    /** @return list<array> */
    public function listIndexes(string $collection): array
    {
        return $this->http->request(
            'GET',
            "{$this->ns}/collections/" . Http::seg($collection) . '/indexes',
        )['indexes'];
    }

    /**
     * Create a secondary index (idempotent). Field entries are "field" or
     * "field:asc" / "field:desc", max 8.
     *
     * @param list<string> $fields
     */
    public function createIndex(string $collection, array $fields, bool $unique = false): array
    {
        $res = $this->http->request(
            'POST',
            "{$this->ns}/collections/" . Http::seg($collection) . '/indexes',
            body: ['fields' => $fields, 'unique' => $unique],
        );
        return $res['index'];
    }

    public function deleteIndex(string $collection, int $indexId): bool
    {
        $res = $this->http->request(
            'DELETE',
            "{$this->ns}/collections/" . Http::seg($collection) . "/indexes/{$indexId}",
        );
        return $res['deleted'];
    }

    // --- namespaces (instance-wide, not bound to this client's namespace) ---

    /** @return array{namespaces: list<string>, has_more: bool} */
    public function listNamespaces(?string $q = null, ?int $limit = null): array
    {
        return $this->http->request('GET', "{$this->base}/namespaces", ['q' => $q, 'limit' => $limit]);
    }

    /** Delete a namespace and everything in it. Requires a `full` grant. */
    public function deleteNamespace(string $namespace): bool
    {
        return $this->http->request('DELETE', "{$this->base}/namespaces/" . Http::seg($namespace))['deleted'];
    }
}
