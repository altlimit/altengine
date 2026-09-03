# @altengine/sdk

Official JavaScript/TypeScript SDK for [altengine](https://www.altengine.net) —
managed **datastore**, **search**, realtime **channels**, **blob** storage and
**containers** behind one API key. Zero dependencies, fetch-based, works in
Node ≥20, browsers, and edge runtimes.

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
const docs = await db.get("todos", keys);           // array in → array out, nulls for missing
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

const doc = await idx.get("p1");                 // null when missing
const docs = await idx.get(["p1", "nope"]);      // array in → array out, nulls for missing

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

## Automation

Run scripts on your own Windows machines — driving desktop applications and
internal endpoints nothing on the internet can reach. Scripts are deployed with
the CLI (`altengine automation deploy`); this starts them and reads what they
produced.

```ts
const auto = ae.automation("fleet");

// Start and wait. The run is finished when its OUTPUT has landed, not when the
// script returned — so a `done` here always has its files, with live links.
const { run, artifacts } = await auto.run(
  { script: "nightly-export", params: { date: "2026-08-30" }, labels: ["site-dallas"] },
  { onPoll: (r) => console.log(r.status, r.phase ?? "") },
);
for (const a of artifacts) console.log(a.name, a.size_bytes, a.url);

// Or start and come back later.
const started = await auto.start({ script: "nightly-export" });
const page = await auto.logs(started.id);
await auto.cancel(started.id);
```

A `queued` run is not a failure: no matching machine is online yet, and an office
PC being asleep is the ordinary case. `wait`'s `timeoutMs` bounds the WAIT, not
the run — giving up leaves the job running on the machine, so call `cancel` if
that is what you meant.

Organization API key only. End-user identity tokens are refused here as they are
for containers: a run spends time on hardware you own, and access rules bound
what a user may read, not what they may spend.

## Blob

Files, stored and served. **The bytes never travel through the API**: every
upload and download goes to a presigned URL, straight to storage.

```ts
const files = ae.blob("assets");

// `put` picks the transfer — one PUT, or a multipart upload for a Blob/File
// bigger than storage takes in a single request.
const rec = await files.put("invoice.pdf", pdfBytes, { contentType: "application/pdf" });
rec.blobkey;   // blob:<instance>:<id> — the handle to store in a document
rec.url;       // the public URL, or null when the object is private

const { download_url, expires_at } = await files.get(rec.id);   // signed, minutes
const bytes = await files.bytes(rec.id);

for await (const b of files.listAll({ prefix: "invoices/" })) console.log(b.name, b.size);
await files.setPublic(rec.id);
await files.delete([rec.id]);
```

For a browser upload, your backend decides whether this user may upload and how
big the file may be, then hands over a URL that does exactly that:

```ts
const minted = await files.uploadUrl({ name, size, content_type: type });
// the page PUTs the bytes to minted.upload_url with that content-type
```

The size is signed into the URL, so it is the real bound on what whoever holds it
can make you store. There is no commit step — the row is promoted by the side
that received the bytes, so a `get` straight after an upload reads `ready`.

A container job that was given a store reads it from its own environment:

```ts
import { blobFromJobEnv } from "@altengine/sdk";

const out = blobFromJobEnv();          // AE_BLOB_URL + AE_BLOB_TOKEN
await out.put("result.csv", csv, { contentType: "text/csv" });
```

That token puts and gets. It cannot `list` the store, cannot publish (`setPublic`,
or `public: true` on an upload) and cannot delete — each a 403 naming what was
refused, never a quiet downgrade.

## Containers

A Docker image run as a background job, for work that will not fit in a function.

```ts
const jobs = ae.container("batch");

const job = await jobs.run({
  image: "ghcr.io/me/worker:1",
  cmd: ["./run", "--period", "2026-08"],
  size: "medium",
});
const done = await jobs.wait(job.id, { onPoll: (j) => console.log(j.status) });
console.log(done.exit_code, done.cost_usd);

const { lines, cursor } = await jobs.logs(job.id);
await jobs.cancel(job.id);
const { sizes, allowed_images, max_timeout_ms } = await jobs.sizes();
```

Only images on the instance's allowlist launch; an empty allowlist runs nothing.
`run` returns as soon as the machine exists — a job's output goes to blob or to a
completion function, not back through this call. `wait`'s `timeoutMs` bounds the
wait, not the job: giving up leaves the machine running and billing, so call
`cancel` if that is what you meant.

`list` is keyset-paged and its `cursor` feeds back as `before`; `listAll` walks
the pages for you.

Organization API key only, like automation.

### Levels

A grant level is a ceiling, and they are cumulative: `read` < `write` < `full`.

| Level | Blob | Container |
|---|---|---|
| `read` | `get`, `bytes`, `list` | `get`, `list`, `logs`, `sizes` |
| `write` | `put`, `uploadUrl`, `setPublic` | `run`, `cancel` |
| `full` | `delete` | — |

## Auth

Auth gives your **end users** accounts and identity tokens, so a browser app can
talk to Datastore and Channel **directly — with no backend of your own**. Access
rules on the auth instance decide what each user may read and write, which is what
makes it safe to hold that token in a browser.

```ts
import { AuthClient } from "@altengine/sdk/auth";
import { AltEngine } from "@altengine/sdk";

const auth = new AuthClient({ instance: "myapp-auth" });

// Render a sign-in form that matches the instance's configured fields.
const cfg = await auth.config(); // { identity_field, fields, methods, ... }

const res = await auth.signIn("alice@example.com", "hunter2");
if (res.status === "mfa_required") {
  await auth.verifyMfa(res.mfaToken, promptForCode());
}

// Hand the client to the SDK — every request now carries the user's id_token,
// refreshed automatically before it expires.
const ae = new AltEngine({ auth });
const { documents } = await ae.datastore("myapp").query("posts", { limit: 20 });
```

Also available: `signUp`, `passwordlessStart`/`passwordlessVerify` (emailed
one-time code), `passwordResetStart`/`passwordResetVerify`, `me`, `refresh`, and
`signOut`. `auth.onChange(fn)` fires on sign-in/sign-out so your UI can react, and
`auth.user` / `auth.isSignedIn` survive a page reload.

The session persists in `localStorage` by default; pass `storage: memoryStorage()`
(or your own `TokenStorage`) to change that. Like the channel subpath,
`@altengine/sdk/auth` contains no API-key code.

> An org API key is a **backend** credential — never ship one to a browser. That
> is why passing `auth` takes precedence over `apiKey`.

## Local development

Run the [altengine emulator](https://github.com/altlimit/altengine) and construct
the client with `dev: true` (or set `ALTENGINE_URL`) — any non-empty API key works:

```bash
alt install altlimit/altengine && altengine dev
```

Get an API key and instances for production at [www.altengine.net](https://www.altengine.net).
