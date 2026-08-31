// Package sync implements the busy-block propagation logic between
// multiple Google Calendar accounts.
package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	calendar "google.golang.org/api/calendar/v3"
)

// ownerKey and sourceEventKey are the extendedProperties keys used to tag
// blocks that calendar-bridge created, and to remember which source event
// each block mirrors. These are the only signal that lets the sync engine
// tell "a real event a human made" apart from "a block calendar-bridge
// made", so every block calendar-bridge writes must carry both.
const (
	ownerKey          = "calendarBridgeOwner"
	ownerValue        = "calendar-bridge"
	sourceAccountKey  = "calendarBridgeSourceAccount"
	sourceEventKey    = "calendarBridgeSourceEventID"
	sourceCalendarKey = "calendarBridgeSourceCalendarID"
)

// lookBackBuffer is how far behind "now" the fetch window starts, so an event
// already in progress when a pass runs is still seen and still has a block.
const lookBackBuffer = 24 * time.Hour

// Account pairs a Calendar API client with the metadata sync needs to
// address it. Client is a CalendarClient (interface), not a concrete
// *calendar.Service, so tests can substitute a fake.
type Account struct {
	Name       string
	CalendarID string
	Client     CalendarClient
}

// Engine propagates busy blocks across a set of accounts.
type Engine struct {
	Accounts      []Account
	BlockTitle    string
	LookaheadDays int
	Logger        *slog.Logger

	// Now returns the current time. Nil means time.Now. Injecting it lets
	// tests pin the fetch window, so lookahead-boundary, look-back-buffer,
	// DST and clock-skew behaviour are assertable instead of depending on
	// when the suite happens to run.
	Now func() time.Time
}

func (e *Engine) log() *slog.Logger {
	if e.Logger == nil {
		return slog.Default()
	}
	return e.Logger
}

func (e *Engine) now() time.Time {
	if e.Now == nil {
		return time.Now()
	}
	return e.Now()
}

// Result summarises what one pass did. It is returned alongside the error so
// callers (the run loop, the web UI status panel, the metrics exporter) can
// report progress even when the pass was partially unsuccessful.
type Result struct {
	// Started and Finished bound the pass.
	Started, Finished time.Time
	// Created, Updated, Deleted count block writes across all accounts.
	Created, Updated, Deleted int
	// Skipped counts source events deliberately not propagated (marked Free,
	// or declined).
	Skipped int
	// HealthyAccounts and FailedAccounts name the accounts whose fetch
	// succeeded and failed this pass.
	HealthyAccounts []string
	FailedAccounts  []string
}

// Duration is how long the pass took.
func (r Result) Duration() time.Duration { return r.Finished.Sub(r.Started) }

// isOwnedBlock reports whether ev is a block calendar-bridge created
// (rather than a real event a human made), based on extendedProperties.
func isOwnedBlock(ev *calendar.Event) bool {
	if ev.ExtendedProperties == nil || ev.ExtendedProperties.Private == nil {
		return false
	}
	return ev.ExtendedProperties.Private[ownerKey] == ownerValue
}

// sourceIdentity extracts the (account, calendar, event ID) an owned block
// mirrors, for matching against live source events during GC.
func sourceIdentity(ev *calendar.Event) (account, calendarID, eventID string, ok bool) {
	if ev.ExtendedProperties == nil || ev.ExtendedProperties.Private == nil {
		return "", "", "", false
	}
	p := ev.ExtendedProperties.Private
	account = p[sourceAccountKey]
	calendarID = p[sourceCalendarKey]
	eventID = p[sourceEventKey]
	return account, calendarID, eventID, account != "" && calendarID != "" && eventID != ""
}

// selfResponseStatus returns the invitation response of the calendar owner for
// ev, or "" when the event has no attendee marked as self (a personal
// appointment, or an event on a calendar the owner isn't an attendee of).
func selfResponseStatus(ev *calendar.Event) string {
	for _, a := range ev.Attendees {
		if a != nil && a.Self {
			return a.ResponseStatus
		}
	}
	return ""
}

// blocksTime reports whether a real source event should produce a busy block
// on the other accounts.
//
// Two kinds of event deliberately do not:
//
//   - Events marked Free (Google's transparency: "transparent"). The owner has
//     explicitly said this does not consume their time; mirroring it as an
//     opaque Busy block elsewhere would contradict them.
//   - Invitations the owner declined. Declining a meeting and still losing the
//     slot on every other calendar is the single most confusing thing this tool
//     could do.
//
// A tentative invitation DOES still block time. That is a deliberate choice,
// not an oversight: a "maybe" is a real risk of being busy, and the failure
// mode of holding a slot you turn out not to need is much milder than
// double-booking one you do.
func blocksTime(ev *calendar.Event) bool {
	if ev.Transparency == "transparent" {
		return false
	}
	return selfResponseStatus(ev) != "declined"
}

// blockIndex maps a source identity ("account|eventID") to the owned block
// mirroring it on one destination calendar.
type blockIndex map[string]*calendar.Event

func indexKey(srcAccount, srcEventID string) string { return srcAccount + "|" + srcEventID }

// SyncOnce runs a single synchronization pass: for every account, fetch
// real (non-owned) events in the lookahead window, then ensure a matching
// busy block exists on every other account, and remove blocks whose source
// event is gone.
//
// A failure on one account (expired token, transient API error, etc.) does
// not abort the whole pass: SyncOnce logs it, excludes that account from
// this cycle's propagation and GC, and keeps going for every account that
// is healthy. All per-account fetch errors are joined and returned at the
// end so callers can alert on them, but a partial pass still makes forward
// progress for the accounts that did work.
//
// The returned Result is always populated, including when the error is
// non-nil — a partial pass still did real work worth reporting.
func (e *Engine) SyncOnce(ctx context.Context) (res Result, err error) {
	log := e.log()
	res.Started = e.now()
	// Named returns: a plain `res` local would be copied into the return value
	// before this ran, leaving Finished zero on every path.
	defer func() { res.Finished = e.now() }()

	now := res.Started
	timeMin := now.Add(-lookBackBuffer)
	timeMax := now.Add(time.Duration(e.LookaheadDays) * 24 * time.Hour)

	log.Info("sync.pass.start", "accounts", len(e.Accounts), "window_start", timeMin, "window_end", timeMax)

	// 1. Pull real events (and existing owned blocks, for GC) from every account.
	type accountEvents struct {
		real  []*calendar.Event
		owned []*calendar.Event
		// index lets step 2 find an existing block without an API call. It
		// covers only blocks inside the fetch window; ensureBlock falls back to
		// FindBlockBySource on a miss, which is authoritative and unwindowed.
		index blockIndex
	}
	byAccount := make(map[string]accountEvents, len(e.Accounts))
	healthy := make([]Account, 0, len(e.Accounts))
	var fetchErrs []error

	for _, acc := range e.Accounts {
		events, err := acc.Client.ListEvents(ctx, acc.CalendarID, timeMin, timeMax)
		if err != nil {
			log.Error("sync.account.fetch_failed", "account", acc.Name, "error", err,
				"consequence", "excluded from this pass's propagation and GC")
			fetchErrs = append(fetchErrs, fmt.Errorf("listing events for account %s: %w", acc.Name, err))
			res.FailedAccounts = append(res.FailedAccounts, acc.Name)
			continue
		}

		ae := accountEvents{index: make(blockIndex)}
		var skipped int
		for _, ev := range events {
			if ev == nil || ev.Status == "cancelled" {
				continue
			}
			if isOwnedBlock(ev) {
				ae.owned = append(ae.owned, ev)
				if srcAcc, _, srcID, ok := sourceIdentity(ev); ok {
					ae.index[indexKey(srcAcc, srcID)] = ev
				}
				continue
			}
			if !blocksTime(ev) {
				skipped++
				continue
			}
			ae.real = append(ae.real, ev)
		}
		res.Skipped += skipped
		byAccount[acc.Name] = ae
		healthy = append(healthy, acc)
		res.HealthyAccounts = append(res.HealthyAccounts, acc.Name)
		log.Info("sync.account.fetched", "account", acc.Name,
			"real", len(ae.real), "owned_blocks", len(ae.owned), "skipped_free_or_declined", skipped)
	}

	// Accounts that failed to fetch are excluded from propagation and GC
	// entirely this cycle: we don't have their current event state, so we
	// can neither safely push blocks to them nor safely delete blocks that
	// mirror their events elsewhere.
	if len(healthy) < 2 {
		fetchErrs = append(fetchErrs, fmt.Errorf("fewer than 2 healthy accounts (%d), nothing to sync this pass", len(healthy)))
		return res, errors.Join(fetchErrs...)
	}

	// 2. For every (source account, real event) x (target account) pair,
	// ensure a busy block exists.
	var syncErrs []error
	for _, src := range healthy {
		for _, ev := range byAccount[src.Name].real {
			for _, dst := range healthy {
				if dst.Name == src.Name {
					continue
				}
				outcome, err := e.ensureBlock(ctx, src, ev, dst, byAccount[dst.Name].index)
				if err != nil {
					syncErrs = append(syncErrs, fmt.Errorf("ensuring block for %s/%s on %s: %w", src.Name, ev.Id, dst.Name, err))
					continue
				}
				switch outcome {
				case outcomeCreated:
					res.Created++
					log.Info("sync.block.created", "account", dst.Name, "source", indexKey(src.Name, ev.Id))
				case outcomeUpdated:
					res.Updated++
					log.Info("sync.block.updated", "account", dst.Name, "source", indexKey(src.Name, ev.Id))
				}
			}
		}
	}

	// 3. Garbage-collect blocks whose source event no longer exists or is
	// no longer in the live "real" set (deleted / cancelled upstream).
	liveSourceIDs := make(map[string]bool)
	for _, src := range healthy {
		for _, ev := range byAccount[src.Name].real {
			liveSourceIDs[indexKey(src.Name, ev.Id)] = true
		}
	}

	for _, dst := range healthy {
		for _, block := range byAccount[dst.Name].owned {
			srcAccount, _, srcEventID, ok := sourceIdentity(block)
			if !ok {
				continue
			}
			// Only GC when the source account itself was healthy this
			// pass; otherwise liveSourceIDs is incomplete for it and we'd
			// wrongly delete a block mirroring an event we simply failed
			// to fetch this cycle.
			if !accountIsHealthy(healthy, srcAccount) {
				continue
			}
			if liveSourceIDs[indexKey(srcAccount, srcEventID)] {
				continue
			}
			log.Info("sync.block.deleted", "account", dst.Name, "block_id", block.Id,
				"source", indexKey(srcAccount, srcEventID))
			// Conditional on the ETag we listed: if the block changed since
			// the fetch, the delete fails rather than removing a now-altered
			// event. block.Etag may be empty (e.g. test fakes) → unconditional.
			if err := dst.Client.DeleteEvent(ctx, dst.CalendarID, block.Id, block.Etag); err != nil {
				syncErrs = append(syncErrs, fmt.Errorf("deleting stale block %s on %s: %w", block.Id, dst.Name, err))
				continue
			}
			res.Deleted++
		}
	}

	err = errors.Join(append(fetchErrs, syncErrs...)...)
	log.Info("sync.pass.complete",
		"created", res.Created, "updated", res.Updated, "deleted", res.Deleted,
		"skipped", res.Skipped, "healthy_accounts", len(res.HealthyAccounts),
		"failed_accounts", len(res.FailedAccounts), "ok", err == nil)
	return res, err
}

func accountIsHealthy(healthy []Account, name string) bool {
	for _, a := range healthy {
		if a.Name == name {
			return true
		}
	}
	return false
}

type outcome int

const (
	outcomeUnchanged outcome = iota
	outcomeCreated
	outcomeUpdated
)

// ensureBlock makes dst carry exactly one busy block mirroring srcEvent.
//
// idx is the destination's in-memory index of owned blocks seen during this
// pass's fetch. A hit avoids an API call entirely, which is what keeps a
// steady-state pass at N list calls rather than N + (events x accounts)
// lookups. A miss falls back to FindBlockBySource, which is authoritative and
// not limited to the fetch window — necessary because a source event that
// moved INTO the window has a block still sitting outside it.
func (e *Engine) ensureBlock(ctx context.Context, src Account, srcEvent *calendar.Event, dst Account, idx blockIndex) (outcome, error) {
	existing, ok := idx[indexKey(src.Name, srcEvent.Id)]
	if !ok {
		var err error
		existing, err = dst.Client.FindBlockBySource(ctx, dst.CalendarID, src.Name, srcEvent.Id)
		if err != nil {
			return outcomeUnchanged, err
		}
	}

	if existing != nil {
		// Re-assert the times and the title. The title matters because
		// changing block_title in config would otherwise leave every existing
		// block carrying the old one forever.
		timesMatch := timesEqual(existing.Start, srcEvent.Start) && timesEqual(existing.End, srcEvent.End)
		titleMatches := existing.Summary == e.BlockTitle
		if timesMatch && titleMatches {
			return outcomeUnchanged, nil
		}
		// Build the payload as a COPY rather than mutating the block we hold.
		// Google's update is a full replace, so it must carry every field —
		// hence the struct copy — but mutating in place would leave the block
		// we (and the index) still reference showing the new times even when
		// the write FAILS: an in-memory state that never reached the API.
		updated := *existing
		updated.Start = copySpan(srcEvent.Start)
		updated.End = copySpan(srcEvent.End)
		updated.Summary = e.BlockTitle
		// Conditional on the ETag we listed, mirroring the delete path: if the
		// block changed since the fetch, the update fails its precondition
		// rather than overwriting an event that may no longer be ours.
		result, err := dst.Client.UpdateEvent(ctx, dst.CalendarID, existing.Id, &updated, existing.Etag)
		if err != nil {
			return outcomeUnchanged, err
		}
		// Only now is the new state real. Point the index at it, so a later
		// lookup within this same pass sees what the API actually holds.
		if result == nil {
			result = &updated
		}
		idx[indexKey(src.Name, srcEvent.Id)] = result
		return outcomeUpdated, nil
	}

	// Create a new block. Only the time span crosses the account boundary —
	// never the source event's summary, description, location, or attendees.
	block := &calendar.Event{
		Summary:      e.BlockTitle,
		Start:        copySpan(srcEvent.Start),
		End:          copySpan(srcEvent.End),
		Transparency: "opaque", // shows as Busy
		Visibility:   "private",
		ExtendedProperties: &calendar.EventExtendedProperties{
			Private: map[string]string{
				ownerKey:          ownerValue,
				sourceAccountKey:  src.Name,
				sourceCalendarKey: src.CalendarID,
				sourceEventKey:    srcEvent.Id,
			},
		},
	}
	created, err := dst.Client.InsertEvent(ctx, dst.CalendarID, block)
	if err != nil {
		return outcomeUnchanged, err
	}
	// Record it so a second source event in the same pass, or a later pass
	// re-using this index, doesn't look it up again.
	if created != nil {
		idx[indexKey(src.Name, srcEvent.Id)] = created
	}
	return outcomeCreated, nil
}

// copySpan returns a fresh EventDateTime with the same values. The block must
// never share an EventDateTime pointer with the source event it mirrors: they
// live on different accounts, and aliasing would let a write to one silently
// mutate the other in memory.
func copySpan(dt *calendar.EventDateTime) *calendar.EventDateTime {
	if dt == nil {
		return nil
	}
	c := *dt
	return &c
}

func timesEqual(a, b *calendar.EventDateTime) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.DateTime == b.DateTime && a.Date == b.Date && a.TimeZone == b.TimeZone
}
