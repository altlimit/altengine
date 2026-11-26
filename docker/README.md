# altengine emulator in Docker

`ghcr.io/altlimit/altengine` is the local emulator (`altengine dev`) as a container image, for
running it next to your app in docker compose. Every CLI release pushes it, for linux/amd64 and
linux/arm64.

The image is small: the static binary on `scratch`, plus CA certificates, and nothing else. There
is no shell or package manager, and it pulls at about 11 MB. It runs as a non-root user (uid
65532) and keeps its data in `/data`.

```bash
docker run --rm -p 127.0.0.1:9191:9191 ghcr.io/altlimit/altengine
# → http://127.0.0.1:9191  admin console + API
```

| Tag | What it is |
|---|---|
| `latest` | The newest release. |
| `0.2` | The newest `0.2.x` release. |
| `0.2.39` | Exactly that release. Pin this in CI. |

- [How it behaves in a container](#how-it-behaves-in-a-container)
- [Samples](#samples): [app + emulator](#app--emulator), [in-memory](#in-memory-for-tests-and-ci),
  [data in a project folder](#data-in-a-project-folder), [fresh on every start](#fresh-on-every-start),
  [deploy a manifest](#deploy-a-manifest-at-startup), [serve a site](#serve-a-built-site),
  [receive mail over SMTP](#receive-mail-over-smtp), [another service name](#another-service-name),
  [everything at once](#everything-at-once)
- [Reference](#reference)
- [Troubleshooting](#troubleshooting)

## How it behaves in a container

- **The emulator has no authentication.** Any `Authorization: Bearer <token>` gets full access.
  Publish its ports on the host's loopback, as `127.0.0.1:9191:9191`. Plain `9191:9191` exposes
  it on every network interface. Run only code you trust in it. See
  [SECURITY.md](../SECURITY.md).
- **Other services reach it at `http://altengine:9191`.** To guard against DNS rebinding, the
  emulator answers only to requests whose Host header is localhost or a name listed in
  `ALTENGINE_ALLOW_HOST`. The image sets that to `altengine`, so name the service `altengine` or
  set the variable to its name.
- **The SDKs read `ALTENGINE_URL`.** Set it on your app to `http://altengine:9191`, and pass any
  non-empty API key.
- **Container jobs answer 503.** The image has no Docker daemon. If you need the containers
  service, run `altengine dev` on the host.
- **Automation and static deploys are hosted-only**, as with the binary.

## Samples

Each sample is a complete `compose.yaml`. Copy it and run `docker compose up`.

### App + emulator

This is the usual setup. Data persists in a named volume, the console is on
`http://127.0.0.1:9191`, and the app waits until the emulator is healthy.

```yaml
services:
  altengine:
    image: ghcr.io/altlimit/altengine:latest
    ports:
      - "127.0.0.1:9191:9191"
    volumes:
      - altengine:/data

  app:
    build: .
    environment:
      ALTENGINE_URL: http://altengine:9191
      ALTENGINE_API_KEY: dev            # any non-empty key works against the emulator
    depends_on:
      altengine:
        condition: service_healthy

volumes:
  altengine:
```

In the app, nothing is specific to the emulator:

```ts
const ae = new AltEngine();             // reads ALTENGINE_URL and ALTENGINE_API_KEY
await ae.datastore("myapp").put("todos", [{ data: { title: "ship it" } }]);
```

`docker compose down` keeps the data. `docker compose down -v` deletes it.

### In-memory, for tests and CI

`--memory` keeps everything in RAM. The emulator starts empty, writes nothing to disk, and needs
no volume.

```yaml
services:
  altengine:
    image: ghcr.io/altlimit/altengine:0.2.39   # pin a version in CI
    command: [dev, --host, 0.0.0.0, --memory]
    ports:
      - "127.0.0.1:9191:9191"

  tests:
    build: .
    command: npm test
    environment:
      ALTENGINE_URL: http://altengine:9191
      ALTENGINE_API_KEY: dev
    depends_on:
      altengine:
        condition: service_healthy
```

```bash
docker compose run --rm tests && docker compose down
```

### Data in a project folder

To keep the data next to your code, where you can inspect or delete it, bind-mount a folder. The
container's user has to be able to write to the folder. Create the folder first, because Docker
creates a missing one owned by root, and run the container as your own user:

```bash
mkdir -p .altengine
```

```yaml
services:
  altengine:
    image: ghcr.io/altlimit/altengine:latest
    user: "1000:1000"                    # your `id -u`:`id -g`
    ports:
      - "127.0.0.1:9191:9191"
    volumes:
      - ./.altengine:/data
```

The folder holds signing secrets, so add it to your `.gitignore`. Inside the container its path is
`/data`, so the emulator doesn't write the `.gitignore` it puts in a host folder named
`.altengine`.

### Fresh on every start

`--reset --yes` empties the data directory each time the container starts. Unlike `--memory`,
the data stays on disk after the container stops, so you can inspect what a run left behind.

```yaml
services:
  altengine:
    image: ghcr.io/altlimit/altengine:latest
    command: [dev, --host, 0.0.0.0, --data, /data, --reset, --yes]
    ports:
      - "127.0.0.1:9191:9191"
    volumes:
      - altengine:/data

volumes:
  altengine:
```

`--reset` refuses a directory that does not hold the emulator's `control.json`, so it never
empties a folder that isn't a data directory.

### Deploy a manifest at startup

`--deploy` applies a [Deploy to AltEngine manifest](../cli/README.md#deploying-a-manifest-locally)
before the emulator starts serving. If the manifest has problems, the container exits non-zero.

```yaml
services:
  altengine:
    image: ghcr.io/altlimit/altengine:latest
    command:
      - dev
      - --host
      - 0.0.0.0
      - --data
      - /data
      - --deploy
      - /deploy/altengine.json
      - --var
      - board=acme
      - --secret
      - functions/RESEND_KEY=re_test_123
    ports:
      - "127.0.0.1:9191:9191"
      - "127.0.0.1:9192:9192"              # the manifest's first static site, if it has one
    volumes:
      - ./deploy:/deploy:ro                # altengine.json and the files it names
      - altengine:/data

volumes:
  altengine:
```

To redeploy after editing the manifest, run the deploy inside the container:

```bash
docker compose exec altengine /altengine dev deploy /deploy/altengine.json --var board=acme
```

Running `altengine dev deploy` from the host does not work against the container. The deploy
endpoint answers only requests that come from inside the container, and a request through a
published port comes from Docker's network instead.

A manifest's site URLs (`${key:url}`) are `http://127.0.0.1:<port>/`. Those are browser URLs, so
publish each site's port on the same port number on the host. The first site gets 9192, the next
9193, and so on.

### Serve a built site

`--static` serves a build directory on its own port, 9192, and reads it from disk on every
request. A rebuild therefore shows on reload, with no restart.

```yaml
services:
  altengine:
    image: ghcr.io/altlimit/altengine:latest
    command: [dev, --host, 0.0.0.0, --data, /data, --static, /site]
    ports:
      - "127.0.0.1:9191:9191"
      - "127.0.0.1:9192:9192"
    volumes:
      - ./dist:/site:ro
      - altengine:/data

volumes:
  altengine:
```

Add `--spa` to serve `index.html` for unmatched paths, if detection gets it wrong.

### Receive mail over SMTP

`--smtp` accepts mail and delivers it into local mailboxes. Inside the container, listen on all
interfaces. Then point your app's SMTP client at `altengine:2525`.

```yaml
services:
  altengine:
    image: ghcr.io/altlimit/altengine:latest
    command: [dev, --host, 0.0.0.0, --data, /data, --smtp, --smtp-addr, "0.0.0.0:2525"]
    environment:
      ALTENGINE_MAIL_DOMAIN: mail.localhost   # the domain every mailbox address is on
    ports:
      - "127.0.0.1:9191:9191"
      - "127.0.0.1:2525:2525"
    volumes:
      - altengine:/data

volumes:
  altengine:
```

Add `--smtp-auth none` to mark received mail as unauthenticated. The default, `pass`, lets a
mailbox in `reply` send mode answer it.

### Another service name

If the service isn't named `altengine`, list the name that other services use. For more than one
name, separate them with commas.

```yaml
services:
  emulator:
    image: ghcr.io/altlimit/altengine:latest
    environment:
      ALTENGINE_ALLOW_HOST: emulator,altengine.internal
    networks:
      default:
        aliases: [altengine.internal]
    ports:
      - "127.0.0.1:9191:9191"
```

Names are matched exactly: there is no wildcard, and `emulator.example.com` is not `emulator`.

### Everything at once

```yaml
services:
  altengine:
    image: ghcr.io/altlimit/altengine:0.2.39
    command:
      - dev
      - --host
      - 0.0.0.0
      - --data
      - /data
      - --deploy
      - /deploy/altengine.json
      - --static
      - /site
      - --smtp
      - --smtp-addr
      - 0.0.0.0:2525
      - --allow-origin
      - http://192.168.1.20:5173            # a phone or second machine on the LAN
    ports:
      - "127.0.0.1:9191:9191"
      - "127.0.0.1:9192:9192"
      - "127.0.0.1:2525:2525"
    volumes:
      - ./deploy:/deploy:ro
      - ./dist:/site:ro
      - altengine:/data
    restart: unless-stopped

  app:
    build: .
    environment:
      ALTENGINE_URL: http://altengine:9191
      ALTENGINE_API_KEY: dev
      SMTP_HOST: altengine
      SMTP_PORT: "2525"
    depends_on:
      altengine:
        condition: service_healthy

volumes:
  altengine:
```

## Reference

The image's default command is `dev --host 0.0.0.0 --data /data`. A `command:` replaces all of
it, so repeat `dev --host 0.0.0.0` when adding flags. That is the container's own interface. The
published port decides who can reach it.

| Flag | Env | Default in the image | |
|---|---|---|---|
| `--host` | | `0.0.0.0` | Interface to bind. Keep `0.0.0.0` in a container. |
| `--port` | | `9191` | API and console port. If you change it, change the healthcheck too (below). |
| `--data` | | `/data` | Data directory: SQLite files and `control.json`. |
| `--memory` | | off | Keep everything in RAM. `--data` is ignored. |
| `--reset --yes` | | off | Empty the data directory before starting. |
| `--allow-host` | `ALTENGINE_ALLOW_HOST` | `altengine` | Extra Host names to answer to, comma-separated. |
| `--allow-origin` | | | Extra browser origins for the API, beyond localhost. |
| `--static` | | | Serve this build directory as a site. |
| `--static-port` | | `9192` | That site's port. |
| `--deploy` | | | Apply this manifest at startup. Takes `--var`, `--secret`, `--bind` and `--name`. |
| `--smtp` | | off | Accept mail over SMTP. |
| `--smtp-addr` | | `127.0.0.1:2525` | Use `0.0.0.0:2525` in a container. |
| `--mail-domain` | `ALTENGINE_MAIL_DOMAIN` | `mail.localhost` | The domain every mailbox address is on. |

Run `docker run --rm ghcr.io/altlimit/altengine dev --help` for the full list.

**Healthcheck.** The image runs `altengine health` every 10 s. It probes `/healthz` on port 9191,
and it's what `condition: service_healthy` waits on. If you change `--port`, override the
healthcheck to match:

```yaml
    healthcheck:
      test: ["CMD", "/altengine", "health", "--port", "8080"]
```

**Ports.** 9191 is the API, console and `/mcp`. 9192 is the `--static` site, or a manifest's first
site. 2525 is SMTP, with `--smtp`.

**Other commands** run the same way: `docker compose exec altengine /altengine version`.

**Verify an image** with its signed build provenance:

```bash
gh attestation verify oci://ghcr.io/altlimit/altengine:0.2.39 --repo altlimit/altengine
```

**Build it yourself** from the repository root: `docker build -t altengine cli/`.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `403 this emulator only answers on localhost` | The Host the caller used isn't allowed. Set `ALTENGINE_ALLOW_HOST` to the service name or alias the caller uses. |
| `startup error: ... /data/...: permission denied` | A bind-mounted folder the container's user can't write to. Create the folder before starting, and add `user: "<uid>:<gid>"` (see [Data in a project folder](#data-in-a-project-folder)). Or use a named volume. |
| `the deploy endpoint only answers requests from this machine` | `altengine dev deploy` was run from the host. Use `docker compose exec altengine /altengine dev deploy ...`. |
| A browser can't open a blob upload URL or a channel `ws_endpoint` | These URLs are built from the Host the request used, so a request made to `altengine:9191` returns `altengine:9191` URLs, which only containers can resolve. Have the browser make that request itself, through `http://127.0.0.1:9191`, or replace the host in the URL before handing it to the browser. |
| Container jobs return 503 | The image has no Docker. Run `altengine dev` on the host for the containers service. |
| `refusing to reset /data: it has no control.json` | `/data` holds files the emulator did not write. Point `--data` somewhere else, or empty the volume (`docker compose down -v`). |
