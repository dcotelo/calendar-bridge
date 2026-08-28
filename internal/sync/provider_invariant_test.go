package sync

import (
	"context"
	"errors"
	"testing"
	"time"
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
	_, err := p.UpdateBlockTime(context.Background(), "primary", block, start, end)
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("UpdateBlockTime err = %v, want ErrNotOwned", err)
	}
}

func TestProviderClient_InsertRejectsUntaggedEvent(t *testing.T) {
	fake := newFakeCalendarClient()
	c := NewProviderClient(NewGoogleProvider(fake), "Busy")
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
	c := NewProviderClient(NewGoogleProvider(fake), "Busy")
	ev := realEventIn("real-1", time.Hour, time.Hour) // untagged
	_, err := c.UpdateEvent(context.Background(), "primary", "real-1", ev)
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("UpdateEvent err = %v, want ErrNotOwned for untagged event", err)
	}
}

// --- concurrent-delete path: UpdateBlockTime returns (nil,nil) when the block
// vanished between find and update ---

func TestGoogleProvider_UpdateConcurrentDeleteReturnsNil(t *testing.T) {
	fake := newFakeCalendarClient()
	p := NewGoogleProvider(fake)

	// Insert a real owned block, then simulate it disappearing before update
	// by making FindBlockBySource return nothing (empty fake => no match).
	block := Event{Ownership: ownedFor("a", "real-1")}
	start := TimeSpan{DateTime: time.Now().Format(time.RFC3339)}
	end := TimeSpan{DateTime: time.Now().Add(time.Hour).Format(time.RFC3339)}
	got, err := p.UpdateBlockTime(context.Background(), "primary", block, start, end)
	if err != nil {
		t.Fatalf("UpdateBlockTime err = %v, want nil (concurrent delete is not an error)", err)
	}
	if got != nil {
		t.Errorf("UpdateBlockTime event = %v, want nil (block already gone)", got)
	}
}

func TestProviderClient_UpdateConcurrentDeleteReturnsNil(t *testing.T) {
	fake := newFakeCalendarClient()
	c := NewProviderClient(NewGoogleProvider(fake), "Busy")
	// Build an owned google event whose block isn't present in the fake.
	ev := realEventIn("real-1", time.Hour, time.Hour)
	ev.ExtendedProperties = extProps(map[string]string{
		ownerKey:          ownerValue,
		sourceAccountKey:  "a",
		sourceCalendarKey: "primary",
		sourceEventKey:    "real-1",
	})
	got, err := c.UpdateEvent(context.Background(), "primary", "missing-id", ev)
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
				return p.DeleteBlock(context.Background(), "primary", "some-id")
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

func TestGoogleProvider_UpdatePropagatesFindError(t *testing.T) {
	sentinel := errors.New("find failed")
	fake := newFakeCalendarClient()
	fake.failFind = sentinel
	p := NewGoogleProvider(fake)
	block := Event{Ownership: ownedFor("a", "real-1")}
	start := TimeSpan{DateTime: time.Now().Format(time.RFC3339)}
	end := TimeSpan{DateTime: time.Now().Add(time.Hour).Format(time.RFC3339)}
	ev, err := p.UpdateBlockTime(context.Background(), "primary", block, start, end)
	if !errors.Is(err, sentinel) {
		t.Errorf("UpdateBlockTime err = %v, want sentinel", err)
	}
	if ev != nil {
		t.Errorf("UpdateBlockTime returned event on error")
	}
}
