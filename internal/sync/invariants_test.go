package sync

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
)

// This file covers the sync invariants that had no direct test: no sync loops,
// idempotency, the fetch-window edges, timezone handling, free/declined
// filtering, and the structural guarantee that no event content ever crosses an
// account boundary.
//
// Every test here pins the clock through Engine.Now, so window behaviour is
// asserted rather than inferred from whenever the suite happens to run.

// baseTime is a fixed instant used as "now" throughout. It is deliberately not
// midnight and not UTC-aligned to a day boundary, so an off-by-a-day error
// can't pass by accident.
var baseTime = time.Date(2026, 3, 12, 14, 37, 11, 0, time.UTC)

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// at builds a timed event whose span is expressed relative to baseTime.
func at(id string, startOffset, dur time.Duration) *calendar.Event {
	start := baseTime.Add(startOffset)
	return &calendar.Event{
		Id:      id,
		Summary: "Quarterly planning with Acme",
		Start:   &calendar.EventDateTime{DateTime: start.Format(time.RFC3339), TimeZone: "UTC"},
		End:     &calendar.EventDateTime{DateTime: start.Add(dur).Format(time.RFC3339), TimeZone: "UTC"},
	}
}

// harness wires n fake calendars into an Engine with a pinned clock.
type harness struct {
	engine *Engine
	fakes  map[string]*fakeCalendarClient
}

func newHarness(t *testing.T, names ...string) *harness {
	t.Helper()
	h := &harness{fakes: make(map[string]*fakeCalendarClient, len(names))}
	accounts := make([]Account, 0, len(names))
	for _, n := range names {
		f := newFakeCalendarClient()
		h.fakes[n] = f
		accounts = append(accounts, Account{Name: n, CalendarID: "primary", Client: f})
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

func (h *harness) run(t *testing.T) Result {
	t.Helper()
	res, err := h.engine.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	return res
}

// ---- Invariant 2: no sync loops ----

// A block created on B because of an event on A must never be re-propagated
// from B onward to C. This is the invariant the whole ownership-tagging design
// exists to guarantee, and it had no direct test.
func TestSyncOnce_BlockNeverPropagatesToAThirdCalendar(t *testing.T) {
	h := newHarness(t, "personal", "work-acme", "work-other")
	h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))

	h.run(t)

	// One real event on personal => exactly one block on each of the other two.
	for _, name := range []string{"work-acme", "work-other"} {
		if got := len(h.fakes[name].ownedBlocks()); got != 1 {
			t.Fatalf("%s has %d owned blocks after pass 1, want 1", name, got)
		}
	}
	if got := len(h.fakes["personal"].ownedBlocks()); got != 0 {
		t.Fatalf("the source account has %d owned blocks, want 0", got)
	}

	// Run repeatedly: if a block were ever treated as a real event, each pass
	// would breed more blocks (B's block spawning one on A and C, and so on).
	for pass := 2; pass <= 4; pass++ {
		h.run(t)
		for _, name := range []string{"work-acme", "work-other"} {
			if got := len(h.fakes[name].ownedBlocks()); got != 1 {
				t.Fatalf("after pass %d, %s has %d owned blocks, want 1 — a block was propagated onward", pass, name, got)
			}
		}
		if got := len(h.fakes["personal"].ownedBlocks()); got != 0 {
			t.Fatalf("after pass %d, the source account has %d owned blocks, want 0 — a block echoed back to its source", pass, got)
		}
	}
}

// A block's source identity must always name the ORIGINAL account, never an
// intermediate one. If it ever named the intermediate, GC on the third calendar
// would key off the wrong account's health.
func TestSyncOnce_BlockSourceIdentityNamesTheOriginatingAccount(t *testing.T) {
	h := newHarness(t, "personal", "work-acme", "work-other")
	h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))
	h.run(t)

	for _, name := range []string{"work-acme", "work-other"} {
		blocks := h.fakes[name].ownedBlocks()
		if len(blocks) != 1 {
			t.Fatalf("%s: want 1 block, got %d", name, len(blocks))
		}
		acc, _, evID, ok := sourceIdentity(blocks[0])
		if !ok {
			t.Fatalf("%s: block has an incomplete source identity", name)
		}
		if acc != "personal" || evID != "evt-1" {
			t.Errorf("%s: block source = %s/%s, want personal/evt-1", name, acc, evID)
		}
	}
}

// ---- Invariant 9: idempotency ----

func TestSyncOnce_SecondPassPerformsNoWrites(t *testing.T) {
	h := newHarness(t, "personal", "work-acme")
	h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))
	h.fakes["personal"].seed("evt-2", at("evt-2", 72*time.Hour, 30*time.Minute))
	h.fakes["work-acme"].seed("evt-3", at("evt-3", 96*time.Hour, 2*time.Hour))

	h.run(t) // pass 1 creates the blocks

	for _, f := range h.fakes {
		f.resetCounts()
	}
	res := h.run(t) // pass 2 must be a pure no-op

	for name, f := range h.fakes {
		if got := f.writes(); got != 0 {
			t.Errorf("%s made %d write calls on the second pass, want 0", name, got)
		}
	}
	if res.Created+res.Updated+res.Deleted != 0 {
		t.Errorf("second pass reported created=%d updated=%d deleted=%d, want all zero",
			res.Created, res.Updated, res.Deleted)
	}
}

// The in-memory block index exists so a steady-state pass costs one list call
// per account and nothing else. Without it, every (event x target account) pair
// costs a FindBlockBySource round trip.
func TestSyncOnce_SteadyStatePassMakesNoPerEventLookups(t *testing.T) {
	h := newHarness(t, "personal", "work-acme")
	for i, off := range []time.Duration{24, 48, 72, 96, 120} {
		id := string(rune('a'+i)) + "-evt"
		h.fakes["personal"].seed(id, at(id, off*time.Hour, time.Hour))
	}

	h.run(t)
	for _, f := range h.fakes {
		f.resetCounts()
	}
	h.run(t)

	if got := h.fakes["work-acme"].lookups(); got != 0 {
		t.Errorf("steady-state pass made %d FindBlockBySource calls, want 0 — "+
			"the blocks were all in the fetched window and should have come from the in-memory index", got)
	}
}

// The index covers only the fetch window. A source event that moved INTO the
// window has a block still sitting outside it, so the lookup fallback must
// still run — otherwise the pass creates a duplicate block.
func TestSyncOnce_FallsBackToLookupForABlockOutsideTheWindow(t *testing.T) {
	h := newHarness(t, "personal", "work-acme")

	// A block on work-acme mirroring personal/evt-1, sitting far beyond the
	// 30-day lookahead — where the source event used to be.
	far := baseTime.Add(60 * 24 * time.Hour)
	h.fakes["work-acme"].seed("stale-block", &calendar.Event{
		Summary: "Busy (calendar-bridge)",
		Start:   &calendar.EventDateTime{DateTime: far.Format(time.RFC3339), TimeZone: "UTC"},
		End:     &calendar.EventDateTime{DateTime: far.Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC"},
		ExtendedProperties: &calendar.EventExtendedProperties{Private: map[string]string{
			ownerKey:          ownerValue,
			sourceAccountKey:  "personal",
			sourceCalendarKey: "primary",
			sourceEventKey:    "evt-1",
		}},
	})
	// The source event has moved back into the window.
	h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))

	h.run(t)

	blocks := h.fakes["work-acme"].ownedBlocks()
	if len(blocks) != 1 {
		t.Fatalf("work-acme has %d owned blocks, want 1 — the out-of-window block must be moved, not duplicated", len(blocks))
	}
	// Pin the MECHANISM, not just the end state: it must be the same block,
	// moved, and the fallback lookup must actually have run. Without these, a
	// hypothetical delete-then-insert would satisfy the count and the span.
	if blocks[0].Id != "stale-block" {
		t.Errorf("surviving block is %q, want the seeded stale-block moved rather than replaced", blocks[0].Id)
	}
	if h.fakes["work-acme"].lookups() == 0 {
		t.Error("no FindBlockBySource call was made; the out-of-window fallback did not run")
	}
	wantStart := baseTime.Add(48 * time.Hour).Format(time.RFC3339)
	if blocks[0].Start.DateTime != wantStart {
		t.Errorf("block start = %q, want %q", blocks[0].Start.DateTime, wantStart)
	}
}

// ---- Invariants 5d/5f: free and declined events ----

func TestSyncOnce_DoesNotBlockTimeForFreeOrDeclinedEvents(t *testing.T) {
	free := at("free-1", 24*time.Hour, time.Hour)
	free.Transparency = "transparent"

	declined := at("declined-1", 48*time.Hour, time.Hour)
	declined.Attendees = []*calendar.EventAttendee{
		{Email: "someone-else@example.test", ResponseStatus: "accepted"},
		{Self: true, ResponseStatus: "declined"},
	}

	tentative := at("tentative-1", 72*time.Hour, time.Hour)
	tentative.Attendees = []*calendar.EventAttendee{{Self: true, ResponseStatus: "tentative"}}

	accepted := at("accepted-1", 96*time.Hour, time.Hour)
	accepted.Attendees = []*calendar.EventAttendee{{Self: true, ResponseStatus: "accepted"}}

	needsAction := at("needs-action-1", 120*time.Hour, time.Hour)
	needsAction.Attendees = []*calendar.EventAttendee{{Self: true, ResponseStatus: "needsAction"}}

	plain := at("plain-1", 144*time.Hour, time.Hour) // no attendees at all

	h := newHarness(t, "personal", "work-acme")
	for _, ev := range []*calendar.Event{free, declined, tentative, accepted, needsAction, plain} {
		h.fakes["personal"].seed(ev.Id, ev)
	}

	res := h.run(t)

	// Blocked: tentative, accepted, needsAction, plain. Skipped: free, declined.
	blocked := map[string]bool{}
	for _, b := range h.fakes["work-acme"].ownedBlocks() {
		_, _, srcID, _ := sourceIdentity(b)
		blocked[srcID] = true
	}

	wantBlocked := []string{"tentative-1", "accepted-1", "needs-action-1", "plain-1"}
	wantSkipped := []string{"free-1", "declined-1"}

	for _, id := range wantBlocked {
		if !blocked[id] {
			t.Errorf("%s produced no busy block, but it should hold time", id)
		}
	}
	for _, id := range wantSkipped {
		if blocked[id] {
			t.Errorf("%s produced a busy block; events marked Free and declined invitations must not hold time elsewhere", id)
		}
	}
	if res.Skipped != len(wantSkipped) {
		t.Errorf("Result.Skipped = %d, want %d", res.Skipped, len(wantSkipped))
	}
}

// Marking an existing event Free must remove the block it already produced, not
// just stop creating new ones.
func TestSyncOnce_MarkingAnEventFreeRemovesItsExistingBlock(t *testing.T) {
	h := newHarness(t, "personal", "work-acme")
	ev := at("evt-1", 48*time.Hour, time.Hour)
	h.fakes["personal"].seed("evt-1", ev)

	h.run(t)
	if got := len(h.fakes["work-acme"].ownedBlocks()); got != 1 {
		t.Fatalf("want 1 block after pass 1, got %d", got)
	}

	// The user marks it Free.
	h.fakes["personal"].byID("evt-1").Transparency = "transparent"
	res := h.run(t)

	if got := len(h.fakes["work-acme"].ownedBlocks()); got != 0 {
		t.Errorf("want 0 blocks after the source was marked Free, got %d", got)
	}
	if res.Deleted != 1 {
		t.Errorf("Result.Deleted = %d, want 1", res.Deleted)
	}
}

// ---- Invariant 12: no event content crosses an account boundary ----

func TestSyncOnce_BlockCarriesNoContentFromTheSourceEvent(t *testing.T) {
	h := newHarness(t, "personal", "work-acme")
	secret := at("evt-1", 48*time.Hour, time.Hour)
	secret.Summary = "Divorce lawyer consultation"
	secret.Description = "bring the paperwork"
	secret.Location = "12 Confidential Street"
	secret.Attendees = []*calendar.EventAttendee{
		{Email: "lawyer@example.test", ResponseStatus: "accepted"},
		{Self: true, ResponseStatus: "accepted"},
	}
	secret.HangoutLink = "https://meet.example.test/private-room"
	h.fakes["personal"].seed("evt-1", secret)

	h.run(t)

	blocks := h.fakes["work-acme"].ownedBlocks()
	if len(blocks) != 1 {
		t.Fatalf("want 1 block, got %d", len(blocks))
	}
	b := blocks[0]

	if b.Summary != "Busy (calendar-bridge)" {
		t.Errorf("block summary = %q, want the configured block title", b.Summary)
	}
	if b.Description != "" {
		t.Errorf("block carries a description (%q); event content must never cross accounts", b.Description)
	}
	if b.Location != "" {
		t.Errorf("block carries a location (%q)", b.Location)
	}
	if len(b.Attendees) != 0 {
		t.Errorf("block carries %d attendees; attendee identities must never cross accounts", len(b.Attendees))
	}
	if b.HangoutLink != "" {
		t.Errorf("block carries a conferencing link (%q)", b.HangoutLink)
	}
	// Belt and braces: no field anywhere on the block may contain the source's
	// distinctive strings.
	for _, needle := range []string{"Divorce", "paperwork", "Confidential", "lawyer@", "private-room"} {
		if strings.Contains(renderEvent(b), needle) {
			t.Errorf("block leaks %q from the source event", needle)
		}
	}
	if b.Visibility != "private" {
		t.Errorf("block visibility = %q, want private", b.Visibility)
	}
	if b.Transparency != "opaque" {
		t.Errorf("block transparency = %q, want opaque (it must show as Busy)", b.Transparency)
	}
}

func renderEvent(ev *calendar.Event) string {
	var sb strings.Builder
	sb.WriteString(ev.Summary)
	sb.WriteString("\x00")
	sb.WriteString(ev.Description)
	sb.WriteString("\x00")
	sb.WriteString(ev.Location)
	sb.WriteString("\x00")
	sb.WriteString(ev.HangoutLink)
	for _, a := range ev.Attendees {
		sb.WriteString("\x00")
		sb.WriteString(a.Email)
		sb.WriteString(a.DisplayName)
	}
	if ev.ExtendedProperties != nil {
		// Both maps. Private is where our ownership tags live; Shared is where
		// a future change could plausibly put something copied from the source,
		// and this helper is what backs the no-content-propagation claim.
		for k, v := range ev.ExtendedProperties.Private {
			sb.WriteString("\x00" + k + "=" + v)
		}
		for k, v := range ev.ExtendedProperties.Shared {
			sb.WriteString("\x00" + k + "=" + v)
		}
	}
	return sb.String()
}

// ---- Invariants 6 and 7: fetch-window edges ----

func TestSyncOnce_FetchWindowSpansLookBackBufferToLookahead(t *testing.T) {
	h := newHarness(t, "personal", "work-acme")

	var gotMin, gotMax time.Time
	probe := &windowProbe{inner: h.fakes["personal"], record: func(lo, hi time.Time) { gotMin, gotMax = lo, hi }}
	h.engine.Accounts[0].Client = probe

	h.run(t)

	wantMin := baseTime.Add(-24 * time.Hour)
	wantMax := baseTime.Add(30 * 24 * time.Hour)
	if !gotMin.Equal(wantMin) {
		t.Errorf("timeMin = %v, want %v (a 24h look-back buffer keeps in-progress events visible)", gotMin, wantMin)
	}
	if !gotMax.Equal(wantMax) {
		t.Errorf("timeMax = %v, want %v (lookahead_days = 30)", gotMax, wantMax)
	}
}

func TestSyncOnce_LookaheadBoundary(t *testing.T) {
	cases := []struct {
		name        string
		startOffset time.Duration
		dur         time.Duration
		wantBlock   bool
	}{
		{"well inside the window", 24 * time.Hour, time.Hour, true},
		{"in progress right now", -30 * time.Minute, time.Hour, true},
		{"finished 12h ago, inside the look-back buffer", -13 * time.Hour, time.Hour, true},
		{"finished 30h ago, before the look-back buffer", -31 * time.Hour, time.Hour, false},
		{"starts one hour before the lookahead edge", 30*24*time.Hour - time.Hour, 30 * time.Minute, true},
		{"starts one hour after the lookahead edge", 30*24*time.Hour + time.Hour, time.Hour, false},
		{"straddles the lookahead edge", 30*24*time.Hour - time.Hour, 4 * time.Hour, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "personal", "work-acme")
			h.fakes["personal"].seed("evt-1", at("evt-1", tc.startOffset, tc.dur))

			h.run(t)

			got := len(h.fakes["work-acme"].ownedBlocks())
			want := 0
			if tc.wantBlock {
				want = 1
			}
			if got != want {
				t.Errorf("%d blocks created, want %d", got, want)
			}
		})
	}
}

// A source event pushed beyond the lookahead horizon must have its block
// collected: the block still sits at the old (in-window) time, so GC can see it,
// and its source is no longer live.
func TestSyncOnce_SourcePushedBeyondLookaheadLosesItsBlock(t *testing.T) {
	h := newHarness(t, "personal", "work-acme")
	h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))
	h.run(t)
	if got := len(h.fakes["work-acme"].ownedBlocks()); got != 1 {
		t.Fatalf("want 1 block, got %d", got)
	}

	// The meeting slips to next quarter, far past lookahead_days.
	src := h.fakes["personal"].byID("evt-1")
	src.Start.DateTime = baseTime.Add(90 * 24 * time.Hour).Format(time.RFC3339)
	src.End.DateTime = baseTime.Add(90*24*time.Hour + time.Hour).Format(time.RFC3339)

	res := h.run(t)

	if got := len(h.fakes["work-acme"].ownedBlocks()); got != 0 {
		t.Errorf("want 0 blocks once the source moved beyond the lookahead horizon, got %d", got)
	}
	if res.Deleted != 1 {
		t.Errorf("Result.Deleted = %d, want 1", res.Deleted)
	}
}

// Documenting a deliberate limitation rather than asserting a fix: once BOTH a
// block and its source have aged out of the fetch window (past the 24h
// look-back buffer), the block is invisible to GC and is left in place. That is
// harmless — it is a busy block in the past — but it means "every block is
// eventually collected" is not true, and a clean-uninstall path cannot rely on
// GC alone.
func TestSyncOnce_BlocksThatAgedOutOfTheWindowAreLeftAlone(t *testing.T) {
	h := newHarness(t, "personal", "work-acme")
	h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))
	h.run(t)

	// Four days on, both the event and its block are older than the look-back
	// buffer, so neither is fetched.
	h.engine.Now = fixedClock(baseTime.Add(4 * 24 * time.Hour))
	res := h.run(t)

	if got := len(h.fakes["work-acme"].ownedBlocks()); got != 1 {
		t.Errorf("want the aged-out block left in place, got %d blocks", got)
	}
	if res.Deleted != 0 {
		t.Errorf("Result.Deleted = %d, want 0 — a block outside the fetch window is never seen, let alone collected", res.Deleted)
	}
}

// windowProbe records the [timeMin, timeMax) a pass asks for, delegating
// everything else.
type windowProbe struct {
	inner  CalendarClient
	record func(timeMin, timeMax time.Time)
}

func (w *windowProbe) ListEvents(ctx context.Context, calendarID string, timeMin, timeMax time.Time) ([]*calendar.Event, error) {
	w.record(timeMin, timeMax)
	return w.inner.ListEvents(ctx, calendarID, timeMin, timeMax)
}

func (w *windowProbe) FindBlockBySource(ctx context.Context, calendarID, a, b string) (*calendar.Event, error) {
	return w.inner.FindBlockBySource(ctx, calendarID, a, b)
}

func (w *windowProbe) GetEvent(ctx context.Context, calendarID, id string) (*calendar.Event, error) {
	return w.inner.GetEvent(ctx, calendarID, id)
}

func (w *windowProbe) InsertEvent(ctx context.Context, calendarID string, ev *calendar.Event) (*calendar.Event, error) {
	return w.inner.InsertEvent(ctx, calendarID, ev)
}

func (w *windowProbe) UpdateEvent(ctx context.Context, calendarID, id string, ev *calendar.Event, ifMatchETag string) (*calendar.Event, error) {
	return w.inner.UpdateEvent(ctx, calendarID, id, ev, ifMatchETag)
}

func (w *windowProbe) DeleteEvent(ctx context.Context, calendarID, id, etag string) error {
	return w.inner.DeleteEvent(ctx, calendarID, id, etag)
}

// ---- Invariant 8: timezone handling ----

// Two accounts in different zones must still agree on the instant. The block
// copies the source's exact start/end/zone strings, so the block's wall-clock
// text differs from the destination's local zone — and that is correct: Google
// renders it at the right instant either way.
func TestSyncOnce_PreservesSourceZoneOnTheBlock(t *testing.T) {
	h := newHarness(t, "sao-paulo", "berlin")

	// 09:00 in São Paulo (UTC-3) on a fixed date.
	sp, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	start := time.Date(2026, 3, 20, 9, 0, 0, 0, sp)
	h.fakes["sao-paulo"].seed("evt-1", &calendar.Event{
		Id:      "evt-1",
		Summary: "Standup",
		Start:   &calendar.EventDateTime{DateTime: start.Format(time.RFC3339), TimeZone: "America/Sao_Paulo"},
		End:     &calendar.EventDateTime{DateTime: start.Add(time.Hour).Format(time.RFC3339), TimeZone: "America/Sao_Paulo"},
	})

	h.run(t)

	blocks := h.fakes["berlin"].ownedBlocks()
	if len(blocks) != 1 {
		t.Fatalf("want 1 block on berlin, got %d", len(blocks))
	}
	b := blocks[0]
	if b.Start.TimeZone != "America/Sao_Paulo" {
		t.Errorf("block TimeZone = %q, want the source's zone preserved verbatim", b.Start.TimeZone)
	}
	gotStart, err := time.Parse(time.RFC3339, b.Start.DateTime)
	if err != nil {
		t.Fatalf("block start is not RFC3339: %v", err)
	}
	if !gotStart.Equal(start) {
		t.Errorf("block start = %v, want the same instant as the source (%v)", gotStart, start)
	}

	// And a second pass must not see the copied span as "changed" and rewrite it.
	h.fakes["berlin"].resetCounts()
	h.run(t)
	if got := h.fakes["berlin"].writes(); got != 0 {
		t.Errorf("second pass made %d writes on a cross-zone block; the span comparison is unstable", got)
	}
}

// A block whose span crosses a DST transition must survive a second pass
// unchanged, for the same reason.
func TestSyncOnce_DSTTransitionIsStableAcrossPasses(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	// 2026-03-08 01:30 EST -> the DST jump is at 02:00 local.
	start := time.Date(2026, 3, 8, 1, 30, 0, 0, ny)

	h := newHarness(t, "personal", "work-acme")
	h.engine.Now = fixedClock(start.Add(-24 * time.Hour))
	h.fakes["personal"].seed("evt-1", &calendar.Event{
		Id:    "evt-1",
		Start: &calendar.EventDateTime{DateTime: start.Format(time.RFC3339), TimeZone: "America/New_York"},
		End:   &calendar.EventDateTime{DateTime: start.Add(2 * time.Hour).Format(time.RFC3339), TimeZone: "America/New_York"},
	})

	h.run(t)
	h.fakes["work-acme"].resetCounts()
	h.run(t)

	if got := h.fakes["work-acme"].writes(); got != 0 {
		t.Errorf("second pass made %d writes on a block spanning a DST transition, want 0", got)
	}
	if got := len(h.fakes["work-acme"].ownedBlocks()); got != 1 {
		t.Errorf("want exactly 1 block, got %d", got)
	}
}

// ---- Invariant 5b/5c and title drift ----

func TestSyncOnce_SourceEventEdits(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(ev *calendar.Event)
		// wantDeleted means the block should be collected rather than moved.
		wantDeleted bool
	}{
		{
			name: "shortened",
			mutate: func(ev *calendar.Event) {
				ev.End.DateTime = baseTime.Add(48*time.Hour + 15*time.Minute).Format(time.RFC3339)
			},
		},
		{
			name: "lengthened",
			mutate: func(ev *calendar.Event) {
				ev.End.DateTime = baseTime.Add(48*time.Hour + 4*time.Hour).Format(time.RFC3339)
			},
		},
		{
			name: "moved to a different day",
			mutate: func(ev *calendar.Event) {
				ev.Start.DateTime = baseTime.Add(120 * time.Hour).Format(time.RFC3339)
				ev.End.DateTime = baseTime.Add(121 * time.Hour).Format(time.RFC3339)
			},
		},
		{
			name:        "cancelled upstream",
			mutate:      func(ev *calendar.Event) { ev.Status = "cancelled" },
			wantDeleted: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "personal", "work-acme")
			h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))
			h.run(t)

			tc.mutate(h.fakes["personal"].byID("evt-1"))
			res := h.run(t)

			blocks := h.fakes["work-acme"].ownedBlocks()
			if tc.wantDeleted {
				if len(blocks) != 0 {
					t.Fatalf("want the block collected, got %d blocks", len(blocks))
				}
				if res.Deleted != 1 {
					t.Errorf("Result.Deleted = %d, want 1", res.Deleted)
				}
				return
			}

			if len(blocks) != 1 {
				t.Fatalf("want exactly 1 block (moved, not duplicated), got %d", len(blocks))
			}
			src := h.fakes["personal"].byID("evt-1")
			if blocks[0].Start.DateTime != src.Start.DateTime || blocks[0].End.DateTime != src.End.DateTime {
				t.Errorf("block span = %s..%s, want %s..%s",
					blocks[0].Start.DateTime, blocks[0].End.DateTime, src.Start.DateTime, src.End.DateTime)
			}
			if res.Updated != 1 {
				t.Errorf("Result.Updated = %d, want 1", res.Updated)
			}
		})
	}
}

// Changing block_title must re-title the blocks that already exist, not only
// stamp the new title on future ones.
func TestSyncOnce_RetitlesExistingBlocksWhenBlockTitleChanges(t *testing.T) {
	h := newHarness(t, "personal", "work-acme")
	h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))
	h.run(t)

	h.engine.BlockTitle = "Unavailable"
	res := h.run(t)

	blocks := h.fakes["work-acme"].ownedBlocks()
	if len(blocks) != 1 {
		t.Fatalf("want 1 block, got %d", len(blocks))
	}
	if blocks[0].Summary != "Unavailable" {
		t.Errorf("block title = %q, want %q", blocks[0].Summary, "Unavailable")
	}
	if res.Updated != 1 {
		t.Errorf("Result.Updated = %d, want 1", res.Updated)
	}

	// And it must settle: a third pass rewrites nothing.
	h.fakes["work-acme"].resetCounts()
	h.run(t)
	if got := h.fakes["work-acme"].writes(); got != 0 {
		t.Errorf("third pass made %d writes, want 0", got)
	}
}

// ---- Result plumbing ----

func TestSyncOnce_ResultReportsAccountHealth(t *testing.T) {
	h := newHarness(t, "personal", "work-acme", "work-other")
	h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))
	h.fakes["work-other"].failList = errTestFetch

	res, err := h.engine.SyncOnce(context.Background())
	if err == nil {
		t.Fatal("want an error reporting the failed account")
	}
	if len(res.HealthyAccounts) != 2 {
		t.Errorf("HealthyAccounts = %v, want 2 entries", res.HealthyAccounts)
	}
	if len(res.FailedAccounts) != 1 || res.FailedAccounts[0] != "work-other" {
		t.Errorf("FailedAccounts = %v, want [work-other]", res.FailedAccounts)
	}
	if res.Created != 1 {
		t.Errorf("Created = %d, want 1 (the two healthy accounts still sync)", res.Created)
	}
	if res.Started.IsZero() || res.Finished.IsZero() {
		t.Error("Result must carry Started/Finished even on a partial pass")
	}
}

// A zero-value Engine must not panic for want of a logger.
func TestSyncOnce_NilLoggerDoesNotPanic(t *testing.T) {
	a, b := newFakeCalendarClient(), newFakeCalendarClient()
	a.seed("evt-1", realEventIn("evt-1", 48*time.Hour, time.Hour))
	e := &Engine{
		Accounts: []Account{
			{Name: "a", CalendarID: "primary", Client: a},
			{Name: "b", CalendarID: "primary", Client: b},
		},
		BlockTitle:    "Busy",
		LookaheadDays: 30,
		// Logger deliberately nil.
	}
	if _, err := e.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce with a nil Logger: %v", err)
	}
}

var errTestFetch = &testError{"simulated fetch failure"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// ---- classification edges ----

// A source event that is skipped (Free/declined) on an account that then FAILS
// to fetch must not have its blocks collected elsewhere. The skip and the
// failure are different reasons for an event to be absent from the live set,
// and only one of them means "it is gone".
func TestSyncOnce_FailedSourceAccountPreservesBlocksEvenForSkippedEvents(t *testing.T) {
	h := newHarness(t, "personal", "work-acme", "work-globex")
	h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))
	h.run(t)

	if got := len(h.fakes["work-acme"].ownedBlocks()); got != 1 {
		t.Fatalf("setup: want 1 block on work-acme, got %d", got)
	}

	// personal now fails to fetch entirely.
	h.fakes["personal"].failList = errTestFetch
	res, err := h.engine.SyncOnce(context.Background())
	if err == nil {
		t.Fatal("want an error for the failed account")
	}
	if res.Deleted != 0 {
		t.Errorf("Deleted = %d, want 0 — a block must never be collected because its source account failed", res.Deleted)
	}
	if got := len(h.fakes["work-acme"].ownedBlocks()); got != 1 {
		t.Errorf("work-acme has %d blocks, want the block preserved", got)
	}
}

// An owned block that is ALSO transparent must still be classified as owned.
// Classification order matters: checking transparency first would drop our own
// block out of the owned set, making it invisible to GC forever.
func TestSyncOnce_OwnedBlockThatIsAlsoTransparentStaysOwned(t *testing.T) {
	h := newHarness(t, "personal", "work-acme")
	start := baseTime.Add(24 * time.Hour)
	blk := &calendar.Event{
		Summary:      "Busy (calendar-bridge)",
		Transparency: "opaque",
		Visibility:   "private",
		Start:        &calendar.EventDateTime{DateTime: start.Format(time.RFC3339), TimeZone: "UTC"},
		End:          &calendar.EventDateTime{DateTime: start.Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC"},
		ExtendedProperties: &calendar.EventExtendedProperties{Private: map[string]string{
			ownerKey:          ownerValue,
			sourceAccountKey:  "personal",
			sourceCalendarKey: "primary",
			sourceEventKey:    "gone-1",
		}},
	}
	blk.Transparency = "transparent" // someone edited it in the UI
	h.fakes["work-acme"].seed("blk-1", blk)
	h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))

	res := h.run(t)

	// Its source is gone, so it must be collected — which only happens if it
	// was classified as owned despite being transparent.
	if res.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1 — a transparent owned block must still be collected", res.Deleted)
	}
}

// A real event carrying a source identity but NO owner tag must be propagated
// as a normal event, not treated as one of ours.
func TestSyncOnce_SourceTaggedButUnownedEventIsTreatedAsReal(t *testing.T) {
	h := newHarness(t, "personal", "work-acme")
	impostor := at("evt-1", 48*time.Hour, time.Hour)
	impostor.ExtendedProperties = &calendar.EventExtendedProperties{Private: map[string]string{
		sourceAccountKey:  "work-acme",
		sourceCalendarKey: "primary",
		sourceEventKey:    "whatever",
		// no ownerKey
	}}
	h.fakes["personal"].seed("evt-1", impostor)

	res := h.run(t)

	if res.Created != 1 {
		t.Errorf("Created = %d, want 1 — an untagged event is a real event and must propagate", res.Created)
	}
	if got := len(h.fakes["personal"].ownedBlocks()); got != 0 {
		t.Errorf("the impostor was counted as an owned block")
	}
}

// ---- write-failure paths ----
//
// The fake supports failInsert / failUpdate / failDelete, but nothing exercised
// them: every other test drives successful writes or pre-write refusals. These
// cover what happens when a write is accepted for dispatch and then fails.

func TestSyncOnce_WriteFailuresSurfaceAndLeaveStateIntact(t *testing.T) {
	cases := []struct {
		name string
		// arrange seeds the fixture and injects the failure; it returns a
		// function asserting the destination is unchanged afterwards.
		arrange func(t *testing.T, h *harness) func(t *testing.T)
	}{
		{
			name: "insert failure",
			arrange: func(t *testing.T, h *harness) func(*testing.T) {
				h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))
				h.fakes["work-acme"].failInsert = errTestWrite
				return func(t *testing.T) {
					if got := len(h.fakes["work-acme"].ownedBlocks()); got != 0 {
						t.Errorf("work-acme has %d blocks after a failed insert, want 0", got)
					}
				}
			},
		},
		{
			name: "update failure leaves the previous span",
			arrange: func(t *testing.T, h *harness) func(*testing.T) {
				h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))
				h.run(t) // create the block
				// Move the source, then make the update fail.
				src := h.fakes["personal"].byID("evt-1")
				src.Start.DateTime = baseTime.Add(96 * time.Hour).Format(time.RFC3339)
				src.End.DateTime = baseTime.Add(97 * time.Hour).Format(time.RFC3339)
				h.fakes["work-acme"].failUpdate = errTestWrite

				before := h.fakes["work-acme"].ownedBlocks()[0].Start.DateTime
				return func(t *testing.T) {
					blocks := h.fakes["work-acme"].ownedBlocks()
					if len(blocks) != 1 {
						t.Fatalf("want the block still present, got %d", len(blocks))
					}
					if blocks[0].Start.DateTime != before {
						t.Errorf("block moved to %q despite the update failing; want it left at %q",
							blocks[0].Start.DateTime, before)
					}
				}
			},
		},
		{
			name: "delete failure leaves the stale block",
			arrange: func(t *testing.T, h *harness) func(*testing.T) {
				h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))
				h.run(t) // create the block
				h.fakes["personal"].remove("evt-1")
				h.fakes["work-acme"].failDelete = errTestWrite
				return func(t *testing.T) {
					if got := len(h.fakes["work-acme"].ownedBlocks()); got != 1 {
						t.Errorf("work-acme has %d blocks after a failed delete, want the stale one kept", got)
					}
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "personal", "work-acme")
			assertUnchanged := tc.arrange(t, h)

			res, err := h.engine.SyncOnce(context.Background())
			if err == nil {
				t.Fatal("SyncOnce returned nil; a failed write must surface")
			}
			if !strings.Contains(err.Error(), errTestWrite.Error()) {
				t.Errorf("error %v does not carry the underlying write failure", err)
			}
			// A failed write must not be counted as work done.
			if res.Created+res.Updated+res.Deleted != 0 {
				t.Errorf("Result counted a failed write: created=%d updated=%d deleted=%d",
					res.Created, res.Updated, res.Deleted)
			}
			assertUnchanged(t)
		})
	}
}

// One account failing to write must not stop the others. A pass is degraded,
// not abandoned.
func TestSyncOnce_WriteFailureOnOneAccountDoesNotStopTheOthers(t *testing.T) {
	h := newHarness(t, "personal", "work-acme", "work-globex")
	h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))
	h.fakes["work-acme"].failInsert = errTestWrite

	res, err := h.engine.SyncOnce(context.Background())
	if err == nil {
		t.Fatal("want the write failure surfaced")
	}
	if got := len(h.fakes["work-globex"].ownedBlocks()); got != 1 {
		t.Errorf("work-globex has %d blocks, want 1 — a failure on one account must not stop another", got)
	}
	if res.Created != 1 {
		t.Errorf("Created = %d, want 1 (globex succeeded, acme failed)", res.Created)
	}
}

var errTestWrite = &testError{"simulated write failure"}

// ---- lookup and precondition failures ----

// When the in-memory index misses, ensureBlock falls back to a lookup. If that
// lookup FAILS, the engine must surface the error and must NOT insert —
// inserting on an unknown state is how duplicate blocks appear.
//
// Only FindBlockBySource is on this path. GetEvent is reached solely from
// googleProvider.DeleteBlock's pre-delete ownership read, so its failure is
// covered where it is actually called: see TestGoogleProvider_PropagatesClientErrors
// and TestProductionWiring_PreDeleteReadFailureDoesNotDelete.
func TestSyncOnce_FallbackLookupFailureSurfacesWithoutInserting(t *testing.T) {
	h := newHarness(t, "personal", "work-acme")

	// A block for personal/evt-1 far outside the fetch window, so the index
	// cannot cover it and the fallback lookup is forced.
	far := baseTime.Add(60 * 24 * time.Hour)
	h.fakes["work-acme"].seed("stale-block", &calendar.Event{
		Summary: "Busy (calendar-bridge)",
		Start:   &calendar.EventDateTime{DateTime: far.Format(time.RFC3339), TimeZone: "UTC"},
		End:     &calendar.EventDateTime{DateTime: far.Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC"},
		ExtendedProperties: &calendar.EventExtendedProperties{Private: map[string]string{
			ownerKey:          ownerValue,
			sourceAccountKey:  "personal",
			sourceCalendarKey: "primary",
			sourceEventKey:    "evt-1",
		}},
	})
	h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))
	h.fakes["work-acme"].failFind = errTestLookup

	res, err := h.engine.SyncOnce(context.Background())
	if err == nil {
		t.Fatal("a failed fallback lookup must surface")
	}
	if !strings.Contains(err.Error(), errTestLookup.Error()) {
		t.Errorf("error %v does not carry the lookup failure", err)
	}
	if res.Created != 0 {
		t.Errorf("Created = %d, want 0 — never insert on an unknown state", res.Created)
	}
	if got := len(h.fakes["work-acme"].ownedBlocks()); got != 1 {
		t.Errorf("work-acme has %d owned blocks, want the single seeded one — a failed lookup "+
			"must never produce a duplicate", got)
	}
}

// The engine's update is conditional on the ETag it listed. If the block
// changed after the fetch, the update must fail its precondition rather than
// overwrite an event that may no longer be ours.
func TestSyncOnce_UpdateIsConditionalOnTheListedETag(t *testing.T) {
	h := newHarness(t, "personal", "work-acme")
	h.fakes["personal"].seed("evt-1", at("evt-1", 48*time.Hour, time.Hour))
	h.run(t)

	// Give the block an ETag, then move the source so an update is required.
	blk := h.fakes["work-acme"].ownedBlocks()[0]
	blk.Etag = `"etag-current"`
	src := h.fakes["personal"].byID("evt-1")
	src.Start.DateTime = baseTime.Add(96 * time.Hour).Format(time.RFC3339)
	src.End.DateTime = baseTime.Add(97 * time.Hour).Format(time.RFC3339)

	// A concurrent writer touches the block between the list and the update, so
	// the ETag the engine listed is already stale when the write lands.
	h.fakes["work-acme"].etagRaceOnUpdate = `"etag-changed"`

	_, err := h.engine.SyncOnce(context.Background())
	if err == nil {
		t.Fatal("the update should have failed its ETag precondition")
	}
	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) || apiErr.Code != http.StatusPreconditionFailed {
		t.Fatalf("error = %v, want a 412 precondition failure", err)
	}
}

var errTestLookup = &testError{"simulated lookup failure"}
