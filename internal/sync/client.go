package sync

import (
	"context"
	"errors"
	"fmt"
	"time"

	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
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

	// GetEvent returns the event with the given ID on calendarID, or nil if it
	// does not exist. Used to re-verify a block's ownership tag immediately
	// before deletion.
	GetEvent(ctx context.Context, calendarID, eventID string) (*calendar.Event, error)

	// InsertEvent creates ev on calendarID.
	InsertEvent(ctx context.Context, calendarID string, ev *calendar.Event) (*calendar.Event, error)

	// UpdateEvent updates the event with the given ID on calendarID. If
	// ifMatchETag is non-empty, the update is conditional on the event still
	// matching that ETag (If-Match); a precondition failure surfaces as an
	// error so the caller can re-verify ownership rather than overwrite an
	// event that changed since it was read.
	UpdateEvent(ctx context.Context, calendarID, eventID string, ev *calendar.Event, ifMatchETag string) (*calendar.Event, error)

	// DeleteEvent deletes the event with the given ID on calendarID. If
	// ifMatchETag is non-empty, the delete is conditional on the event still
	// matching that ETag (If-Match); a precondition failure surfaces as an
	// error so the caller can re-verify rather than delete a changed event.
	DeleteEvent(ctx context.Context, calendarID, eventID, ifMatchETag string) error
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
	// Follow nextPageToken to completion, exactly as ListEvents does. The
	// Calendar API can return a page with NO items but a non-empty
	// nextPageToken, so stopping at the first response can miss an owned block
	// that exists — and the caller then inserts a second block for the same
	// source event, producing a duplicate busy block on the destination.
	pageToken := ""
	for {
		// Both filters must go in ONE call. The generated setter uses SetMulti,
		// which REPLACES the parameter rather than appending, so chaining two
		// calls silently drops the first — leaving the query filtered on the
		// event ID alone and able to match a block belonging to a different
		// source account.
		call := c.svc.Events.List(calendarID).PrivateExtendedProperty(
			sourceAccountKey+"="+srcAccount,
			sourceEventKey+"="+srcEventID,
		)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}

		res, err := call.Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("querying existing block (source=%s/%s): %w", srcAccount, srcEventID, err)
		}
		for _, ev := range res.Items {
			if ev == nil || ev.Status == "cancelled" {
				continue
			}
			// The privateExtendedProperty filter only guarantees the queried
			// keys match — it says nothing about whether this event actually
			// carries the full calendar-bridge ownership tag. A real user event
			// could coincidentally (or adversarially) carry the same source
			// properties without being one calendar-bridge created. Callers
			// update whatever this returns, so an unowned event here would let
			// sync silently overwrite a real event. isOwnedBlock is the actual
			// ownership check.
			if !isOwnedBlock(ev) {
				continue
			}
			// Re-verify the source identity locally rather than trusting the
			// server-side filter to have applied both terms.
			acc, _, evID, ok := sourceIdentity(ev)
			if !ok || acc != srcAccount || evID != srcEventID {
				continue
			}
			return ev, nil
		}
		if res.NextPageToken == "" {
			return nil, nil
		}
		pageToken = res.NextPageToken
	}
}

func (c *googleCalendarClient) InsertEvent(ctx context.Context, calendarID string, ev *calendar.Event) (*calendar.Event, error) {
	return c.svc.Events.Insert(calendarID, ev).Context(ctx).Do()
}

func (c *googleCalendarClient) GetEvent(ctx context.Context, calendarID, eventID string) (*calendar.Event, error) {
	ev, err := c.svc.Events.Get(calendarID, eventID).Context(ctx).Do()
	if err != nil {
		var apiErr *googleapi.Error
		if errors.As(err, &apiErr) && apiErr.Code == 404 {
			return nil, nil // treat "gone" as no event, not an error
		}
		return nil, err
	}
	return ev, nil
}

func (c *googleCalendarClient) UpdateEvent(ctx context.Context, calendarID, eventID string, ev *calendar.Event, ifMatchETag string) (*calendar.Event, error) {
	call := c.svc.Events.Update(calendarID, eventID, ev).Context(ctx)
	if ifMatchETag != "" {
		call.Header().Set("If-Match", ifMatchETag)
	}
	return call.Do()
}

func (c *googleCalendarClient) DeleteEvent(ctx context.Context, calendarID, eventID, ifMatchETag string) error {
	call := c.svc.Events.Delete(calendarID, eventID).Context(ctx)
	if ifMatchETag != "" {
		call.Header().Set("If-Match", ifMatchETag)
	}
	return call.Do()
}
