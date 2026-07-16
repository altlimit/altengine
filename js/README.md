# @altengine/sdk

Official JavaScript/TypeScript SDK for [altengine](https://www.altengine.net) —
managed **datastore**, **search**, and realtime **channels** behind one API key.
Zero dependencies, fetch-based, works in Node ≥20, browsers, and edge runtimes.

```bash
npm i @altengine/sdk
```

## Setup

```ts
import { AltEngine } from "@altengine/sdk";

const ae = new AltEngine({ apiKey: "ae_..." });   // production: https://api.altengine.net
```

Everything is overridable, nothing is required:

- `apiKey` — falls back to the `ALTENGINE_API_KEY` env var
- `dev: true` — target the [local emulator](https://github.com/altlimit/altengine)
  at `http://127.0.0.1:9191` (`altengine dev`)
- `baseUrl` — explicit origin; also settable via the `ALTENGINE_URL` env var
  (resolution: `baseUrl` → `dev` → `ALTENGINE_URL` → production)

```ts
const local = new AltEngine({ dev: true, apiKey: "dev" });        // emulator
// ALTENGINE_URL=http://127.0.0.1:9191 node app.js                 // same, via env
```

Retryable failures (429 with `Retry-After`, 502/503/504, network errors) are retried
up to 3 times with jittered backoff — except transactions and publishes, which are
never auto-retried. Errors throw `AltEngineError` with `code`, `status`, `details`,
and `retryAfter`.

## Datastore

JSON documents in collections, per-namespace, with index-served queries,
aggregates, and atomic transactions.

```ts
const db = ae.datastore("myapp", { namespace: "prod" });

const { keys } = await db.put("todos", [{ data: { title: "ship SDK", done: false } }]);
const doc = await db.get("todos", keys[0]);         // null when missing
await db.batchGet("todos", keys);
await db.delete("todos", keys);                     // missing keys are no-ops

// Queries (cursor pagination handled by queryAll)
const page = await db.query("todos", {
  where: [{ field: "done", op: "=", value: false }],
  order: [{ field: "priority", dir: "desc" }],
  limit: 100,
});
for await (const d of db.queryAll("todos", { where: [{ field: "owner", op: "=", value: "ana" }] })) {
  // …
}

// Aggregates
const { groups } = await db.aggregate("todos", {
  group: ["owner"],
  metrics: [{ fn: "count", as: "n" }, { fn: "sum", field: "priority", as: "total" }],
});

// Transactions (atomic within a namespace; failed check throws 409)
await db.transaction([
  { op: "check", collection: "users", key: "u1", exists: true },
  { op: "mutate", collection: "counters", key: "todos", increment: { total: 1 }, upsert: true },
]);

// Indexes
await db.indexes.create("todos", { fields: ["owner", "created:desc"] });
```

## Search

Full-text search with typed fields, facets, snippets, sorting, and cursors.

```ts
import { f, facet } from "@altengine/sdk";

const idx = ae.search("myapp").index("products");

await idx.put([{
  id: "p1",
  fields: [
    f.text("title", "Blue Suede Shoes"),
    f.atom("sku", "BS-001"),
    f.number("price", 59),
    f.date("added", new Date()),
    f.geo("store", { lat: 37.77, lng: -122.41 }),
  ],
  facets: [facet.atom("category", "shoes"), facet.number("price", 59)],
}]);

const res = await idx.search({
  query: 'shoes price<100',
  facets: ["category"],
  snippet: { fields: ["title"] },
  sort: [{ expr: "price" }],
  limit: 20,
});

for await (const hit of idx.searchAll({ query: "shoes" })) { /* cursor-paged */ }
```

## Channels

Server side — mint subscriber tokens, publish, read presence:

```ts
const ch = ae.channel("myapp");
const tok = await ch.createToken({ channels: ["room:1"], ttl_seconds: 3600, presence_id: "u42" });
await ch.publish("room:1", { hello: "world" });
const roster = await ch.presence("room:1");
```

Browser side — hand the page a token from your backend and subscribe over
WebSocket (auto-reconnect, resubscribe, keepalive):

```ts
import { ChannelSocket } from "@altengine/sdk/channel";

const sock = new ChannelSocket({
  getToken: () => fetch("/api/channel-token").then((r) => r.json()), // { token, ws_url }
});
sock.on("message", ({ channel, data }) => render(data));
await sock.connect();
await sock.subscribe(["room:2"]);
```

The `@altengine/sdk/channel` subpath contains no API-key code — safe to bundle
into client apps.

## Local development

Run the [altengine emulator](https://github.com/altlimit/altengine) and construct
the client with `dev: true` (or set `ALTENGINE_URL`) — any non-empty API key works:

```bash
alt install altlimit/altengine && altengine dev
```

Get an API key and instances for production at [www.altengine.net](https://www.altengine.net).
