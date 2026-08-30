package sync

import (
	"context"
	"errors"
	"time"
)

// This file defines the provider-neutral seam that lets calendar-bridge
// support backends other than Google (Outlook/Microsoft 365, iCloud/CalDAV)
// without the sync engine depending on any single provider's SDK types.
//
// Design:
//   - Event is a minimal, provider-neutral representation of the only two
//     things sync actually needs: an event's time span and its
//     calendar-bridge ownership metadata. Titles, attendees, descriptions,
//     and locations are deliberately absent — sync never reads or copies
//     them, which keeps the free/busy-only privacy guarantee structural
//     rather than merely conventional.
//   - Provider is the neutral port. A concrete provider (Google today,
//     Outlook/iCloud later) implements it by mapping its own SDK types
//     to/from Event.
//
// The existing CalendarClient interface (client.go) remains the seam the
// current Google-typed engine and its test fakes use. Provider is the
// forward-looking seam; googleProvider below is the first adapter, and
// providerClient bridges a Provider back into a CalendarClient so the
// existing Engine can drive any Provider unchanged.

// TimeSpan is a provider-neutral start/end pair. Exactly one of DateTime or
// Date is expected to be set per endpoint, mirroring how calendar systems
// distinguish timed events from all-day events.
type TimeSpan struct {
	// DateTime is an RFC3339 timestamp for a timed event endpoint. Empty for
	// all-day events.
	DateTime string
	// Date is a YYYY-MM-DD date for an all-day event endpoint. Empty for
	// timed events.
	Date string
	// TimeZone is the IANA zone name (e.g. "America/Sao_Paulo") the endpoint
	// is expressed in, when the provider supplies one.
	TimeZone string
}

// Equal reports whether two spans are field-for-field identical (same
// DateTime, Date, and TimeZone strings). It is a cheap structural comparison,
// not a semantic instant comparison: two RFC3339 values that denote the same
// instant with different UTC offsets are treated as unequal. That is
// intentional and sufficient here — calendar-bridge copies a source event's
// exact start/end/zone strings onto its block, so an unchanged source
// round-trips to byte-identical fields, and any real edit changes them.
func (s TimeSpan) Equal(o TimeSpan) bool {
	return s.DateTime == o.DateTime && s.Date == o.Date && s.TimeZone == o.TimeZone
}

// Ownership is the calendar-bridge tagging metadata carried by a block it
// created, used to (a) tell owned blocks apart from real user events and
// (b) match a block back to the source event it mirrors during GC.
type Ownership struct {
	// Owner is set to the calendar-bridge owner sentinel on blocks we
	// created; empty on real user events.
	Owner string
	// SourceAccount, SourceCalendarID, SourceEventID identify the real event
	// this block mirrors.
	SourceAccount    string
	SourceCalendarID string
	SourceEventID    string
}

// IsOwned reports whether this metadata marks a calendar-bridge-owned block.
func (o Ownership) IsOwned() bool { return o.Owner == ownerValue }

// ErrNotOwned is returned by provider write operations when they are asked to
// create, update, or delete a block whose ownership metadata is missing or
// incomplete. It exists to enforce, at every provider boundary, the invariant
// that calendar-bridge never writes or removes anything it cannot prove it
// owns — so a future non-Google Provider (or a direct caller bypassing the
// engine) cannot accidentally clobber a real user event.
var ErrNotOwned = errors.New("refusing to write a block that is not calendar-bridge-owned (missing owner tag or source identity)")

// validForWrite reports whether this ownership is safe to act on: it must carry
// the calendar-bridge owner tag AND a complete source identity (account +
// event ID). Anything less means we can neither prove ownership nor match the
// block back to a real source, so writing/deleting on it is unsafe.
func (o Ownership) validForWrite() bool {
	return o.IsOwned() && o.SourceAccount != "" && o.SourceEventID != ""
}

// Event is the provider-neutral event the sync engine reasons about.
type Event struct {
	// ID is the provider's opaque identifier for the event.
	ID string
	// Start and End are the event's time span.
	Start TimeSpan
	End   TimeSpan
	// Cancelled reports whether the provider marks this event as cancelled
	// (Google's "cancelled" status; equivalents on other providers).
	Cancelled bool
	// Transparent reports whether the provider marks this event as NOT
	// consuming the owner's time ("Free" in Google Calendar's UI,
	// transparency: "transparent"). A transparent event must not produce a
	// busy block elsewhere.
	Transparent bool
	// SelfResponse is the calendar owner's invitation response
	// ("accepted" / "declined" / "tentative" / "needsAction"), or "" when the
	// owner is not an attendee (a personal appointment). A declined
	// invitation must not produce a busy block elsewhere.
	SelfResponse string
	// Title is the busy block's title, and is populated ONLY for
	// calendar-bridge-owned blocks — where it is a string this tool chose, not
	// user content. It is deliberately left empty for real user events: a real
	// event's summary must never enter the neutral model, or the "event content
	// never crosses an account boundary" guarantee would stop being structural.
	//
	// The engine needs it so a change to block_title can be detected on blocks
	// that already exist.
	Title string
	// Ownership carries the calendar-bridge tagging metadata (empty Owner for
	// real events).
	Ownership Ownership
}

// Provider is the provider-neutral port the sync engine can be built on. A
// concrete backend (Google today; Outlook/Microsoft 365 and iCloud/CalDAV
// in future) implements it by translating its own API to these neutral
// operations.
//
// Semantics mirror the documented CalendarClient contract:
//   - ListEvents follows pagination to completion and returns events whose
//     time overlaps [timeMin, timeMax).
//   - FindOwnedBlock returns the single calendar-bridge-owned block mirroring
//     (srcAccount, srcEventID), or nil — implementations MUST verify true
//     ownership, not merely a property-filter match, to avoid ever returning
//     a real user event.
//   - InsertBlock creates a new owned busy block for the given source over
//     the given span, with the configured title. Implementations MUST reject
//     (with ErrNotOwned) an Ownership that is not owner-tagged or lacks a
//     source account/event ID — calendar-bridge must never create a busy block
//     it cannot later identify and clean up.
//   - UpdateBlock moves an existing owned block (identified and carried by
//     block, which retains its ownership metadata) to a new span and re-asserts
//     its title. Implementations MUST reject (with ErrNotOwned) a block that is
//     not owner-tagged with a complete source identity, and MUST preserve the
//     block's ownership tagging; for providers whose update is a full replace
//     (e.g. Google), send the complete block, not just the changed fields.
//   - DeleteBlock removes an owned block, identified by ID. The implementation
//     MUST re-read the target and reject an untagged (non-owned) event with
//     ErrNotOwned rather than deleting it — an untagged real event must never
//     be deletable through this path, regardless of caller discipline. A
//     target that no longer exists is treated as a successful no-op.
type Provider interface {
	ListEvents(ctx context.Context, calendarID string, timeMin, timeMax time.Time) ([]Event, error)
	FindOwnedBlock(ctx context.Context, calendarID, srcAccount, srcEventID string) (*Event, error)
	InsertBlock(ctx context.Context, calendarID, title string, start, end TimeSpan, own Ownership) (*Event, error)
	UpdateBlock(ctx context.Context, calendarID string, block Event, title string, start, end TimeSpan) (*Event, error)
	DeleteBlock(ctx context.Context, calendarID, blockID string) error
}
