package sync

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	stdsync "sync"
	"time"

	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
)

// fakeCalendarClient is an in-memory CalendarClient used to exercise
// SyncOnce without any real Google API calls. Each fake owns exactly one
// calendar's worth of events, mirroring how one Account maps to one
// CalendarClient in production.
type fakeCalendarClient struct {
	mu     stdsync.Mutex
	events map[string]*calendar.Event // event ID -> event
	nextID int

	// Failure injection: when set, the corresponding method always
	// returns this error instead of performing its normal operation.
	failList   error
	failFind   error
	failInsert error
	failUpdate error
	failDelete error
	failGet    error
}

func newFakeCalendarClient() *fakeCalendarClient {
	return &fakeCalendarClient{events: make(map[string]*calendar.Event)}
}

// seed adds a real (non-owned) event directly, bypassing sync logic, to set
// up test fixtures.
func (f *fakeCalendarClient) seed(id string, ev *calendar.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ev.Id = id
	f.events[id] = ev
}

func (f *fakeCalendarClient) ListEvents(ctx context.Context, calendarID string, timeMin, timeMax time.Time) ([]*calendar.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failList != nil {
		return nil, f.failList
	}
	out := make([]*calendar.Event, 0, len(f.events))
	for _, ev := range f.events {
		if !eventInWindow(ev, timeMin, timeMax) {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

// eventInWindow mirrors the real Calendar API's TimeMin/TimeMax filtering:
// an event is included if it starts before timeMax and ends after timeMin.
// Events with unparseable or missing start/end times are excluded rather
// than risking an incorrect match.
func eventInWindow(ev *calendar.Event, timeMin, timeMax time.Time) bool {
	start, ok := eventTime(ev.Start)
	if !ok {
		return false
	}
	end, ok := eventTime(ev.End)
	if !ok {
		return false
	}
	return start.Before(timeMax) && end.After(timeMin)
}

func eventTime(dt *calendar.EventDateTime) (time.Time, bool) {
	if dt == nil {
		return time.Time{}, false
	}
	if dt.DateTime != "" {
		t, err := time.Parse(time.RFC3339, dt.DateTime)
		if err != nil {
			return time.Time{}, false
		}
		return t, true
	}
	if dt.Date != "" {
		t, err := time.Parse("2006-01-02", dt.Date)
		if err != nil {
			return time.Time{}, false
		}
		return t, true
	}
	return time.Time{}, false
}

func (f *fakeCalendarClient) FindBlockBySource(ctx context.Context, calendarID, srcAccount, srcEventID string) (*calendar.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFind != nil {
		return nil, f.failFind
	}
	for _, ev := range f.events {
		if !isOwnedBlock(ev) {
			continue // mirror the real client: never match an unowned event
		}
		acc, _, evID, ok := sourceIdentity(ev)
		if ok && acc == srcAccount && evID == srcEventID {
			return ev, nil
		}
	}
	return nil, nil
}

func (f *fakeCalendarClient) GetEvent(ctx context.Context, calendarID, eventID string) (*calendar.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGet != nil {
		return nil, f.failGet
	}
	return f.events[eventID], nil // nil if absent, matching the real client's 404->nil
}

func (f *fakeCalendarClient) InsertEvent(ctx context.Context, calendarID string, ev *calendar.Event) (*calendar.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failInsert != nil {
		return nil, f.failInsert
	}
	f.nextID++
	ev.Id = fmt.Sprintf("fake-block-%d", f.nextID)
	f.events[ev.Id] = ev
	return ev, nil
}

func (f *fakeCalendarClient) UpdateEvent(ctx context.Context, calendarID, eventID string, ev *calendar.Event) (*calendar.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failUpdate != nil {
		return nil, f.failUpdate
	}
	if _, ok := f.events[eventID]; !ok {
		return nil, fmt.Errorf("fake: event %s not found", eventID)
	}
	ev.Id = eventID
	f.events[eventID] = ev
	return ev, nil
}

func (f *fakeCalendarClient) DeleteEvent(ctx context.Context, calendarID, eventID, ifMatchETag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDelete != nil {
		return f.failDelete
	}
	ev, ok := f.events[eventID]
	if !ok {
		return fmt.Errorf("fake: event %s not found", eventID)
	}
	// Model the real API's conditional delete: an If-Match that doesn't match
	// the event's current ETag fails its precondition (412) rather than
	// silently deleting, so a caller that deletes with a stale ETag is caught
	// here instead of only in production. A blank ETag on either side (a
	// caller/fixture that doesn't model ETags at all) is treated as a match.
	if ifMatchETag != "" && ev.Etag != "" && ifMatchETag != ev.Etag {
		return &googleapi.Error{Code: http.StatusPreconditionFailed}
	}
	delete(f.events, eventID)
	return nil
}

// ownedBlocks returns every event in the fake that is tagged as
// calendar-bridge-owned, for test assertions.
func (f *fakeCalendarClient) ownedBlocks() []*calendar.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*calendar.Event
	for _, ev := range f.events {
		if isOwnedBlock(ev) {
			out = append(out, ev)
		}
	}
	return out
}

// realEventIn builds a fixture event offset from time.Now(), so it always
// falls inside a typical (e.g. 30-day) lookahead window regardless of when
// the test suite runs.
func realEventIn(id string, startOffset, duration time.Duration) *calendar.Event {
	start := time.Now().Add(startOffset)
	end := start.Add(duration)
	return &calendar.Event{
		Id:      id,
		Summary: "Some real meeting",
		Start:   &calendar.EventDateTime{DateTime: start.Format(time.RFC3339), TimeZone: "UTC"},
		End:     &calendar.EventDateTime{DateTime: end.Format(time.RFC3339), TimeZone: "UTC"},
	}
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
