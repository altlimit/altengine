<?php

declare(strict_types=1);

namespace AltEngine;

/** Operations on one search index. All payloads are wire-verbatim arrays. */
class SearchIndex
{
    private string $path;

    /** @param array<string, string>|null $headers */
    public function __construct(
        private readonly Http $http,
        string $base,
        public readonly string $name,
        private readonly ?array $headers,
    ) {
        $this->path = "{$base}/indexes/" . Http::seg($name);
    }

    /**
     * Upsert up to 200 documents (see the F field builders). Returns their
     * ids in order (server-assigned when a document omits 'id').
     *
     * @param list<array> $documents
     * @return list<string>
     */
    public function put(array $documents): array
    {
        $res = $this->http->request(
            'PUT',
            "{$this->path}/documents",
            body: ['documents' => $documents],
            headers: $this->headers,
        );
        return $res['ids'];
    }

    /** Fetch one document, or null when it (or the index) doesn't exist. */
    public function get(string $id): ?array
    {
        try {
            $res = $this->http->request(
                'GET',
                "{$this->path}/documents/" . Http::seg($id),
                headers: $this->headers,
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
     * Delete up to 200 documents by id; missing ids are no-ops. Requires a
     * `full` grant. Returns the number actually deleted.
     *
     * @param list<string> $ids
     */
    public function delete(array $ids): int
    {
        $res = $this->http->request(
            'POST',
            "{$this->path}/documents/delete",
            body: ['ids' => $ids],
            headers: $this->headers,
        );
        return $res['deleted'];
    }

    /**
     * Run a search request. $query is the boolean query language; $options is
     * the rest of the wire request (limit, cursor, sort, facets, snippet,
     * collapse, ids_only, ...).
     */
    public function search(string $query = '', array $options = []): array
    {
        return $this->http->request(
            'POST',
            "{$this->path}/search",
            body: ['query' => $query, ...$options],
            headers: $this->headers,
        );
    }

    /**
     * Iterate every hit across cursor pages.
     *
     * @return \Generator<int, array>
     */
    public function searchAll(string $query = '', array $options = []): \Generator
    {
        unset($options['offset'], $options['cursor']);
        $cursor = null;
        do {
            $page = $this->search($query, $cursor === null ? $options : [...$options, 'cursor' => $cursor]);
            foreach ($page['results'] as $hit) {
                yield $hit;
            }
            $cursor = $page['cursor'] ?? null;
        } while ($cursor);
    }

    /** One page of documents in id order (keyset pagination via start_id). */
    public function listDocuments(
        ?string $startId = null,
        ?bool $includeStart = null,
        ?int $limit = null,
        ?bool $idsOnly = null,
    ): array {
        return $this->http->request('GET', "{$this->path}/documents", [
            'start_id' => $startId,
            'include_start' => $includeStart,
            'limit' => $limit,
            'ids_only' => $idsOnly,
        ], headers: $this->headers);
    }

    /**
     * Iterate every document in the index (keyset pagination handled for you).
     *
     * @return \Generator<int, array>
     */
    public function listAllDocuments(?int $limit = null): \Generator
    {
        $startId = null;
        while (true) {
            $page = $this->listDocuments($startId, includeStart: $startId === null, limit: $limit);
            $docs = $page['documents'] ?? [];
            if ($docs === []) {
                return;
            }
            foreach ($docs as $doc) {
                yield $doc;
            }
            $startId = $docs[count($docs) - 1]['id'];
        }
    }

    /**
     * The index's union field schema: for every field name, the types it has
     * been indexed with.
     */
    public function schema(): array
    {
        return $this->http->request('GET', "{$this->path}/schema", headers: $this->headers);
    }
}
