import { beforeAll, describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import path from "node:path";
import { AltEngineError, type DatastoreClient } from "../../src/index.js";
import { client, destructiveOk, uniq } from "./helpers.js";

const seed = JSON.parse(
  readFileSync(path.resolve(__dirname, "../../../conformance/fixtures/datastore-seed.json"), "utf8")
) as {
  collection: string;
  documents: { key: string; data: Record<string, unknown> }[];
  owners: { key: string; data: Record<string, unknown> }[];
};

const instance = uniq("sdk-conf");
let db: DatastoreClient;

beforeAll(async () => {
  db = client().datastore(instance, { namespace: uniq("ns") });
  await db.put(seed.collection, seed.documents);
  await db.put("owners", seed.owners);
});

describe("datastore CRUD", () => {
  it("put → get roundtrip preserves data and reports timestamps", async () => {
    const doc = await db.get<Record<string, unknown>>("todos", "t1");
    expect(doc).not.toBeNull();
    expect(doc!.data.title).toBe("ship SDK");
    expect(doc!.created).toBeGreaterThan(0);
    expect(doc!.updated).toBeGreaterThanOrEqual(doc!.created);
  });

  it("puts are upserts and preserve created", async () => {
    const before = await db.get("todos", "t1");
    await db.put("todos", [{ key: "t1", data: { title: "ship SDK v2", owner: "ana", done: false, priority: 1 } }]);
    const after = await db.get<{ title: string }>("todos", "t1");
    expect(after!.data.title).toBe("ship SDK v2");
    expect(after!.created).toBe(before!.created);
    // restore
    await db.put("todos", [seed.documents.find((d) => d.key === "t1")!]);
  });

  it("auto-id put returns a generated key; numeric keys coerce to strings", async () => {
    const { keys } = await db.put("todos", [{ data: { title: "auto" } }, { key: 42, data: { title: "num" } }]);
    expect(keys).toHaveLength(2);
    expect(keys[0]!.length).toBeGreaterThan(0);
    expect(keys[1]).toBe("42");
    expect((await db.get("todos", "42"))!.data).toMatchObject({ title: "num" });
    await db.delete("todos", keys as string[]);
  });

  it("batchGet returns found docs and omits missing", async () => {
    const { documents } = await db.batchGet("todos", ["t1", "does-not-exist", "t3"]);
    expect(documents.map((d) => d.key).sort()).toEqual(["t1", "t3"]);
  });

  it("get of missing key returns null; delete of missing is a no-op", async () => {
    expect(await db.get("todos", "ghost")).toBeNull();
    await db.delete("todos", ["ghost"]); // must not throw
  });
});

describe("datastore query", () => {
  it("filters with = and orders desc with cursor pagination", async () => {
    const all: string[] = [];
    let cursor: string | null | undefined;
    do {
      const page = await db.query<{ priority: number }>("todos", {
        where: [{ field: "done", op: "=", value: false }],
        order: [{ field: "priority", dir: "desc" }],
        limit: 1,
        cursor: cursor ?? undefined,
      });
      for (const d of page.documents ?? []) all.push(d.key);
      cursor = page.cursor;
    } while (cursor);
    expect(all).toEqual(["t4", "t3", "t1"]);
  });

  it("supports in, dot-paths, and keys_only", async () => {
    const res = await db.query("todos", {
      where: [{ field: "meta.tag", op: "in", value: ["home"] }],
      keys_only: true,
    });
    expect(res.keys!.sort()).toEqual(["t3", "t5"]);
  });

  it("queryAll iterates to exhaustion", async () => {
    const keys: string[] = [];
    for await (const d of db.queryAll("todos", { where: [{ field: "owner", op: "=", value: "ana" }], limit: 1 })) {
      keys.push(d.key);
    }
    expect(keys.sort()).toEqual(["t1", "t2"]);
  });

  it("join attaches the referenced document", async () => {
    const res = await db.query<Record<string, unknown>>("todos", {
      where: [{ field: "__key__", op: "=", value: "t1" }],
      join: [{ as: "owner_doc", collection: "owners", local_field: "owner" }],
    });
    const doc = res.documents![0]!;
    expect((doc.joins!.owner_doc as { data: { name: string } }).data.name).toBe("Ana");
  });

  it("aggregates count/sum/avg with grouping", async () => {
    const res = await db.aggregate("todos", {
      group: ["owner"],
      metrics: [
        { fn: "count", as: "n" },
        { fn: "sum", field: "priority", as: "total" },
      ],
      order: [{ field: "owner", dir: "asc" }],
    });
    const ana = res.groups.find((g) => g.group.owner === "ana")!;
    expect(ana.metrics.n).toBe(2);
    expect(ana.metrics.total).toBe(3);
  });
});

describe("datastore transactions & indexes", () => {
  it("applies put/mutate/check atomically", async () => {
    await db.put("counters", [{ key: "c1", data: { total: 0 } }]);
    const res = await db.transaction([
      { op: "check", collection: "counters", key: "c1", exists: true },
      { op: "mutate", collection: "counters", key: "c1", increment: { total: 5 } },
      { op: "put", collection: "counters", key: "c2", data: { total: 1 } },
    ]);
    expect(res.keys).toHaveLength(3);
    expect((await db.get<{ total: number }>("counters", "c1"))!.data.total).toBe(5);
    expect((await db.get("counters", "c2"))).not.toBeNull();
  });

  it("failed check aborts with 409 and applies nothing", async () => {
    const err = await db
      .transaction([
        { op: "check", collection: "counters", key: "ghost", exists: true },
        { op: "mutate", collection: "counters", key: "c1", increment: { total: 100 } },
      ])
      .catch((e) => e);
    expect(err).toBeInstanceOf(AltEngineError);
    expect(err.status).toBe(409);
    expect((await db.get<{ total: number }>("counters", "c1"))!.data.total).toBe(5);
  });

  it("index create is idempotent; unique index rejects duplicates", async () => {
    const spec = { fields: ["email"], unique: true };
    const a = await db.indexes.create("users", spec);
    const b = await db.indexes.create("users", spec);
    expect(b.index.id).toBe(a.index.id);
    await db.put("users", [{ key: "u1", data: { email: "x@y.z" } }]);
    const err = await db.put("users", [{ key: "u2", data: { email: "x@y.z" } }]).catch((e) => e);
    expect(err).toBeInstanceOf(AltEngineError);
    const { indexes } = await db.indexes.list("users");
    expect(indexes.some((ix) => ix.id === a.index.id)).toBe(true);
  });
});

describe("datastore namespaces", () => {
  it("namespaces are isolated and listable", async () => {
    const other = db.withNamespace(uniq("other"));
    await other.put("todos", [{ key: "only-here", data: { a: 1 } }]);
    expect(await db.get("todos", "only-here")).toBeNull();
    expect((await other.get("todos", "only-here"))).not.toBeNull();

    const { namespaces } = await db.namespaces.list();
    expect(namespaces).toContain(other.namespace);

    if (destructiveOk()) {
      await db.namespaces.delete(other.namespace);
      expect(await other.get("todos", "only-here")).toBeNull();
    }
  });
});
