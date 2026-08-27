package sync

import (
	"context"
	"errors"
	"testing"
	"time"

	calendar "google.golang.org/api/calendar/v3"
)

func newTestEngine(accounts ...Account) *Engine {
	return &Engine{
		Accounts:      accounts,
		BlockTitle:    "Busy (test)",
		LookaheadDays: 30,
		Logger:        newTestLogger(),
	}
}

func TestSyncOnce_PropagatesBusyBlockBothWays(t *testing.T) {
	a := newFakeCalendarClient()
	b := newFakeCalendarClient()
	a.seed("real-a-1", realEventIn("real-a-1", 24*time.Hour, time.Hour))

	engine := newTestEngine(
		Account{Name: "a", CalendarID: "primary", Client: a},
		Account{Name: "b", CalendarID: "primary", Client: b},
	)

	if err := engine.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce() error = %v, want nil", err)
	}

	bBlocks := b.ownedBlocks()
	if len(bBlocks) != 1 {
		t.Fatalf("account b owned blocks = %d, want 1", len(bBlocks))
	}
	acc, _, evID, ok := sourceIdentity(bBlocks[0])
	if !ok || acc != "a" || evID != "real-a-1" {
		t.Errorf("block source identity = (%q, %q, ok=%v), want (a, real-a-1, true)", acc, evID, ok)
	}

	// a has no real event from b, so a should get no block.
	if len(a.ownedBlocks()) != 0 {
		t.Errorf("account a owned blocks = %d, want 0 (b has no real events)", len(a.ownedBlocks()))
	}
}

func TestSyncOnce_GarbageCollectsRemovedSourceEvent(t *testing.T) {
	a := newFakeCalendarClient()
	b := newFakeCalendarClient()
	a.seed("real-a-1", realEventIn("real-a-1", 24*time.Hour, time.Hour))

	engine := newTestEngine(
		Account{Name: "a", CalendarID: "primary", Client: a},
		Account{Name: "b", CalendarID: "primary", Client: b},
	)

	if err := engine.SyncOnce(context.Background()); err != nil {
		t.Fatalf("first SyncOnce() error = %v, want nil", err)
	}
	if len(b.ownedBlocks()) != 1 {
		t.Fatalf("after first sync, b owned blocks = %d, want 1", len(b.ownedBlocks()))
	}

	// The source event is deleted upstream.
	a.mu.Lock()
	delete(a.events, "real-a-1")
	a.mu.Unlock()

	if err := engine.SyncOnce(context.Background()); err != nil {
		t.Fatalf("second SyncOnce() error = %v, want nil", err)
	}
	if len(b.ownedBlocks()) != 0 {
		t.Errorf("after source deletion, b owned blocks = %d, want 0 (should be GC'd)", len(b.ownedBlocks()))
	}
}

func TestSyncOnce_FailedAccountExcludedButOthersStillSync(t *testing.T) {
	a := newFakeCalendarClient()
	b := newFakeCalendarClient()
	c := newFakeCalendarClient()

	a.seed("real-a-1", realEventIn("real-a-1", 24*time.Hour, time.Hour))
	b.failList = errors.New("simulated: token expired")

	engine := newTestEngine(
		Account{Name: "a", CalendarID: "primary", Client: a},
		Account{Name: "b", CalendarID: "primary", Client: b},
		Account{Name: "c", CalendarID: "primary", Client: c},
	)

	err := engine.SyncOnce(context.Background())
	if err == nil {
		t.Fatal("SyncOnce() error = nil, want an error reporting the failed account")
	}

	// c is healthy and should still receive a's block even though b failed.
	cBlocks := c.ownedBlocks()
	if len(cBlocks) != 1 {
		t.Fatalf("account c owned blocks = %d, want 1 (should sync despite b's failure)", len(cBlocks))
	}

	// b failed to fetch, so it must receive no blocks and must not be
	// written to at all this pass.
	if len(b.events) != 0 {
		t.Errorf("account b events = %d, want 0 (failed account must not be written to)", len(b.events))
	}
}

func TestSyncOnce_FailedSourceAccountDoesNotTriggerGC(t *testing.T) {
	a := newFakeCalendarClient()
	b := newFakeCalendarClient()

	a.seed("real-a-1", realEventIn("real-a-1", 24*time.Hour, time.Hour))

	engine := newTestEngine(
		Account{Name: "a", CalendarID: "primary", Client: a},
		Account{Name: "b", CalendarID: "primary", Client: b},
	)

	if err := engine.SyncOnce(context.Background()); err != nil {
		t.Fatalf("first SyncOnce() error = %v, want nil", err)
	}
	if len(b.ownedBlocks()) != 1 {
		t.Fatalf("after first sync, b owned blocks = %d, want 1", len(b.ownedBlocks()))
	}

	// Account a now fails to fetch (e.g. token expired) — NOT because the
	// event was deleted. b's block mirroring a's event must survive this
	// pass, since we have no evidence the source event is actually gone.
	a.failList = errors.New("simulated: token expired")

	if err := engine.SyncOnce(context.Background()); err == nil {
		t.Fatal("SyncOnce() error = nil, want error reporting a's failure")
	}
	if len(b.ownedBlocks()) != 1 {
		t.Errorf("after a's fetch failure, b owned blocks = %d, want 1 (must NOT be GC'd on an unrelated fetch failure)", len(b.ownedBlocks()))
	}
}

func TestSyncOnce_UpdatesBlockWhenSourceEventMoves(t *testing.T) {
	a := newFakeCalendarClient()
	b := newFakeCalendarClient()
	a.seed("real-a-1", realEventIn("real-a-1", 24*time.Hour, time.Hour))

	engine := newTestEngine(
		Account{Name: "a", CalendarID: "primary", Client: a},
		Account{Name: "b", CalendarID: "primary", Client: b},
	)

	if err := engine.SyncOnce(context.Background()); err != nil {
		t.Fatalf("first SyncOnce() error = %v, want nil", err)
	}

	// Move the source event to a new time.
	moved := realEventIn("real-a-1", 4*24*time.Hour, time.Hour)
	a.seed("real-a-1", moved)

	if err := engine.SyncOnce(context.Background()); err != nil {
		t.Fatalf("second SyncOnce() error = %v, want nil", err)
	}

	blocks := b.ownedBlocks()
	if len(blocks) != 1 {
		t.Fatalf("b owned blocks = %d, want 1 (moved, not duplicated)", len(blocks))
	}
	if blocks[0].Start.DateTime != moved.Start.DateTime {
		t.Errorf("block start = %q, want the moved time %q", blocks[0].Start.DateTime, moved.Start.DateTime)
	}
}

func TestSyncOnce_FewerThanTwoHealthyAccountsReturnsError(t *testing.T) {
	a := newFakeCalendarClient()
	b := newFakeCalendarClient()
	a.failList = errors.New("simulated failure")
	b.failList = errors.New("simulated failure")

	engine := newTestEngine(
		Account{Name: "a", CalendarID: "primary", Client: a},
		Account{Name: "b", CalendarID: "primary", Client: b},
	)

	if err := engine.SyncOnce(context.Background()); err == nil {
		t.Fatal("SyncOnce() error = nil, want error when fewer than 2 accounts are healthy")
	}
}

// TestSyncOnce_NeverOverwritesUnownedEventWithMatchingProperties is a
// regression test for a real bug CodeRabbit's review caught: the Calendar
// API's privateExtendedProperty filter only guarantees the two queried
// properties match — it says nothing about whether the event is actually
// calendar-bridge-owned. If FindBlockBySource returned any event matching
// those two properties (owned or not), ensureBlock would unconditionally
// overwrite it, which could silently clobber a real user event that
// happened to carry the same property values.
func TestSyncOnce_NeverOverwritesUnownedEventWithMatchingProperties(t *testing.T) {
	a := newFakeCalendarClient()
	b := newFakeCalendarClient()
	a.seed("real-a-1", realEventIn("real-a-1", 24*time.Hour, time.Hour))

	// A real, unowned event on b that happens to carry the exact private
	// properties a legitimate block would use for this source. This must
	// never be matched/returned by FindBlockBySource, and must survive
	// SyncOnce completely untouched.
	imposter := &calendar.Event{
		Id:      "b-imposter",
		Summary: "Someone else's real meeting",
		Start:   &calendar.EventDateTime{DateTime: time.Now().Add(2 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"},
		End:     &calendar.EventDateTime{DateTime: time.Now().Add(3 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"},
		ExtendedProperties: &calendar.EventExtendedProperties{
			Private: map[string]string{
				// Deliberately omits ownerKey/ownerValue — an unowned
				// event that coincidentally carries the source properties
				// calendar-bridge would use to look up a block for a's
				// event.
				sourceAccountKey:  "a",
				sourceCalendarKey: "primary",
				sourceEventKey:    "real-a-1",
			},
		},
	}
	b.seed("b-imposter", imposter)

	if err := engine(a, b).SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce() error = %v, want nil", err)
	}

	b.mu.Lock()
	got, stillThere := b.events["b-imposter"]
	b.mu.Unlock()
	if !stillThere {
		t.Fatal("imposter event was deleted, want it left untouched")
	}
	if got.Summary != "Someone else's real meeting" {
		t.Errorf("imposter event was overwritten: summary = %q, want unchanged", got.Summary)
	}
	if isOwnedBlock(got) {
		t.Error("imposter event became owned after sync, want it to remain a real, unowned event")
	}

	// calendar-bridge should have created a *separate*, properly-owned
	// block for a's event rather than reusing the imposter.
	owned := b.ownedBlocks()
	if len(owned) != 1 {
		t.Fatalf("b owned blocks = %d, want 1 (a new block, distinct from the imposter)", len(owned))
	}
	if owned[0].Id == "b-imposter" {
		t.Error("the owned block has the imposter's ID — it was reused instead of creating a new one")
	}
}

func engine(clients ...*fakeCalendarClient) *Engine {
	accounts := make([]Account, len(clients))
	names := []string{"a", "b", "c", "d", "e"}
	for i, c := range clients {
		accounts[i] = Account{Name: names[i], CalendarID: "primary", Client: c}
	}
	return newTestEngine(accounts...)
}
