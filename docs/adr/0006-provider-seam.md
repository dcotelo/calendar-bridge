# 6. A provider-neutral seam, wired into production

**Status:** Accepted

## Context

calendar-bridge only speaks to Google. Outlook and CalDAV are the obvious next
backends, and the request comes up.

The engine was originally written against Google's generated API types, which
would mean rewriting it to add a second backend.

Separately: the ownership rules that make deletion safe need a single place to
live. Scattered through the engine, each new call site is a chance to forget one.

## Decision

A provider-neutral `Provider` port defines the operations the engine needs in
terms of a neutral `Event` type carrying no vendor types. `googleProvider`
implements it over the Google client, and `providerClient` bridges any
`Provider` back into the interface the engine consumes.

Crucially, **this seam is on the production path**, not an aspirational
abstraction. Every account's client is:

```text
providerClient → googleProvider → retryingClient → googleCalendarClient
```

## Consequences

**Good.**

- The ownership rules live in one place — `googleProvider` — and every write
  goes through them. It refuses to insert or update anything without a complete
  ownership tag, and re-reads and re-verifies the target before every delete.
  A new call site cannot forget the check, because the check is below it.
- A new backend implements one interface and inherits the engine, the retry
  behaviour, and the ownership enforcement.
- The neutral `Event` is where [ADR 0003](0003-no-event-content-propagation.md)
  is enforced: it has no content fields, so no backend can propagate content
  even by accident.
- Retry sits *below* the provider, so each individual call the provider makes —
  the pre-delete read as well as the delete — gets its own backoff, rather than
  a retry replaying a whole check-and-write sequence.

**Bad, and accepted.**

- Two adapters and a bridge for one backend is more indirection than one backend
  needs. The cost is a real reading tax on anyone tracing a call.
- The engine is still written in Google's types internally, so the bridge
  converts in both directions on every pass. Measurably cheap, conceptually
  awkward.
- The neutral model is shaped by what Google exposes, so the first non-Google
  provider will probably reveal a wrong assumption.

## History worth recording

The seam existed for some time **without being wired into production** — the
engine talked to the Google client directly, and roughly 370 lines of
ownership-enforcing code executed only under test, while production deletes
relied on the list ETag alone. It looked correct to anyone reading
`google_provider.go`.

Wiring it in immediately surfaced a real bug: the bridge did not round-trip the
block title, so the engine saw every block as untitled, decided the title had
drifted, and would have rewritten every block on every pass forever. That is the
argument for this ADR's central point — **an abstraction that production does not
use is not tested, whatever its test coverage says.**

The invariant tests now run twice: once against the fake directly, and once
through the exact production stack.
