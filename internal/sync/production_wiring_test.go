package sync

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	calendar "google.golang.org/api/calendar/v3"
)

// The engine in production does not talk to the Google client directly. It
// talks through the same stack cmd/calendar-bridge builds:
//
//	providerClient -> googleProvider -> retryingClient -> googleCalendarClient
//
// These tests exercise that exact composition, because the ownership
// enforcement that makes deletion safe lives in googleProvider — and an engine
// wired straight to the Google client would skip all of it.

// productionStack mirrors buildEngine's client composition over a fake.
func productionStack(f CalendarClient, title string) CalendarClient {
	retrying := NewRetryingClient(f, RetryPolicy{MaxAttempts: 2, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond}, newTestLogger(), "test")
	return NewProviderClient(NewGoogleProvider(retrying), title)
}

func productionHarness(t *testing.T, names ...string) *harness {
	t.Helper()
	h := &harness{fakes: make(map[string]*fakeCalendarClient, len(names))}
	accounts := make([]Account, 0, len(names))
	for _, n := range names {
		f := newFakeCalendarClient()
		h.fakes[n] = f
		accounts = append(accounts, Account{Name: n, CalendarID: "primary", Client: productionStack(f, "Busy (calendar-bridge)")})
	}
	h.engine = &Engine{
		Accounts:      accounts,
		BlockTitle:    "Busy (calendar-bridge)",
		LookaheadDays: 30,
		Logger:        newTestLogger(),
		Now:           fixedClock(baseTime),
	}
	return h
}

func TestProductionWiring_PropagatesAndCollects(t *testing.T) {
	h := productionHarness(t, "personal", "work-acme")
	h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))

	res := h.run(t)
	if res.Created != 1 {
		t.Fatalf("Created = %d, want 1", res.Created)
	}
	blocks := h.fakes["work-acme"].ownedBlocks()
	if len(blocks) != 1 {
		t.Fatalf("want 1 block, got %d", len(blocks))
	}
	if blocks[0].Summary != "Busy (calendar-bridge)" {
		t.Errorf("block title = %q", blocks[0].Summary)
	}

	// Source removed => block collected, through the provider's checked delete.
	h.fakes["personal"].remove("evt-1")
	res = h.run(t)
	if res.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1", res.Deleted)
	}
	if got := len(h.fakes["work-acme"].ownedBlocks()); got != 0 {
		t.Errorf("want the block collected, got %d", got)
	}
}

func TestProductionWiring_SecondPassPerformsNoWrites(t *testing.T) {
	h := productionHarness(t, "personal", "work-acme")
	h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))
	h.run(t)

	for _, f := range h.fakes {
		f.resetCounts()
	}
	h.run(t)
	for name, f := range h.fakes {
		if got := f.writes(); got != 0 {
			t.Errorf("%s made %d writes on the second pass through the production stack, want 0", name, got)
		}
	}
}

func TestProductionWiring_NoSyncLoopAcrossThreeAccounts(t *testing.T) {
	h := productionHarness(t, "personal", "work-acme", "work-other")
	h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))

	for range 3 {
		h.run(t)
	}
	for _, name := range []string{"work-acme", "work-other"} {
		if got := len(h.fakes[name].ownedBlocks()); got != 1 {
			t.Errorf("%s has %d owned blocks, want 1", name, got)
		}
	}
	if got := len(h.fakes["personal"].ownedBlocks()); got != 0 {
		t.Errorf("the source account grew %d blocks, want 0", got)
	}
}

// The load-bearing one: an event that lost its owner tag between the list and
// the delete must NOT be deleted. Through the production stack, googleProvider
// re-reads the target and refuses. Wired straight to the Google client this
// would fall through to an ETag-only guard.
func TestProductionWiring_RefusesToDeleteAnEventThatLostItsOwnerTag(t *testing.T) {
	f := newFakeCalendarClient()
	client := productionStack(f, "Busy")

	f.seed("blk-1", &calendar.Event{
		Summary: "Busy",
		Start:   &calendar.EventDateTime{DateTime: baseTime.Format(time.RFC3339)},
		End:     &calendar.EventDateTime{DateTime: baseTime.Add(time.Hour).Format(time.RFC3339)},
		// No ownership tag: as far as the system is concerned, a real event.
	})

	err := client.DeleteEvent(context.Background(), "primary", "blk-1", "")
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("DeleteEvent on an untagged event = %v, want ErrNotOwned", err)
	}
	if f.byID("blk-1") == nil {
		t.Fatal("the untagged event was deleted; a real user event must never be removable through this path")
	}
}

// Insert must refuse an untagged block at the bridge, before it can become an
// orphan nothing can identify or collect.
func TestProductionWiring_RefusesToInsertAnUntaggedBlock(t *testing.T) {
	f := newFakeCalendarClient()
	client := productionStack(f, "Busy")

	_, err := client.InsertEvent(context.Background(), "primary", &calendar.Event{
		Summary: "Busy",
		Start:   &calendar.EventDateTime{DateTime: baseTime.Format(time.RFC3339)},
		End:     &calendar.EventDateTime{DateTime: baseTime.Add(time.Hour).Format(time.RFC3339)},
	})
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("InsertEvent with no ownership tag = %v, want ErrNotOwned", err)
	}
	if got := len(f.events); got != 0 {
		t.Errorf("an untagged block was created anyway (%d events on the calendar)", got)
	}
}

// The free/busy signals must survive the neutral-model round trip, or the
// bridge would silently re-introduce the bug where declined and Free events
// still hold time.
func TestProductionWiring_PreservesFreeAndDeclinedSignals(t *testing.T) {
	h := productionHarness(t, "personal", "work-acme")

	free := at("free-1", 24*time.Hour, time.Hour)
	free.Transparency = "transparent"
	declined := at("declined-1", 48*time.Hour, time.Hour)
	declined.Attendees = []*calendar.EventAttendee{{Self: true, ResponseStatus: "declined"}}
	busy := at("busy-1", 72*time.Hour, time.Hour)

	for _, ev := range []*calendar.Event{free, declined, busy} {
		h.fakes["personal"].seed(ev.Id, ev)
	}

	res := h.run(t)

	if res.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2 — the transparency and response-status signals were lost crossing the Provider seam", res.Skipped)
	}
	blocks := h.fakes["work-acme"].ownedBlocks()
	if len(blocks) != 1 {
		t.Fatalf("want 1 block, got %d", len(blocks))
	}
	if _, _, srcID, _ := sourceIdentity(blocks[0]); srcID != "busy-1" {
		t.Errorf("block mirrors %q, want busy-1", srcID)
	}
}

// Event content must not survive the bridge either. The neutral Event has no
// content fields at all, so this is structural — but it is the project's
// headline privacy claim and deserves a test that fails loudly if it changes.
func TestProductionWiring_NeutralModelCarriesNoEventContent(t *testing.T) {
	h := productionHarness(t, "personal", "work-acme")
	secret := at("evt-1", 48*time.Hour, time.Hour)
	secret.Summary = "Oncology follow-up"
	secret.Description = "results"
	secret.Location = "St Elsewhere"
	secret.Attendees = []*calendar.EventAttendee{{Email: "dr@example.test"}, {Self: true, ResponseStatus: "accepted"}}
	h.fakes["personal"].seed("evt-1", secret)

	h.run(t)

	blocks := h.fakes["work-acme"].ownedBlocks()
	if len(blocks) != 1 {
		t.Fatalf("want 1 block, got %d", len(blocks))
	}
	rendered := renderEvent(blocks[0])
	for _, needle := range []string{"Oncology", "results", "Elsewhere", "dr@example.test"} {
		if strings.Contains(rendered, needle) {
			t.Errorf("block leaks %q across the account boundary", needle)
		}
	}
}

// A write that fails after passing every ownership check must surface, and must
// leave nothing partial or untagged behind. Exercised through the production
// stack, where the failure has to travel back up through the provider bridge
// and the retry layer.
func TestProductionWiring_WriteFailuresSurfaceAndLeaveNoPartialBlock(t *testing.T) {
	t.Run("insert failure leaves no block", func(t *testing.T) {
		h := productionHarness(t, "personal", "work-acme")
		h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))
		h.fakes["work-acme"].failInsert = errTestWrite

		res, err := h.engine.SyncOnce(context.Background())
		if err == nil {
			t.Fatal("a failed insert must surface")
		}
		if res.Created != 0 {
			t.Errorf("Created = %d, want 0", res.Created)
		}
		if got := len(h.fakes["work-acme"].ownedBlocks()); got != 0 {
			t.Errorf("work-acme holds %d blocks after a failed insert, want 0", got)
		}
		// Nothing untagged may be left behind either.
		for _, ev := range h.fakes["work-acme"].allEvents() {
			if !isOwnedBlock(ev) {
				t.Errorf("a non-owned event was created on the destination: %+v", ev)
			}
		}
	})

	t.Run("update failure leaves the block at its previous span", func(t *testing.T) {
		h := productionHarness(t, "personal", "work-acme")
		h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))
		h.run(t)

		before := h.fakes["work-acme"].ownedBlocks()[0].Start.DateTime
		src := h.fakes["personal"].byID("evt-1")
		src.Start.DateTime = baseTime.Add(96 * time.Hour).Format(time.RFC3339)
		src.End.DateTime = baseTime.Add(97 * time.Hour).Format(time.RFC3339)
		h.fakes["work-acme"].failUpdate = errTestWrite

		res, err := h.engine.SyncOnce(context.Background())
		if err == nil {
			t.Fatal("a failed update must surface")
		}
		if res.Updated != 0 {
			t.Errorf("Updated = %d, want 0", res.Updated)
		}
		blocks := h.fakes["work-acme"].ownedBlocks()
		if len(blocks) != 1 {
			t.Fatalf("want the block still present, got %d", len(blocks))
		}
		if blocks[0].Start.DateTime != before {
			t.Errorf("block shows %q after a FAILED update; want %q — the in-memory state must not "+
				"advance past what the API actually accepted", blocks[0].Start.DateTime, before)
		}
		if !isOwnedBlock(blocks[0]) {
			t.Error("the block lost its ownership tag through a failed update")
		}
	})
}
