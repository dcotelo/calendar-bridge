package sync

import (
	"context"
	"errors"
	"testing"
	"time"

	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
)

// etagClient is a CalendarClient stub that models ETags and a one-time 412 on
// delete, to exercise googleProvider.DeleteBlock's conditional-delete retry.
type etagClient struct {
	ev           *calendar.Event
	deleteCalls  int
	failFirst412 bool
	lastIfMatch  []string
	deleted      bool
}

func (e *etagClient) ListEvents(ctx context.Context, calendarID string, a, b time.Time) ([]*calendar.Event, error) {
	return nil, nil
}
func (e *etagClient) FindBlockBySource(ctx context.Context, calendarID, sa, se string) (*calendar.Event, error) {
	return nil, nil
}
func (e *etagClient) GetEvent(ctx context.Context, calendarID, id string) (*calendar.Event, error) {
	if e.deleted {
		return nil, nil
	}
	return e.ev, nil
}
func (e *etagClient) InsertEvent(ctx context.Context, calendarID string, ev *calendar.Event) (*calendar.Event, error) {
	return ev, nil
}
func (e *etagClient) UpdateEvent(ctx context.Context, calendarID, id string, ev *calendar.Event) (*calendar.Event, error) {
	return ev, nil
}
func (e *etagClient) DeleteEvent(ctx context.Context, calendarID, id, ifMatch string) error {
	e.deleteCalls++
	e.lastIfMatch = append(e.lastIfMatch, ifMatch)
	if e.failFirst412 && e.deleteCalls == 1 {
		return &googleapi.Error{Code: 412}
	}
	e.deleted = true
	return nil
}

func ownedEvent(id, etag string) *calendar.Event {
	return &calendar.Event{
		Id:   id,
		Etag: etag,
		ExtendedProperties: &calendar.EventExtendedProperties{Private: map[string]string{
			ownerKey: ownerValue, sourceAccountKey: "a", sourceCalendarKey: "primary", sourceEventKey: "e1",
		}},
	}
}

func TestDeleteBlock_UsesETagIfMatch(t *testing.T) {
	c := &etagClient{ev: ownedEvent("blk", "etag-123")}
	p := NewGoogleProvider(c)
	if err := p.DeleteBlock(context.Background(), "primary", "blk"); err != nil {
		t.Fatalf("DeleteBlock err = %v", err)
	}
	if c.deleteCalls != 1 {
		t.Fatalf("delete calls = %d, want 1", c.deleteCalls)
	}
	if len(c.lastIfMatch) != 1 || c.lastIfMatch[0] != "etag-123" {
		t.Errorf("If-Match = %v, want [etag-123]", c.lastIfMatch)
	}
}

func TestDeleteBlock_RetriesOnPreconditionFailure(t *testing.T) {
	c := &etagClient{ev: ownedEvent("blk", "etag-123"), failFirst412: true}
	p := NewGoogleProvider(c)
	if err := p.DeleteBlock(context.Background(), "primary", "blk"); err != nil {
		t.Fatalf("DeleteBlock err = %v, want nil after 412 re-verify", err)
	}
	if c.deleteCalls != 2 {
		t.Errorf("delete calls = %d, want 2 (one 412 then success)", c.deleteCalls)
	}
}

func TestDeleteBlock_RefusesUntaggedUnderETag(t *testing.T) {
	// A real (untagged) event at the target ID must never be deleted.
	c := &etagClient{ev: &calendar.Event{Id: "blk", Etag: "e"}}
	p := NewGoogleProvider(c)
	if err := p.DeleteBlock(context.Background(), "primary", "blk"); !errors.Is(err, ErrNotOwned) {
		t.Errorf("DeleteBlock err = %v, want ErrNotOwned", err)
	}
	if c.deleteCalls != 0 {
		t.Errorf("delete calls = %d, want 0 (untagged must not be deleted)", c.deleteCalls)
	}
}

func TestInsertBlock_IdempotentReusesExisting(t *testing.T) {
	// If a block for the source already exists, InsertBlock returns it instead
	// of creating a duplicate (guards a retried insert after a lost success).
	fake := newFakeCalendarClient()
	existing := ownedEvent("existing-blk", "e")
	fake.seed("existing-blk", existing)
	p := NewGoogleProvider(fake)

	got, err := p.InsertBlock(context.Background(), "primary", "Busy",
		TimeSpan{DateTime: time.Now().Format(time.RFC3339)},
		TimeSpan{DateTime: time.Now().Add(time.Hour).Format(time.RFC3339)},
		ownedFor("a", "e1"))
	if err != nil {
		t.Fatalf("InsertBlock err = %v", err)
	}
	if got == nil || got.ID != "existing-blk" {
		t.Errorf("InsertBlock returned %v, want the existing block", got)
	}
	if n := len(fake.ownedBlocks()); n != 1 {
		t.Errorf("owned blocks = %d, want 1 (no duplicate created)", n)
	}
}
