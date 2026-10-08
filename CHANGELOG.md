# Changelog

Changes that affect how the CLI, the emulator or an SDK is used. Releases are tagged `vX.Y.Z`
(CLI), `go/vX.Y.Z`, `js/vX.Y.Z`, `php/vX.Y.Z` and `python/vX.Y.Z`.

## Unreleased

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

### SDKs

- All SDKs fall back to `ALTENGINE_KEY` after `ALTENGINE_API_KEY`.
- JS: `require()` gets CommonJS type declarations; `ChannelSocket` works on Node 20 with the
  `webSocket` option.
- Go, Python and PHP send a `User-Agent`; the Python package ships `py.typed`.
