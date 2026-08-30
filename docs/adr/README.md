# Architecture decision records

Short records of decisions that shaped calendar-bridge and would otherwise be
re-litigated. Each states the context, the decision, and — most usefully — the
consequences we accepted.

Format: [MADR](https://adr.github.io/madr/), trimmed.

| # | Decision | Status |
|---|---|---|
| [0001](0001-polling-first.md) | Polling is the primary sync mechanism; push is opt-in | Accepted |
| [0002](0002-extended-properties-ownership.md) | Ownership is tagged with private extended properties | Accepted |
| [0003](0003-no-event-content-propagation.md) | Event content never crosses an account boundary | Accepted |
| [0004](0004-loopback-only-web-ui.md) | The web UI binds loopback only, unconditionally | Accepted |
| [0005](0005-no-database.md) | No database; the calendars are the state | Accepted |
| [0006](0006-provider-seam.md) | A provider-neutral seam, wired into production | Accepted |
