# altengine CLI (local emulator)

A local **emulator** for the altengine services — **datastore**, **search**, and
**channels** — plus a built-in **admin console**. It lets you develop against the
altengine APIs on your machine without connecting to the hosted service. One static
Go binary, no external dependencies.

```bash
alt install altlimit/altengine   # via https://github.com/altlimit/alt
altengine dev
# → http://127.0.0.1:9191  (admin console + all three data planes)

# or from source:
cd cli && go build -o altengine ./cmd/altengine
```

Point your SDK/base URL at `http://127.0.0.1:9191` and use **any** `Authorization: Bearer <token>`.

## Why

This emulator re-implements the **data-plane HTTP/WebSocket contracts** the hosted
services expose, backed by local SQLite (pure-Go `modernc.org/sqlite`, so search/datastore inherit real FTS5 + `json_extract` +
keyset-cursor semantics) and an in-memory pub/sub hub for channels. Feature parity is high where
it's cheap and clearly documented where it isn't (see [Not emulated](#not-emulated)).

## Install / run

```bash
altengine dev [flags]

  --port 9191            port to listen on
  --host 127.0.0.1       host to bind
  --data ./.altengine     data directory (SQLite files + control.json)
  --memory               keep everything in RAM (nothing persisted)
  --reset                wipe the data directory before starting
```

Data persists to `--data` by default and survives restarts. Instances **auto-create on first use**,
so no setup is needed — just start issuing requests.

## Auth (dev-open)

The emulator accepts any `Authorization: Bearer <token>` and grants full access on a single local
dev org — mirroring the altengine engine's development-only `DEV_API_KEY` full-access bypass (which
production actively refuses to boot with). You can still mint grant-scoped keys in the
console (**API keys** tab) to exercise `read < write < full` grant enforcement locally.

## Admin console

Open `http://127.0.0.1:9191/`. Vanilla SPA (embedded in the binary, no build step):

- **Overview** — instances per service and base URLs.
- **Datastore** — namespace/collection browser, query & aggregate consoles, insert, index editor.
- **Search** — index browser, query box (full query language), put/delete documents.
- **Channels** — live WebSocket playground: subscribe, publish (HTTP + WS), see delivered frames.
- **API keys** — mint scoped keys (shown once) and revoke.

## Services & endpoints

All data-plane routes require `Authorization: Bearer <token>`. `{instance}` auto-creates.

### Datastore — `/v1/datastore/{instance}`

| Method & path | Purpose |
|---|---|
| `POST /namespaces/{ns}/collections/{c}/documents` | put/upsert `{documents:[{key?,data}]}` → `{keys}` |
| `GET  /namespaces/{ns}/collections/{c}/documents/{key}` | get one |
| `POST /namespaces/{ns}/collections/{c}/documents/batchGet` | `{keys:[]}` → `{documents}` |
| `POST /namespaces/{ns}/collections/{c}/documents/delete` | `{keys:[]}` → `{deleted}` |
| `POST /namespaces/{ns}/collections/{c}/query` | `{where,order,limit,cursor,keys_only,join}` |
| `POST /namespaces/{ns}/collections/{c}/aggregate` | `{where,group,metrics,order,limit}` |
| `POST /namespaces/{ns}/transaction` | `{operations:[put/delete/mutate/check]}` |
| `GET/POST/DELETE /namespaces/{ns}/collections/{c}/indexes[/{id}]` | index CRUD |

Query DSL: `where` = `[{field,op,value}]` with ops `= != < <= > >= in`; dot-paths + meta selectors
`__key__ __created__ __updated__`; keyset `cursor`; `join` by foreign key. Queries must be
**index-served** or (with `autoIndex` on, the default) the suggested index is auto-created and the
response carries `auto_indexed`. Transactions are atomic within a namespace, with field-level
`mutate` (`set`/`increment`/`remove`) and optimistic `check`.

### Search — `/v1/search/{instance}`

| Method & path | Purpose |
|---|---|
| `PUT  /indexes/{index}/documents` | batch put (lazy index create) → `{ids}` |
| `GET  /indexes/{index}/documents/{docId}` | get one |
| `GET  /indexes/{index}/documents` | list/range |
| `POST /indexes/{index}/documents/delete` | `{ids:[]}` → `{deleted}` |
| `POST /indexes/{index}/search` | search |
| `GET  /indexes/{index}/schema` | union field schema |
| `GET  /indexes` · `DELETE /indexes/{index}` | list / drop |

Field types: `text html atom number date geo tokenprefix untokenprefix`. Query language: bare/global
terms, field scope (`title:up`, `genre = "sci fi"`), numeric/date compares (`rating > 3`),
`AND`/`OR`/`NOT`/`-`, grouping (`genre:(comedy OR drama)`), phrases, prefix (`term*`), stemming
(`~running`), and geo (`distance(loc, geopoint(37.7,-122.4)) < 1000`). Supports `limit`/`offset`/
`cursor`, `sort`, `ids_only`, `returned_fields`, facet discovery + refinements, and bounded
`total_hits` with `total_hits_accuracy`.

Result shaping matches production too: **snippets** (`snippet` block → per-result highlighted,
HTML-escaped excerpts via FTS5) and **field collapsing** (`collapse: {field, limit}` — top-N per
distinct atom/number value; `total_hits`/facets stay uncollapsed). Instance config applies the same
way as production: **synonyms** (`equivalents`/`oneWay` + the computed numeral toggles
`numberRoman`/`numberWords`, so `godfather 2` can find *Part II*) and **query rules**
(pin/hide/`rule_data`) — set them via the instance's admin config. One fidelity gap: multi-word
synonym *keys* ("laptop computer" as a source) aren't matched against word runs here; single-word
keys and multi-word targets work.

### Channels — `/v1/channel/{instance}`

| Method & path | Purpose |
|---|---|
| `POST /tokens` | mint a subscriber JWT `{channels,ttl_seconds,publish,presence_id}` |
| `POST /publish` | `{channel,data}` → `{delivered}` (API key or publish-JWT) |
| `GET  /subscribe` | WebSocket upgrade (`?token=` or `Sec-WebSocket-Protocol: bearer,<jwt>`) |
| `GET  /presence` | server-side roster (requires `presence` config on) |

Delivered wire frame: `{"channel","data","ts"}` (`ts` ms). Over the socket: `{type:subscribe|unsubscribe|publish}`
in, `{type:subscribed|published|error}` and `{type:presence,event}` control frames out. HS256 tokens
signed with the instance secret; `publish` may be `true`/`"http"`/`"ws"`/`"all"`.

## Quick examples

```bash
BASE=http://127.0.0.1:9191
AUTH='Authorization: Bearer devkey'

# datastore
curl -H "$AUTH" -XPOST $BASE/v1/datastore/shop/namespaces/main/collections/orders/documents \
  -d '{"documents":[{"key":"o1","data":{"total":150,"user_id":"u1"}}]}'
curl -H "$AUTH" -XPOST $BASE/v1/datastore/shop/namespaces/main/collections/orders/query \
  -d '{"where":[{"field":"total","op":">","value":100}]}'

# search
curl -H "$AUTH" -XPUT $BASE/v1/search/app/indexes/movies/documents \
  -d '{"documents":[{"id":"m1","fields":[{"name":"title","type":"text","value":"Up in the Air"},{"name":"rating","type":"number","value":4}]}]}'
curl -H "$AUTH" -XPOST $BASE/v1/search/app/indexes/movies/search -d '{"query":"title:air rating > 3"}'

# channels: mint token, then connect a WebSocket to the returned ws_url
curl -H "$AUTH" -XPOST $BASE/v1/channel/chat/tokens -d '{"channels":["room1"],"publish":true}'
curl -H "$AUTH" -XPOST $BASE/v1/channel/chat/publish -d '{"channel":"room1","data":{"hi":"there"}}'
```

## Development

```bash
go build ./...    # build
go test ./...     # unit + integration tests (in-process, no network)
go vet ./...
```

Layout:

```
cmd/altengine/main.go       CLI (dev subcommand)
internal/common/            error envelope, JSON helpers, ids
internal/auth/              dev-open bearer + grants
internal/control/           instance & key registry (JSON-persisted)
internal/datastore/         store + query/aggregate/transaction engine (SQLite json_extract)
internal/search/            store + query lexer/parser/compiler + search (SQLite FTS5 + EAV)
internal/channel/           in-memory rooms/connections + JWT + WebSocket
internal/admin/             control-plane REST + embedded console (web/*)
internal/server/            HTTP wiring
```

## Not emulated

Intentionally out of scope for a local dev tool (documented so the parity gap is explicit):
metering/billing, real user sessions & OAuth (auth is dev-open), CSRF, rate limiting, usage
archival, datastore PITR/backups, and multi-node channel fan-out (single-process hub). The
index-served guard matches the hosted service on field coverage but is lenient on sort
*direction*.
