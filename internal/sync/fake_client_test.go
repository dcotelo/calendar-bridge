package sync

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	stdsync "sync"
	"time"

	calendar "google.golang.org/api/calendar/v3"
)

// fakeCalendarClient is an in-memory CalendarClient used to exercise
// SyncOnce without any real Google API calls. Each fake owns exactly one
// calendar's worth of events, mirroring how one Account maps to one
// CalendarClient in production.
type fakeCalendarClient struct {
	mu       stdsync.Mutex
	events   map[string]*calendar.Event // event ID -> event
	nextID   int
	failList error // if set, ListEvents always returns this error
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
		out = append(out, ev)
	}
	return out, nil
}

func (f *fakeCalendarClient) FindBlockBySource(ctx context.Context, calendarID, srcAccount, srcEventID string) (*calendar.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ev := range f.events {
		acc, _, evID, ok := sourceIdentity(ev)
		if ok && acc == srcAccount && evID == srcEventID {
			return ev, nil
		}
	}
	return nil, nil
}

func (f *fakeCalendarClient) InsertEvent(ctx context.Context, calendarID string, ev *calendar.Event) (*calendar.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	ev.Id = fmt.Sprintf("fake-block-%d", f.nextID)
	f.events[ev.Id] = ev
	return ev, nil
}

func (f *fakeCalendarClient) UpdateEvent(ctx context.Context, calendarID, eventID string, ev *calendar.Event) (*calendar.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.events[eventID]; !ok {
		return nil, fmt.Errorf("fake: event %s not found", eventID)
	}
	ev.Id = eventID
	f.events[eventID] = ev
	return ev, nil
}

func (f *fakeCalendarClient) DeleteEvent(ctx context.Context, calendarID, eventID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.events[eventID]; !ok {
		return fmt.Errorf("fake: event %s not found", eventID)
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

func realEvent(id, startRFC3339, endRFC3339 string) *calendar.Event {
	return &calendar.Event{
		Id:      id,
		Summary: "Some real meeting",
		Start:   &calendar.EventDateTime{DateTime: startRFC3339, TimeZone: "UTC"},
		End:     &calendar.EventDateTime{DateTime: endRFC3339, TimeZone: "UTC"},
	}
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
