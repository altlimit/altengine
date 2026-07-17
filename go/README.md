# altengine Go SDK

Official Go SDK for [altengine](https://www.altengine.net) — managed
**datastore**, **search**, and realtime **channels** behind one API key.
Stdlib-only HTTP; the WebSocket subscriber uses `github.com/coder/websocket`.

```bash
go get github.com/altlimit/altengine/go
```

## Setup

```go
import altengine "github.com/altlimit/altengine/go"

ae := altengine.New(altengine.WithAPIKey("ae_...")) // production: https://api.altengine.net
```

Everything is overridable, nothing is required:

- `WithAPIKey` — falls back to the `ALTENGINE_API_KEY` env var
- `WithDev()` — target the [local emulator](https://github.com/altlimit/altengine)
  at `http://127.0.0.1:9191` (`altengine dev`)
- `WithBaseURL` — explicit origin; also settable via the `ALTENGINE_URL` env var
  (resolution: `WithBaseURL` → `WithDev` → `ALTENGINE_URL` → production)
- `WithHTTPClient`, `WithTimeout`, `WithRetry` — transport tuning

Retryable failures (429 with `Retry-After`, 502/503/504, network errors) are
retried up to 3 times with jittered backoff — except transactions and
publishes, which are never auto-retried. API failures are `*APIError` with
`Code`, `Status`, `Details`, and `RetryAfter`; match with `errors.As`.

## Datastore

```go
db := ae.Datastore("myapp").WithNamespace("prod")

key, err := db.Put(ctx, "todos", nil, map[string]any{"title": "ship SDK", "done": false}) // nil key → auto-id
keys, err := db.PutMulti(ctx, "todos", []altengine.PutDocument{{Key: "t2", Data: todo}})
doc, err := db.Get(ctx, "todos", key)               // nil, nil when missing
docs, err := db.GetMulti(ctx, "todos", []any{"t1", "t2"}) // order-preserving, nil for missing
ok, err := db.Delete(ctx, "todos", key)             // missing keys are no-ops
n, err := db.DeleteMulti(ctx, "todos", []any{"t1", "t2"})

// Queries (QueryAll pages cursors for you)
page, err := db.Query(ctx, "todos", altengine.QueryRequest{
    Where: []altengine.Filter{{Field: "done", Op: "=", Value: false}},
    Order: []altengine.Order{{Field: "priority", Dir: "desc"}},
    Limit: 100,
})
for doc, err := range db.QueryAll(ctx, "todos", altengine.QueryRequest{
    Where: []altengine.Filter{{Field: "owner", Op: "=", Value: "ana"}},
}) {
    if err != nil { return err }
    var todo Todo
    _ = doc.DataAs(&todo)
}

// Aggregates
res, err := db.Aggregate(ctx, "todos", altengine.AggregateRequest{
    Group:   []string{"owner"},
    Metrics: []altengine.Metric{{Fn: "count", As: "n"}, {Fn: "sum", Field: "priority", As: "total"}},
})

// Transactions (atomic within a namespace; a failed check returns a 409)
_, err = db.Transaction(ctx, []altengine.TxnOp{
    altengine.TxnCheck("users", "u1", true),
    altengine.TxnMutate("counters", "todos", altengine.TxnOp{
        Increment: map[string]float64{"total": 1}, Upsert: true,
    }),
})

// Indexes
ix, err := db.CreateIndex(ctx, "todos", []string{"owner", "created:desc"}, false)
```

## Search

```go
idx := ae.Search("myapp").Index("products")

ids, err := idx.Put(ctx, []altengine.SearchDocument{{
    ID: "p1",
    Fields: []altengine.SearchField{
        altengine.Text("title", "Blue Suede Shoes"),
        altengine.Atom("sku", "BS-001"),
        altengine.Number("price", 59),
        altengine.Date("added", time.Now()),
        altengine.Geo("store", 37.77, -122.41),
    },
    Facets: []altengine.SearchFacet{altengine.AtomFacet("category", "shoes")},
}})

res, err := idx.Search(ctx, altengine.SearchRequest{
    Query:   "shoes price<100",
    Facets:  []string{"category"},
    Snippet: &altengine.SnippetRequest{Fields: []string{"title"}},
    Sort:    []altengine.SortSpec{{Expr: "price"}},
    Limit:   20,
})

for hit, err := range idx.SearchAll(ctx, altengine.SearchRequest{Query: "shoes"}) {
    if err != nil { return err }
    _ = hit // cursor-paged
}
```

## Channels

Server side — mint subscriber tokens, publish, read presence:

```go
ch := ae.Channel("myapp")
tok, err := ch.CreateToken(ctx, altengine.TokenRequest{
    Channels: []string{"room:1"}, TTLSeconds: 3600, PresenceID: "u42",
})
delivered, err := ch.Publish(ctx, "room:1", map[string]any{"hello": "world"})
roster, err := ch.Presence(ctx, "room:1")
```

Managed WebSocket subscriber (auto-reconnect, resubscribe, keepalive):

```go
sock := altengine.NewChannelSocket(altengine.SocketOptions{
    GetToken: func(ctx context.Context) (*altengine.TokenResponse, error) {
        return ch.CreateToken(ctx, altengine.TokenRequest{Channels: []string{"room:2"}})
    },
    OnMessage: func(m altengine.ChannelMessage) { render(m) },
})
if err := sock.Connect(ctx); err != nil { return err }
defer sock.Close()
```

## Local development

Run the [altengine emulator](https://github.com/altlimit/altengine) and
construct the client with `WithDev()` (or set `ALTENGINE_URL`) — any non-empty
API key works:

```bash
alt install altlimit/altengine && altengine dev
```

The conformance suite runs against a spawned emulator:

```bash
go test -tags conformance ./...
```

Get an API key and instances for production at [www.altengine.net](https://www.altengine.net).
