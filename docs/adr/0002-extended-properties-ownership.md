# 2. Ownership is tagged with private extended properties

**Status:** Accepted

## Context

The engine must answer one question about every event it sees: *did I create
this, or did a human?* Get it wrong in one direction and it deletes a real
meeting. Get it wrong in the other and blocks accumulate forever, or propagate
in a loop.

Options considered:

1. **A local database** mapping source events to created blocks.
2. **Match on the title** — treat anything titled `block_title` as ours.
3. **Private extended properties** on each created event.

## Decision

Every block calendar-bridge creates carries four private extended properties:

| Property | Value |
|---|---|
| `calendarBridgeOwner` | `calendar-bridge` |
| `calendarBridgeSourceAccount` | the source account's name |
| `calendarBridgeSourceCalendarID` | the source calendar |
| `calendarBridgeSourceEventID` | the source event's ID |

`calendarBridgeOwner` is the **only** signal used to classify an event as ours.
The other three exist to match a block back to the event it mirrors.

## Consequences

**Good.**

- The state lives with the data it describes. Nothing to back up, migrate, or
  get out of sync; deleting a calendar deletes its blocks' state with it.
- Recovery is automatic. Lose the machine, restore from the config alone, and
  the next pass sees exactly which blocks exist and which sources are live.
- Google's `extendedProperties.private` is per-copy and per-calendar, so an
  external organiser cannot forge the tag on your copy of an invitation. The
  guarantee is enforced by Google's own data model, not by our filtering.
- The property is queryable server-side (`privateExtendedProperty`), so finding
  a specific block does not mean listing a whole calendar.

**Bad, and accepted.**

- A user who **duplicates** a block in Google Calendar's UI gets a copy carrying
  the same properties, so two events claim the same source. Only one is
  time-updated, and garbage collection removes both when the source dies. Not
  dangerous, but surprising. Documented in
  [TROUBLESHOOTING.md](../TROUBLESHOOTING.md).
- Renaming an account orphans every block that names the old one. The next pass
  removes them and creates fresh ones — correct, but one noisy pass.
- Blocks outside the fetch window are invisible to garbage collection, so a
  block whose time has passed beyond the look-back buffer is left in place
  forever. Harmless (it is in the past) but it means "every block is eventually
  collected" is not true, and a clean-uninstall command cannot rely on GC alone.
- We depend on Google preserving arbitrary private properties across its own
  internal operations. It does, but it is a dependency.

## Why not the alternatives

**A local database** would be a second source of truth that can disagree with
reality. If it is lost or stale, calendar-bridge either orphans every block or —
much worse — has no way to know which events are safe to delete. It also turns a
stateless binary into something with backups and migrations.

**Title matching** is the dangerous one, and it is worth naming why: a user who
types "Busy (calendar-bridge)" as a real event title would have it deleted. So
would a block a colleague copied onto their own calendar. Ownership must not be
inferable from anything a human can type.

## Enforcement

Because everything rests on this one bit, it is checked at every layer rather
than trusted once: the Google client re-verifies ownership after the server-side
property filter, the provider refuses to insert or update anything without a
complete tag, and it re-reads and re-verifies the target immediately before every
delete, with the delete additionally conditional on the ETag. See the invariant
coverage matrix in [`QUALITY_AUDIT.md`](../../QUALITY_AUDIT.md).
