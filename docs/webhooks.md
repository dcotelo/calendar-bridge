# Push notifications (webhooks) — design

Status: scaffolding landed, opt-in via `webhook.enabled`. This documents how
the near-real-time path works, why it's shaped this way, and what an operator
must provide to run it.

## Goal

Reduce change-propagation latency from up to `poll_interval` (minutes) to
seconds, without changing the correctness model. Polling remains the safety
net; push is an accelerator, never a second, divergent mutation path.

## How Google Calendar push works

An app registers a **watch channel** on a calendar via `events.watch`,
supplying:

- a **public HTTPS callback URL** Google will POST to,
- an opaque **channel token** we generate and keep secret.

When anything on that calendar changes, Google POSTs to the callback URL. The
notification contains **no event data** — only headers:

| Header | Meaning |
|---|---|
| `X-Goog-Channel-ID` | the channel UUID we generated at watch time |
| `X-Goog-Channel-Token` | the token we supplied — used to authenticate the request |
| `X-Goog-Resource-ID` | opaque ID of the watched resource (needed to stop the channel) |
| `X-Goog-Resource-State` | `sync` (initial handshake), `exists`, or `not_exists` |
| `X-Goog-Message-Number` | monotonic per-channel counter |

Because the body is empty, a notification is only a **nudge**: "something
changed on this calendar; go reconcile." calendar-bridge responds by running
its normal `SyncOnce` pass — the identical reconcile it runs on a poll tick.
This is deliberate: push and poll share one code path and one set of safety
invariants (owned-block tagging, GC guards, healthy-account handling). Push
cannot create a divergent way to mutate calendars.

## Components (all in `internal/webhook`)

```
Google ──POST /webhook──▶ Receiver ──Notify()──▶ Debouncer ──C──▶ run loop ──▶ SyncOnce
                             ▲                                         │
                             │                                         │
                        (token auth)                            (poll ticker also
                                                                 fires SyncOnce)

Manager ──events.watch / channels.stop──▶ Google   (registers + renews channels)
```

- **`Receiver`** (`receiver.go`): an `http.Handler` that authenticates each
  POST by constant-time-comparing `X-Goog-Channel-Token` against the
  configured secret, drops the `sync` handshake, and calls `Notify()` for real
  changes. Rejects non-POST and bad tokens before any work.
- **`Debouncer`** (`receiver.go`): coalesces a burst of notifications into a
  single trigger on channel `C` (default window 5s), so a flurry of edits
  causes one reconcile, not dozens. The run loop selects on `C`.
- **`Manager`** (`manager.go`): registers one watch channel per
  (account, calendar) and renews each ahead of expiry (`renewSkew` before
  `Expiry`). If registration/renewal fails for one calendar, it logs and
  continues — polling covers that calendar in the meantime.
- **`ChannelWatcher`** (`manager.go`): the provider-neutral capability the
  Manager depends on. `googleWatcher` (`google_watcher.go`) implements it with
  `events.watch` / `channels.stop`. A future Outlook subscription API would
  implement the same interface.

## Run-loop integration (`cmd/calendar-bridge`)

When `webhook.enabled` is true, `run` starts the receiver HTTP server and the
Manager, then selects on **both** the debounced push trigger and the poll
ticker. Whichever fires first runs `SyncOnce`. On SIGINT/SIGTERM the server is
gracefully shut down and all channels are stopped best-effort.

`sync-once` is unaffected — push only makes sense for the long-running `run`.

## Security

- **Authentication.** Every notification is authenticated by the channel token
  (constant-time compare). An attacker who discovers the public URL but not the
  token gets `403` and triggers no work. The token is a credential: it lives in
  `config.yaml`, is gitignored, and is never logged.
- **No data exposure.** Notifications carry no event content, so even a
  forged-but-authenticated request can at worst trigger a reconcile — it can
  never inject or read event data.
- **HTTPS required.** Google refuses to deliver to non-HTTPS callback URLs;
  config validation rejects a non-`https://` `public_url` up front. In
  practice you terminate TLS at a reverse proxy that forwards to `listen_addr`.
- **DoS surface.** The receiver does minimal work and debounces, so a flood of
  authenticated notifications collapses into at most one sync per debounce
  window. `ReadHeaderTimeout` is set to bound slow-header attacks.

## What the operator must provide

1. A publicly reachable HTTPS endpoint routing to the receiver's `listen_addr`
   (e.g. a reverse proxy / load balancer terminating TLS at
   `https://cb.example.com` → `:8080`).
2. Config:

   ```yaml
   webhook:
     enabled: true
     public_url: https://cb.example.com   # must be https
     listen_addr: ":8080"
     verification_token: "<long random secret>"
     channel_ttl: "24h"
     debounce_interval: "5s"
   ```

3. The same OAuth scope already used (`CalendarEventsScope` covers
   `events.watch`).

## Deliberate limitations (current scaffolding)

- Channel state is in-memory: a restart re-registers channels (old ones expire
  on Google's side or are replaced on renew). Fine for a single long-running
  process; a future improvement is persisting channel IDs to stop them
  explicitly across restarts.
- No per-channel mapping from notification → specific account is used to scope
  the reconcile; any authenticated change triggers a full `SyncOnce`. This is
  simplest and safe (reconcile is idempotent); scoping is a latency
  optimization for later.
