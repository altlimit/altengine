// searchAll's stopping condition.
//
// Search paging ends at a fixed depth, and a server that keeps issuing a cursor past it hands back
// the SAME page. "Follow the cursor until it is absent" — which is exactly what this loop did, and
// what the docs tell people to write — then fetches that page for ever, billing each round. The
// server no longer issues such a cursor, but an SDK outlives the deployment it was written
// against, so it stops on a cursor that does not advance as well as on a missing one.

import { describe, expect, it, vi } from "vitest";
import { AltEngine } from "../../src/index.js";

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } });

function index(fetchImpl: typeof fetch) {
  return new AltEngine({ baseUrl: "http://test.local", apiKey: "k", fetch: fetchImpl }).search("app").index("idx");
}

async function collect(gen: AsyncGenerator<unknown>, cap = 50): Promise<unknown[]> {
  const out: unknown[] = [];
  for await (const hit of gen) {
    out.push(hit);
    if (out.length > cap) throw new Error("searchAll did not terminate");
  }
  return out;
}

describe("searchAll", () => {
  it("stops when the server repeats a cursor it cannot advance past", async () => {
    // The old server behaviour: the same cursor, and the same page, for ever.
    const fetchMock = vi.fn(async () => json({ results: [{ id: "a" }], cursor: "STUCK" }));
    const hits = await collect(index(fetchMock as never).searchAll({ query: "x" }));
    // Two pages, not one: a cursor cannot be known to repeat until it has been followed once.
    // That is the whole cost — it used to be unbounded.
    expect(fetchMock.mock.calls.length, "the stuck cursor must be followed at most once").toBe(2);
    expect(hits.length).toBe(2);
  });

  it("stops when a cursor it has already followed comes back", async () => {
    const pages = [
      { results: [{ id: "a" }], cursor: "p2" },
      { results: [{ id: "b" }], cursor: "p3" },
      { results: [{ id: "c" }], cursor: "p2" }, // a loop, one page wide
    ];
    let i = 0;
    const fetchMock = vi.fn(async () => json(pages[Math.min(i++, pages.length - 1)]));
    const hits = await collect(index(fetchMock as never).searchAll({ query: "x" }));
    expect(hits.map((h: any) => h.id)).toEqual(["a", "b", "c"]);
  });

  it("still pages through to the end of a normal result set", async () => {
    const pages = [
      { results: [{ id: "a" }], cursor: "p2" },
      { results: [{ id: "b" }], cursor: "p3" },
      { results: [{ id: "c" }] }, // no cursor: the last page
    ];
    let i = 0;
    const fetchMock = vi.fn(async () => json(pages[i++]));
    const hits = await collect(index(fetchMock as never).searchAll({ query: "x" }));
    expect(hits.map((h: any) => h.id)).toEqual(["a", "b", "c"]);
    expect(fetchMock.mock.calls.length).toBe(3);
  });

  it("stops on an empty page even if a cursor comes with it", async () => {
    const fetchMock = vi.fn(async () => json({ results: [], cursor: "onwards" }));
    expect(await collect(index(fetchMock as never).searchAll({ query: "x" }))).toEqual([]);
  });
});
