# 1. Polling is the primary sync mechanism; push is opt-in

**Status:** Accepted

## Context

Google Calendar offers two ways to learn that a calendar changed:

- **Polling** `events.list` on an interval. Needs nothing but outbound HTTPS.
- **Push** (`events.watch`). Google POSTs a notification to a callback URL.
  Needs a publicly reachable HTTPS endpoint with a valid certificate, and watch
  channels expire (a week at most, often less) so they must be renewed on a
  timer.

Push gives seconds of latency instead of minutes. Polling gives a deployment
that works on a laptop behind NAT with no domain, no certificate, and no inbound
firewall rule.

## Decision

Polling is the primary mechanism and is always on. Push is opt-in behind
`webhook.enabled`, and when enabled it **triggers the same `SyncOnce` pass**
that a poll tick does rather than being a second way to mutate calendars.

## Consequences

**Good.**

- The default deployment is a single binary with outbound HTTPS. That is the
  difference between "run it on your laptop" and "provision a domain and a
  certificate first", and it is what makes the tool approachable.
- Polling is the correctness backstop. A missed, delayed, or dropped push
  notification degrades latency; it can never mean a permanently-missed change.
- Push and poll share one code path and therefore one set of safety invariants.
  There is no second, divergent write path to reason about — a notification is
  only a nudge saying "reconcile now".
- A watch channel that fails to register logs and continues, because polling
  still covers that calendar.

**Bad, and accepted.**

- Default latency is up to `poll_interval` (5 minutes). Someone booking you at
  1:58pm for 2pm may find the block appears after the conflicting invite.
- Polling costs API calls whether anything changed or not. At the 5-minute
  default across three accounts that is roughly 864 calls a day, comfortably
  inside Google's free quota but not zero.
- The push path is extra code that most users never exercise, so it gets less
  real-world testing than the polling path.

## Alternatives rejected

**Push-only.** Halves the code, but makes a public HTTPS endpoint a hard
requirement for a tool whose whole pitch is "run it yourself, easily". It also
removes the backstop: a silently-expired channel would mean silently-stale
calendars.

**Sync tokens (incremental sync).** Google supports incremental `events.list`
via `syncToken`. It would cut bandwidth substantially. Rejected for now because
a sync token can be invalidated at any time, requiring a full-resync fallback
that must be correct — meaning we would carry both paths and the full path would
be the rarely-exercised one. Worth revisiting if API volume becomes a real
constraint.
