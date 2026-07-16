<?php

declare(strict_types=1);

namespace AltEngine;

/**
 * Client for one search instance, bound to a namespace (sent as X-Namespace,
 * default "").
 */
class Search
{
    private string $base;

    public function __construct(
        private readonly Http $http,
        public readonly string $instance,
        public readonly string $namespace = '',
    ) {
        $this->base = '/v1/search/' . Http::seg($instance);
    }

    /** Same instance, different namespace. */
    public function withNamespace(string $namespace): self
    {
        return new self($this->http, $this->instance, $namespace);
    }

    /** @return array<string, string>|null */
    private function headers(): ?array
    {
        return $this->namespace !== '' ? ['x-namespace' => $this->namespace] : null;
    }

    public function index(string $name): SearchIndex
    {
        return new SearchIndex($this->http, $this->base, $name, $this->headers());
    }

    /** @return array{indexes: list<array>, has_more: bool} */
    public function listIndexes(?string $q = null, ?int $limit = null): array
    {
        return $this->http->request('GET', "{$this->base}/indexes", [
            'q' => $q,
            'limit' => $limit,
            'namespace' => $this->namespace !== '' ? $this->namespace : null,
        ]);
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
        return $this->http->request('GET', "{$this->base}/namespaces", ['q' => $q, 'limit' => $limit]);
    }

    /** Delete an index and all its documents. Requires a `full` grant. */
    public function deleteIndex(string $name): bool
    {
        $res = $this->http->request(
            'DELETE',
            "{$this->base}/indexes/" . Http::seg($name),
            headers: $this->headers(),
        );
        return $res['deleted'];
    }
}
