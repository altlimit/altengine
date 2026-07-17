import { Http, seg, nsSeg } from "../http.js";
import type {
  AggregateRequest,
  AggregateResult,
  CreateIndexRequest,
  DatastoreDocument,
  IndexSpec,
  Key,
  NamespacesPage,
  PutDocument,
  QueryRequest,
  QueryResult,
  TransactionResult,
  TxnOp,
} from "./types.js";

export interface DatastoreOptions {
  /** Namespace bound to this client (default `""`). */
  namespace?: string;
}

/** Client for one datastore instance, bound to a namespace. */
export class DatastoreClient {
  readonly instance: string;
  readonly namespace: string;
  /** Namespace administration for the whole instance (not namespace-bound). */
  readonly namespaces: NamespaceAdmin;
  private readonly http: Http;
  private readonly base: string;
  private readonly nsBase: string;

  constructor(http: Http, instance: string, opts: DatastoreOptions = {}) {
    this.http = http;
    this.instance = instance;
    this.namespace = opts.namespace ?? "";
    this.base = `/v1/datastore/${seg(instance)}`;
    this.nsBase = `${this.base}/ns/${nsSeg(this.namespace)}`;
    this.namespaces = new NamespaceAdmin(http, this.base);
  }

  /** Same instance, different namespace. */
  withNamespace(namespace: string): DatastoreClient {
    return new DatastoreClient(this.http, this.instance, { namespace });
  }

  /** Upsert up to 500 documents; returns their keys in order. */
  async put<T>(collection: string, documents: PutDocument<T>[]): Promise<{ keys: string[] }> {
    return this.http.request("POST", `${this.nsBase}/col/${seg(collection)}/documents`, {
      body: { documents },
    });
  }

  /** Fetch one document (`null` when missing), or — passed an array — up to 500
   * documents in one round trip, order-preserving with `null` placeholders for
   * missing keys (App Engine `db.get` semantics). Both forms ride the batch
   * endpoint — there is no single-document route on the wire. */
  async get<T = unknown>(collection: string, key: Key): Promise<DatastoreDocument<T> | null>;
  async get<T = unknown>(collection: string, keys: Key[]): Promise<(DatastoreDocument<T> | null)[]>;
  async get<T = unknown>(
    collection: string,
    keyOrKeys: Key | Key[]
  ): Promise<DatastoreDocument<T> | null | (DatastoreDocument<T> | null)[]> {
    const keys = Array.isArray(keyOrKeys) ? keyOrKeys : [keyOrKeys];
    const res = await this.http.request<{ documents: DatastoreDocument<T>[] }>(
      "POST",
      `${this.nsBase}/col/${seg(collection)}/documents/get`,
      { body: { keys } }
    );
    // The wire response omits missing keys; rebuild positional correspondence
    // (numeric keys are stored as decimal strings, so String() aligns them).
    const byKey = new Map(res.documents.map((d) => [d.key, d]));
    const docs = keys.map((k) => byKey.get(String(k)) ?? null);
    return Array.isArray(keyOrKeys) ? docs : docs[0]!;
  }

  /** Delete up to 500 documents by key; missing keys are no-ops. */
  async delete(collection: string, keys: Key[]): Promise<{ deleted: number }> {
    return this.http.request("POST", `${this.nsBase}/col/${seg(collection)}/documents/delete`, {
      body: { keys },
    });
  }

  /** Run one page of an index-served query. */
  async query<T = unknown>(collection: string, req: QueryRequest = {}): Promise<QueryResult<T>> {
    return this.http.request("POST", `${this.nsBase}/col/${seg(collection)}/query`, { body: req });
  }

  /** Iterate every matching document across pages (cursor handled for you). */
  async *queryAll<T = unknown>(collection: string, req: QueryRequest = {}): AsyncGenerator<DatastoreDocument<T>> {
    let cursor: string | null | undefined = req.cursor;
    do {
      const page: QueryResult<T> = await this.query<T>(collection, { ...req, cursor: cursor ?? undefined });
      for (const doc of page.documents ?? []) yield doc;
      cursor = page.cursor;
    } while (cursor);
  }

  /** Grouped metrics over an index-served filter. */
  async aggregate(collection: string, req: AggregateRequest): Promise<AggregateResult> {
    return this.http.request("POST", `${this.nsBase}/col/${seg(collection)}/aggregate`, { body: req });
  }

  /** Apply up to 500 operations atomically within this namespace. NOT retried
   * automatically (increments would double-apply); a failed `check` throws a 409. */
  async transaction(operations: TxnOp[]): Promise<TransactionResult> {
    return this.http.request("POST", `${this.nsBase}/transaction`, { body: { operations }, retry: false });
  }

  readonly indexes = {
    list: async (collection: string): Promise<{ indexes: IndexSpec[] }> =>
      this.http.request("GET", `${this.nsBase}/col/${seg(collection)}/indexes`),
    create: async (collection: string, req: CreateIndexRequest): Promise<{ index: IndexSpec }> =>
      this.http.request("POST", `${this.nsBase}/col/${seg(collection)}/indexes`, { body: req }),
    delete: async (collection: string, id: number): Promise<{ deleted: boolean }> =>
      this.http.request("DELETE", `${this.nsBase}/col/${seg(collection)}/indexes/${seg(id)}`),
  };
}

export class NamespaceAdmin {
  constructor(private readonly http: Http, private readonly base: string) {}

  async list(opts: { q?: string; limit?: number } = {}): Promise<NamespacesPage> {
    return this.http.request("GET", `${this.base}/ns`, { query: { q: opts.q, limit: opts.limit } });
  }

  /** Delete a namespace and everything in it. Requires a `full` grant. */
  async delete(namespace: string): Promise<{ deleted: boolean }> {
    return this.http.request("DELETE", `${this.base}/ns/${nsSeg(namespace)}`);
  }
}
