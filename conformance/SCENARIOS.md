# SDK Conformance Scenarios

Every SDK (js/, go/, python/, php/) must ship an integration suite covering the
scenarios below, run against a live emulator (`altengine dev --memory`). The same
suite must be retargetable at a hosted instance via environment variables:

- `ALTENGINE_CONFORMANCE_URL` — base URL (defaults to the spawned emulator)
- `ALTENGINE_CONFORMANCE_KEY` — API key (any non-empty value works on the emulator)
- `ALTENGINE_CONFORMANCE_DESTRUCTIVE=1` — enable delete-instance/namespace/index
  scenarios against a hosted target (always enabled on the emulator)

Shared payloads live in `fixtures/` so every language exercises identical data.

## Cross-cutting

- [ ] Error envelope: failures parse to `{error:{code,message,details?}}` with the
      HTTP status; unknown codes don't break the client
- [ ] 429 carries `Retry-After`; client retry honors it; 507 INDEX_FULL not retried
- [ ] Puts are upserts (repeat put succeeds, `created` preserved); deletes of
      missing keys/ids are no-ops
- [ ] Batch limits enforced server-side: search 200/batch, datastore 500/batch →
      INVALID_ARGUMENT
- [ ] Grant scoping: read key can't write, write key can't delete-instance-level
      (`full`) resources (hosted target only; the emulator's minted keys honor grants)

## Datastore

- [ ] put → get roundtrip (auto-id and explicit key; numeric key ≡ decimal string)
- [ ] bulk get (POST documents/get) returns found docs, omits missing on the wire; SDKs map to order-preserving nulls
- [ ] delete → get returns null/NOT_FOUND
- [ ] query: `=`, `!=`, range ops, `in` (≤80 values), dot-path fields, `__key__`
- [ ] order asc/desc + limit + cursor pagination to exhaustion
- [ ] keys_only query returns keys
- [ ] join attaches joined docs under `joins[as]`
- [ ] non-index-served query: auto_indexed on default instances; INDEX_REQUIRED
      with `details.suggested_index` when auto-index is off
- [ ] aggregate: count/sum/avg/min/max, group by, order, limit
- [ ] transaction: put + mutate(set/increment/remove) + delete atomically
- [ ] transaction check failure → 409, nothing applied
- [ ] index CRUD: create (idempotent), list, delete; unique index violation
- [ ] namespaces: isolation between namespaces, list, delete (destructive)
- [ ] document too large (>1 MiB) → DOCUMENT_TOO_LARGE

## Search

- [ ] put with all field types (text, html, atom, number, date, geo, tokenprefix,
      untokenprefix) → get roundtrip; server-assigned ids returned in order
- [ ] query language: bare term, field:value, AND/OR/NOT/-, phrase, comparison,
      stem (~), atom exact match
- [ ] sort by field/rank with default; ids_only; returned_fields
- [ ] cursor pagination to exhaustion; total_hits accuracy behavior
- [ ] facets: explicit + discover, value counts, number ranges, refinements
- [ ] snippets: pre/post tags, max_tokens
- [ ] collapse (distinct) on a field
- [ ] listDocuments keyset pagination (start_id/include_start) to exhaustion
- [ ] schema reflects the union of indexed fields
- [ ] delete docs → deleted count; delete index (destructive)
- [ ] namespace isolation via X-Namespace / ?namespace=
- [ ] listNamespaces returns namespaces with live indexes (default namespace as "")
- [ ] invalid namespace (NUL via %00, >100 bytes, non-ASCII) → 400 INVALID_ARGUMENT

## Channel

- [ ] createToken: channels echoed, expires_at sane, ws_url usable
- [ ] publish via HTTP → delivered count matches live subscribers
- [ ] WS lifecycle: connect with token → receive published message with
      channel/data/ts
- [ ] subscribe/unsubscribe over the socket (ack frames)
- [ ] publish over the socket requires ws-capable token; delivered ack
- [ ] token channel narrowing via ?channels= subset; channel outside claims → error
- [ ] presence: occupancy/member_count with presence_id (instance flag on)
- [ ] reconnect: kill the socket server-side → client reconnects and resubscribes
      (SDKs with a managed socket)
- [ ] message > 32 KiB → INVALID_ARGUMENT
