# calendar-bridge

[![CI](https://github.com/dcotelo/calendar-bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/dcotelo/calendar-bridge/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/dcotelo/calendar-bridge)](https://goreportcard.com/report/github.com/dcotelo/calendar-bridge)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Contributor Covenant](https://img.shields.io/badge/Contributor%20Covenant-2.1-4baaaa.svg)](CODE_OF_CONDUCT.md)

Self-hosted, open-source busy-time sync across multiple Google Calendar
accounts (personal Gmail + Google Workspace domains). Runs on your own
infrastructure. No third-party service ever sees your calendar data.

## The problem

Google Calendar has no native way to auto-block time across separate
accounts, and it's worse across different Workspace domains (personal Gmail
+ multiple company Workspaces). Hosted tools like OneCal or Calendar Bridge
solve this, but your event data flows through their servers to do it.
calendar-bridge does the same job entirely inside infrastructure you
control.

## What it does

- Polls N Google Calendar accounts you authenticate (OAuth2; tokens never
  leave your infra).
- When a real event appears, moves, or is removed on one calendar, upserts
  or deletes a matching "Busy" block on every other configured calendar.
- Tags every block it creates via Calendar API `extendedProperties` so it
  can always tell "a real event a human made" apart from "a block
  calendar-bridge made" — this is what prevents sync loops and accidental
  deletion of real events.
- Propagates free/busy state only, one-way per event. Titles, attendees,
  locations, and descriptions never leave the calendar an event was created
  on; the block's title is a fixed string you configure.

## What it does *not* do

- **Not** a full two-way mirror. Event content is never copied — only a
  generic "Busy" placeholder.
- **Not** real-time. It polls on an interval (default 5 minutes), it does
  not use Calendar API push notifications/webhooks (yet — see
  [Roadmap](#roadmap)).
- **Not** a replacement for genuine multi-calendar overlay views (Google's
  own "other calendars" sidebar already does that within one account).

## How it works

Each configured account is polled for events in a rolling window (default:
30 days ahead, plus a 24h look-back buffer for events already in progress).
Every pass:

1. **Fetch.** List events on every account, split into "real" events (made
   by a human) and "owned" blocks (previously created by calendar-bridge,
   identified by the `calendarBridgeOwner` extended property).
2. **Propagate.** For every real event on every account, ensure a matching
   busy block exists on every *other* account — create it if missing,
   update its time if the source event moved, leave it alone if already
   correct.
3. **Garbage collect.** Delete any owned block whose source event is no
   longer live (deleted, cancelled, or moved out of the sync window).

If one account's token has expired or its API call fails, that account is
excluded from the current pass (fetch, propagation, *and* GC) rather than
aborting the whole run — the other healthy accounts still get synced, and
the error is surfaced in the return value / logs so you can act on it.

```text
                 ┌──────────────┐
                 │   personal   │
                 │  (Gmail)     │
                 └──────┬───────┘
                         │ real events
              ┌──────────┼──────────┐
              ▼                     ▼
      ┌──────────────┐      ┌──────────────┐
      │  work-acme   │◄────►│  work-other  │
      │ (Workspace)  │      │ (Workspace)  │
      └──────────────┘      └──────────────┘
        Busy blocks flow in every direction; only free/busy
        state crosses account boundaries, never event content.
```

## Setup

### 1. Create OAuth2 credentials per Google account

For each account you want to sync (personal Gmail, each Workspace domain):

1. Create/select a project in [Google Cloud Console](https://console.cloud.google.com/).
2. Enable the **Google Calendar API**.
3. Create an OAuth 2.0 Client ID, application type **Desktop app**.
4. Download the credentials JSON.
5. If the app is in Testing mode, add that account as a test user under
   **APIs & Services → OAuth consent screen → Test users**.

### 2. Configure

```bash
cp config.example.yaml config.yaml
mkdir -p secrets
# place each downloaded credentials JSON under secrets/, matching the
# credentials_file paths in config.yaml
```

Edit `config.yaml` with one entry per account (minimum 2):

```yaml
accounts:
  - name: personal
    credentials_file: secrets/personal-credentials.json
    token_file: secrets/personal-token.json
    calendar_id: primary
  - name: work-acme
    credentials_file: secrets/work-acme-credentials.json
    token_file: secrets/work-acme-token.json
    calendar_id: primary

poll_interval: 5m
lookahead_days: 30
block_title: "Busy (calendar-bridge)"
```

### 3. Authorize each account

```bash
go run ./cmd/calendar-bridge auth -config config.yaml -account personal
go run ./cmd/calendar-bridge auth -config config.yaml -account work-acme
```

Each run prints an authorization URL. Open it, sign in, approve, then paste
back either the authorization **code** or the **full redirect URL** the
browser lands on (both are accepted — most browsers show something like
`http://localhost:1/?code=4/0A...&scope=...`, so you don't need to pick the
code out by hand). This writes the account's token file under `secrets/`.

### 4. Run

```bash
# one-off pass, useful to verify config before leaving it running
go run ./cmd/calendar-bridge sync-once -config config.yaml

# continuous loop, polling at the configured interval, exits cleanly on
# SIGINT/SIGTERM
go run ./cmd/calendar-bridge run -config config.yaml
```

## Deploying

### Fly.io (recommended, light lift)

```bash
fly launch --no-deploy
fly volumes create cb_config --size 1 -a <your-app-name>

# Get config.yaml + secrets/ (credentials + tokens) onto the volume.
# Easiest path: run a one-off machine with the volume mounted and sftp your
# local files in:
fly ssh console -a <your-app-name> -C "mkdir -p /app/config/secrets"
fly ssh sftp shell -a <your-app-name>
# > put config.yaml /app/config/config.yaml
# > put secrets/personal-credentials.json /app/config/secrets/personal-credentials.json
# > put secrets/personal-token.json /app/config/secrets/personal-token.json
# ... repeat per account

fly deploy
```

Run `auth` locally first (step 3 above) so token files already exist before
you upload them — the OAuth interactive flow needs a browser, which a
headless Fly machine doesn't have.

### Docker / self-hosted

```bash
docker build -t calendar-bridge .
docker run -v $(pwd)/config.yaml:/app/config/config.yaml:ro \
           -v $(pwd)/secrets:/app/config/secrets:ro \
           calendar-bridge
```

### Kubernetes

Mount `config.yaml` and `secrets/` from a `Secret` (not a `ConfigMap` —
token files are live credentials) at `/app/config`, and run the image as a
single-replica `Deployment`. The process handles SIGTERM by cancelling the
in-flight sync pass and exiting promptly rather than being hard-killed after
the grace period — but SIGTERM does cancel that pass, it does not let it
finish first, so a rolling update or node drain can still interrupt a sync
mid-flight (the next pass simply picks up where it left off). A `CronJob`
running `sync-once` on a schedule also works if you'd rather not keep a pod
always running.

## Known limitations

- **Polling, not push.** A change on one calendar can take up to
  `poll_interval` to propagate. Google Calendar API push notifications
  (webhooks) would make this near-instant but add real infrastructure
  requirements (a public HTTPS endpoint, channel renewal); tracked in
  [Roadmap](#roadmap).
- **No retry/backoff on transient API errors.** A single 429/5xx from the
  Calendar API fails that account for the current pass; it's picked up
  again next cycle. Fine at low poll frequency, but not ideal under load.
- **Refresh tokens aren't re-persisted.** Google rarely rotates the refresh
  token for installed-app OAuth flows, so this is low-risk in practice, but
  if it ever issues a new one, the on-disk token file goes stale until you
  re-run `auth`.
- **No interface abstraction over the Calendar API client** — resolved: `internal/sync.CalendarClient` is a small interface (list/find/insert/update/delete) that both the real Google-backed client and test fakes implement, so `SyncOnce` is covered end-to-end by fakes, not just its pure helper functions.

## Roadmap

- [ ] Google Calendar API push notifications (webhooks) as an alternative
      to polling.
- [ ] Retry with backoff on transient (429/5xx) API errors.
- [x] Fakeable Calendar API client interface for full `SyncOnce` unit
      tests without live credentials.
- [ ] Structured metrics (sync duration, blocks created/deleted per pass)
      for observability.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `account not yet authorized, run: calendar-bridge auth <account-name>` | Token file missing or unreadable — run the `auth` step for that account. |
| `sync failed: fewer than 2 healthy accounts` | Two or more accounts failed to fetch this pass (expired tokens, revoked access, network). Check logs for the per-account errors, re-run `auth` for any account whose token expired. |
| Busy blocks not appearing | Confirm `poll_interval` has actually elapsed since the source event was created, and that the source event falls within `lookahead_days`. |
| Duplicate/orphaned "Busy" blocks after uninstalling | calendar-bridge only garbage-collects blocks it can currently see and match to a live source event. If you stop running it, or delete `config.yaml`, existing blocks are left in place — delete them manually or search each calendar for events titled with your `block_title`. |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for dev setup, PR guidelines, and
where things live in the codebase. Issues and PRs welcome. CI runs
`go build`, `go vet`, `go test -race`, `gofmt`, `golangci-lint`, and
`govulncheck` on every PR; [CodeRabbit](https://coderabbit.ai) reviews
automatically. See `.coderabbit.yaml` for the review guidance applied to
this repo, especially the invariants called out for `internal/sync` and
`internal/googleauth` — those two packages are the security- and
correctness-critical ones. Please report security issues per
[SECURITY.md](SECURITY.md), not as a public issue.

## License

MIT
