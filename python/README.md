# altengine Python SDK

Official Python SDK for [altengine](https://www.altengine.net) — managed
**datastore**, **search**, and realtime **channels** behind one API key.
Built on `httpx` (sync + async); the WebSocket subscriber uses `websockets`.

```bash
pip install altengine          # HTTP clients
pip install "altengine[ws]"    # + the managed WebSocket subscriber
```

## Setup

```python
from altengine import AltEngine

ae = AltEngine(api_key="ae_...")   # production: https://api.altengine.net
```

Everything is overridable, nothing is required:

- `api_key` — falls back to the `ALTENGINE_API_KEY` env var
- `dev=True` — target the [local emulator](https://github.com/altlimit/altengine)
  at `http://127.0.0.1:9191` (`altengine dev`)
- `base_url` — explicit origin; also settable via the `ALTENGINE_URL` env var
  (resolution: `base_url` → `dev` → `ALTENGINE_URL` → production)

There is an async twin with the same surface:

```python
from altengine import AsyncAltEngine

async with AsyncAltEngine(api_key="ae_...") as ae:
    doc = await ae.datastore("myapp").get("todos", "t1")
```

Requests and responses are **wire-verbatim dicts** — what the REST API
documents is exactly what you pass and get back. Retryable failures (429 with
`Retry-After`, 502/503/504, network errors) are retried up to 3 times with
jittered backoff — except transactions and publishes, which are never
auto-retried. Errors raise `AltEngineError` with `code`, `status`, `details`,
and `retry_after`.

## Datastore

```python
db = ae.datastore("myapp", namespace="prod")

keys = db.put("todos", [{"data": {"title": "ship SDK", "done": False}}])
doc = db.get("todos", keys[0])          # None when missing
docs = db.get("todos", keys)            # list in → list out, None for missing
db.delete("todos", keys)                # missing keys are no-ops

# Queries (query_all pages cursors for you)
page = db.query(
    "todos",
    where=[{"field": "done", "op": "=", "value": False}],
    order=[{"field": "priority", "dir": "desc"}],
    limit=100,
)
for doc in db.query_all("todos", where=[{"field": "owner", "op": "=", "value": "ana"}]):
    ...

# Aggregates
res = db.aggregate(
    "todos",
    group=["owner"],
    metrics=[{"fn": "count", "as": "n"}, {"fn": "sum", "field": "priority", "as": "total"}],
)

# Transactions (atomic within a namespace; a failed check raises a 409)
db.transaction([
    {"op": "check", "collection": "users", "key": "u1", "exists": True},
    {"op": "mutate", "collection": "counters", "key": "todos", "increment": {"total": 1}, "upsert": True},
])

# Indexes
db.create_index("todos", ["owner", "created:desc"])
```

## Search

```python
from altengine import f, facet

idx = ae.search("myapp").index("products")

idx.put([{
    "id": "p1",
    "fields": [
        f.text("title", "Blue Suede Shoes"),
        f.atom("sku", "BS-001"),
        f.number("price", 59),
        f.date("added", datetime.now(timezone.utc)),
        f.geo("store", 37.77, -122.41),
    ],
    "facets": [facet.atom("category", "shoes"), facet.number("price", 59)],
}])

doc = idx.get("p1")                  # None when missing
docs = idx.get(["p1", "nope"])       # list in → list out, None for missing

res = idx.search(
    "shoes price<100",
    facets=["category"],
    snippet={"fields": ["title"]},
    sort=[{"expr": "price"}],
    limit=20,
)

for hit in idx.search_all("shoes"):   # cursor-paged
    ...
```

## Channels

Server side — mint subscriber tokens, publish, read presence:

```python
ch = ae.channel("myapp")
tok = ch.create_token(channels=["room:1"], ttl_seconds=3600, presence_id="u42")
delivered = ch.publish("room:1", {"hello": "world"})
roster = ch.presence("room:1")
```

Managed WebSocket subscriber (asyncio; auto-reconnect, resubscribe, keepalive):

```python
from altengine import ChannelSocket

sock = ChannelSocket(get_token=lambda: ch.create_token(channels=["room:2"]))
await sock.connect()
async for msg in sock.messages():
    print(msg["channel"], msg["data"])
```

## Local development

Run the [altengine emulator](https://github.com/altlimit/altengine) and
construct the client with `dev=True` (or set `ALTENGINE_URL`) — any non-empty
API key works:

```bash
alt install altlimit/altengine && altengine dev
```

The conformance suite runs against a spawned emulator:

```bash
pip install -e ".[ws]" pytest && pytest
```

Get an API key and instances for production at [www.altengine.net](https://www.altengine.net).
