/** Wire types for the search data plane (App Engine Search API parity).
 * Field names are wire-verbatim (snake_case). */

export type FieldType =
  | "text"
  | "html"
  | "atom"
  | "number"
  | "date"
  | "geo"
  | "tokenprefix"
  | "untokenprefix";

export interface GeoPoint {
  lat: number;
  lng: number;
}

export interface SearchField {
  name: string;
  type: FieldType;
  /** `string` for text/html/atom/tokenprefix/untokenprefix, `number` for number,
   * ISO string or epoch ms for date, `{lat,lng}` for geo. */
  value: unknown;
  language?: string;
}

export type FacetType = "atom" | "number";

export interface SearchFacet {
  name: string;
  type: FacetType;
  value: string | number;
}

export interface SearchDocument {
  /** Omit to have the server assign one (returned from `put`). */
  id?: string;
  /** Sort/tiebreak rank; defaults to seconds since 2011-01-01. */
  rank?: number;
  lang?: string;
  fields: SearchField[];
  facets?: SearchFacet[];
}

export interface SortSpec {
  /** Field name, `"rank"`, or `"_score"`. */
  expr: string;
  desc?: boolean;
  /** Sort value for documents missing the field. */
  default?: number;
}

export interface FacetRefinement {
  name: string;
  /** Atom facet value… */
  value?: string;
  /** …or a number range (min inclusive, max exclusive). */
  min?: number;
  max?: number;
}

export interface SnippetRequest {
  /** Text/html fields to snippet; defaults to the query's fields. */
  fields?: string[];
  /** Token count (not chars), clamped 1..64. */
  max_tokens?: number;
  pre_tag?: string;
  post_tag?: string;
  ellipsis?: string;
}

export interface CollapseRequest {
  /** Atom or number field to collapse (distinct) on. */
  field: string;
  /** Top docs kept per distinct value, clamped 1..10. Incompatible with `sort`. */
  limit?: number;
}

export interface SearchRequest {
  /** Boolean query language: bare terms, `field:value`, comparisons, AND/OR/NOT,
   * `-term`, grouping, `"phrases"`, `~stem`, `distance(loc, geopoint(lat,lng)) < n`. */
  query: string;
  /** Default 20, max 1000. */
  limit?: number;
  /** Max 1000; prefer `cursor` for deep paging. */
  offset?: number;
  cursor?: string;
  ids_only?: boolean;
  returned_fields?: string[];
  sort?: SortSpec[];
  /** Cap on documents examined for sorting (default 1000, max 10000). */
  sort_limit?: number;
  scorer?: "match" | "rescore";
  /** Auto-discover the top-N facets over matching documents. */
  facet_discover?: number;
  /** Explicit facet names to aggregate. */
  facets?: string[];
  facet_value_limit?: number;
  facet_refinements?: FacetRefinement[];
  /** Matching docs examined for facet counts (max 10000). */
  facet_depth?: number;
  /** Count accuracy cap — `total_hits` is exact up to this (default 20, max 10000). */
  total_hits_accuracy?: number;
  snippet?: SnippetRequest;
  collapse?: CollapseRequest;
}

export interface SearchHit {
  id: string;
  rank: number;
  score?: number;
  /** Absent when `ids_only`. */
  document?: SearchDocument;
  /** Field name → highlighted HTML. */
  snippet?: Record<string, string>;
}

export interface FacetValueResult {
  value: string;
  count: number;
  min?: number;
  max?: number;
}

export interface FacetResult {
  name: string;
  type: FacetType;
  values: FacetValueResult[];
}

export interface SearchResponse {
  total_hits: number;
  total_hits_exact: boolean;
  returned: number;
  results: SearchHit[];
  /** Present iff more pages exist. */
  cursor?: string;
  facets?: FacetResult[];
  /** Free-form data attached by matching query rules. */
  rule_data?: unknown[];
}

export interface IndexInfo {
  name: string;
  namespace: string;
  created_at: number;
}

export interface IndexesPage {
  indexes: IndexInfo[];
  has_more: boolean;
}

export interface ListDocumentsOptions {
  /** Keyset pagination: resume from this document id. */
  start_id?: string;
  /** Include `start_id` itself (default true). */
  include_start?: boolean;
  limit?: number;
}

/** The union schema of an index: for every field name, the types it has been
 * indexed with. */
export interface IndexSchema {
  name: string;
  namespace: string;
  fields: Record<string, FieldType[]>;
}
