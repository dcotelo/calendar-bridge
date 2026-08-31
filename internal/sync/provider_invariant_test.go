package sync

import (
	"context"
	"errors"
	"testing"
	"time"

	calendar "google.golang.org/api/calendar/v3"
)

func ownedFor(src, evID string) Ownership {
	return Ownership{Owner: ownerValue, SourceAccount: src, SourceCalendarID: "primary", SourceEventID: evID}
}

// --- invalid-ownership rejection: Insert/Update must refuse untagged writes ---

func TestGoogleProvider_InsertRejectsInvalidOwnership(t *testing.T) {
	cases := []struct {
		name string
		own  Ownership
	}{
		{"not owned", Ownership{SourceAccount: "a", SourceEventID: "e1"}},
		{"owned but no source account", Ownership{Owner: ownerValue, SourceEventID: "e1"}},
		{"owned but no source event", Ownership{Owner: ownerValue, SourceAccount: "a"}},
		{"empty", Ownership{}},
	}
	start := TimeSpan{DateTime: time.Now().Format(time.RFC3339)}
	end := TimeSpan{DateTime: time.Now().Add(time.Hour).Format(time.RFC3339)}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeCalendarClient()
			p := NewGoogleProvider(fake)
			_, err := p.InsertBlock(context.Background(), "primary", "Busy", start, end, tc.own)
			if !errors.Is(err, ErrNotOwned) {
				t.Fatalf("InsertBlock err = %v, want ErrNotOwned", err)
			}
			// No event should have been created.
			if len(fake.events) != 0 {
				t.Errorf("fake events = %d, want 0 (insert must not reach the client)", len(fake.events))
			}
		})
	}
}

func TestGoogleProvider_UpdateRejectsInvalidOwnership(t *testing.T) {
	fake := newFakeCalendarClient()
	p := NewGoogleProvider(fake)
	// A block Event with no ownership tag.
	block := Event{ID: "x", Ownership: Ownership{SourceAccount: "a"}} // not owned
	start := TimeSpan{DateTime: time.Now().Format(time.RFC3339)}
	end := TimeSpan{DateTime: time.Now().Add(time.Hour).Format(time.RFC3339)}
	_, err := p.UpdateBlock(context.Background(), "primary", block, "Busy", start, end)
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("UpdateBlock err = %v, want ErrNotOwned", err)
	}
}

func TestProviderClient_InsertRejectsUntaggedEvent(t *testing.T) {
	fake := newFakeCalendarClient()
	c := NewProviderClient(NewGoogleProvider(fake))
	// A plain, untagged event (no ExtendedProperties).
	ev := realEventIn("real-1", time.Hour, time.Hour)
	_, err := c.InsertEvent(context.Background(), "primary", ev)
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("InsertEvent err = %v, want ErrNotOwned for untagged event", err)
	}
	if len(fake.events) != 0 {
		t.Errorf("fake events = %d, want 0", len(fake.events))
	}
}

func TestProviderClient_UpdateRejectsUntaggedEvent(t *testing.T) {
	fake := newFakeCalendarClient()
	c := NewProviderClient(NewGoogleProvider(fake))
	ev := realEventIn("real-1", time.Hour, time.Hour) // untagged
	_, err := c.UpdateEvent(context.Background(), "primary", "real-1", ev, "")
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("UpdateEvent err = %v, want ErrNotOwned for untagged event", err)
	}
}

// --- concurrent-delete path: UpdateBlock returns (nil,nil) when the block
// vanished between find and update ---

func TestGoogleProvider_UpdateConcurrentDeleteReturnsNil(t *testing.T) {
	fake := newFakeCalendarClient()
	p := NewGoogleProvider(fake)

	// Insert a real owned block, then simulate it disappearing before update
	// by making FindBlockBySource return nothing (empty fake => no match).
	block := Event{Ownership: ownedFor("a", "real-1")}
	start := TimeSpan{DateTime: time.Now().Format(time.RFC3339)}
	end := TimeSpan{DateTime: time.Now().Add(time.Hour).Format(time.RFC3339)}
	got, err := p.UpdateBlock(context.Background(), "primary", block, "Busy", start, end)
	if err != nil {
		t.Fatalf("UpdateBlock err = %v, want nil (concurrent delete is not an error)", err)
	}
	if got != nil {
		t.Errorf("UpdateBlock event = %v, want nil (block already gone)", got)
	}
}

func TestProviderClient_UpdateConcurrentDeleteReturnsNil(t *testing.T) {
	fake := newFakeCalendarClient()
	c := NewProviderClient(NewGoogleProvider(fake))
	// Build an owned google event whose block isn't present in the fake.
	ev := realEventIn("real-1", time.Hour, time.Hour)
	ev.ExtendedProperties = extProps(map[string]string{
		ownerKey:          ownerValue,
		sourceAccountKey:  "a",
		sourceCalendarKey: "primary",
		sourceEventKey:    "real-1",
	})
	got, err := c.UpdateEvent(context.Background(), "primary", "missing-id", ev, "")
	if err != nil {
		t.Fatalf("UpdateEvent err = %v, want nil on concurrent delete", err)
	}
	if got != nil {
		t.Errorf("UpdateEvent event = %v, want nil", got)
	}
}

// --- error propagation: each adapter method surfaces the underlying error and
// returns a nil event ---

func TestGoogleProvider_PropagatesClientErrors(t *testing.T) {
	sentinel := errors.New("boom")
	own := ownedFor("a", "real-1")
	start := TimeSpan{DateTime: time.Now().Format(time.RFC3339)}
	end := TimeSpan{DateTime: time.Now().Add(time.Hour).Format(time.RFC3339)}

	cases := []struct {
		name string
		call func(p Provider, f *fakeCalendarClient) error
		set  func(f *fakeCalendarClient)
	}{
		{
			name: "ListEvents",
			set:  func(f *fakeCalendarClient) { f.failList = sentinel },
			call: func(p Provider, f *fakeCalendarClient) error {
				evs, err := p.ListEvents(context.Background(), "primary", time.Now(), time.Now().Add(time.Hour))
				if evs != nil {
					t.Errorf("ListEvents returned events on error")
				}
				return err
			},
		},
		{
			name: "InsertBlock",
			set:  func(f *fakeCalendarClient) { f.failInsert = sentinel },
			call: func(p Provider, f *fakeCalendarClient) error {
				ev, err := p.InsertBlock(context.Background(), "primary", "Busy", start, end, own)
				if ev != nil {
					t.Errorf("InsertBlock returned event on error")
				}
				return err
			},
		},
		{
			name: "DeleteBlock",
			set:  func(f *fakeCalendarClient) { f.failDelete = sentinel },
			call: func(p Provider, f *fakeCalendarClient) error {
				// Seed an owned block so DeleteBlock's ownership re-check passes
				// and it reaches the (failing) DeleteEvent call.
				blk := realEventIn("blk", time.Hour, time.Hour)
				blk.ExtendedProperties = extProps(map[string]string{
					ownerKey: ownerValue, sourceAccountKey: "a",
					sourceCalendarKey: "primary", sourceEventKey: "real-1",
				})
				f.seed("blk", blk)
				return p.DeleteBlock(context.Background(), "primary", "blk")
			},
		},
		{
			name: "DeleteBlock GetEvent error",
			set:  func(f *fakeCalendarClient) { f.failGet = sentinel; f.failDelete = errors.New("must not be reached") },
			call: func(p Provider, f *fakeCalendarClient) error {
				// GetEvent fails first, so DeleteBlock must surface that error
				// and never call DeleteEvent.
				return p.DeleteBlock(context.Background(), "primary", "blk")
			},
		},
		{
			name: "UpdateBlock",
			set:  func(f *fakeCalendarClient) { f.failUpdate = sentinel },
			call: func(p Provider, f *fakeCalendarClient) error {
				// Seed the owned block so the find succeeds and update is reached.
				blk := realEventIn("blk", time.Hour, time.Hour)
				blk.ExtendedProperties = extProps(map[string]string{
					ownerKey: ownerValue, sourceAccountKey: "a",
					sourceCalendarKey: "primary", sourceEventKey: "real-1",
				})
				f.seed("blk", blk)
				ev, err := p.UpdateBlock(context.Background(), "primary", Event{Ownership: own}, "Busy", start, end)
				if ev != nil {
					t.Errorf("UpdateBlock returned event on error")
				}
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeCalendarClient()
			tc.set(fake)
			p := NewGoogleProvider(fake)
			if err := tc.call(p, fake); !errors.Is(err, sentinel) {
				t.Errorf("%s err = %v, want sentinel", tc.name, err)
			}
		})
	}
}

func TestGoogleProvider_DeleteRejectsUnownedTarget(t *testing.T) {
	fake := newFakeCalendarClient()
	// A plain, untagged real event sitting at the target ID.
	fake.seed("real-1", realEventIn("real-1", time.Hour, time.Hour))
	p := NewGoogleProvider(fake)

	err := p.DeleteBlock(context.Background(), "primary", "real-1")
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("DeleteBlock err = %v, want ErrNotOwned for an untagged target", err)
	}
	if _, ok := fake.events["real-1"]; !ok {
		t.Error("untagged event was deleted; want it left intact")
	}
}

func TestGoogleProvider_DeleteMissingIsNoop(t *testing.T) {
	fake := newFakeCalendarClient()
	p := NewGoogleProvider(fake)
	if err := p.DeleteBlock(context.Background(), "primary", "nope"); err != nil {
		t.Errorf("DeleteBlock on missing id = %v, want nil (already gone)", err)
	}
}

func TestGoogleProvider_FindOwnedBlockPropagatesError(t *testing.T) {
	sentinel := errors.New("find failed")
	fake := newFakeCalendarClient()
	fake.failFind = sentinel
	p := NewGoogleProvider(fake)
	ev, err := p.FindOwnedBlock(context.Background(), "primary", "a", "real-1")
	if !errors.Is(err, sentinel) {
		t.Errorf("FindOwnedBlock err = %v, want sentinel", err)
	}
	if ev != nil {
		t.Errorf("FindOwnedBlock returned event on error")
	}
}

// looseFindClient returns a preset event from FindBlockBySource WITHOUT the
// real client's ownership filter, simulating a buggy/adversarial provider
// client. It records whether UpdateEvent ran.
type looseFindClient struct {
	inner   *fakeCalendarClient
	ret     *calendar.Event
	updated bool
}

func (l *looseFindClient) ListEvents(ctx context.Context, calendarID string, timeMin, timeMax time.Time) ([]*calendar.Event, error) {
	return l.inner.ListEvents(ctx, calendarID, timeMin, timeMax)
}
func (l *looseFindClient) FindBlockBySource(ctx context.Context, calendarID, srcAccount, srcEventID string) (*calendar.Event, error) {
	return l.ret, nil
}
func (l *looseFindClient) GetEvent(ctx context.Context, calendarID, eventID string) (*calendar.Event, error) {
	return l.inner.GetEvent(ctx, calendarID, eventID)
}
func (l *looseFindClient) InsertEvent(ctx context.Context, calendarID string, ev *calendar.Event) (*calendar.Event, error) {
	return l.inner.InsertEvent(ctx, calendarID, ev)
}
func (l *looseFindClient) UpdateEvent(ctx context.Context, calendarID, eventID string, ev *calendar.Event, ifMatchETag string) (*calendar.Event, error) {
	l.updated = true
	return l.inner.UpdateEvent(ctx, calendarID, eventID, ev, ifMatchETag)
}
func (l *looseFindClient) DeleteEvent(ctx context.Context, calendarID, eventID, ifMatchETag string) error {
	return l.inner.DeleteEvent(ctx, calendarID, eventID, ifMatchETag)
}

// TestGoogleProvider_IgnoresSourceMatchButUntagged proves the adapter does not
// trust a CalendarClient that returns a source-property match without the full
// ownership tag: FindOwnedBlock returns nil and UpdateBlock refuses.
func TestGoogleProvider_IgnoresSourceMatchButUntagged(t *testing.T) {
	fake := newFakeCalendarClient()
	imposter := realEventIn("imposter", time.Hour, time.Hour)
	imposter.ExtendedProperties = extProps(map[string]string{
		sourceAccountKey:  "a",
		sourceCalendarKey: "primary",
		sourceEventKey:    "real-1",
		// deliberately no ownerKey
	})
	loose := &looseFindClient{inner: fake, ret: imposter}
	p := NewGoogleProvider(loose)

	got, err := p.FindOwnedBlock(context.Background(), "primary", "a", "real-1")
	if err != nil {
		t.Fatalf("FindOwnedBlock err = %v", err)
	}
	if got != nil {
		t.Error("FindOwnedBlock returned an untagged source-match; want nil")
	}

	_, err = p.UpdateBlock(context.Background(), "primary", Event{Ownership: ownedFor("a", "real-1")}, "Busy",
		TimeSpan{DateTime: time.Now().Format(time.RFC3339)}, TimeSpan{DateTime: time.Now().Add(time.Hour).Format(time.RFC3339)})
	if !errors.Is(err, ErrNotOwned) {
		t.Errorf("UpdateBlock err = %v, want ErrNotOwned for untagged re-read", err)
	}
	if loose.updated {
		t.Error("UpdateBlock called UpdateEvent on an untagged event")
	}
}

func TestGoogleProvider_UpdatePropagatesFindError(t *testing.T) {
	sentinel := errors.New("find failed")
	fake := newFakeCalendarClient()
	fake.failFind = sentinel
	p := NewGoogleProvider(fake)
	block := Event{Ownership: ownedFor("a", "real-1")}
	start := TimeSpan{DateTime: time.Now().Format(time.RFC3339)}
	end := TimeSpan{DateTime: time.Now().Add(time.Hour).Format(time.RFC3339)}
	ev, err := p.UpdateBlock(context.Background(), "primary", block, "Busy", start, end)
	if !errors.Is(err, sentinel) {
		t.Errorf("UpdateBlock err = %v, want sentinel", err)
	}
	if ev != nil {
		t.Errorf("UpdateBlock returned event on error")
	}
}
