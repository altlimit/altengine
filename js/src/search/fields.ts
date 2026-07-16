import type { FacetType, GeoPoint, SearchFacet, SearchField } from "./types.js";

/** Field builders — `f.text("title", "Blue Shoes")` reads better than object
 * literals and pins the right `type` string. */
export const f = {
  text: (name: string, value: string, language?: string): SearchField =>
    mk(name, "text", value, language),
  html: (name: string, value: string, language?: string): SearchField =>
    mk(name, "html", value, language),
  atom: (name: string, value: string): SearchField => mk(name, "atom", value),
  number: (name: string, value: number): SearchField => mk(name, "number", value),
  /** Accepts a Date, ISO string, or epoch milliseconds. */
  date: (name: string, value: Date | string | number): SearchField =>
    mk(name, "date", value instanceof Date ? value.toISOString() : value),
  geo: (name: string, value: GeoPoint): SearchField => mk(name, "geo", value),
  tokenprefix: (name: string, value: string): SearchField => mk(name, "tokenprefix", value),
  untokenprefix: (name: string, value: string): SearchField => mk(name, "untokenprefix", value),
};

/** Facet builders for document facets. */
export const facet = {
  atom: (name: string, value: string): SearchFacet => ({ name, type: "atom" as FacetType, value }),
  number: (name: string, value: number): SearchFacet => ({ name, type: "number" as FacetType, value }),
};

function mk(name: string, type: SearchField["type"], value: unknown, language?: string): SearchField {
  const field: SearchField = { name, type, value };
  if (language !== undefined) field.language = language;
  return field;
}
