import { describe, expect, it, vi } from "vitest";
import { AltEngine, AltEngineError } from "../../src/index.js";

const json = (status: number, body: unknown, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json", ...headers } });

function client(fetchImpl: typeof fetch, retry = {}) {
  return new AltEngine({ baseUrl: "http://test.local", apiKey: "k", fetch: fetchImpl, retry });
}

describe("transport", () => {
  it("sends bearer auth and JSON body to the right path", async () => {
    const fetchMock = vi.fn(async (url: any, init: any) => {
      expect(String(url)).toBe("http://test.local/v1/datastore/app/namespaces/ns%2F1/collections/todos/documents");
      expect(init.method).toBe("POST");
      expect(init.headers.authorization).toBe("Bearer k");
      expect(JSON.parse(init.body)).toEqual({ documents: [{ data: { a: 1 } }] });
      return json(200, { keys: ["x"] });
    });
    const db = client(fetchMock as any).datastore("app", { namespace: "ns/1" });
    const res = await db.put("todos", [{ data: { a: 1 } }]);
    expect(res.keys).toEqual(["x"]);
  });

  it("parses the error envelope into AltEngineError", async () => {
    const fetchMock = vi.fn(async () =>
      json(400, { error: { code: "INVALID_ARGUMENT", message: "bad", details: { hint: "x" } } })
    );
    const db = client(fetchMock as any).datastore("app");
    const err = await db.query("todos").catch((e) => e);
    expect(err).toBeInstanceOf(AltEngineError);
    expect(err.code).toBe("INVALID_ARGUMENT");
    expect(err.status).toBe(400);
    expect(err.details).toEqual({ hint: "x" });
    expect(err.retryable).toBe(false);
  });

  it("retries 429 honoring Retry-After, then succeeds", async () => {
    let calls = 0;
    const fetchMock = vi.fn(async () => {
      calls++;
      if (calls === 1) return json(429, { error: { code: "RATE_LIMITED", message: "slow down" } }, { "retry-after": "0" });
      return json(200, { namespaces: [], has_more: false });
    });
    const db = client(fetchMock as any, { baseDelayMs: 1, maxDelayMs: 2 }).datastore("app");
    const res = await db.namespaces.list();
    expect(res.has_more).toBe(false);
    expect(calls).toBe(2);
  });

  it("does NOT retry transactions", async () => {
    let calls = 0;
    const fetchMock = vi.fn(async () => {
      calls++;
      return json(429, { error: { code: "RATE_LIMITED", message: "no" } }, { "retry-after": "0" });
    });
    const db = client(fetchMock as any, { baseDelayMs: 1 }).datastore("app");
    await expect(db.transaction([{ op: "check", collection: "c", key: "k", exists: true }])).rejects.toMatchObject({
      code: "RATE_LIMITED",
    });
    expect(calls).toBe(1);
  });

  it("does NOT retry channel publish", async () => {
    let calls = 0;
    const fetchMock = vi.fn(async () => {
      calls++;
      return json(503, { error: { code: "INTERNAL", message: "overloaded" } });
    });
    const ch = client(fetchMock as any, { baseDelayMs: 1 }).channel("app");
    await expect(ch.publish("room", { a: 1 })).rejects.toMatchObject({ status: 503 });
    expect(calls).toBe(1);
  });

  it("get() returns null on 404", async () => {
    const fetchMock = vi.fn(async () => json(404, { error: { code: "NOT_FOUND", message: "missing" } }));
    const db = client(fetchMock as any).datastore("app");
    expect(await db.get("todos", "nope")).toBeNull();
  });
});

describe("pagination iterators", () => {
  it("queryAll walks cursors to exhaustion", async () => {
    const pages = [
      { documents: [{ key: "1", data: {}, created: 0, updated: 0 }], cursor: "c1" },
      { documents: [{ key: "2", data: {}, created: 0, updated: 0 }], cursor: null },
    ];
    let call = 0;
    const fetchMock = vi.fn(async (_url: any, init: any) => {
      const body = JSON.parse(init.body);
      if (call === 1) expect(body.cursor).toBe("c1");
      return json(200, pages[call++]);
    });
    const db = client(fetchMock as any).datastore("app");
    const keys: string[] = [];
    for await (const doc of db.queryAll("todos", {})) keys.push(doc.key);
    expect(keys).toEqual(["1", "2"]);
    expect(call).toBe(2);
  });

  it("searchAll walks search cursors", async () => {
    const pages = [
      { total_hits: 2, total_hits_exact: true, returned: 1, results: [{ id: "a", rank: 1 }], cursor: "n" },
      { total_hits: 2, total_hits_exact: true, returned: 1, results: [{ id: "b", rank: 2 }] },
    ];
    let call = 0;
    const fetchMock = vi.fn(async () => json(200, pages[call++]));
    const idx = client(fetchMock as any).search("app").index("products");
    const ids: string[] = [];
    for await (const hit of idx.searchAll({ query: "x" })) ids.push(hit.id);
    expect(ids).toEqual(["a", "b"]);
  });
});

describe("search namespace routing", () => {
  it("sends X-Namespace on index calls and ?namespace= on list", async () => {
    const fetchMock = vi.fn(async (url: any, init: any) => {
      const u = String(url);
      if (u.includes("/search/app/indexes?")) {
        expect(u).toContain("namespace=prod");
        return json(200, { indexes: [], has_more: false });
      }
      expect(init.headers["x-namespace"]).toBe("prod");
      return json(200, { ids: ["1"] });
    });
    const s = client(fetchMock as any).search("app", { namespace: "prod" });
    await s.listIndexes();
    await s.index("products").put([{ fields: [{ name: "t", type: "text", value: "x" }] }]);
  });
});
