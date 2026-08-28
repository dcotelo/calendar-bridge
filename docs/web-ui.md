# Configuration web UI

calendar-bridge ships an optional local web UI for editing `config.yaml`
without hand-editing YAML: manage accounts, poll interval, lookahead window,
and busy-block title, view basic status, and trigger a manual sync.

It is **off by default** and designed to be a *local admin surface*, not a
hosted service — consistent with the project's whole reason to exist (your
calendar data and credentials never leave infrastructure you control).

## Security model (read this before exposing it)

The UI can rewrite `config.yaml`, so it is treated as privileged:

- **Loopback-only by default.** It binds `127.0.0.1:8090`. It **refuses to
  start** on a non-loopback address (e.g. `0.0.0.0`) unless an auth token is
  configured — so it can never be silently exposed to a network without
  authentication.
- **Bearer-token auth.** When `web_ui.auth_token` is set, every request must
  send `Authorization: Bearer <token>`, compared in **constant time**. This is
  mandatory to bind any non-loopback address.
- **Credentials never enter the browser.** The UI edits config *fields*,
  including the credential/token file *paths* — it never reads or serves the
  *contents* of those files. OAuth secrets stay on disk. The interactive OAuth
  flow stays in the CLI (`calendar-bridge auth`).
- **Safe writes.** Saving goes through the same validation the daemon uses and
  is written atomically at `0600`. An invalid edit (e.g. fewer than 2 accounts)
  is rejected and the existing file is left untouched.
- **Hardened responses.** The page is fully self-contained (no external assets)
  and served with a strict `Content-Security-Policy` and `X-Content-Type-Options: nosniff`.

Even with auth, exposing an admin UI to a network is inherently riskier than
keeping it on localhost. The recommended pattern is to leave it loopback-only
and reach it over an SSH tunnel:

```bash
ssh -L 8090:127.0.0.1:8090 you@your-server
# then open http://127.0.0.1:8090 locally
```

## Enabling it

```yaml
# config.yaml
web_ui:
  enabled: true
  listen_addr: "127.0.0.1:8090"   # default; loopback-only
  # auth_token: "<long random secret>"  # REQUIRED to bind a non-loopback addr
```

Run it:

```bash
calendar-bridge ui -config config.yaml
```

Then open `http://127.0.0.1:8090`.

If you set an `auth_token`, the browser must present it. The bundled page reads
the token from the URL fragment (which browsers never send to the server and
which stays out of server logs):

```text
http://your-host:8090/#token=YOUR_TOKEN
```

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| GET | `/` | The single-page UI (embedded in the binary) |
| GET | `/api/config` | Current config as JSON (`web_ui.auth_token` redacted) |
| PUT | `/api/config` | Validate + atomically save a new config |
| GET | `/api/status` | Runtime snapshot (account count, last sync) |
| POST | `/api/sync` | Trigger a one-off sync pass |

On `PUT`, sending an empty `web_ui.auth_token` means "keep the existing token"
— so editing config through the UI never accidentally wipes your auth token.

## Limitations

- The UI intentionally does **not** manage OAuth tokens or run the auth flow —
  use `calendar-bridge auth <account>` for that.
- Status is a minimal snapshot; richer metrics are tracked separately in the
  roadmap.
