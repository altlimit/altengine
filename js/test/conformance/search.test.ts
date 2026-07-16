import { beforeAll, describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import path from "node:path";
import type { SearchClient, SearchDocument, SearchIndex } from "../../src/index.js";
import { client, destructiveOk, uniq } from "./helpers.js";

const corpus = JSON.parse(
  readFileSync(path.resolve(__dirname, "../../../conformance/fixtures/search-corpus.json"), "utf8")
) as { documents: SearchDocument[] };

const instance = uniq("sdk-conf");
let s: SearchClient;
let idx: SearchIndex;

beforeAll(async () => {
  s = client().search(instance);
  idx = s.index("products");
  const { ids } = await idx.put(corpus.documents);
  expect(ids).toEqual(corpus.documents.map((d) => d.id));
});

describe("search documents", () => {
  it("get roundtrips all field types", async () => {
    const doc = await idx.get("p1");
    expect(doc).not.toBeNull();
    const byName = Object.fromEntries(doc!.fields.map((f) => [f.name, f]));
    expect(byName.title!.value).toBe("Blue Suede Shoes");
    expect(byName.price!.value).toBe(59);
    expect(byName.store!.value).toMatchObject({ lat: 37.77, lng: -122.41 });
    expect(doc!.facets?.length).toBe(2);
  });

  it("get of a missing doc returns null", async () => {
    expect(await idx.get("nope")).toBeNull();
  });

  it("server assigns ids when omitted", async () => {
    const { ids } = await idx.put([{ fields: [{ name: "title", type: "text", value: "temp" }] }]);
    expect(ids[0]!.length).toBeGreaterThan(0);
    await idx.delete(ids);
  });

  it("lists documents with keyset pagination", async () => {
    const seen: string[] = [];
    for await (const doc of idx.listAllDocuments({ limit: 2 })) seen.push(doc.id!);
    expect(seen.sort()).toEqual(["p1", "p2", "p3", "p4"]);
  });
});

describe("search queries", () => {
  it("matches bare terms and field terms", async () => {
    const res = await idx.search({ query: "shoes" });
    expect(res.results.map((r) => r.id).sort()).toEqual(["p1", "p2", "p4"]);
    const atom = await idx.search({ query: 'sku:"BS-001"' });
    expect(atom.results.map((r) => r.id)).toEqual(["p1"]);
  });

  it("supports boolean operators and comparisons", async () => {
    const res = await idx.search({ query: "shoes AND price<100" });
    expect(res.results.map((r) => r.id).sort()).toEqual(["p1", "p4"]);
    const not = await idx.search({ query: "shoes NOT blue" });
    expect(not.results.map((r) => r.id)).toEqual(["p2"]);
  });

  it("sorts by field and paginates with cursors", async () => {
    const page1 = await idx.search({ query: "shoes", sort: [{ expr: "price" }], limit: 2 });
    expect(page1.results.map((r) => r.id)).toEqual(["p4", "p1"]);
    expect(page1.cursor).toBeTruthy();
    const page2 = await idx.search({ query: "shoes", sort: [{ expr: "price" }], limit: 2, cursor: page1.cursor });
    expect(page2.results.map((r) => r.id)).toEqual(["p2"]);
  });

  it("returns facets with counts and honors refinements", async () => {
    const res = await idx.search({ query: "", facets: ["category"] });
    const cat = res.facets!.find((f) => f.name === "category")!;
    expect(cat.values.find((v) => v.value === "shoes")!.count).toBe(3);

    const refined = await idx.search({
      query: "",
      facet_refinements: [{ name: "category", value: "accessories" }],
    });
    expect(refined.results.map((r) => r.id)).toEqual(["p3"]);
  });

  it("snippets highlight matches", async () => {
    const res = await idx.search({ query: "suede", snippet: { fields: ["title"], pre_tag: "<em>", post_tag: "</em>" } });
    expect(res.results[0]!.snippet!.title).toContain("<em>");
  });

  it("collapse keeps top doc per distinct value", async () => {
    const res = await idx.search({ query: "shoes", collapse: { field: "category", limit: 1 } });
    expect(res.results).toHaveLength(1);
  });

  it("ids_only omits documents", async () => {
    const res = await idx.search({ query: "shoes", ids_only: true });
    expect(res.results.every((r) => r.document === undefined)).toBe(true);
  });
});

describe("search index management", () => {
  it("schema reflects the union of fields", async () => {
    const schema = await idx.schema();
    expect(schema.name).toBe("products");
    expect(Object.keys(schema.fields)).toEqual(expect.arrayContaining(["title", "price", "sku"]));
    expect(schema.fields.price).toContain("number");
  });

  it("namespaces are isolated", async () => {
    const other = s.withNamespace(uniq("ns"));
    await other.index("products").put([{ id: "only", fields: [{ name: "title", type: "text", value: "hidden" }] }]);
    expect(await idx.get("only")).toBeNull();
    const { indexes } = await other.listIndexes();
    expect(indexes.some((ix) => ix.name === "products")).toBe(true);

    // listNamespaces sees both the default namespace ("") and the new one.
    const page = await s.listNamespaces({ limit: 100 });
    expect(page.namespaces).toContain(other.namespace);
    expect(page.namespaces).toContain("");
    const filtered = await s.listNamespaces({ q: other.namespace.slice(0, 8) });
    expect(filtered.namespaces).toContain(other.namespace);

    if (destructiveOk()) await other.deleteIndex("products");
  });

  it("rejects invalid namespaces with 400 INVALID_ARGUMENT", async () => {
    // Via ?namespace= (listIndexes) — a query param survives URL-encoding for all
    // three cases, including the NUL byte a header could never carry.
    for (const bad of ["a\u0000b", "x".repeat(101), "café"]) {
      const err = await s.withNamespace(bad).listIndexes().catch((e) => e);
      expect(err.status, `namespace ${JSON.stringify(bad)}`).toBe(400);
      expect(err.code).toBe("INVALID_ARGUMENT");
    }
  });

  it("lists indexes and deletes documents", async () => {
    const { indexes } = await s.listIndexes();
    expect(indexes.some((ix) => ix.name === "products")).toBe(true);

    await idx.put([{ id: "todelete", fields: [{ name: "title", type: "text", value: "x" }] }]);
    const { deleted } = await idx.delete(["todelete", "never-existed"]);
    expect(deleted).toBe(1);
  });
});
