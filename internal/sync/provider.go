package sync

import (
	"context"
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

// Equal reports whether two spans denote the same instant/date and zone.
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
//     the given span, with the configured title.
//   - UpdateBlockTime moves an existing owned block (identified and carried
//     by block, which retains its ownership metadata) to a new span.
//     Implementations MUST preserve the block's ownership tagging and title;
//     for providers whose update is a full replace (e.g. Google), send the
//     complete block, not just the new times.
//   - DeleteBlock removes an owned block by ID.
type Provider interface {
	ListEvents(ctx context.Context, calendarID string, timeMin, timeMax time.Time) ([]Event, error)
	FindOwnedBlock(ctx context.Context, calendarID, srcAccount, srcEventID string) (*Event, error)
	InsertBlock(ctx context.Context, calendarID, title string, start, end TimeSpan, own Ownership) (*Event, error)
	UpdateBlockTime(ctx context.Context, calendarID string, block Event, start, end TimeSpan) (*Event, error)
	DeleteBlock(ctx context.Context, calendarID, blockID string) error
}
