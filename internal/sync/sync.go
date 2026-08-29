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
}

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
func (e *Engine) SyncOnce(ctx context.Context) error {
	now := time.Now()
	timeMin := now.Add(-24 * time.Hour) // small backward buffer for in-progress events
	timeMax := now.Add(time.Duration(e.LookaheadDays) * 24 * time.Hour)

	// 1. Pull real events (and existing owned blocks, for GC) from every account.
	type accountEvents struct {
		real  []*calendar.Event
		owned []*calendar.Event
	}
	byAccount := make(map[string]accountEvents, len(e.Accounts))
	healthy := make([]Account, 0, len(e.Accounts))
	var fetchErrs []error

	for _, acc := range e.Accounts {
		events, err := acc.Client.ListEvents(ctx, acc.CalendarID, timeMin, timeMax)
		if err != nil {
			e.Logger.Error("failed to list events, excluding account from this sync pass", "account", acc.Name, "error", err)
			fetchErrs = append(fetchErrs, fmt.Errorf("listing events for account %s: %w", acc.Name, err))
			continue
		}

		var ae accountEvents
		for _, ev := range events {
			if ev.Status == "cancelled" {
				continue
			}
			if isOwnedBlock(ev) {
				ae.owned = append(ae.owned, ev)
			} else {
				ae.real = append(ae.real, ev)
			}
		}
		byAccount[acc.Name] = ae
		healthy = append(healthy, acc)
		e.Logger.Info("fetched events", "account", acc.Name, "real", len(ae.real), "owned_blocks", len(ae.owned))
	}

	// Accounts that failed to fetch are excluded from propagation and GC
	// entirely this cycle: we don't have their current event state, so we
	// can neither safely push blocks to them nor safely delete blocks that
	// mirror their events elsewhere.
	if len(healthy) < 2 {
		fetchErrs = append(fetchErrs, fmt.Errorf("fewer than 2 healthy accounts (%d), nothing to sync this pass", len(healthy)))
		return errors.Join(fetchErrs...)
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
				if err := e.ensureBlock(ctx, src, ev, dst); err != nil {
					syncErrs = append(syncErrs, fmt.Errorf("ensuring block for %s/%s on %s: %w", src.Name, ev.Id, dst.Name, err))
				}
			}
		}
	}

	// 3. Garbage-collect blocks whose source event no longer exists or is
	// no longer in the live "real" set (deleted / cancelled upstream).
	liveSourceIDs := make(map[string]bool)
	for _, src := range healthy {
		for _, ev := range byAccount[src.Name].real {
			liveSourceIDs[src.Name+"|"+ev.Id] = true
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
			if !liveSourceIDs[srcAccount+"|"+srcEventID] {
				e.Logger.Info("removing stale block", "account", dst.Name, "block_id", block.Id, "source", srcAccount+"/"+srcEventID)
				// Conditional on the ETag we listed: if the block changed since
				// the fetch, the delete fails rather than removing a now-altered
				// event. block.Etag may be empty (e.g. test fakes) → unconditional.
				if err := dst.Client.DeleteEvent(ctx, dst.CalendarID, block.Id, block.Etag); err != nil {
					syncErrs = append(syncErrs, fmt.Errorf("deleting stale block %s on %s: %w", block.Id, dst.Name, err))
				}
			}
		}
	}

	return errors.Join(append(fetchErrs, syncErrs...)...)
}

func accountIsHealthy(healthy []Account, name string) bool {
	for _, a := range healthy {
		if a.Name == name {
			return true
		}
	}
	return false
}

// deterministicBlockKey returns a stable, deterministic private-property
// value used to look up whether a block already exists for a given source
// event on a given destination account, without depending on the
// destination event's own ID (which we don't know until after creation).
func deterministicBlockKey(srcAccount, srcEventID string) string {
	return srcAccount + "|" + srcEventID
}

func (e *Engine) ensureBlock(ctx context.Context, src Account, srcEvent *calendar.Event, dst Account) error {
	// Look for an existing block on dst tagged with this exact source.
	existing, err := dst.Client.FindBlockBySource(ctx, dst.CalendarID, src.Name, srcEvent.Id)
	if err != nil {
		return err
	}

	if existing != nil {
		// Update times if the source event moved.
		if !timesEqual(existing.Start, srcEvent.Start) || !timesEqual(existing.End, srcEvent.End) {
			existing.Start = srcEvent.Start
			existing.End = srcEvent.End
			_, err := dst.Client.UpdateEvent(ctx, dst.CalendarID, existing.Id, existing)
			return err
		}
		return nil // up to date
	}

	// Create a new block.
	block := &calendar.Event{
		Summary:      e.BlockTitle,
		Start:        srcEvent.Start,
		End:          srcEvent.End,
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
	_, err = dst.Client.InsertEvent(ctx, dst.CalendarID, block)
	return err
}

func timesEqual(a, b *calendar.EventDateTime) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.DateTime == b.DateTime && a.Date == b.Date && a.TimeZone == b.TimeZone
}
