package sync

import (
	"context"
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
// carrying only what the engine inspects: ID, times, cancelled status, and
// ownership extended properties.
func eventToGoogle(e Event) *calendar.Event {
	ev := &calendar.Event{
		Id:    e.ID,
		Start: spanToGoogle(e.Start),
		End:   spanToGoogle(e.End),
	}
	if e.Cancelled {
		ev.Status = "cancelled"
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
	own := ownershipFromGoogle(ev)
	// Never forward an untagged event to InsertBlock: that would create an
	// opaque busy block the engine can't later identify or GC. (googleProvider
	// re-checks too; this fails fast at the bridge.)
	if !own.validForWrite() {
		return nil, ErrNotOwned
	}
	created, err := c.p.InsertBlock(ctx, calendarID, c.title, spanFromGoogle(ev.Start), spanFromGoogle(ev.End), own)
	if err != nil || created == nil {
		return nil, err
	}
	return eventToGoogle(*created), nil
}

func (c *providerClient) UpdateEvent(ctx context.Context, calendarID, eventID string, ev *calendar.Event) (*calendar.Event, error) {
	block := eventFromGoogle(ev)
	// Refuse to update anything not provably ours (owner tag + source identity).
	if !block.Ownership.validForWrite() {
		return nil, ErrNotOwned
	}
	updated, err := c.p.UpdateBlockTime(ctx, calendarID, block, spanFromGoogle(ev.Start), spanFromGoogle(ev.End))
	if err != nil || updated == nil {
		return nil, err
	}
	return eventToGoogle(*updated), nil
}

func (c *providerClient) DeleteEvent(ctx context.Context, calendarID, eventID string) error {
	return c.p.DeleteBlock(ctx, calendarID, eventID)
}
