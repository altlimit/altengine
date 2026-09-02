# Security policy

## Reporting a vulnerability

**Do not open a public issue.**

- Preferred: [report it privately through GitHub](https://github.com/altlimit/altengine/security/advisories/new).
- By email: **security@altengine.net**.

Include what you did, what happened, and what you expected. A proof of concept helps.
You will get an acknowledgement within 3 business days and an assessment within 10.
Please give us 90 days before disclosing publicly, or less if a fix ships sooner.

## What this covers

This repository holds the **local emulator** (`cli/`) and the **client SDKs** (`js/`, `go/`,
`python/`, `php/`). Issues in the hosted service at `api.altengine.net` are not in this
repository — mail security@altengine.net for those.

In the SDKs, we care about anything that mishandles a credential: a key or token written to a
log, sent to the wrong host, attached to a request that should not carry it, or a TLS or
certificate check that can be bypassed.

## The emulator is open by design, and that is not a vulnerability

`altengine dev` is a development tool. It deliberately has no authentication:

- **Any** `Authorization: Bearer <token>` is accepted and granted full access to every service.
- `/mcp` is served unauthenticated, and its tools can create, read and delete local data.
- Functions run **arbitrary JavaScript** you deploy to it.
- Containers start jobs on your **local Docker daemon**.
- `--static` serves a directory you name, from disk, on every request.

That is why it binds `127.0.0.1` by default. Reports that amount to "the emulator has no
authentication" or "an unauthenticated request can write data" describe the intended behaviour
and will be closed.

**Run only code you trust in it, and keep it bound to localhost.** `--host 0.0.0.0` puts an
unauthenticated JavaScript runtime and your Docker daemon on the network for anyone who can
reach the port.

### What IS a vulnerability here

The emulator's job is to make local behaviour match the hosted service, so the security bugs
that matter are the ones where it **diverges in the permissive direction**:

- The emulator allows something the hosted service refuses — a call that succeeds locally and
  is denied on deploy means a developer built against a permission they do not have. This is
  the divergence that costs something, and we treat it as a real bug.
- Access rules, grant levels or row-level rules evaluated more loosely here than hosted.
- Anything reachable from a served request that escapes the directory or data directory it
  was pointed at: path traversal, a symlink out of a `--static` root, or a write outside
  `--data`.
- A local file being readable by a request that names no path to it.
