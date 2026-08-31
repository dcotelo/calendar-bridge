package sync

import (
	"context"
	"fmt"
	"time"

	calendar "google.golang.org/api/calendar/v3"
)

// providerClient bridges any neutral Provider back into the Google-typed
// CalendarClient the existing Engine consumes. This is what lets a non-Google
// backend (Outlook, iCloud) drive the current engine unchanged: implement
// Provider, wrap it here, hand the result to sync.Account.Client.
//
// The engine only ever reads an event's ownership metadata and time span and
// writes owner-tagged blocks — exactly the fields the neutral Event carries —
// so mapping neutral Events into minimal *calendar.Event values is lossless
// for the engine's purposes.
type providerClient struct {
	p     Provider
	title string
}

// NewProviderClient adapts a neutral Provider into a CalendarClient. title is
// the busy-block title to stamp on inserts (the engine also sets it, but
// providers create the block, so they need it here).
func NewProviderClient(p Provider, title string) CalendarClient {
	return &providerClient{p: p, title: title}
}

// eventToGoogle maps a neutral Event back to a minimal *calendar.Event
// carrying only what the engine inspects: ID, times, cancelled status, the
// free/busy signals (transparency and the owner's invitation response), and
// ownership extended properties. Event content — summary, description,
// location, real attendee identities — is deliberately absent.
func eventToGoogle(e Event) *calendar.Event {
	ev := &calendar.Event{
		Id:    e.ID,
		Start: spanToGoogle(e.Start),
		End:   spanToGoogle(e.End),
	}
	if e.Cancelled {
		ev.Status = "cancelled"
	}
	// Round-trip the block title. Without it the engine sees every block as
	// untitled, decides the title has drifted, and rewrites it on every single
	// pass — an API write per block per poll interval, forever.
	ev.Summary = e.Title
	// Carry the free/busy signals across the bridge. Without these the engine
	// would treat every event coming through a Provider as opaque and
	// un-responded, and would block time for events the owner marked Free or
	// declined.
	if e.Transparent {
		ev.Transparency = "transparent"
	}
	if e.SelfResponse != "" {
		ev.Attendees = []*calendar.EventAttendee{{Self: true, ResponseStatus: e.SelfResponse}}
	}
	if e.Ownership != (Ownership{}) {
		ev.ExtendedProperties = &calendar.EventExtendedProperties{
			Private: map[string]string{
				ownerKey:          e.Ownership.Owner,
				sourceAccountKey:  e.Ownership.SourceAccount,
				sourceCalendarKey: e.Ownership.SourceCalendarID,
				sourceEventKey:    e.Ownership.SourceEventID,
			},
		}
	}
	return ev
}

// blockTitle picks the title to stamp on a block: whatever the engine set on
// the event it handed down, falling back to the title this bridge was
// constructed with. Preferring the engine's value means a block_title change
// takes effect through the bridge, not just on the direct Google path.
func (c *providerClient) blockTitle(ev *calendar.Event) string {
	if ev != nil && ev.Summary != "" {
		return ev.Summary
	}
	return c.title
}

func (c *providerClient) ListEvents(ctx context.Context, calendarID string, timeMin, timeMax time.Time) ([]*calendar.Event, error) {
	events, err := c.p.ListEvents(ctx, calendarID, timeMin, timeMax)
	if err != nil {
		return nil, err
	}
	out := make([]*calendar.Event, 0, len(events))
	for _, e := range events {
		out = append(out, eventToGoogle(e))
	}
	return out, nil
}

func (c *providerClient) FindBlockBySource(ctx context.Context, calendarID, srcAccount, srcEventID string) (*calendar.Event, error) {
	e, err := c.p.FindOwnedBlock(ctx, calendarID, srcAccount, srcEventID)
	if err != nil || e == nil {
		return nil, err
	}
	return eventToGoogle(*e), nil
}

func (c *providerClient) InsertEvent(ctx context.Context, calendarID string, ev *calendar.Event) (*calendar.Event, error) {
	if ev == nil {
		return nil, fmt.Errorf("providerClient: InsertEvent called with nil event")
	}
	own := ownershipFromGoogle(ev)
	// Never forward an untagged event to InsertBlock: that would create an
	// opaque busy block the engine can't later identify or GC. (googleProvider
	// re-checks too; this fails fast at the bridge.)
	if !own.validForWrite() {
		return nil, ErrNotOwned
	}
	created, err := c.p.InsertBlock(ctx, calendarID, c.blockTitle(ev), spanFromGoogle(ev.Start), spanFromGoogle(ev.End), own)
	if err != nil || created == nil {
		return nil, err
	}
	return eventToGoogle(*created), nil
}

func (c *providerClient) UpdateEvent(ctx context.Context, calendarID, eventID string, ev *calendar.Event, ifMatchETag string) (*calendar.Event, error) {
	// The neutral Provider carries no ETag: UpdateBlock does its own re-read
	// and issues the conditional write itself, exactly as DeleteBlock does.
	_ = ifMatchETag
	if ev == nil {
		return nil, fmt.Errorf("providerClient: UpdateEvent called with nil event")
	}
	block := eventFromGoogle(ev)
	// Refuse to update anything not provably ours (owner tag + source identity).
	if !block.Ownership.validForWrite() {
		return nil, ErrNotOwned
	}
	updated, err := c.p.UpdateBlock(ctx, calendarID, block, c.blockTitle(ev), spanFromGoogle(ev.Start), spanFromGoogle(ev.End))
	if err != nil || updated == nil {
		return nil, err
	}
	return eventToGoogle(*updated), nil
}

func (c *providerClient) DeleteEvent(ctx context.Context, calendarID, eventID, ifMatchETag string) error {
	// The neutral Provider.DeleteBlock performs its own read + ownership-checked
	// conditional delete, so the caller-supplied ifMatchETag isn't threaded
	// through here (a neutral Event carries no ETag).
	_ = ifMatchETag
	return c.p.DeleteBlock(ctx, calendarID, eventID)
}

// GetEvent is not supported through the Provider bridge: the neutral Provider
// interface has no get-by-ID operation (blocks are located by source identity
// via FindOwnedBlock). The engine never calls GetEvent on an account client —
// only googleProvider.DeleteBlock does, and it calls it on its own inner
// CalendarClient, not on this bridge. Returning an explicit error keeps the
// CalendarClient contract satisfied without pretending to support it.
func (c *providerClient) GetEvent(ctx context.Context, calendarID, eventID string) (*calendar.Event, error) {
	return nil, fmt.Errorf("providerClient: GetEvent is not supported through the Provider bridge")
}
