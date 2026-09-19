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
- [ ] delete → get returns null (single-doc get is SDK sugar over the batch
      endpoint — the wire has no single-document route)
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
- [ ] bulk get (POST documents/get) returns found docs, omits missing on the wire; SDKs map to order-preserving nulls
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
- [ ] namespace isolation via the path (`/ns/{ns}/idx/{index}/…`)
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

## Auth

Auth is the odd one out: its endpoints are **public** (no API key) because the end
user's own credentials are the trust boundary. The `id_token` it issues is what
data-plane requests carry instead of an org key, and row-level access rules on the
auth instance scope what that user can reach.

- [ ] `GET /config` is fetchable unauthenticated and exposes no secrets
      (identity_field, fields, methods, captcha.site_key only)
- [ ] signup → signin roundtrip; the configured identity field is the login handle
      and every other collected field lands in the user's `claims`
- [ ] duplicate identifier → ALREADY_EXISTS
- [ ] wrong password and unknown identifier return the **same** error (no account
      enumeration)
- [ ] `/me` with the id_token returns the user; without it → UNAUTHENTICATED
- [ ] refresh rotates the refresh token and issues a fresh id_token; the old
      refresh token stops working
- [ ] signout revokes the refresh token
- [ ] passwordless start → verify signs in; `/start` returns 200 for an unknown
      identifier too (no enumeration)
- [ ] identity token on the data plane: accepted by datastore/channel; a target
      with no `access` entry → PERMISSION_DENIED (default-deny)
- [ ] row rules: `create.stamp` overwrites author fields from the token (a forged
      author in the request body is ignored); read filters scope a query to the
      caller's own rows; update/delete of another user's row → PERMISSION_DENIED;
      changing an `immutable` field → PERMISSION_DENIED
- [ ] channel tokens minted for an identity are restricted to the templated
      channel patterns and are subscribe-only
- [ ] live bridge: a committed datastore write publishes `{op, keys}` on
      `<collection>.<keyBy-value>` to the bound channel instance

## MCP

`POST /mcp` (JSON-RPC 2.0, stateless) is the surface an AI agent uses, and the
whole reason it is worth emulating is that a sequence of calls worked out
locally must run unchanged against the hosted service. So the contract here is
about **names**, not just behavior — a tool or argument spelled differently
turns "works locally" into a confusing production failure.

These apply to the emulator and the hosted server alike; run them against both.

- [ ] `initialize` → `tools/list` → `tools/call` round-trip; `ping` answers `{}`
- [ ] a notification (no `id`) gets **no** response body (202); every client
      sends `notifications/initialized` right after `initialize`
- [ ] a batch answers with an array; a batch over 20 messages is refused
- [ ] a missing `Authorization` header is refused (hosted: a key without MCP
      access is refused too, with a message saying how to fix it)
- [ ] **tool names match hosted exactly**: whoami, list_instances,
      create_instance, get_instance_config, patch_instance_config,
      delete_instance, datastore_query, datastore_put,
      datastore_list_collections, search_query, search_put_documents,
      search_list_indexes, functions_list, functions_deploy, functions_errors,
      usage_summary. No tool exists in one place and not the other.
- [ ] **argument names match hosted exactly** — notably `datastore_query.where`
      (NOT `filters`) and `order[].dir` (NOT `desc`)
- [ ] an unrecognised argument is **refused and names the valid ones**, never
      silently ignored: a dropped filter returns every document and looks right
- [ ] `patch_instance_config` merges — sending one field preserves its siblings
      and the response reports what actually changed; a field that IS sent is
      replaced wholesale, never merged into
- [ ] destructive tools refuse without `confirm: true`; with it,
      `delete_instance` removes the instance and everything it HOLDS — its
      databases and its files, not merely the row that names them (hosted also
      refuses while another resource depends on it: a blob store an automation
      fleet keeps its artifacts in, a fleet with machines still enrolled. The
      emulator serves neither service, so it has nothing that could depend on
      an instance and nothing to refuse)
- [ ] `functions_deploy` omitting `schedules` KEEPS the existing ones; `[]`
      clears them; `functions_list` reports them back
- [ ] `functions_versions` gives every version a `url` that runs that version's
      code with the function's CURRENT grants and secrets (hosted
      `https://{slug}--v{n}-fn.<suffix>/{fn}`, or null with no tenant host;
      emulator `/fn/{instance}--v{n}/{fn}`); an unknown `n` answers 404
      `NOT_FOUND` "function '<fn>' has no version <n>"; `v0`, `v03` and a
      versioned blob address are not served
- [ ] functions `keepVersions` (default 3) outside 1..50 or not an integer is
      refused `INVALID_ARGUMENT` by `patch_instance_config`; lowering it prunes
      at once; a deploy prunes past it; the ACTIVE version is never pruned (roll
      back to v1, deploy with `activate: false` past the limit → v1 still runs)
- [ ] `resources/list` offers the four `docs://` grounding resources and every
      argument they document is one the matching tool actually accepts
- [ ] results are bounded: list/query tools cap below the REST default and say so

## Path grammar

Both data planes share one shape, labels kept short:

- datastore: `/v1/datastore/{instance}/ns/{ns}/col/{collection}/…`
- search:    `/v1/search/{instance}/ns/{ns}/idx/{index}/…`
- namespace listing: `GET …/ns`

The namespace always rides the **path** (search included — no `X-Namespace`
header, no `?namespace=`). The empty (default) namespace can't be a URL segment
(routers collapse `//`), so it travels as the reserved sentinel **`_default`**;
the canonical value everywhere else (storage, listings) stays `""`. A namespace
literally named `_default` is rejected.

## Wire minimalism

Single-document reads are SDK-level sugar, not endpoints: both `get(key|id)` and
their array forms ride `POST …/documents/get` (`{keys|ids:[…]}` → `{documents}`,
misses omitted; SDKs rebuild order with nulls). Every batch operation is `POST`
(there is no `PUT`); servers expose no `GET …/documents/:id` route.

## Per-language exceptions

- **php/** has no WebSocket client (v1 is token/publish/presence only) — the
  "WS lifecycle" scenarios don't apply; everything else does.
- **Auth ships in js/ only for v1.** The auth client exists to let a *browser*
  sign an end user in and hold their token; go/, python/ and php/ are backend
  SDKs, where the org API key is the correct credential and a sign-in flow has no
  caller. The **server-side** halves of the Auth scenarios (identity tokens on the
  data plane, row rules, the live bridge) are language-agnostic and should be
  added to the other SDKs if and when they grow an auth client.
