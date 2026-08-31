package sync

import (
	"context"
	"errors"
	"strings"
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
	always412     bool // event keeps changing concurrently forever: every delete 412s
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
func (e *etagClient) UpdateEvent(ctx context.Context, calendarID, id string, ev *calendar.Event, ifMatchETag string) (*calendar.Event, error) {
	return ev, nil
}
func (e *etagClient) DeleteEvent(ctx context.Context, calendarID, id, ifMatch string) error {
	e.deleteCalls++
	e.lastIfMatch = append(e.lastIfMatch, ifMatch)
	if e.always412 || (e.failFirst412 && e.deleteCalls == 1) {
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

func TestDeleteBlock_ExhaustsRetriesOnPersistent412(t *testing.T) {
	// The event keeps changing concurrently (still owned each re-read, but
	// the ETag never settles), so every delete attempt hits 412. DeleteBlock
	// must give up after its bounded number of attempts rather than loop
	// forever, and must never delete an event it couldn't confirm as stable.
	c := &etagClient{
		ev:            ownedEvent("blk", "etag-1"),
		afterConflict: ownedEvent("blk", "etag-2"), // still owned, still conflicting
		always412:     true,
	}
	err := NewGoogleProvider(c).DeleteBlock(context.Background(), "primary", "blk")
	if err == nil {
		t.Fatal("DeleteBlock err = nil, want error after exhausting retries on persistent 412")
	}
	if errors.Is(err, ErrNotOwned) {
		t.Errorf("DeleteBlock err = %v, want a retry-exhaustion error, not ErrNotOwned", err)
	}
	if c.deleteCalls != 2 {
		t.Errorf("delete calls = %d, want 2 (bounded attempts, no unbounded retry)", c.deleteCalls)
	}
	if c.deleted {
		t.Error("event marked deleted, want it left in place since ownership was never confirmed stable")
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

// ---- update is conditional too ----
//
// UpdateBlock re-reads and verifies ownership, then writes. Without an If-Match
// on that re-read's ETag the check and the write are not atomic: an event that
// loses its owner tag in between would be overwritten. DeleteBlock has always
// been conditional; these pin the same guarantee for updates.

// updateETagClient serves a block via FindBlockBySource and models a one-time
// 412 on update, swapping in whatever the concurrent writer left behind.
type updateETagClient struct {
	ev            *calendar.Event
	afterConflict *calendar.Event
	updates       int
	failFirst412  bool
	always412     bool // the event keeps changing: every update 412s
	lastIfMatch   []string
	conflicted    bool
}

func (u *updateETagClient) ListEvents(context.Context, string, time.Time, time.Time) ([]*calendar.Event, error) {
	return nil, nil
}
func (u *updateETagClient) FindBlockBySource(context.Context, string, string, string) (*calendar.Event, error) {
	if u.conflicted && u.afterConflict != nil {
		return u.afterConflict, nil
	}
	return u.ev, nil
}
func (u *updateETagClient) GetEvent(context.Context, string, string) (*calendar.Event, error) {
	return u.ev, nil
}
func (u *updateETagClient) InsertEvent(_ context.Context, _ string, ev *calendar.Event) (*calendar.Event, error) {
	return ev, nil
}
func (u *updateETagClient) UpdateEvent(_ context.Context, _, _ string, ev *calendar.Event, ifMatch string) (*calendar.Event, error) {
	u.updates++
	u.lastIfMatch = append(u.lastIfMatch, ifMatch)
	if u.always412 || (u.failFirst412 && u.updates == 1) {
		u.conflicted = true
		return nil, &googleapi.Error{Code: 412}
	}
	return ev, nil
}
func (u *updateETagClient) DeleteEvent(context.Context, string, string, string) error { return nil }

func ownedNeutral() Event {
	return Event{Ownership: Ownership{
		Owner: ownerValue, SourceAccount: "a", SourceCalendarID: "primary", SourceEventID: "e1",
	}}
}

func updateSpans() (TimeSpan, TimeSpan) {
	return TimeSpan{DateTime: "2026-03-12T10:00:00Z"}, TimeSpan{DateTime: "2026-03-12T11:00:00Z"}
}

func TestUpdateBlock_ETagPreconditions(t *testing.T) {
	untagged := ownedEvent("blk-1", `"etag-new"`)
	delete(untagged.ExtendedProperties.Private, ownerKey)

	cases := []struct {
		name   string
		client *updateETagClient
		// wantErr is checked with errors.Is when non-nil; want412 expects a
		// bounded-retry failure instead.
		wantErr     error
		want412Msg  bool
		wantUpdates int
		wantIfMatch []string
	}{
		{
			name:        "conditional on the re-read ETag",
			client:      &updateETagClient{ev: ownedEvent("blk-1", `"etag-1"`)},
			wantUpdates: 1,
			wantIfMatch: []string{`"etag-1"`},
		},
		{
			name: "412 re-reads and retries with the fresh ETag",
			client: &updateETagClient{
				ev:            ownedEvent("blk-1", `"etag-old"`),
				afterConflict: ownedEvent("blk-1", `"etag-new"`),
				failFirst412:  true,
			},
			wantUpdates: 2,
			wantIfMatch: []string{`"etag-old"`, `"etag-new"`},
		},
		{
			// The one that matters: the owner tag disappeared in the window.
			name: "412 then untagged is refused before writing",
			client: &updateETagClient{
				ev:            ownedEvent("blk-1", `"etag-old"`),
				afterConflict: untagged,
				failFirst412:  true,
			},
			wantErr:     ErrNotOwned,
			wantUpdates: 1,
		},
		{
			// Bounded, not infinite: an event changing on every attempt must
			// stop rather than retry forever.
			name: "persistent 412 gives up after the bounded retries",
			client: &updateETagClient{
				ev:            ownedEvent("blk-1", `"etag-old"`),
				afterConflict: ownedEvent("blk-1", `"etag-newer"`),
				always412:     true,
			},
			want412Msg:  true,
			wantUpdates: 2,
		},
	}

	start, end := updateSpans()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewGoogleProvider(tc.client).
				UpdateBlock(context.Background(), "primary", ownedNeutral(), "Busy", start, end)

			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case tc.want412Msg:
				if err == nil {
					t.Fatal("a persistently conflicting update must fail, not loop forever")
				}
				if !strings.Contains(err.Error(), "precondition") {
					t.Errorf("err = %v, want it to name the ownership precondition", err)
				}
			default:
				if err != nil {
					t.Fatalf("UpdateBlock: %v", err)
				}
			}

			if tc.client.updates != tc.wantUpdates {
				t.Errorf("made %d update calls, want %d", tc.client.updates, tc.wantUpdates)
			}
			if tc.wantIfMatch != nil {
				if len(tc.client.lastIfMatch) != len(tc.wantIfMatch) {
					t.Fatalf("If-Match sequence = %v, want %v", tc.client.lastIfMatch, tc.wantIfMatch)
				}
				for i, want := range tc.wantIfMatch {
					if tc.client.lastIfMatch[i] != want {
						t.Errorf("If-Match[%d] = %q, want %q", i, tc.client.lastIfMatch[i], want)
					}
				}
			}
		})
	}
}
