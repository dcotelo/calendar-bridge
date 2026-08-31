# Observability

calendar-bridge emits structured logs always, and optionally serves Prometheus
metrics plus liveness and readiness probes.

## What is and isn't exposed

Nothing here carries event **titles**, descriptions, locations, attendees,
calendar IDs, OAuth tokens, or credential file contents. That is a property of
the design rather than of the logging configuration: the neutral event model
the engine works with has no content fields at all — see the `Event` struct in
`internal/sync/provider.go`, which is the whole of what the engine can see.

The two surfaces differ, and the difference matters if you ship logs somewhere
you do not control:

| Surface | Carries |
|---|---|
| `/metrics` | Counts, timestamps, and account names — the short labels you wrote into `config.yaml`. No per-event data of any kind. |
| Structured logs | The above, **plus Google's opaque event ID** for the source event, in the `source` field. |

An event ID is not a title and reveals nothing about what the event *is*. It is
still stable, per-event metadata: anyone holding two sets of logs could use it
to tell that the same source event appears in both, and it identifies that
event to anyone who can also query the calendar. If your logs leave your
infrastructure, that is the field to be aware of. Metrics do not contain it.

---

## Structured logs

Logs go to stdout via `log/slog` in the standard text format. Every line
carries a stable `msg` event name, so you can build alerts on the vocabulary
rather than on free text.

| Event | When | Key fields |
|---|---|---|
| `sync.pass.start` | A pass begins | `accounts`, `window_start`, `window_end`, `dry_run` |
| `sync.account.fetched` | An account's events were listed | `account`, `real`, `owned_blocks`, `skipped_free_or_declined` |
| `sync.account.fetch_failed` | An account could not be listed | `account`, `error`, `consequence` |
| `sync.block.created` | A busy block was created | `account`, `source`, `dry_run` |
| `sync.block.updated` | A block's time or title was corrected | `account`, `source`, `dry_run` |
| `sync.block.deleted` | A stale block was removed | `account`, `block_id`, `source`, `dry_run` |
| `sync.pass.complete` | A pass finished | `created`, `updated`, `deleted`, `skipped`, `healthy_accounts`, `failed_accounts`, `ok` |

`source` is `"<account>|<event-id>"`. The event ID is Google's opaque
identifier, not the event's title.

To ship logs somewhere, run calendar-bridge under a supervisor that captures
stdout (systemd, Docker, Kubernetes all do this by default).

---

## Metrics

Off by default. Enable it in `config.yaml`:

```yaml
metrics:
  enabled: true
  listen_addr: "127.0.0.1:9090"  # bind loopback, or a private interface
  ready_max_age: "15m"           # default: 3x poll_interval; "0" disables the check
```

Then:

```bash
calendar-bridge run -config config.yaml
curl -s http://127.0.0.1:9090/metrics
```

The endpoint is **read-only and unauthenticated**, like any other exporter.
Bind it where only your monitoring can reach it: loopback with a local scraper,
a private interface, or a container network. Do not put it on a public address.

### Endpoints

| Path | Purpose | Status |
|---|---|---|
| `GET /metrics` | Prometheus text exposition format 0.0.4 | always 200 |
| `GET /healthz` | Liveness: the process is up and answering | always 200 |
| `GET /readyz` | Readiness: a sync has succeeded recently | 200, or 503 with a reason |

**`/healthz` deliberately ignores sync health.** An instance that cannot reach
Google should be left alone to keep retrying with backoff, not killed and
restarted by its orchestrator — a restart fixes nothing and loses the backoff
state. Wire your Kubernetes `livenessProbe` to `/healthz` and your
`readinessProbe` to `/readyz`.

### Series

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `calendar_bridge_build_info` | gauge | `version`, `commit`, `go_version` | Always 1; identifies the running binary |
| `calendar_bridge_sync_passes_total` | counter | `outcome` = `success` \| `failure` | Passes completed |
| `calendar_bridge_blocks_total` | counter | `action` = `created` \| `updated` \| `deleted` | Busy blocks written |
| `calendar_bridge_events_skipped_total` | counter | — | Source events not propagated because they were marked Free or declined |
| `calendar_bridge_sync_duration_seconds` | histogram | — | Pass duration |
| `calendar_bridge_account_healthy` | gauge | `account` | 1 if that account's events were fetched on the last pass, 0 otherwise |
| `calendar_bridge_account_fetch_errors_total` | counter | `account` | Passes in which that account could not be fetched |
| `calendar_bridge_last_success_timestamp_seconds` | gauge | — | Unix time of the last fully-successful pass; 0 if never |
| `calendar_bridge_last_pass_timestamp_seconds` | gauge | — | Unix time of the last pass, successful or not |
| `calendar_bridge_start_time_seconds` | gauge | — | Unix time the process started |

`last_success_timestamp_seconds` is exported as `0` before the first successful
pass, rather than being omitted, so a staleness alert fires on a brand-new
instance that has never worked instead of silently evaluating against a missing
series.

### Scrape config

```yaml
scrape_configs:
  - job_name: calendar-bridge
    static_configs:
      - targets: ["127.0.0.1:9090"]
```

---

## Alerting

The single most useful alert: **the last successful sync is older than three
poll intervals.** One missed pass is normal (a transient 429, a brief network
blip); three in a row is not.

```yaml
groups:
  - name: calendar-bridge
    rules:
      # Substitute your own poll_interval. These thresholds assume the 5m
      # default: 15m = 3 intervals, 60m = 12.
      - alert: CalendarBridgeSyncStale
        expr: time() - calendar_bridge_last_success_timestamp_seconds > 900
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "calendar-bridge has not completed a successful sync in over 15 minutes"
          description: >-
            Busy blocks are no longer being kept in step across accounts.
            Check calendar_bridge_account_healthy to see which account is
            failing, then the process logs for the underlying error. The usual
            cause is an expired or revoked OAuth token: re-run
            `calendar-bridge auth -account <name>`.

      - alert: CalendarBridgeSyncStaleCritical
        expr: time() - calendar_bridge_last_success_timestamp_seconds > 3600
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "calendar-bridge has not synced successfully in over an hour"

      - alert: CalendarBridgeAccountUnhealthy
        expr: calendar_bridge_account_healthy == 0
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "calendar-bridge cannot fetch events for {{ $labels.account }}"
          description: >-
            That account is excluded from every pass until it recovers: no
            blocks are pushed to it, and no blocks mirroring its events are
            garbage-collected elsewhere. Usually an expired token — re-run
            `calendar-bridge auth -account {{ $labels.account }}`.

      # A pass that suddenly deletes far more than usual is worth a look. It is
      # legitimate after a bulk calendar cleanup, but it is also what a
      # misconfiguration would look like.
      - alert: CalendarBridgeUnusualDeletionRate
        expr: increase(calendar_bridge_blocks_total{action="deleted"}[1h]) > 50
        labels:
          severity: info
        annotations:
          summary: "calendar-bridge removed an unusual number of busy blocks in the last hour"
```

### Useful queries

```promql
# Pass failure rate over the last hour.
rate(calendar_bridge_sync_passes_total{outcome="failure"}[1h])
  / rate(calendar_bridge_sync_passes_total[1h])

# 95th-percentile pass duration.
histogram_quantile(0.95, rate(calendar_bridge_sync_duration_seconds_bucket[1h]))

# How long ago the last successful sync was, in minutes.
(time() - calendar_bridge_last_success_timestamp_seconds) / 60

# Accounts currently failing.
calendar_bridge_account_healthy == 0
```

---

## Without Prometheus

If you don't run Prometheus, `/readyz` alone is a usable health signal for any
uptime checker, and `sync.pass.complete` with `ok=false` is the log line to
alert on. A minimal cron check:

```bash
curl -fsS http://127.0.0.1:9090/readyz > /dev/null \
  || echo "calendar-bridge is not syncing" | mail -s "calendar-bridge" you@example.com
```
