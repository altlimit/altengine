import { Http, seg, nsSeg } from "../http.js";
import type {
  IndexSchema,
  IndexesPage,
  IndexInfo,
  ListDocumentsOptions,
  SearchDocument,
  SearchHit,
  SearchRequest,
  SearchResponse,
} from "./types.js";

export interface SearchOptions {
  /** Namespace for all index operations (default `""`). Rides the request path
   * (`/ns/{ns}/idx/{index}/…`), mirroring datastore. */
  namespace?: string;
}

/** Client for one search instance, bound to a namespace. */
export class SearchClient {
  readonly instance: string;
  readonly namespace: string;
  private readonly http: Http;
  private readonly base: string;
  /** Namespace-scoped base: `/v1/search/{instance}/ns/{ns}`. */
  private readonly nsBase: string;

  constructor(http: Http, instance: string, opts: SearchOptions = {}) {
    this.http = http;
    this.instance = instance;
    this.namespace = opts.namespace ?? "";
    this.base = `/v1/search/${seg(instance)}`;
    this.nsBase = `${this.base}/ns/${nsSeg(this.namespace)}`;
  }

  /** Same instance, different namespace. */
  withNamespace(namespace: string): SearchClient {
    return new SearchClient(this.http, this.instance, { namespace });
  }

  index(name: string): SearchIndex {
    return new SearchIndex(this.http, this.nsBase, name);
  }

  /** Indexes in this client's namespace. When `has_more` is set, pass the returned `cursor`
   *  back as `cursor` for the next page. */
  async listIndexes(opts: { q?: string; limit?: number; cursor?: string } = {}): Promise<IndexesPage> {
    return this.http.request("GET", `${this.nsBase}/idx`, {
      query: { q: opts.q, limit: opts.limit, cursor: opts.cursor },
    });
  }

  /** Every index in this client's namespace, following the cursor for you.
   *
   *  Stops on a cursor that does not advance as well as on a missing one: an SDK outlives the
   *  deployment it was written against, and a server that keeps handing back the same cursor
   *  would otherwise be followed for ever, billing each round. Same rule as `searchAll`. */
  async *listAllIndexes(opts: { q?: string; limit?: number } = {}): AsyncGenerator<IndexInfo> {
    const seen = new Set<string>();
    let cursor: string | undefined;
    for (;;) {
      const page: IndexesPage = await this.listIndexes({ ...opts, cursor });
      for (const index of page.indexes ?? []) yield index;
      const next = page.cursor ?? undefined;
      if (!page.indexes?.length || !next || seen.has(next)) return;
      seen.add(next);
      cursor = next;
    }
  }

  /** Distinct namespaces with live indexes — alphabetical, `q` substring search,
   * `limit` default 50 (max 100). Instance-wide (not bound to this client's
   * namespace); the default namespace appears as `""`. */
  async listNamespaces(opts: { q?: string; limit?: number } = {}): Promise<{ namespaces: string[]; has_more: boolean }> {
    return this.http.request("GET", `${this.base}/ns`, { query: { q: opts.q, limit: opts.limit } });
  }

  /** Delete an index and all its documents. Requires a `full` grant. */
  async deleteIndex(name: string): Promise<{ deleted: boolean }> {
    return this.http.request("DELETE", `${this.nsBase}/idx/${seg(name)}`);
  }
}

/** Operations on one search index. */
export class SearchIndex {
  private readonly path: string;

  constructor(private readonly http: Http, nsBase: string, readonly name: string) {
    this.path = `${nsBase}/idx/${seg(name)}`;
  }

  /** Upsert up to 200 documents; returns their ids in order (server-assigned when
   * a document omits `id`). */
  async put(documents: SearchDocument[]): Promise<{ ids: string[] }> {
    return this.http.request("POST", `${this.path}/documents`, { body: { documents } });
  }

  /** Fetch one document (`null` when missing), or — passed an array — up to 200
   * documents in one round trip, order-preserving with `null` placeholders for
   * missing ids. Both forms ride the batch endpoint. */
  async get(id: string): Promise<SearchDocument | null>;
  async get(ids: string[]): Promise<(SearchDocument | null)[]>;
  async get(idOrIds: string | string[]): Promise<SearchDocument | null | (SearchDocument | null)[]> {
    const ids = Array.isArray(idOrIds) ? idOrIds : [idOrIds];
    const res = await this.http.request<{ documents: SearchDocument[] }>(
      "POST",
      `${this.path}/documents/get`,
      { body: { ids } }
    );
    const byId = new Map(res.documents.map((d) => [d.id, d]));
    const docs = ids.map((id) => byId.get(id) ?? null);
    return Array.isArray(idOrIds) ? docs : docs[0]!;
  }

  /** Delete up to 200 documents by id; missing ids are no-ops. Requires `full`. */
  async delete(ids: string[]): Promise<{ deleted: number }> {
    return this.http.request("POST", `${this.path}/documents/delete`, { body: { ids } });
  }

  /** Run a search request (see `SearchRequest` for the full surface). */
  async search(req: SearchRequest): Promise<SearchResponse> {
    return this.http.request("POST", `${this.path}/search`, { body: req });
  }

  /**
   * Iterate every hit across cursor pages.
   *
   * Stops on a cursor that does not advance, as well as on a missing one. Search paging ends at a
   * fixed depth, and a server that kept issuing a cursor past it handed back the SAME page — so
   * "follow the cursor until it is absent", which is what this loop did, fetched that page for
   * ever and billed each round. The server no longer does that; this is the half that does not
   * depend on which version you are talking to, since an SDK outlives the deployment it was
   * written against.
   */
  async *searchAll(req: SearchRequest): AsyncGenerator<SearchHit> {
    let cursor: string | undefined = req.cursor;
    const seen = new Set<string>();
    for (;;) {
      const page: SearchResponse = await this.search({ ...req, cursor, offset: undefined });
      for (const hit of page.results) yield hit;
      const next = page.cursor;
      if (!next || next === cursor || seen.has(next) || page.results.length === 0) return;
      seen.add(next);
      cursor = next;
    }
  }

  /** One page of documents in id order (keyset pagination via `start_id`). */
  async listDocuments(opts: ListDocumentsOptions & { ids_only?: false }): Promise<{ documents: SearchDocument[] }>;
  async listDocuments(opts: ListDocumentsOptions & { ids_only: true }): Promise<{ ids: string[] }>;
  async listDocuments(
    opts: ListDocumentsOptions & { ids_only?: boolean } = {}
  ): Promise<{ documents?: SearchDocument[]; ids?: string[] }> {
    return this.http.request("GET", `${this.path}/documents`, {
      query: {
        start_id: opts.start_id,
        include_start: opts.include_start,
        limit: opts.limit,
        ids_only: opts.ids_only,
      },
    });
  }

  /** Iterate every document in the index (keyset pagination handled for you). */
  async *listAllDocuments(opts: { limit?: number } = {}): AsyncGenerator<SearchDocument> {
    let startId: string | undefined;
    for (;;) {
      const page = await this.listDocuments({
        start_id: startId,
        include_start: startId === undefined,
        limit: opts.limit,
      });
      const docs = page.documents;
      if (!docs.length) return;
      for (const doc of docs) yield doc;
      startId = docs[docs.length - 1]!.id;
    }
  }

  /** The index's union field schema. */
  async schema(): Promise<IndexSchema> {
    return this.http.request("GET", `${this.path}/schema`);
  }
}
