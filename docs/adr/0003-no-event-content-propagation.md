# 3. Event content never crosses an account boundary

**Status:** Accepted

## Context

The obvious feature request is "show me *what* the conflict is, not just that
there is one". Copying the title would make the blocks far more useful.

It would also mean that a client meeting's title appears on a different
employer's calendar, that a medical appointment's title appears on a work
calendar, and that anyone with visibility of either calendar sees both.

## Decision

**No event content crosses an account boundary.** What does cross is a time
span, the fixed `block_title` string, and synchronization metadata.

The no-content part is enforced structurally, not by convention. The
provider-neutral `Event` type the engine reasons about has fields for an ID, a
start, an end, cancellation, transparency, the owner's invitation response, and
ownership metadata. It has **no fields at all** for a description, location, or
attendees, and no field carrying a *user's* title. There is nowhere for content
to flow.

`Event.Title` is the one text field, and it is deliberate and narrow: populated
only for calendar-bridge's *own* blocks, holding the operator's configured
`block_title` so the engine can notice that setting changing. It is left empty
for real user events, so a real summary cannot enter the model even by accident.

The metadata is the part worth being explicit about, because it is written onto
the block in the destination calendar as private extended properties:
`calendarBridgeOwner` (a constant), `calendarBridgeSourceAccount` (the name you
gave the account), `calendarBridgeSourceCalendarID` (which may be a secondary
calendar's `c_…@group.calendar.google.com` address), and
`calendarBridgeSourceEventID` (Google's opaque event ID).

Those three source fields are not incidental — they are what lets garbage
collection match a block back to its origin and delete only provably-dead ones.
Without them the tool could not safely delete anything. The cost is that a
reader of the destination calendar can see which source account and calendar a
block came from, though not what the event was.

## Consequences

**Good.**

- The privacy claim is a property of the type system rather than a promise. A
  future change that tried to propagate a title would have to add a field, which
  is a visible, reviewable act — not a one-line slip.
- Cross-tenant safety. A Workspace admin at one employer cannot learn anything
  about another employer's meetings beyond the fact that time is occupied.
- The blocks are boring, which is the point: a colleague sees "Busy", not "1:1
  with recruiter".

**Bad, and accepted.**

- You cannot tell from the block what the conflict is. You have to open the
  other calendar. This is a real usability cost, paid deliberately.
- Blocks are visually identical, so a day with four of them looks like four
  unexplained holes.
- The internal model cannot be reused for a full mirroring feature without
  changing this decision.

## Alternatives rejected

**A `propagate_titles` config option.** Tempting, and it would satisfy the
people who ask for it. Rejected because it turns a structural guarantee into a
runtime flag: the type would need content fields, so the guarantee would become
"we check a boolean before copying", and the failure mode of getting that check
wrong is disclosing a client meeting to a different employer. The asymmetry
between the benefit (convenience) and the cost (a confidentiality breach you
cannot undo) does not justify it.

**Propagating a redacted title** (e.g. first word only). All the risk of the
above with less of the benefit.

## Verification

A test creates a source event with a distinctive title, description, location,
attendees, and conferencing link, and asserts that none of those strings appear
anywhere on the resulting block — including across the provider seam. It is
written to fail loudly if this decision is ever quietly reversed.
