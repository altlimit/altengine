# altengine CLI (local emulator)

A local **emulator** for the altengine services — **datastore**, **search**, **channels**,
and **auth** — plus a built-in **admin console**. It lets you develop against the
altengine APIs on your machine without connecting to the hosted service. One static
Go binary, no external dependencies.

```bash
alt install altlimit/altengine   # via https://github.com/altlimit/alt
altengine dev
# → http://127.0.0.1:9191  (admin console + all four data planes)

# or from source:
cd cli && go build -o altengine ./cmd/altengine
```

Point your SDK/base URL at `http://127.0.0.1:9191` and use **any** `Authorization: Bearer <token>`.

## Deploying functions

`deploy` talks to the **hosted** service, not the emulator. It bundles your entry file and
everything it imports into a single ES module (the server resolves no imports) and uploads it.

```bash
export ALTENGINE_URL=https://api.altengine.net
export ALTENGINE_KEY=ak_...          # org API key, 'full' access to the instance
export ALTENGINE_INSTANCE=prod

altengine deploy --name hello ./hello.js       # bundle + deploy + activate
altengine deploy --dry-run ./hello.js          # bundle only, report the size
altengine deploy --minify --no-activate ./hello.js

altengine functions list
altengine functions versions hello
altengine functions rollback --version 3 hello
altengine functions pull --out hello.js hello
```

Access is granted per function, in the same `service[:instance]=level` form an API key uses:

```bash
altengine deploy --grants "datastore:appdb=full,search=read" --name hello ./hello.js
```

Omit `--grants` and the function **keeps the access it already had** — a routine redeploy of
source never silently strips it. Deploying itself needs `full` on the functions instance:
it replaces the code that runs with that instance's capabilities, so it is more powerful
than writing data through them.

### Schedules

A function can run on a timer as well as on request. Expressions are five-field cron, **in
UTC**, and you can give more than one:

```bash
altengine deploy --schedule "0 3 * * *" --name nightly ./nightly.js
altengine deploy --schedule "0 9 * * 1-5" --schedule "0 12 * * 6" --name digest ./digest.js
altengine deploy --unschedule --name nightly ./nightly.js     # back to HTTP-only
```

Several expressions are not a convenience — within one expression the hour and day-of-week
fields are **ANDed**, and cron's only OR is the fixed day-of-month/day-of-week rule, so
"09:00 on weekdays and 12:00 on Saturday" genuinely cannot be written as one.

Like `--grants`, omitting `--schedule` **keeps the existing schedules**, so deploying code
never silently unschedules a job. `--unschedule` is how you remove them.

A scheduled run arrives as a `POST` with `x-ae-trigger: cron` and a body of
`{"crons": [...], "scheduled_for": <epoch ms>}` — `crons` is always a list, so a function
that later gains a second schedule does not see its own payload change shape.

`altengine dev` runs the same scheduler locally, ticking on the minute. Three behaviours
match the hosted service exactly, because getting them wrong locally would be worse than
having no local scheduler:

- **One at a time.** A run still in flight when the next is due is skipped, not queued — a
  job never overlaps itself, and one that is permanently slower than its interval does not
  build a backlog it can never drain.
- **Not on startup.** A function runs because its expression came due while the scheduler
  was watching, not because the process restarted.
- **No retries.** A failed run is logged and not retried.

## Deploying a site

`static` hosts your built front end. Point it at your build output DIRECTORY, not a file.

```bash
export ALTENGINE_URL=https://api.altengine.net
export ALTENGINE_KEY=ak_...
export ALTENGINE_INSTANCE=marketing

npm run build
altengine static deploy ./dist

altengine static info                       # where it lives, which build is live
altengine static list                       # deploy history, * marks the live one
altengine static rollback <deployment-id>   # switch back to an earlier one
```

Files are hashed locally and only the ones the site does not already have are uploaded, so
redeploying a site where one page changed uploads one page.

A deployment is not live until it is activated, which `deploy` does for you. `--no-activate`
uploads one to publish later — and that publish and a rollback are the same operation. Neither
uploads anything, so switching between builds is immediate.

```bash
altengine static deploy --message "release 2.1" ./dist
altengine static deploy --dry-run ./dist       # hash and report, upload nothing
altengine static deploy --no-activate ./dist
```

The label defaults to the current commit, so `altengine static list` is readable without one.

Everything in the directory is deployed, dotfiles included — `.well-known/` has to survive, or
certificate renewal and app-association files break. A symlink pointing outside the directory is
refused rather than followed, since publishing whatever it points at is rarely what was meant.

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

## Credentials

There are two kinds of data-plane credential, and the emulator accepts both.

**Org API keys (dev-open).** The emulator accepts any `Authorization: Bearer <token>` and grants
full access on a single local dev org, so you can start issuing requests with no setup. You can
still mint grant-scoped keys in the console (**API keys** tab) to exercise `read < write < full`
grant enforcement locally.

**End-user identity tokens.** An auth instance (`/v1/auth/{instance}`) signs up and signs in your
app's *end users* and hands each one a short-lived `id_token`. Sending that token to the datastore
or channel plane makes the caller an end user rather than a trusted backend: grants come from the
auth instance's `access` config, and datastore reads/writes are additionally row-scoped by its
`rules`. This is the "no backend of your own" path — see [Auth](#auth--v1authinstance).

## Admin console

Open `http://127.0.0.1:9191/`. Vanilla SPA (embedded in the binary, no build step):

- **Overview** — instances per service and base URLs.
- **Datastore** — namespace/collection browser, query & aggregate consoles, insert, index editor.
- **Search** — index browser, query box (full query language), put/delete documents.
- **Channels** — live WebSocket playground: subscribe, publish (HTTP + WS), see delivered frames.
- **API keys** — mint scoped keys (shown once) and revoke.

Auth instances are managed through the admin API (`/admin/auth`) — create them, edit their
`signup`/`access` config, and rotate the signing secret (which invalidates every outstanding
identity token).

## Services & endpoints

All data-plane routes require `Authorization: Bearer <token>` (except the public auth endpoints,
which are browser-facing). `{instance}` auto-creates.

### Datastore — `/v1/datastore/{instance}`

| Method & path | Purpose |
|---|---|
| `GET  /ns` | list namespaces |
| `DELETE /ns/{ns}` | drop a namespace |
| `POST /ns/{ns}/col/{c}/documents` | put/upsert `{documents:[{key?,data}]}` → `{keys}` |
| `POST /ns/{ns}/col/{c}/documents/get` | `{keys:[]}` → `{documents}` (the only point-read) |
| `POST /ns/{ns}/col/{c}/documents/delete` | `{keys:[]}` → `{deleted}` |
| `POST /ns/{ns}/col/{c}/query` | `{where,order,limit,cursor,keys_only,join}` |
| `POST /ns/{ns}/col/{c}/aggregate` | `{where,group,metrics,order,limit}` |
| `POST /ns/{ns}/transaction` | `{operations:[put/delete/mutate/check]}` |
| `GET/POST /ns/{ns}/col/{c}/indexes` · `DELETE …/indexes/{id}` | index CRUD |

`{ns}` is the namespace segment; the default (empty) namespace is spelled `_default`. There is no
single-document GET — SDKs implement it as a one-key batch against `documents/get`.

Query DSL: `where` = `[{field,op,value}]` with ops `= != < <= > >= in`; dot-paths + meta selectors
`__key__ __created__ __updated__`; keyset `cursor`; `join` by foreign key. Queries must be
**index-served** or (with `autoIndex` on, the default) the suggested index is auto-created and the
response carries `auto_indexed`. Transactions are atomic within a namespace, with field-level
`mutate` (`set`/`increment`/`remove`) and optimistic `check`.

### Search — `/v1/search/{instance}`

| Method & path | Purpose |
|---|---|
| `GET  /ns` | list namespaces |
| `POST /ns/{ns}/idx/{index}/documents` | batch put (lazy index create) → `{ids}` |
| `POST /ns/{ns}/idx/{index}/documents/get` | `{ids:[]}` → `{documents}` |
| `GET  /ns/{ns}/idx/{index}/documents` | list/range |
| `POST /ns/{ns}/idx/{index}/documents/delete` | `{ids:[]}` → `{deleted}` |
| `POST /ns/{ns}/idx/{index}/search` | search |
| `GET  /ns/{ns}/idx/{index}/schema` | union field schema |
| `GET  /ns/{ns}/idx` · `DELETE /ns/{ns}/idx/{index}` | list / drop |

`{ns}` is the namespace segment (`_default` for the default one), the same as datastore. There is
no single-document GET — use the `documents/get` batch.

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

Minting with an **end-user identity token** instead of an API key is scoped: the requested channels
must match the auth instance's `channels` patterns (a trailing `*` is a prefix wildcard, and
`$auth.uid` / `$auth.identifier` / `$auth.email` / `$auth.claims.X` interpolate inside the string
from the verified token), the minted token is always subscribe-only, and `presence_id` is forced to
the end user's uid. End users publish by writing to the datastore, whose live bridge broadcasts the
change (see below).

### Auth — `/v1/auth/{instance}`

The end-user identity provider. These endpoints are **public** (browser-facing) — no API key.

| Method & path | Purpose |
|---|---|
| `GET  /config` | client-safe config: `allow_signup`, `identity_field`, `fields`, `captcha`, `methods` |
| `POST /signup` | the configured form + `password` → `{id_token,refresh_token,expires_at,refresh_expires_at,user}` (201) |
| `POST /signin` | `{identifier,password}` → the same token bundle |
| `POST /token/refresh` | `{refresh_token}` → a fresh `id_token` + a **rotated** `refresh_token` |
| `POST /signout` | `{refresh_token}` → `{ok:true}` (revokes it) |
| `GET  /me` | `Authorization: Bearer <id_token>` → `{user}` |
| `POST /passwordless/start` · `/passwordless/verify` | one-time emailed code sign-in |
| `POST /password/reset/start` · `/password/reset/verify` | one-time emailed code reset |

The signup form is **configurable** per instance (`signup` config): which fields to collect and
which one (`identityField`) is the unique login handle. That field's value becomes the user's
`identifier`; every other collected field becomes a **custom claim**, which is what row rules read
as `$auth.claims.X`. Passwords are hashed with PBKDF2-HMAC-SHA256; refresh tokens and one-time
codes are stored only as hashes.

Sign-in is deliberately non-enumerating: an unknown handle and a wrong password return the exact
same error, and `/passwordless/start` always answers 200 whether or not the account exists.

**One-time codes are printed, not emailed.** The emulator has no mailer, so a passwordless or reset
code is written to the server log:

```
[auth] sign-in code for alice@example.com: 481920
```

…and, **because the emulator runs dev-open**, also echoed in the `/start` response as `dev_code` so
tests and local clients can complete the flow without a mailbox. The hosted service never returns a
code — do not build on that field.

#### Row-level access for end users

An auth instance's `access` config decides what a token it issued may reach:

```jsonc
{
  "datastore:appdb": {
    "level": "write",
    "rules": {
      "posts": {
        "read": "authenticated",                                  // or "public", or a filter list
        "create": { "stamp": { "author_uid": "$auth.uid" } },     // server-set, forge-proof
        "update": {
          "match": [{ "field": "author_uid", "op": "=", "value": "$auth.uid" }],
          "immutable": ["author_uid"]
        },
        "delete": { "match": [{ "field": "author_uid", "op": "=", "value": "$auth.uid" }] }
      }
    }
  },
  "channel:appfeed": { "level": "read", "channels": ["posts.*", "dm.$auth.uid"] }
}
```

Enforcement matches the hosted service:

- **read** — the rule's filters are ANDed into `query`/`aggregate` *before* the index-served guard,
  and applied server-side to `documents/get` (a row you may not read is omitted, never leaked).
- **create** — `stamp` overwrites those fields from the verified token, so authorship can't be
  forged; an optional `match` then validates the resulting document.
- **update / delete** — the **existing** row must satisfy `match`, and `immutable` fields may not
  change. A put replaces the whole document, so an omitted immutable field counts as a change.
- `$auth.*` resolves **only** from the verified token. A placeholder that can't be resolved is a
  hard 403 — a rule never silently degrades into an unconstrained match.
- **Default-deny**: once `rules` is present, a collection or mode that isn't listed is refused.
- Namespace administration, transactions, and index management stay **backend-only** (org API key).

An org API key bypasses rules entirely: it is a trusted backend credential.

#### Datastore → channel live bridge

A datastore instance's `live` config turns committed writes into channel events:

```json
{ "live": { "channelInstance": "appfeed", "collections": { "posts": { "keyBy": "board" } } } }
```

After a committed put or delete the emulator publishes `{op, keys, ns, collection}` to
`appfeed` on channel `posts.<board-value>` (or bare `posts` with no `keyBy`). Keys only — never a
document body — so subscribers re-fetch through the access-controlled read path and a live event
can't leak a row they couldn't read. It is best-effort: a publish failure never fails the write.

## Quick examples

```bash
BASE=http://127.0.0.1:9191
AUTH='Authorization: Bearer devkey'

# datastore
curl -H "$AUTH" -XPOST $BASE/v1/datastore/shop/ns/main/col/orders/documents \
  -d '{"documents":[{"key":"o1","data":{"total":150,"user_id":"u1"}}]}'
curl -H "$AUTH" -XPOST $BASE/v1/datastore/shop/ns/main/col/orders/query \
  -d '{"where":[{"field":"total","op":">","value":100}]}'

# search
curl -H "$AUTH" -XPOST $BASE/v1/search/app/ns/_default/idx/movies/documents \
  -d '{"documents":[{"id":"m1","fields":[{"name":"title","type":"text","value":"Up in the Air"},{"name":"rating","type":"number","value":4}]}]}'
curl -H "$AUTH" -XPOST $BASE/v1/search/app/ns/_default/idx/movies/search -d '{"query":"title:air rating > 3"}'

# auth: sign up an end user, then use the id_token as the datastore credential
curl -XPOST $BASE/v1/auth/appauth/signup -d '{"email":"alice@example.com","password":"hunter2hunter"}'
ID_TOKEN=...   # the id_token from the response
curl -H "Authorization: Bearer $ID_TOKEN" -XPOST $BASE/v1/datastore/shop/ns/main/col/posts/query -d '{}'

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
internal/auth/              dev-open bearer API keys + grants
internal/control/           instance & key registry (JSON-persisted)
internal/datastore/         store + query/aggregate/transaction engine (SQLite json_extract)
internal/search/            store + query lexer/parser/compiler + search (SQLite FTS5 + EAV)
internal/channel/           in-memory rooms/connections + JWT + WebSocket
internal/identity/          auth data plane: end-user store, identity tokens, row rules
internal/admin/             control-plane REST + embedded console (web/*)
internal/server/            HTTP wiring
```

## Not emulated

Intentionally out of scope for a local dev tool (documented so the parity gap is explicit):
metering/billing, the hosted console's own admin sessions & OAuth (the emulator's `/admin` is open
on localhost), CSRF, rate limiting, usage archival, datastore PITR/backups, and multi-node channel
fan-out (single-process hub). The index-served guard matches the hosted service on field coverage
but is lenient on sort *direction*.

On the auth plane specifically:

- **TOTP 2FA** and **passkeys / WebAuthn** are not emulated — both need a real authenticator, which
  a local tool can't stand in for. Their endpoints answer `501 UNIMPLEMENTED` rather than
  pretending to succeed, so a client that requires them fails loudly. Sign-in therefore never
  returns an `mfa_required` challenge here.
- **No email is sent.** Passwordless and password-reset codes are printed to the server log and
  echoed as `dev_code` in the `/start` response (dev-open only).
- **CAPTCHA** is configurable and reported by `GET /config`, but no token is verified.
- Origin allowlists and per-IP rate limits are not enforced.
