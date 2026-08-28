package sync

import (
	"context"
	"testing"
	"time"

	calendar "google.golang.org/api/calendar/v3"
)

// extProps builds a Google EventExtendedProperties with the given private map.
func extProps(private map[string]string) *calendar.EventExtendedProperties {
	return &calendar.EventExtendedProperties{Private: private}
}

// TestGoogleProvider_RoundTripsNeutralModel verifies the neutral Provider
// seam works end to end over the existing fake: listing maps google events to
// neutral Events (including ownership), inserting creates a properly-tagged
// owned block, finding returns it, updating preserves ownership, and deleting
// removes it. This is the contract a future Outlook/iCloud Provider must also
// satisfy.
func TestGoogleProvider_RoundTripsNeutralModel(t *testing.T) {
	fake := newFakeCalendarClient()
	fake.seed("real-1", realEventIn("real-1", 24*time.Hour, time.Hour))

	p := NewGoogleProvider(fake)
	ctx := context.Background()

	// List: the seeded real event should map to a neutral, unowned Event.
	events, err := p.ListEvents(ctx, "primary", time.Now().Add(-time.Hour), time.Now().Add(48*time.Hour))
	if err != nil {
		t.Fatalf("ListEvents error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("listed events = %d, want 1", len(events))
	}
	if events[0].Ownership.IsOwned() {
		t.Error("real event reported as owned, want unowned")
	}
	if events[0].ID != "real-1" {
		t.Errorf("event ID = %q, want real-1", events[0].ID)
	}

	// Insert an owned block mirroring the real event.
	own := Ownership{
		Owner:            ownerValue,
		SourceAccount:    "acct-a",
		SourceCalendarID: "primary",
		SourceEventID:    "real-1",
	}
	start := TimeSpan{DateTime: time.Now().Add(24 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}
	end := TimeSpan{DateTime: time.Now().Add(25 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}
	block, err := p.InsertBlock(ctx, "primary", "Busy (test)", start, end, own)
	if err != nil {
		t.Fatalf("InsertBlock error = %v", err)
	}
	if !block.Ownership.IsOwned() {
		t.Error("inserted block not owned")
	}

	// Find it back by source.
	found, err := p.FindOwnedBlock(ctx, "primary", "acct-a", "real-1")
	if err != nil {
		t.Fatalf("FindOwnedBlock error = %v", err)
	}
	if found == nil {
		t.Fatal("FindOwnedBlock returned nil, want the inserted block")
	}

	// Update its time — ownership MUST survive (Google update is full replace).
	newStart := TimeSpan{DateTime: time.Now().Add(72 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}
	newEnd := TimeSpan{DateTime: time.Now().Add(73 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}
	updated, err := p.UpdateBlockTime(ctx, "primary", *found, newStart, newEnd)
	if err != nil {
		t.Fatalf("UpdateBlockTime error = %v", err)
	}
	if updated == nil {
		t.Fatal("UpdateBlockTime returned nil")
	}
	if !updated.Ownership.IsOwned() {
		t.Error("updated block lost ownership tag — full-replace update wiped extended properties")
	}
	if updated.Ownership.SourceEventID != "real-1" {
		t.Errorf("updated block source = %q, want real-1 (ownership metadata must be preserved)", updated.Ownership.SourceEventID)
	}
	if !updated.Start.Equal(newStart) {
		t.Errorf("updated start = %+v, want %+v", updated.Start, newStart)
	}

	// The fake should still hold exactly one owned block (moved, not duplicated).
	if got := len(fake.ownedBlocks()); got != 1 {
		t.Errorf("owned blocks after update = %d, want 1", got)
	}

	// Delete it.
	if err := p.DeleteBlock(ctx, "primary", updated.ID); err != nil {
		t.Fatalf("DeleteBlock error = %v", err)
	}
	if got := len(fake.ownedBlocks()); got != 0 {
		t.Errorf("owned blocks after delete = %d, want 0", got)
	}
}

// TestGoogleProvider_FindOwnedBlockNeverReturnsUnowned confirms the neutral
// FindOwnedBlock inherits the real client's ownership check: an unowned event
// carrying matching source properties is never returned.
func TestGoogleProvider_FindOwnedBlockNeverReturnsUnowned(t *testing.T) {
	fake := newFakeCalendarClient()
	// An imposter: matching source props but NOT owner-tagged.
	imposter := realEventIn("imposter", 2*time.Hour, time.Hour)
	imposter.ExtendedProperties = extProps(map[string]string{
		sourceAccountKey:  "acct-a",
		sourceCalendarKey: "primary",
		sourceEventKey:    "real-1",
		// no ownerKey
	})
	fake.seed("imposter", imposter)

	p := NewGoogleProvider(fake)
	found, err := p.FindOwnedBlock(context.Background(), "primary", "acct-a", "real-1")
	if err != nil {
		t.Fatalf("FindOwnedBlock error = %v", err)
	}
	if found != nil {
		t.Error("FindOwnedBlock returned an unowned imposter event, want nil")
	}
}

// TestEngine_RunsThroughProviderBridge proves the neutral seam is functional
// end to end: Engine -> providerClient -> googleProvider -> fake. If a future
// Outlook/iCloud Provider is wrapped the same way, the unchanged Engine drives
// it. Mirrors TestSyncOnce_PropagatesBusyBlockBothWays but through the bridge.
func TestEngine_RunsThroughProviderBridge(t *testing.T) {
	fakeA := newFakeCalendarClient()
	fakeB := newFakeCalendarClient()
	fakeA.seed("real-a-1", realEventIn("real-a-1", 24*time.Hour, time.Hour))

	// Wrap each fake as: fake -> Provider -> CalendarClient bridge.
	clientA := NewProviderClient(NewGoogleProvider(fakeA), "Busy (bridge)")
	clientB := NewProviderClient(NewGoogleProvider(fakeB), "Busy (bridge)")

	eng := &Engine{
		Accounts: []Account{
			{Name: "a", CalendarID: "primary", Client: clientA},
			{Name: "b", CalendarID: "primary", Client: clientB},
		},
		BlockTitle:    "Busy (bridge)",
		LookaheadDays: 30,
		Logger:        newTestLogger(),
	}

	if err := eng.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce through bridge error = %v", err)
	}

	blocks := fakeB.ownedBlocks()
	if len(blocks) != 1 {
		t.Fatalf("b owned blocks via bridge = %d, want 1", len(blocks))
	}
	acc, _, evID, ok := sourceIdentity(blocks[0])
	if !ok || acc != "a" || evID != "real-a-1" {
		t.Errorf("bridged block source = (%q,%q,ok=%v), want (a, real-a-1, true)", acc, evID, ok)
	}

	// Move the source, sync again: block must move, not duplicate, and keep
	// its ownership through the neutral update path.
	fakeA.seed("real-a-1", realEventIn("real-a-1", 5*24*time.Hour, time.Hour))
	if err := eng.SyncOnce(context.Background()); err != nil {
		t.Fatalf("second SyncOnce through bridge error = %v", err)
	}
	blocks = fakeB.ownedBlocks()
	if len(blocks) != 1 {
		t.Fatalf("b owned blocks after move via bridge = %d, want 1 (moved not duplicated)", len(blocks))
	}
}
