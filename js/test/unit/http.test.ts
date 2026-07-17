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
      expect(String(url)).toBe("http://test.local/v1/datastore/app/ns/ns%2F1/col/todos/documents");
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

  it("get() returns null when the batch response omits the key", async () => {
    const fetchMock = vi.fn(async () => json(200, { documents: [] }));
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

describe("base URL resolution", () => {
  const capture = () => {
    const urls: string[] = [];
    const fetchMock = vi.fn(async (url: any) => {
      urls.push(String(url));
      return json(200, { namespaces: [], has_more: false });
    });
    return { urls, fetchMock };
  };

  it("defaults to production", async () => {
    const { urls, fetchMock } = capture();
    await new AltEngine({ apiKey: "k", fetch: fetchMock as any }).datastore("app").namespaces.list();
    expect(urls[0]).toMatch(/^https:\/\/api\.altengine\.net\//);
  });

  it("dev: true targets the local emulator", async () => {
    const { urls, fetchMock } = capture();
    await new AltEngine({ dev: true, apiKey: "k", fetch: fetchMock as any }).datastore("app").namespaces.list();
    expect(urls[0]).toMatch(/^http:\/\/127\.0\.0\.1:9191\//);
  });

  it("ALTENGINE_URL env var overrides the default; explicit baseUrl wins over all", async () => {
    process.env.ALTENGINE_URL = "http://env.local";
    try {
      const a = capture();
      await new AltEngine({ apiKey: "k", fetch: a.fetchMock as any }).datastore("app").namespaces.list();
      expect(a.urls[0]).toMatch(/^http:\/\/env\.local\//);

      const b = capture();
      await new AltEngine({ baseUrl: "http://explicit.local", apiKey: "k", fetch: b.fetchMock as any })
        .datastore("app")
        .namespaces.list();
      expect(b.urls[0]).toMatch(/^http:\/\/explicit\.local\//);
    } finally {
      delete process.env.ALTENGINE_URL;
    }
  });

  it("apiKey falls back to ALTENGINE_API_KEY", async () => {
    process.env.ALTENGINE_API_KEY = "env-key";
    try {
      const fetchMock = vi.fn(async (_url: any, init: any) => {
        expect(init.headers.authorization).toBe("Bearer env-key");
        return json(200, { namespaces: [], has_more: false });
      });
      await new AltEngine({ dev: true, fetch: fetchMock as any }).datastore("app").namespaces.list();
      expect(fetchMock).toHaveBeenCalledOnce();
    } finally {
      delete process.env.ALTENGINE_API_KEY;
    }
  });
});

describe("search namespace routing", () => {
  it("carries the namespace in the path (/ns/{ns}/idx/...)", async () => {
    const seen: string[] = [];
    const fetchMock = vi.fn(async (url: any, init: any) => {
      seen.push(`${init.method} ${new URL(String(url)).pathname}`);
      return json(200, String(url).endsWith("/idx") ? { indexes: [], has_more: false } : { ids: ["1"] });
    });
    const s = client(fetchMock as any).search("app", { namespace: "prod" });
    await s.listIndexes();
    await s.index("products").put([{ fields: [{ name: "t", type: "text", value: "x" }] }]);
    expect(seen).toEqual([
      "GET /v1/search/app/ns/prod/idx",
      "POST /v1/search/app/ns/prod/idx/products/documents",
    ]);
  });

  it("encodes the default namespace as _default in the path", async () => {
    let seen = "";
    const fetchMock = vi.fn(async (url: any, init: any) => {
      seen = `${init.method} ${new URL(String(url)).pathname}`;
      return json(200, { ids: ["1"] });
    });
    const s = client(fetchMock as any).search("app"); // default namespace
    await s.index("products").put([{ fields: [{ name: "t", type: "text", value: "x" }] }]);
    expect(seen).toBe("POST /v1/search/app/ns/_default/idx/products/documents");
  });
});
