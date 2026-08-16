# altengine

Developer tooling and SDKs for [altengine](https://console.altengine.net) — managed
**datastore**, **search**, and realtime **channels** behind one API key.

| Folder | What it is | Install |
|---|---|---|
| [`cli/`](cli/) | Local emulator + admin console (`altengine dev`) | `alt install altlimit/altengine` |
| [`js/`](js/) | JavaScript/TypeScript SDK (Node, browsers, edge) | `npm i @altengine/sdk` |
| [`go/`](go/) | Go SDK (stdlib HTTP + `coder/websocket`) | `go get github.com/altlimit/altengine/go` |
| [`python/`](python/) | Python SDK (sync + async, `httpx`/`websockets`) | `pip install altengine` |
| [`php/`](php/) | PHP SDK (Guzzle; channel tokens/publish, no WS) | `composer require altlimit/altengine` |
| [`conformance/`](conformance/) | Cross-SDK conformance scenarios + shared fixtures | — |

## Quick start

Run the whole platform locally — one static binary, no dependencies, no signup:

```bash
alt install altlimit/altengine     # via https://github.com/altlimit/alt
altengine dev
# → http://127.0.0.1:9191  (admin console + all three data planes)
```

Talk to it with the SDK (any non-empty API key works against the emulator):

```ts
import { AltEngine, f } from "@altengine/sdk";

const ae = new AltEngine({ dev: true, apiKey: "dev" }); // local emulator

// Datastore: JSON documents with queries, aggregates, transactions
const db = ae.datastore("myapp");
await db.put("todos", [{ data: { title: "ship it", done: false } }]);
const open = await db.query("todos", { where: [{ field: "done", op: "=", value: false }] });

// Search: full-text with facets, snippets, sorting
const idx = ae.search("myapp").index("products");
await idx.put([{ fields: [f.text("title", "Blue Suede Shoes"), f.number("price", 59)] }]);
const hits = await idx.search({ query: "shoes price<100" });

// Channels: realtime pub/sub over WebSocket
const ch = ae.channel("myapp");
const { token } = await ch.createToken({ channels: ["room:1"] });
await ch.publish("room:1", { hello: "world" });
```

Drop `dev: true` and the SDK targets production (`https://api.altengine.net`) —
get an API key at [www.altengine.net](https://www.altengine.net). Everything
behaves the same.

## Building with an AI agent

The emulator speaks [MCP](https://modelcontextprotocol.io) at `POST /mcp`, with
the same tools the hosted service exposes — create instances, configure them,
read and write data, deploy functions. Point your agent at the local server and
it can build against `altengine dev` rather than doing its experimenting in
production:

```json
{
  "mcpServers": {
    "altengine": {
      "url": "http://127.0.0.1:9191/mcp",
      "headers": { "Authorization": "Bearer dev" }
    }
  }
}
```

Any bearer token works locally; hosted needs a real key with MCP access. Tool
and argument names are identical in both places, so a sequence of calls that
works here works there — parity is checked by the MCP scenarios in
[`conformance/SCENARIOS.md`](conformance/SCENARIOS.md).

The server also publishes `docs://` resources for the search and datastore query
languages, worth reading before the first query: both are App Engine's syntax,
and a model that has not read them will confidently invent Lucene or SQL instead.

## Development

- Go workspace: `go.work` covers `cli/` — `cd cli && go test ./...`
- JS SDK: `cd js && npm ci && npm test` (the test suite builds and spawns the
  emulator automatically, so Go must be installed)
- Every SDK must pass the scenarios in [`conformance/SCENARIOS.md`](conformance/SCENARIOS.md)
  against the emulator; suites are retargetable at a hosted instance via
  `ALTENGINE_CONFORMANCE_URL` / `ALTENGINE_CONFORMANCE_KEY`.

## Releases

- **CLI**: every push to `main` touching `cli/**` builds and publishes per-platform
  binaries (installable via [`alt`](https://github.com/altlimit/alt)).
- **JS SDK**: push a `js/vX.Y.Z` tag matching `js/package.json` to publish
  `@altengine/sdk` to npm.
