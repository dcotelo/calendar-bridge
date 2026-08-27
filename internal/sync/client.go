package sync

import (
	"context"
	"fmt"
	"time"

	calendar "google.golang.org/api/calendar/v3"
)

// CalendarClient is the minimal subset of the Google Calendar API that the
// sync engine needs. It exists so tests can substitute a fake in-memory
// implementation instead of hitting the real API — the generated
// google.golang.org/api/calendar/v3 client returns concrete builder-chain
// types (e.g. *calendar.EventsListCall) that aren't practical to mock
// directly.
type CalendarClient interface {
	// ListEvents returns every (non-deleted-by-the-API) event on
	// calendarID whose time falls in [timeMin, timeMax), following
	// pagination to completion.
	ListEvents(ctx context.Context, calendarID string, timeMin, timeMax time.Time) ([]*calendar.Event, error)

	// FindBlockBySource returns the single event on calendarID tagged
	// with the given source account/event via the sourceAccountKey and
	// sourceEventKey private extended properties, or nil if none exists.
	FindBlockBySource(ctx context.Context, calendarID, srcAccount, srcEventID string) (*calendar.Event, error)

	// InsertEvent creates ev on calendarID.
	InsertEvent(ctx context.Context, calendarID string, ev *calendar.Event) (*calendar.Event, error)

	// UpdateEvent updates the event with the given ID on calendarID.
	UpdateEvent(ctx context.Context, calendarID, eventID string, ev *calendar.Event) (*calendar.Event, error)

	// DeleteEvent deletes the event with the given ID on calendarID.
	DeleteEvent(ctx context.Context, calendarID, eventID string) error
}

// googleCalendarClient adapts a real *calendar.Service to the CalendarClient
// interface used by the sync engine.
type googleCalendarClient struct {
	svc *calendar.Service
}

// NewGoogleCalendarClient wraps an authenticated *calendar.Service (see
// internal/googleauth) as a CalendarClient.
func NewGoogleCalendarClient(svc *calendar.Service) CalendarClient {
	return &googleCalendarClient{svc: svc}
}

func (c *googleCalendarClient) ListEvents(ctx context.Context, calendarID string, timeMin, timeMax time.Time) ([]*calendar.Event, error) {
	var all []*calendar.Event
	pageToken := ""
	for {
		call := c.svc.Events.List(calendarID).
			TimeMin(timeMin.Format(time.RFC3339)).
			TimeMax(timeMax.Format(time.RFC3339)).
			SingleEvents(true).
			MaxResults(2500)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		res, err := call.Context(ctx).Do()
		if err != nil {
			return nil, err
		}
		all = append(all, res.Items...)
		if res.NextPageToken == "" {
			break
		}
		pageToken = res.NextPageToken
	}
	return all, nil
}

func (c *googleCalendarClient) FindBlockBySource(ctx context.Context, calendarID, srcAccount, srcEventID string) (*calendar.Event, error) {
	call := c.svc.Events.List(calendarID).
		PrivateExtendedProperty(sourceAccountKey + "=" + srcAccount).
		PrivateExtendedProperty(sourceEventKey + "=" + srcEventID)

	res, err := call.Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("querying existing block (source=%s/%s): %w", srcAccount, srcEventID, err)
	}
	for _, ev := range res.Items {
		// The privateExtendedProperty filter only guarantees the two
		// queried keys match — it says nothing about whether this event
		// actually carries the full calendar-bridge ownership tag. A real
		// user event could coincidentally (or adversarially) carry the
		// same source-account/source-event property values without being
		// one calendar-bridge created. ensureBlock unconditionally
		// updates whatever FindBlockBySource returns, so returning an
		// unowned event here would let sync silently overwrite a real
		// event. isOwnedBlock is the actual ownership check.
		if ev.Status != "cancelled" && isOwnedBlock(ev) {
			return ev, nil
		}
	}
	return nil, nil
}

func (c *googleCalendarClient) InsertEvent(ctx context.Context, calendarID string, ev *calendar.Event) (*calendar.Event, error) {
	return c.svc.Events.Insert(calendarID, ev).Context(ctx).Do()
}

func (c *googleCalendarClient) UpdateEvent(ctx context.Context, calendarID, eventID string, ev *calendar.Event) (*calendar.Event, error) {
	return c.svc.Events.Update(calendarID, eventID, ev).Context(ctx).Do()
}

func (c *googleCalendarClient) DeleteEvent(ctx context.Context, calendarID, eventID string) error {
	return c.svc.Events.Delete(calendarID, eventID).Context(ctx).Do()
}
