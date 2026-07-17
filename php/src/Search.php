<?php

declare(strict_types=1);

namespace AltEngine;

/**
 * Client for one search instance, bound to a namespace (carried in the request
 * path, default "").
 */
class Search
{
    private string $base;
    private string $ns;

    public function __construct(
        private readonly Http $http,
        public readonly string $instance,
        public readonly string $namespace = '',
    ) {
        $this->base = '/v1/search/' . Http::seg($instance);
        $this->ns = $this->base . '/ns/' . Http::nsSeg($namespace);
    }

    /** Same instance, different namespace. */
    public function withNamespace(string $namespace): self
    {
        return new self($this->http, $this->instance, $namespace);
    }

    public function index(string $name): SearchIndex
    {
        return new SearchIndex($this->http, $this->ns, $name);
    }

    /** Indexes in this client's namespace. @return array{indexes: list<array>, has_more: bool} */
    public function listIndexes(?string $q = null, ?int $limit = null): array
    {
        return $this->http->request('GET', "{$this->ns}/idx", ['q' => $q, 'limit' => $limit]);
    }

    /**
     * Distinct namespaces with live indexes — alphabetical, $q substring
     * search, $limit default 50 (max 100). Instance-wide; the default
     * namespace appears as "".
     *
     * @return array{namespaces: list<string>, has_more: bool}
     */
    public function listNamespaces(?string $q = null, ?int $limit = null): array
    {
        return $this->http->request('GET', "{$this->base}/ns", ['q' => $q, 'limit' => $limit]);
    }

    /** Delete an index and all its documents. Requires a `full` grant. */
    public function deleteIndex(string $name): bool
    {
        $res = $this->http->request('DELETE', "{$this->ns}/idx/" . Http::seg($name));
        return $res['deleted'];
    }
}
