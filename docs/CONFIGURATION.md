# Configuration reference

calendar-bridge is configured by a single YAML file, chosen with `-config`
(default `config.yaml`). There are no environment-variable overrides: the file
is the only source of truth.

Start from [`config.example.yaml`](../config.example.yaml).

The file is validated on load and on every save. An invalid file is rejected
outright rather than partially applied — the daemon will not start, and the web
UI will not overwrite a good file with a bad one.

**Permissions.** The config references credential and token paths and may
contain the web UI auth token and the webhook verification token. When
calendar-bridge writes it (through the web UI) it writes `0600`. Keep it that
way if you edit it by hand.

---

## Quick reference

| Key | Type | Default | Required |
|---|---|---|---|
| [`accounts[].name`](#accountsname) | string | — | yes |
| [`accounts[].credentials_file`](#accountscredentials_file) | path | — | yes |
| [`accounts[].token_file`](#accountstoken_file) | path | — | yes |
| [`accounts[].calendar_id`](#accountscalendar_id) | string | — | yes |
| [`poll_interval`](#poll_interval) | duration | `5m` | no |
| [`lookahead_days`](#lookahead_days) | integer | `30` | no |
| [`block_title`](#block_title) | string | `Busy (calendar-bridge)` | no |
| [`webhook.enabled`](#webhookenabled) | bool | `false` | no |
| [`webhook.public_url`](#webhookpublic_url) | URL | — | if webhook enabled |
| [`webhook.listen_addr`](#webhooklisten_addr) | host:port | `:8080` | no |
| [`webhook.verification_token`](#webhookverification_token) | secret | — | if webhook enabled |
| [`webhook.channel_ttl`](#webhookchannel_ttl) | duration | `24h` | no |
| [`webhook.debounce_interval`](#webhookdebounce_interval) | duration | `5s` | no |
| [`web_ui.enabled`](#web_uienabled) | bool | `false` | no |
| [`web_ui.listen_addr`](#web_uilisten_addr) | host:port | `127.0.0.1:8090` | no |
| [`web_ui.auth_token`](#web_uiauth_token) | secret | — | no |
| [`metrics.enabled`](#metricsenabled) | bool | `false` | no |
| [`metrics.listen_addr`](#metricslisten_addr) | host:port | `127.0.0.1:9090` | no |
| [`metrics.ready_max_age`](#metricsready_max_age) | duration | 3 × `poll_interval` | no |

Durations are Go duration strings: `30s`, `5m`, `1h30m`. A bare number is not a
duration and is rejected.

---

## accounts

A list of the Google accounts to sync between. **At least two are required** —
with one account there is nothing to bridge, and calendar-bridge refuses to
start rather than run a no-op.

There is no upper limit, but note the cost model: each pass makes one
`events.list` call per account, and propagation is between every pair, so work
grows with the square of the account count. Three to five accounts is the
comfortable range.

```yaml
accounts:
  - name: personal
    credentials_file: /etc/calendar-bridge/secrets/personal-credentials.json
    token_file: /etc/calendar-bridge/secrets/personal-token.json
    calendar_id: primary
  - name: work-acme
    credentials_file: /etc/calendar-bridge/secrets/work-acme-credentials.json
    token_file: /etc/calendar-bridge/secrets/work-acme-token.json
    calendar_id: primary
```

### accounts[].name

A short identifier for this account. It appears in logs, in metrics labels, in
the web UI, and — importantly — **inside every busy block calendar-bridge
creates**, as the `calendarBridgeSourceAccount` extended property.

Must be unique across accounts. Not an email address, and it should not be one:
it is written into event metadata on your other calendars.

Renaming an account is not free. Existing blocks carry the old name, so after a
rename the old blocks are orphaned: garbage collection cannot match them to a
live source, and the next pass removes them and creates fresh ones. That is
correct but churny — expect one noisy pass.

```yaml
name: work-acme
```

### accounts[].credentials_file

Path to the OAuth 2.0 **client credentials** JSON downloaded from Google Cloud
Console (application type: Desktop app). This identifies the *application*, not
the user.

**Relative paths are resolved against the process's working directory, not
against the config file.** This is the single most common deployment mistake:
in a container the working directory is `/app`, so `secrets/token.json` means
`/app/secrets/token.json` regardless of where `config.yaml` lives. Use absolute
paths anywhere the working directory isn't obvious.

calendar-bridge warns at startup if the file is group- or world-readable. It
does not refuse to start.

### accounts[].token_file

Path to this account's OAuth token, written by `calendar-bridge auth` and
refreshed automatically thereafter. One file per account; they are not
interchangeable.

**This file is a live credential.** It is written `0600`, atomically, and
rewritten whenever the token is refreshed or rotated. Anyone who can read it
can read and write that account's calendar until the grant is revoked.

The same relative-path caveat as `credentials_file` applies. The directory must
be writable by the process, since the token is re-persisted on refresh.

### accounts[].calendar_id

Which calendar on this account to read and write. Use `primary` for the
account's default calendar — that is what almost everyone wants.

A secondary calendar's ID looks like
`c_1a2b3c4d5e6f7g8h9i@group.calendar.google.com` and can be found in Google
Calendar under *Settings → your calendar → Integrate calendar → Calendar ID*.

calendar-bridge reads real events from this calendar and writes its busy blocks
to it. It only ever touches this one calendar per account, never the account's
other calendars.

---

## poll_interval

How long to wait between sync passes.

```yaml
poll_interval: 5m
```

Must be positive. `0s` and negative values are rejected — they would spin a
tight loop against the Calendar API.

This is your propagation latency: a change made just after a pass takes up to
one interval to appear elsewhere. Shorter is not automatically better; each
pass costs one API call per account plus a write per changed block, and Google
enforces per-project quotas. `5m` is a sensible default. Below `1m` you are
mostly generating load. With [webhooks](webhooks.md) enabled, changes propagate
in seconds and polling stays on only as a safety net, so a longer interval
(`15m`, `30m`) is reasonable.

The clock starts when a pass *finishes*, so a slow pass does not overlap the
next one.

## lookahead_days

How far into the future to sync.

```yaml
lookahead_days: 30
```

Must not be negative. `0` means "only the look-back window", which is almost
certainly not what you want.

The fetch window is always `[now − 24h, now + lookahead_days]`. The fixed 24h
look-back exists so an event already in progress when a pass runs is still
seen, and still has a block.

Larger values mean more events per pass and more blocks. Note the boundary
behaviour: an event beyond the horizon has no block, and gains one as the
window rolls forward to reach it. An event pushed past the horizon has its
block removed. Both are correct; both mean a busy period further out than
`lookahead_days` is simply not represented yet.

## block_title

The title of every busy block calendar-bridge creates.

```yaml
block_title: "Busy (calendar-bridge)"
```

This is the **only** text that crosses an account boundary. The source event's
own title, description, location and attendees are never copied — that is
structural, not a setting (see [ARCHITECTURE.md](ARCHITECTURE.md)).

Pick something recognisable, so a colleague looking at your calendar
understands the block is a placeholder rather than a real meeting, and so you
can find every block if you ever want to remove them by hand.

Changing it re-titles the blocks that already exist on the next pass, not just
new ones. Expect one pass with a write per existing block.

---

## webhook

Opt-in Google Calendar push notifications, for near-real-time propagation
instead of waiting out `poll_interval`. Polling continues regardless, as a
safety net against a missed or late notification.

Push needs a publicly reachable HTTPS endpoint and channel renewal, which is
real infrastructure a polling-only deployment does not need. Read
[webhooks.md](webhooks.md) before enabling it.

### webhook.enabled

```yaml
webhook:
  enabled: true
```

When `false` (the default) every other key in this block is ignored.

### webhook.public_url

The externally reachable base URL Google will POST notifications to. The
receiver is served at this URL plus `/webhook`.

```yaml
public_url: https://cb.example.com
```

Validated on load: must be `https` (Google refuses plaintext callbacks), must
have a host, and must not contain credentials, a query string, or a fragment.
Only its scheme and host are ever logged, never the full URL.

### webhook.listen_addr

The local address the receiver binds. Typically a loopback address behind a
TLS-terminating reverse proxy on the same host.

```yaml
listen_addr: "127.0.0.1:8080"
```

Prefer loopback so the plaintext receiver cannot be reached directly, bypassing
the proxy's TLS. Bind wider only when the proxy runs elsewhere and network-level
isolation covers the port instead.

### webhook.verification_token

A shared secret Google echoes back in the `X-Goog-Channel-Token` header of every
notification. The receiver compares it in constant time and rejects mismatches
with 403 before doing any work.

```yaml
verification_token: "<long random string>"
```

**This is a credential.** Generate it with `openssl rand -base64 32`. Without
it, anyone who discovers your public URL can force syncs at will. The receiver
never logs it. The web UI redacts it on read and preserves it on write.

### webhook.channel_ttl

How long each watch channel lives before renewal.

```yaml
channel_ttl: "24h"
```

Must be positive. Google caps the maximum (about a week) and may return a
shorter expiry than requested; calendar-bridge uses whatever Google returns and
renews ahead of it. Leave it unset to get the `24h` default.

### webhook.debounce_interval

Coalesces a burst of notifications into a single sync.

```yaml
debounce_interval: "5s"
```

Dragging one event around a calendar can generate a dozen notifications in a
few seconds. Without debouncing that is a dozen sync passes.

---

## web_ui

The optional local configuration UI. See [web-ui.md](web-ui.md) for the full
security model.

### web_ui.enabled

```yaml
web_ui:
  enabled: true
```

Off by default. The `ui` subcommand refuses to start when this is false.

Note: saving from the web UI can never set this back to `false` — an omitted
value means "unchanged", so the UI cannot lock you out of itself. Turn it off
by editing the file.

### web_ui.listen_addr

```yaml
listen_addr: "127.0.0.1:8090"
```

**Must be a loopback address.** A non-loopback bind (`0.0.0.0:8090`, a LAN IP)
is refused unconditionally, auth token or not: the UI serves plaintext HTTP, so
a non-loopback listener would put the auth token and your config on the wire in
the clear. Reach it from elsewhere over an SSH tunnel or a TLS-terminating
reverse proxy pointed at the loopback port.

### web_ui.auth_token

```yaml
auth_token: "<long random string>"
```

Optional. When set, every `/api/*` request must present it as
`Authorization: Bearer <token>`, compared in constant time. The page itself
(`GET /`) stays unauthenticated — a browser cannot attach that header to a
top-level navigation — and carries no secrets.

Worth setting on a shared multi-user host, where "loopback only" does not mean
"only you", and behind a reverse proxy. It does not permit a non-loopback bind.

Treat it as a credential. The web UI redacts it on read and preserves it when a
save omits it.

---

## metrics

Optional Prometheus metrics and health probes. See
[OBSERVABILITY.md](OBSERVABILITY.md) for the full series list and alerting
rules.

### metrics.enabled

```yaml
metrics:
  enabled: true
```

Off by default. When false, nothing is served and no counters are kept.

### metrics.listen_addr

```yaml
listen_addr: "127.0.0.1:9090"
```

Serves `/metrics`, `/healthz` and `/readyz`. **Read-only and
unauthenticated**, like any other exporter. It exposes counts, timestamps and
account names — never event data or credentials — but bind it where only your
monitoring can reach it: loopback, a private interface, or a container network.

Unlike `web_ui.listen_addr`, a non-loopback bind is permitted here, because a
metrics endpoint on a private network is a normal deployment and there is no
credential to leak. Do not put it on a public address.

### metrics.ready_max_age

How stale the last successful sync may be before `/readyz` reports not-ready.

```yaml
ready_max_age: "15m"
```

Defaults to three times `poll_interval`: long enough that one missed pass or a
transient API error doesn't flap readiness, short enough to catch a genuinely
wedged instance. Set `"0"` to disable the staleness check, leaving `/readyz` as
a pure liveness signal. Negative values are rejected.

`/healthz` deliberately ignores sync health — see
[OBSERVABILITY.md](OBSERVABILITY.md#endpoints) for why.

---

## A complete example

```yaml
accounts:
  - name: personal
    credentials_file: /etc/calendar-bridge/secrets/personal-credentials.json
    token_file: /etc/calendar-bridge/secrets/personal-token.json
    calendar_id: primary
  - name: work-acme
    credentials_file: /etc/calendar-bridge/secrets/work-acme-credentials.json
    token_file: /etc/calendar-bridge/secrets/work-acme-token.json
    calendar_id: primary
  - name: work-globex
    credentials_file: /etc/calendar-bridge/secrets/work-globex-credentials.json
    token_file: /etc/calendar-bridge/secrets/work-globex-token.json
    calendar_id: primary

poll_interval: 5m
lookahead_days: 30
block_title: "Busy (calendar-bridge)"

metrics:
  enabled: true
  listen_addr: "127.0.0.1:9090"

# web_ui:
#   enabled: true
#   listen_addr: "127.0.0.1:8090"

# webhook:
#   enabled: true
#   public_url: https://cb.example.com
#   listen_addr: "127.0.0.1:8080"
#   verification_token: "<long random string>"
#   channel_ttl: "24h"
#   debounce_interval: "5s"
```

Check a config without running a full pass:

```bash
calendar-bridge sync-once -config config.yaml -dry-run
```

That validates the file, authenticates every account, fetches events, and
reports exactly what it *would* change — without writing to any calendar.
