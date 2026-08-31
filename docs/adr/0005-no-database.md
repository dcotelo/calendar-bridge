# 5. No database; the calendars are the state

**Status:** Accepted

## Context

A sync engine seems to want persistent state: which blocks exist, what they
mirror, what was seen last pass. The obvious implementation is SQLite.

## Decision

There is no database, and no local state of any kind beyond the config file and
the OAuth tokens. Everything the engine needs is read from the calendars at the
start of every pass, using the ownership tags from
[ADR 0002](0002-extended-properties-ownership.md).

Each pass is a full reconciliation: fetch current state, compute desired state,
make the difference.

## Consequences

**Good.**

- Nothing to back up, migrate, corrupt, or get out of sync with reality. There
  is no schema and no migration path to maintain across versions.
- Reality is authoritative. If someone deletes a block by hand, the next pass
  simply recreates it. There is no cache that can be wrong.
- Rollback is trivial — run the older binary. This is why
  [UPGRADING.md](../UPGRADING.md) has no migration section.
- Moving to a new machine means copying two files.
- Crash safety comes free: a partial pass leaves a partial but correct state,
  and the next pass finishes the job. No transactions, no recovery logic.
- Running two instances simultaneously is wasteful but not corrupting.

**Bad, and accepted.**

- Every pass re-fetches everything in the window. With sync tokens we would
  fetch only deltas. At the scale this tool operates (a handful of accounts, a
  30-day window) the difference is a few hundred kilobytes, and the simplicity
  is worth more.
- No history. "Why did that block disappear last Tuesday?" is answerable only
  from logs, if you kept them.
- No cross-pass memory, so per-account backoff state and similar refinements
  cannot persist across restarts.
- Counts shown in the web UI and in metrics are in-memory and reset on restart.

## Alternatives rejected

**SQLite for a block ledger.** The failure mode is the problem: if the ledger
disagrees with the calendars — because it was lost, restored from an old backup,
or written by a different version — the engine either orphans every block or
believes it owns events it does not. Making a second source of truth
authoritative over someone's calendar is a bad trade for saving a few API calls.

**A cache that is advisory only.** This is effectively what the in-memory block
index is: built fresh each pass from data just fetched, discarded at the end,
and falling back to an authoritative lookup on a miss. It gets the performance
benefit with none of the staleness risk.
