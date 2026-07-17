import { Http, seg } from "../http.js";
import { AltEngineError } from "../errors.js";
import type {
  IndexSchema,
  IndexesPage,
  ListDocumentsOptions,
  SearchDocument,
  SearchHit,
  SearchRequest,
  SearchResponse,
} from "./types.js";

export interface SearchOptions {
  /** Namespace for all index operations (sent as `X-Namespace`, default `""`). */
  namespace?: string;
}

/** Client for one search instance, bound to a namespace. */
export class SearchClient {
  readonly instance: string;
  readonly namespace: string;
  private readonly http: Http;
  private readonly base: string;

  constructor(http: Http, instance: string, opts: SearchOptions = {}) {
    this.http = http;
    this.instance = instance;
    this.namespace = opts.namespace ?? "";
    this.base = `/v1/search/${seg(instance)}`;
  }

  /** Same instance, different namespace. */
  withNamespace(namespace: string): SearchClient {
    return new SearchClient(this.http, this.instance, { namespace });
  }

  private headers(): Record<string, string> | undefined {
    return this.namespace ? { "x-namespace": this.namespace } : undefined;
  }

  index(name: string): SearchIndex {
    return new SearchIndex(this.http, this.base, name, this.headers());
  }

  async listIndexes(opts: { q?: string; limit?: number } = {}): Promise<IndexesPage> {
    return this.http.request("GET", `${this.base}/indexes`, {
      query: { q: opts.q, limit: opts.limit, namespace: this.namespace || undefined },
    });
  }

  /** Distinct namespaces with live indexes — alphabetical, `q` substring search,
   * `limit` default 50 (max 100). Instance-wide (not bound to this client's
   * namespace); the default namespace appears as `""`. */
  async listNamespaces(opts: { q?: string; limit?: number } = {}): Promise<{ namespaces: string[]; has_more: boolean }> {
    return this.http.request("GET", `${this.base}/namespaces`, { query: { q: opts.q, limit: opts.limit } });
  }

  /** Delete an index and all its documents. Requires a `full` grant. */
  async deleteIndex(name: string): Promise<{ deleted: boolean }> {
    return this.http.request("DELETE", `${this.base}/indexes/${seg(name)}`, { headers: this.headers() });
  }
}

/** Operations on one search index. */
export class SearchIndex {
  private readonly path: string;

  constructor(
    private readonly http: Http,
    base: string,
    readonly name: string,
    private readonly hdrs?: Record<string, string>
  ) {
    this.path = `${base}/indexes/${seg(name)}`;
  }

  /** Upsert up to 200 documents; returns their ids in order (server-assigned when
   * a document omits `id`). */
  async put(documents: SearchDocument[]): Promise<{ ids: string[] }> {
    return this.http.request("PUT", `${this.path}/documents`, { body: { documents }, headers: this.hdrs });
  }

  /** Fetch one document, or `null` when it (or the index) doesn't exist.
   * Rides the keyset listing (`start_id` + `limit:1`) — there is no
   * single-document route on the wire. */
  async get(id: string): Promise<SearchDocument | null> {
    try {
      const page = await this.listDocuments({ start_id: id, limit: 1 });
      const doc = page.documents?.[0];
      return doc && doc.id === id ? doc : null;
    } catch (err) {
      if (err instanceof AltEngineError && err.status === 404) return null;
      throw err;
    }
  }

  /** Delete up to 200 documents by id; missing ids are no-ops. Requires `full`. */
  async delete(ids: string[]): Promise<{ deleted: number }> {
    return this.http.request("POST", `${this.path}/documents/delete`, { body: { ids }, headers: this.hdrs });
  }

  /** Run a search request (see `SearchRequest` for the full surface). */
  async search(req: SearchRequest): Promise<SearchResponse> {
    return this.http.request("POST", `${this.path}/search`, { body: req, headers: this.hdrs });
  }

  /** Iterate every hit across cursor pages. */
  async *searchAll(req: SearchRequest): AsyncGenerator<SearchHit> {
    let cursor: string | undefined = req.cursor;
    do {
      const page: SearchResponse = await this.search({ ...req, cursor, offset: undefined });
      for (const hit of page.results) yield hit;
      cursor = page.cursor;
    } while (cursor);
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
      headers: this.hdrs,
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
    return this.http.request("GET", `${this.path}/schema`, { headers: this.hdrs });
  }
}
