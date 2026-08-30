# Deploying on Fly.io

Always-on, no hardware, no home network exposure, and roughly **$2/month**. The
best option if you want it running while you sleep and do not have a machine
that is already on.

---

## Cost expectation

One `shared-cpu-1x` machine with 256 MB and a 1 GB volume. At the time of
writing that is around **$2/month** — the machine is a little under $2 and the
volume about $0.15. calendar-bridge is idle almost all the time.

Do not enable auto-stop. It is a background poller with no inbound HTTP, so
there is no request to wake it: a stopped machine simply stops syncing.

Check [current Fly pricing](https://fly.io/docs/about/pricing/) before relying
on those numbers.

---

## Prerequisites

- `flyctl` and a Fly account.
- OAuth credentials per account, and **tokens created locally** — the `auth`
  flow needs a browser, which a headless Fly machine does not have.

---

## 1. Authorize locally, first

This is the step people skip and then get stuck on.

```bash
mkdir -p secrets && chmod 700 secrets
# place your credential JSONs in secrets/, then:
calendar-bridge auth -config config.yaml -account personal
calendar-bridge auth -config config.yaml -account work-acme
```

You now have a token file per account, ready to upload.

## 2. Create the app and volume

```bash
fly launch --no-deploy      # adjust the app name and region when prompted
fly volumes create cb_config --size 1 --region <your-region> -a <your-app>
```

`fly.toml` mounts that volume at `/app/config`.

## 3. Write the config with absolute container paths

**This is the trap.** Paths inside `config.yaml` resolve against the container's
working directory (`/app`), not against `config.yaml`. A relative
`secrets/personal-token.json` means `/app/secrets/...` — not
`/app/config/secrets/...`, where you are about to put it.

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
  listen_addr: "127.0.0.1:9090"
```

Keep metrics on loopback: with no `[[services]]` block nothing is published
publicly, and you reach it over `fly ssh`.

## 4. Get the files onto the volume

The volume only exists while a machine is running, so deploy once first — it
will fail to find a config, which is expected.

```bash
fly deploy                                  # fails: no config yet. Fine.
fly ssh console -a <your-app> -C "mkdir -p /app/config/secrets"

fly ssh sftp shell -a <your-app>
# > put config.yaml /app/config/config.yaml
# > put secrets/personal-credentials.json /app/config/secrets/personal-credentials.json
# > put secrets/personal-token.json /app/config/secrets/personal-token.json
# > put secrets/work-acme-credentials.json /app/config/secrets/work-acme-credentials.json
# > put secrets/work-acme-token.json /app/config/secrets/work-acme-token.json
# > exit

fly ssh console -a <your-app> -C "chmod 600 /app/config/config.yaml /app/config/secrets/*.json"
```

### Why not `fly secrets`?

`fly secrets` sets environment variables, and calendar-bridge reads a config
file, not the environment. You could stage them in and write files at boot, but
that means an entrypoint script in a distroless image — more moving parts than a
volume. The volume is also what lets refreshed tokens persist, which is the more
important reason.

## 5. Deploy and verify

```bash
fly deploy
fly logs -a <your-app>
```

**You should now see** `sync.pass.start`, a `sync.account.fetched` line per
account, and `sync.pass.complete` with `ok=true`. Then check your calendars for
`Busy (calendar-bridge)` blocks.

```bash
fly ssh console -a <your-app> -C "/app/calendar-bridge version"
fly ssh console -a <your-app> -C "/app/calendar-bridge sync-once -config /app/config/config.yaml -dry-run"
```

To reach metrics:

```bash
fly proxy 9090:9090 -a <your-app>
curl -s http://127.0.0.1:9090/readyz
```

---

## fly.toml, key by key

```toml
app = "calendar-bridge"
```
Your app name. `fly launch` sets it.

```toml
primary_region = "iad"
```
Where the machine runs. Latency is irrelevant here — a sync pass takes seconds
and nothing waits on it — so pick whatever is cheapest or nearest you.

```toml
[build]
```
Empty, so Fly builds from the `Dockerfile` in the repo root. Pin a published
image instead if you would rather not build on deploy:
`image = "ghcr.io/dcotelo/calendar-bridge:v0.2.0"`.

```toml
[mounts]
  source = "cb_config"
  destination = "/app/config"
```
The volume holding `config.yaml` and `secrets/`. It must be writable —
calendar-bridge re-persists refreshed and rotated OAuth tokens. This is the main
reason to use a volume rather than baking files into the image.

```toml
[[vm]]
  size = "shared-cpu-1x"
  memory = "256mb"
```
The smallest machine. calendar-bridge is idle almost all the time and a pass
uses a few tens of megabytes.

**Deliberately absent: any `[[services]]` block.** calendar-bridge is a
background poller, not an HTTP service. With no services, Fly publishes no
ports and the machine is unreachable from the internet — which is what you
want. The metrics endpoint binds loopback and is reached over `fly proxy`.

**Also absent: `auto_stop_machines`.** There is no inbound request to wake a
stopped machine, so auto-stop just means it stops syncing.

---

## Upgrading

```bash
# 1. Note the running version.
fly ssh console -a <your-app> -C "/app/calendar-bridge version"

# 2. Preview against the live config — writes nothing.
fly ssh console -a <your-app> -C "/app/calendar-bridge sync-once -config /app/config/config.yaml -dry-run"

# 3. Deploy.
fly deploy

# 4. Verify.
fly logs -a <your-app>
```

Read [UPGRADING.md](../UPGRADING.md) first.

## Rolling back

```bash
fly releases -a <your-app>
fly deploy --image <the previous image reference>
```

The volume is untouched by a rollback, so config and tokens survive.

## Uninstalling cleanly

```bash
# 1. Stop it.
fly scale count 0 -a <your-app>

# 2. Revoke access: https://myaccount.google.com/permissions

# 3. Destroy the app and volume.
fly apps destroy <your-app>
fly volumes list -a <your-app>     # confirm the volume went with it
```

**4. Remove the busy blocks it created.** Manual. Search each calendar for your
`block_title` and delete the results. **Do this before destroying the app** if
you would rather calendar-bridge clean up after itself: remove one account from
`config.yaml`, run a pass so its blocks are collected elsewhere, repeat. Once
you are down to one account it will refuse to run, and the rest is manual.

---

## Failure modes specific to this target

| Symptom | Cause and fix |
|---|---|
| First deploy fails, no config | Expected. The volume is empty until you upload. Deploy, upload, deploy again. |
| `no such file or directory` for a credentials file | Relative paths in `config.yaml`. Use `/app/config/...`. |
| Files vanished after a deploy | They were in the image layer, not the volume. Anything outside `/app/config` is replaced on every deploy. |
| Machine keeps restarting | `fly logs`. Exit 3 is config, exit 4 is authorization. |
| Stopped syncing overnight | Auto-stop is enabled. Remove it — there is no request to wake it. |
| Cannot reach `/metrics` | It binds loopback by design. Use `fly proxy 9090:9090`. |
| Account breaks after a week | The OAuth consent screen is in **Testing** status, where refresh tokens expire after 7 days. Set it to **In production**. |
| `fly ssh sftp` cannot connect | The machine is not running. `fly status`, and start one. |
| Volume full | 1 GB is enormous for a few JSON files. If it filled, something else is writing — check the logs. |
