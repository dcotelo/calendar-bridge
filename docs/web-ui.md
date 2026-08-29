# Configuration web UI

calendar-bridge ships an optional local web UI for editing `config.yaml`
without hand-editing YAML: manage accounts, poll interval, lookahead window,
and busy-block title, view basic status, and trigger a manual sync.

It is **off by default** and designed to be a *local admin surface*, not a
hosted service — consistent with the project's whole reason to exist (your
calendar data and credentials never leave infrastructure you control).

## Security model (read this before exposing it)

The UI can rewrite `config.yaml`, so it is treated as privileged:

- **Loopback-only.** It binds `127.0.0.1:8090` and **refuses to bind a
  non-loopback address** (e.g. `0.0.0.0`) because it serves plaintext HTTP —
  a non-loopback listener would send the token and config in the clear. To
  reach it from another host, use an SSH tunnel or a TLS-terminating reverse
  proxy pointed at the loopback port (see below).
- **Bearer-token auth on the API.** When `web_ui.auth_token` is set, the API
  endpoints (`/api/*`) require `Authorization: Bearer <token>`, compared in
  **constant time**. The index page (`GET /`) is intentionally public: a
  browser can't attach that header to a top-level navigation, so the page must
  load first and then collect the token for its API calls. The page contains no
  secrets. A token on a loopback bind is defense-in-depth (e.g. on a shared
  multi-user host).
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
  listen_addr: "127.0.0.1:8090"   # required to be loopback; non-loopback binds are refused
  # auth_token: "<long random secret>"  # optional; defense-in-depth behind a reverse proxy
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
