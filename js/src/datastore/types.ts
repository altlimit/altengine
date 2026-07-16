/** Wire types for the datastore data plane. Field names are wire-verbatim
 * (snake_case) — they double as the API reference. */

export type Key = string | number;

export interface DatastoreDocument<T = unknown> {
  key: string;
  data: T;
  created: number;
  updated: number;
  /** Joined documents keyed by the query's `join[].as` names. */
  joins?: Record<string, unknown>;
}

export interface PutDocument<T = unknown> {
  /** Omit to use the instance's auto-id strategy. Numeric keys are stored as
   * decimal strings (`5` ≡ `"5"`). */
  key?: Key;
  data: T;
}

export type FilterOp = "=" | "!=" | "<" | "<=" | ">" | ">=" | "in";

export interface Filter {
  /** Dot-path into the document (`a.b.c`, depth ≤ 8) or `__key__` / `__created__`
   * / `__updated__`. */
  field: string;
  op: FilterOp;
  value: unknown;
}

export interface Order {
  field: string;
  dir?: "asc" | "desc";
}

export interface Join {
  /** Name the joined document appears under in `joins`. */
  as: string;
  collection: string;
  /** Field on the queried document whose value is the joined document's key. */
  local_field: string;
}

export interface QueryRequest {
  where?: Filter[];
  order?: Order[];
  /** Default 25, max 500. */
  limit?: number;
  cursor?: string;
  keys_only?: boolean;
  join?: Join[];
}

export interface QueryResult<T = unknown> {
  documents?: DatastoreDocument<T>[];
  keys?: string[];
  /** Pass back to continue; `null` when exhausted. */
  cursor: string | null;
  /** Present when the instance auto-created an index to serve this query. */
  auto_indexed?: boolean;
}

export type MetricFn = "count" | "sum" | "avg" | "min" | "max";

export interface Metric {
  fn: MetricFn;
  /** Required for all fns except `count`. */
  field?: string;
  /** Result name; defaults to `fn` or `fn_field`. */
  as?: string;
}

export interface AggregateRequest {
  where?: Filter[];
  group?: string[];
  metrics: Metric[];
  order?: Order[];
  limit?: number;
}

export interface AggregateGroup {
  group: Record<string, unknown>;
  metrics: Record<string, number | null>;
}

export interface AggregateResult {
  groups: AggregateGroup[];
  auto_indexed?: boolean;
}

/** One atomic transaction operation. All ops in a transaction apply atomically
 * within a single namespace; a failed `check` aborts with a 409. */
export type TxnOp =
  | { op: "put"; collection: string; key?: Key; data: unknown }
  | { op: "delete"; collection: string; key: Key }
  | {
      op: "mutate";
      collection: string;
      key: Key;
      set?: Record<string, unknown>;
      increment?: Record<string, number>;
      remove?: string[];
      upsert?: boolean;
    }
  | { op: "check"; collection: string; key: Key; exists: boolean };

export interface TransactionResult {
  /** Per-op resulting key; `null` for delete/check ops. */
  keys: (string | null)[];
}

export interface IndexSpec {
  id: string;
  collection: string;
  /** `"field"` or `"field:asc"` / `"field:desc"`, max 8. */
  fields: string[];
  unique: boolean;
}

export interface CreateIndexRequest {
  fields: string[];
  unique?: boolean;
}

export interface NamespacesPage {
  namespaces: string[];
  has_more: boolean;
}
