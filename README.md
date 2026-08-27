# calendar-bridge

Self-hosted, open-source busy-time sync across multiple Google Calendar
accounts (personal Gmail + Google Workspace domains). Runs on your own
infrastructure. No third-party service ever sees your calendar data.

## Problem

Google Calendar has no native way to auto-block time across separate
accounts, and this gets worse across different Workspace domains (personal
Gmail + multiple company Workspaces). Tools like OneCal or Calendar Bridge
solve this, but route your calendar data through their own servers.

## What this does

- Watches N Google Calendar accounts you authenticate (OAuth2, tokens stay
  on your infra).
- When a real event appears/changes/is removed on one calendar, upserts a
  "Busy" placeholder block on the others.
- Tags every block it creates via Calendar API `extendedProperties` so it
  never confuses "real event" with "block it created" (no sync loops, no
  duplicate blocks).
- One-way busy-block propagation, not full two-way mirroring: your real
  event titles/details never leave the calendar they were created on.

## Status

Early scaffold — not yet functional. Built in the open.

## How it works

Each account is polled for events in a rolling window (default: 30 days
ahead). For every real event on one account, calendar-bridge ensures a
matching "Busy" block exists on every *other* configured account. Blocks it
creates are tagged via Calendar API `extendedProperties` so they're never
mistaken for real events (no sync loops), and they're garbage-collected
automatically once the source event is deleted or moved out of the window.

Only free/busy state propagates. Event titles, attendees, and descriptions
never leave the calendar they were created on — the block title is a fixed
string you configure (default: `Busy (calendar-bridge)`).

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

Edit `config.yaml` with one entry per account (minimum 2).

### 3. Authorize each account

```bash
go run ./cmd/calendar-bridge auth -config config.yaml -account personal
go run ./cmd/calendar-bridge auth -config config.yaml -account work-acme
```

Each run opens an authorization URL — sign in, approve, and paste back the
code. This writes the account's token file under `secrets/`.

### 4. Run

```bash
# one-off pass, useful to verify config before leaving it running
go run ./cmd/calendar-bridge sync-once -config config.yaml

# continuous loop, polling at the configured interval
go run ./cmd/calendar-bridge run -config config.yaml
```

## Deploying

### Fly.io (recommended, light lift)

```bash
fly launch --no-deploy
fly volumes create cb_config --size 1 -a <your-app-name>

# Get config.yaml + secrets/ (credentials + tokens) onto the volume.
# Easiest path: run a one-off machine with the volume mounted and scp/cat
# your local files in, e.g.:
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
single-replica `Deployment`. A `CronJob` running `sync-once` on a schedule
also works if you'd rather not keep a pod always running.


## License

MIT
