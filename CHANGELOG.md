# Changelog

Changes that affect how the CLI, the emulator or an SDK is used. Releases are tagged `vX.Y.Z`
(CLI), `go/vX.Y.Z`, `js/vX.Y.Z`, `php/vX.Y.Z` and `python/vX.Y.Z`.

## Unreleased

### Emulator

- Mailbox sends and replies take `attachments` (up to 32; `{ filename, contentType, content,
  contentId }`, `content` base64 — or bytes from a function) on `/v1`, `env.mailbox.send` /
  `reply`, `mailbox_send` / `mailbox_reply` and the console's new Send tab and Reply form. A
  message over 5 MB once encoded is refused with 413 before it counts against a send cap; a
  send or reply body over 8 MB is 413. The sent message lists its attachments and they
  download like received ones; a send looped back to a local mailbox carries them.
- `POST /v1/mailbox/{instance}/messages/{id}/keep` `{ "days": 1–90 }`, `env.mailbox.keep` and
  the `mailbox_keep_message` tool set one message's expiry. `write` extends; an earlier date
  needs `full`; the MCP tool only extends. The console's message view has a "Deleted after"
  control.
- `GET /v1/mailbox/{instance}` and `mailbox_info` return `sendPaused`, always `null` locally.
- Route matching is checked against `conformance/fixtures/mailbox-routes.json`, the cases the
  hosted service asserts too.

### SDKs

No SDK has been published to a package registry yet; these are the changes since the first
SDK commit that a release will carry.

- All SDKs fall back to `ALTENGINE_KEY` after `ALTENGINE_API_KEY`.
- JS: `require()` gets CommonJS type declarations; `ChannelSocket` works on Node 20 with the
  `webSocket` option.
- JS: `verifyWebhook({ secret, body, signature, timestamp, toleranceMs })` checks an automation
  completion webhook's signature and timestamp.
- JS: a channel socket pings at least once a minute; a longer `pingIntervalMs` is capped at
  60000, because the service closes a socket (4408) that goes 15 minutes without one.
- Go, Python and PHP send a `User-Agent`; the Python package ships `py.typed`.
- JS: `ae.mailbox(instance)` — `info`, `list`, `wait`, `get`, `raw`, `attachment`, `markRead`,
  `send`, `reply`, `keep`, `delete` and `addresses.list` / `create` / `disable`. Attachment
  `content` may be base64 text, a `Uint8Array`, an `ArrayBuffer` or a `Blob`.
- JS, Go and Python channel sockets send the token as a subprotocol (`ws_endpoint` +
  `protocols`), not in the URL; a token response with only `ws_url` is still opened as before.
  An explicit `url` / `URL` option now sends the token as a subprotocol too. The token response
  types gain `ws_endpoint` and `protocols`; `ws_url` is deprecated.

## v0.2.33 — 2026-10-26

### Emulator

- Mailbox service: `/v1/mailbox/{instance}`, the `mailbox_*` MCP tools and `env.mailbox`
  (`info`, `list`, `get`, `send`, `reply`, `markRead`, `listAddresses`, `createAddress`,
  `disableAddress`, `delete`). Addresses are `<instance>[.<name>]@mail.localhost`
  (`--mail-domain` or `ALTENGINE_MAIL_DOMAIN`). Routes call functions, publish to channels and
  POST signed webhooks. Nothing sent leaves the emulator: sends are recorded, and a send to a
  local address is delivered into that mailbox (console toggle). The console's Mailbox tab
  composes test mail, uploads `.eml` files and replays a message through the current routes.
  `--smtp` starts an SMTP listener on `127.0.0.1:2525` (`--smtp-addr`, `--smtp-auth pass|none`).
- A functions instance's `defaultFunction` answers `/fn/{instance}/` and every path whose first
  segment names no other function, with the full path. A named function still wins, and `/api`,
  `/admin` and `/v1` never reach it. It must name a deployed function, and deleting that function
  clears it. `functions_list` and `GET /v1/functions/{instance}` return `default_function`.
- A function called by a container job's completion sees `x-ae-trigger: container`.

## v0.2.32 — 2026-10-20

### Emulator

- Channel subscribe takes its token from `Sec-WebSocket-Protocol: altengine.v1,
  altengine.token.<token>` (answered with `altengine.v1`) or `Authorization: Bearer`, as hosted.
  `?token=` still works, wins over a header, and is answered with `Deprecation`,
  `Sunset: Sun, 31 Jan 2027` and a `rel="deprecation"` `Link`. Token mints return `ws_endpoint`
  and `protocols` beside `ws_url`.

## v0.2.31 — 2026-10-20

### CLI

- `altengine login` / `altengine logout` save and remove an API key (stdin, no echo; `0600`
  file in the user config directory, `ALTENGINE_CREDENTIALS` overrides the path).
- The API key is read from `ALTENGINE_API_KEY` (documented) or `ALTENGINE_KEY` (still accepted).
- `ALTENGINE_URL` is optional; it defaults to `https://api.altengine.net`. An `http://` URL is
  refused unless it is `localhost`.
- `--json` on deploy, `functions list|versions`, `static deploy|list|rollback|info` and
  `automation deploy|scripts|agents|run|runs`.
- `functions delete [--version n]` and `automation delete`; both ask first, or need `--yes`.
- `functions activate`, `automation rollback` and `static activate` are aliases of the existing
  commands.
- `automation runs --cursor` and `static list --cursor` continue a paged listing.
- `automation send` exits 1 when no job received the value.
- `<group> --help` prints to stdout and exits 0; usage errors go to stderr with exit 1.
- Hosted calls retry 429/503 and connection failures (and 502/504 for GET/PUT/DELETE).
- `dev --reset` refuses a directory without `control.json`, and asks first on a terminal
  (`--yes` skips).
- `altengine version` reports the module version for a `go install` build.
- Release binaries: `windows/arm64` added; each binary has a build-provenance attestation.

### Emulator

- The config `PUT` replaces the whole config, as hosted; a bare config body is saved.
- Config writes (console `PUT`, `patch_instance_config`, `auth_set_rules`) are validated with
  the hosted rules. Unset rate limits are `null`, not `0`.
- Configured `rateLimit` / `publishRateLimit` / `connectRateLimit` are enforced (429
  `RATE_LIMITED`).
- Function `subRequests` and `cpuMs` are validated on deploy; `fetch()` calls past
  `subRequests` throw.
- Datastore index declarations are validated as hosted (1–8 fields, 64 per collection, …).
- The data directory is `0700`, `control.json` `0600`, and a created data directory gets a
  `.gitignore`.
- A JSON body over 8 MiB is a 413; channel frames over 64 KiB close the socket; multipart parts
  are bounded by the reserved size and abandoned uploads are reaped after 6 hours.

## Earlier

Releases before v0.2.31 are on the [releases page](https://github.com/altlimit/altengine/releases),
each with a comparison of the commits it carried.
