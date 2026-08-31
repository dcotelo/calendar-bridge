# Deploying with Docker

Good for a NAS, a home server, or anywhere you already run containers.

The image is multi-arch (`linux/amd64` and `linux/arm64`), built from
`gcr.io/distroless/static-debian12:nonroot`, runs as UID 65532, and has no
shell.

---

## The most common first-run failure

Read this before anything else. It is the mistake essentially everyone makes,
and its error message does not point at the cause.

**Paths inside `config.yaml` are resolved against the container's working
directory (`/app`), not against `config.yaml`.**

So this config:

```yaml
token_file: secrets/personal-token.json     # WRONG in a container
```

makes the container look in `/app/secrets/personal-token.json` — even if you
mounted your secrets at `/app/config/secrets`. You get:

```
setting up: account personal: reading credentials file personal-credentials.json: no such file or directory
```

Note what the message does **not** tell you: the path it tried. These errors go
to `docker logs`, so they name only the file's base name and never the
directory — which means this error looks identical whether your path was
relative, absolute-but-wrong, or simply not mounted. You cannot diagnose this
one by reading the error more carefully. Check it from the host instead — the
image is distroless and has no shell, so there is nothing to `exec` into, and
the mounted directory *is* what the container sees:

```bash
# 1. What paths does the config name? They must start with /app/.
grep -E 'credentials_file|token_file' ~/calendar-bridge/config/config.yaml

# 2. Are the files actually in the directory you mounted?
ls -l ~/calendar-bridge/config/secrets/
```

If step 1 prints anything that does not start with `/app/`, that is the bug. If
step 1 looks right and step 2 is missing the file, the file never made it into
the mounted directory.

**Use absolute container paths:**

```yaml
token_file: /app/config/secrets/personal-token.json     # correct
```

There is a second, independent trap underneath it: the image runs as UID 65532,
and your token files are `0600` owned by *you*. Even at the right path, the
container cannot read them. Pass `--user "$(id -u):$(id -g)"`, or chown the
directory. Both are shown below.

---

## Prerequisites

- Docker.
- OAuth credentials per account, and **tokens created on a machine with a
  browser** — see the [quickstart](../../README.md#quickstart). The `auth` flow
  is interactive, so run it on your laptop and copy the tokens to the host.

---

## 1. Lay out the config directory

Everything lives in one directory that becomes a single mount.

```bash
mkdir -p ~/calendar-bridge/config/secrets
chmod 700 ~/calendar-bridge/config/secrets
cd ~/calendar-bridge
```

Copy in your credential JSONs, and the token files you created with
`calendar-bridge auth` on your laptop:

```bash
scp you@laptop:~/.config/calendar-bridge/secrets/*.json ~/calendar-bridge/config/secrets/
chmod 600 ~/calendar-bridge/config/secrets/*.json
```

Write `config/config.yaml` with **absolute container paths**:

```yaml
accounts:
  - name: personal
    credentials_file: /app/config/secrets/personal-credentials.json
    token_file: /app/config/secrets/personal-token.json
    calendar_id: primary
  - name: work-acme
    credentials_file: /app/config/secrets/work-acme-credentials.json
    token_file: /app/config/secrets/work-acme-token.json
    calendar_id: primary

poll_interval: 5m
lookahead_days: 30
block_title: "Busy (calendar-bridge)"

metrics:
  enabled: true
  # 0.0.0.0 inside the container; publish it only where your monitoring needs it.
  listen_addr: "0.0.0.0:9090"
```

```bash
chmod 600 config/config.yaml
```

## 2. Check it before daemonising

```bash
docker run --rm \
  --user "$(id -u):$(id -g)" \
  -v ~/calendar-bridge/config:/app/config \
  ghcr.io/dcotelo/calendar-bridge:latest \
  sync-once -config /app/config/config.yaml -dry-run
```

**You should now see** `sync.pass.start`, a `sync.account.fetched` line per
account, and a summary of what would change. Nothing has been written.

If you see the "no such file or directory" error above, your paths are relative.
If you see a permissions error, the `--user` flag is missing or wrong.

Then for real:

```bash
docker run --rm \
  --user "$(id -u):$(id -g)" \
  -v ~/calendar-bridge/config:/app/config \
  ghcr.io/dcotelo/calendar-bridge:latest \
  sync-once -config /app/config/config.yaml
```

**You should now see** `Busy (calendar-bridge)` blocks on your calendars. Check
before continuing.

## 3. Run it — docker compose (recommended)

Copy [`deploy/docker/docker-compose.yml`](../../deploy/docker/docker-compose.yml)
next to your `config/` directory, then:

```bash
CB_UID=$(id -u) CB_GID=$(id -g) docker compose up -d
docker compose logs -f
```

The compose file sets `restart: unless-stopped`, a read-only root filesystem,
`cap_drop: ALL`, `no-new-privileges`, memory and CPU limits, log rotation, and a
healthcheck.

### Or plain docker run

```bash
docker run -d \
  --name calendar-bridge \
  --restart unless-stopped \
  --user "$(id -u):$(id -g)" \
  --read-only \
  --tmpfs /tmp:size=16m \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --memory 128m \
  --cpus 0.5 \
  -v ~/calendar-bridge/config:/app/config \
  ghcr.io/dcotelo/calendar-bridge:latest
```

The default command is `run -config /app/config/config.yaml`, so there is
nothing to append.

## 4. Verify

```bash
docker logs calendar-bridge --tail 20
docker exec calendar-bridge /app/calendar-bridge version
```

With metrics published (`-p 127.0.0.1:9090:9090`):

```bash
curl -s http://127.0.0.1:9090/readyz
curl -s http://127.0.0.1:9090/metrics | grep last_success
```

---

## Why the config mount is not read-only

`:ro` is the instinct, and it is wrong here.

**Refreshed and rotated OAuth tokens are written back to disk.** Google issues a
new access token roughly hourly, and rotates the refresh token outright for apps
whose consent screen is in Testing status. A read-only mount means those never
persist: every restart spends an extra refresh round trip, and a rotated refresh
token is lost entirely, eventually breaking the account.

If you want defence in depth, mount the *credentials* read-only and the *tokens*
read-write as separate mounts. The root filesystem is already read-only.

## Verifying the image

```bash
cosign verify ghcr.io/dcotelo/calendar-bridge:latest \
  --certificate-identity-regexp 'https://github.com/dcotelo/calendar-bridge/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

docker buildx imagetools inspect ghcr.io/dcotelo/calendar-bridge:latest
```

## Building it yourself

```bash
docker build \
  --build-arg VERSION="$(git describe --tags --always)" \
  --build-arg COMMIT="$(git rev-parse --short HEAD)" \
  -t calendar-bridge:local .
```

---

## Upgrading

```bash
# 1. Note the running version, so you can go back.
docker exec calendar-bridge /app/calendar-bridge version

# 2. Pull.
docker compose pull      # or: docker pull ghcr.io/dcotelo/calendar-bridge:latest

# 3. Preview with the NEW image. This writes nothing.
docker run --rm --user "$(id -u):$(id -g)" \
  -v ~/calendar-bridge/config:/app/config \
  ghcr.io/dcotelo/calendar-bridge:latest \
  sync-once -config /app/config/config.yaml -dry-run

# 4. If the counts look right, restart.
docker compose up -d
```

Read [UPGRADING.md](../UPGRADING.md) first. Pin an exact tag rather than
`latest` if you would rather choose when to upgrade.

## Rolling back

```bash
docker compose down
# pin the previous version in docker-compose.yml, then:
docker compose up -d

# or with plain docker:
docker rm -f calendar-bridge
docker run -d --name calendar-bridge ... ghcr.io/dcotelo/calendar-bridge:0.1.0
```

There is no state in the container, so rolling back is just running the old
image. Everything lives in the mounted config directory and in your calendars.

## Uninstalling cleanly

```bash
# 1. Stop and remove.
docker compose down                       # or: docker rm -f calendar-bridge
docker rmi ghcr.io/dcotelo/calendar-bridge:latest

# 2. Revoke access, so the tokens are dead even if a copy survives.
#    https://myaccount.google.com/permissions

# 3. Remove the config and secrets.
rm -rf ~/calendar-bridge
```

**4. Remove the busy blocks it created.** Manual — a stopped container collects
nothing. delete only events carrying the private extended property
`calendarBridgeOwner=calendar-bridge`, which no human event has. **Do not delete
by `block_title`** — it is configurable, it is ordinary text on the event, and a
title search will also match real events someone created with the same name. See
[Removing it cleanly](README.md#removing-it-cleanly) for the API query.

---

## Failure modes specific to this target

| Symptom | Cause and fix |
|---|---|
| `no such file or directory` for a credentials file that exists | Relative paths in `config.yaml`. Use `/app/config/...`. |
| `permission denied` reading a token | The container UID cannot read your `0600` files. Add `--user "$(id -u):$(id -g)"`, or `chown -R 65532:65532 config/`. |
| Tokens never update; a refresh on every start | The config volume is mounted `:ro`. Remove it. |
| Works, then fails after a container recreate | Same as above — a rotated refresh token was never persisted. |
| `exec format error` | Wrong architecture. The image is multi-arch; if you built it yourself, pass `--platform`. |
| Container exits immediately, code 3 | A config error. `docker logs calendar-bridge`. |
| Container exits immediately, code 4 | An account needs authorizing. The `auth` flow needs a browser — run it on your laptop and copy the token in. |
| Cannot `docker exec ... sh` | The image is distroless and has no shell. That is deliberate. Use `docker logs`, and run the binary directly for one-off commands. |
| Times in logs are UTC | Set `TZ` in the environment. It affects log rendering only; sync is always computed in absolute time. |
