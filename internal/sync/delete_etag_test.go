package sync

import (
	"context"
	"errors"
	"testing"
	"time"

	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
)

// etagClient models ETags and a one-time 412 on delete. After the 412 it swaps
// in afterConflict (the re-read result), so tests can assert that DeleteBlock
// re-reads and re-verifies rather than blindly retrying with the stale ETag.
type etagClient struct {
	ev            *calendar.Event
	afterConflict *calendar.Event
	deleteCalls   int
	failFirst412  bool
	lastIfMatch   []string
	deleted       bool
	conflicted    bool
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
	if e.conflicted && e.afterConflict != nil {
		return e.afterConflict, nil
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
		e.conflicted = true
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
	if err := NewGoogleProvider(c).DeleteBlock(context.Background(), "primary", "blk"); err != nil {
		t.Fatalf("DeleteBlock err = %v", err)
	}
	if c.deleteCalls != 1 {
		t.Fatalf("delete calls = %d, want 1", c.deleteCalls)
	}
	if len(c.lastIfMatch) != 1 || c.lastIfMatch[0] != "etag-123" {
		t.Errorf("If-Match = %v, want [etag-123]", c.lastIfMatch)
	}
}

func TestDeleteBlock_On412ReReadsAndUsesNewETag(t *testing.T) {
	c := &etagClient{
		ev:            ownedEvent("blk", "etag-old"),
		afterConflict: ownedEvent("blk", "etag-new"),
		failFirst412:  true,
	}
	if err := NewGoogleProvider(c).DeleteBlock(context.Background(), "primary", "blk"); err != nil {
		t.Fatalf("DeleteBlock err = %v, want nil after 412 re-read", err)
	}
	if c.deleteCalls != 2 {
		t.Fatalf("delete calls = %d, want 2", c.deleteCalls)
	}
	if c.lastIfMatch[0] != "etag-old" || c.lastIfMatch[1] != "etag-new" {
		t.Errorf("If-Match sequence = %v, want [etag-old etag-new] (retry must use re-read ETag)", c.lastIfMatch)
	}
}

func TestDeleteBlock_On412BecomesUntaggedRefuses(t *testing.T) {
	c := &etagClient{
		ev:            ownedEvent("blk", "etag-old"),
		afterConflict: &calendar.Event{Id: "blk", Etag: "etag-new"}, // untagged after conflict
		failFirst412:  true,
	}
	if err := NewGoogleProvider(c).DeleteBlock(context.Background(), "primary", "blk"); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("DeleteBlock err = %v, want ErrNotOwned after event became untagged", err)
	}
	if c.deleteCalls != 1 {
		t.Errorf("delete calls = %d, want 1 (no second delete on an untagged re-read)", c.deleteCalls)
	}
}

func TestDeleteBlock_RefusesUntaggedUnderETag(t *testing.T) {
	c := &etagClient{ev: &calendar.Event{Id: "blk", Etag: "e"}}
	if err := NewGoogleProvider(c).DeleteBlock(context.Background(), "primary", "blk"); !errors.Is(err, ErrNotOwned) {
		t.Errorf("DeleteBlock err = %v, want ErrNotOwned", err)
	}
	if c.deleteCalls != 0 {
		t.Errorf("delete calls = %d, want 0 (untagged must not be deleted)", c.deleteCalls)
	}
}

func TestInsertBlock_IdempotentReusesExisting(t *testing.T) {
	fake := newFakeCalendarClient()
	fake.seed("existing-blk", ownedEvent("existing-blk", "e"))
	got, err := NewGoogleProvider(fake).InsertBlock(context.Background(), "primary", "Busy",
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
