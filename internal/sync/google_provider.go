package sync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
)

// This file provides two adapters:
//
//  1. googleProvider — implements the neutral Provider interface on top of
//     the existing Google-typed CalendarClient. This proves the Provider
//     seam works end to end and is the template a future Outlook/iCloud
//     provider follows: implement Provider, map your SDK <-> neutral Event.
//
//  2. providerClient — bridges any Provider back into the CalendarClient
//     the existing Engine consumes, so the engine can drive a neutral
//     Provider without changing its (Google-typed) internals yet. This keeps
//     the large existing test suite valid while making non-Google backends
//     reachable.
//
// The two adapters are intentionally inverse: googleProvider(CalendarClient)
// -> Provider, and providerClient(Provider) -> CalendarClient. New backends
// only need to implement Provider; the bridge handles the rest.

// ---- neutral <-> google mapping helpers ----

func spanFromGoogle(dt *calendar.EventDateTime) TimeSpan {
	if dt == nil {
		return TimeSpan{}
	}
	return TimeSpan{DateTime: dt.DateTime, Date: dt.Date, TimeZone: dt.TimeZone}
}

func spanToGoogle(s TimeSpan) *calendar.EventDateTime {
	if s == (TimeSpan{}) {
		return nil
	}
	return &calendar.EventDateTime{DateTime: s.DateTime, Date: s.Date, TimeZone: s.TimeZone}
}

func ownershipFromGoogle(ev *calendar.Event) Ownership {
	if ev.ExtendedProperties == nil || ev.ExtendedProperties.Private == nil {
		return Ownership{}
	}
	p := ev.ExtendedProperties.Private
	return Ownership{
		Owner:            p[ownerKey],
		SourceAccount:    p[sourceAccountKey],
		SourceCalendarID: p[sourceCalendarKey],
		SourceEventID:    p[sourceEventKey],
	}
}

func eventFromGoogle(ev *calendar.Event) Event {
	// Only an owned block's title crosses into the neutral model; see
	// Event.Title. A real event's summary is dropped here, which is what makes
	// the no-content-propagation guarantee structural rather than conventional.
	var title string
	if isOwnedBlock(ev) {
		title = ev.Summary
	}
	return Event{
		ID:           ev.Id,
		Title:        title,
		Start:        spanFromGoogle(ev.Start),
		End:          spanFromGoogle(ev.End),
		Cancelled:    ev.Status == "cancelled",
		Transparent:  ev.Transparency == "transparent",
		SelfResponse: selfResponseStatus(ev),
		Ownership:    ownershipFromGoogle(ev),
	}
}

// ---- googleProvider: CalendarClient -> Provider ----

type googleProvider struct {
	client CalendarClient
}

// NewGoogleProvider adapts a Google-typed CalendarClient (real or fake) to
// the neutral Provider interface.
func NewGoogleProvider(client CalendarClient) Provider {
	return &googleProvider{client: client}
}

func (g *googleProvider) ListEvents(ctx context.Context, calendarID string, timeMin, timeMax time.Time) ([]Event, error) {
	raw, err := g.client.ListEvents(ctx, calendarID, timeMin, timeMax)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(raw))
	for _, ev := range raw {
		if ev == nil {
			continue // defensively skip nil entries from a misbehaving client
		}
		out = append(out, eventFromGoogle(ev))
	}
	return out, nil
}

func (g *googleProvider) FindOwnedBlock(ctx context.Context, calendarID, srcAccount, srcEventID string) (*Event, error) {
	ev, err := g.client.FindBlockBySource(ctx, calendarID, srcAccount, srcEventID)
	if err != nil || ev == nil {
		return nil, err
	}
	e := eventFromGoogle(ev)
	// Don't trust the CalendarClient to have filtered correctly: only return a
	// block that is genuinely calendar-bridge-owned AND whose source identity
	// matches what was requested. A source-property match without the owner tag
	// (or with a different source) must never be treated as our block, or the
	// caller could overwrite a real user event.
	if !e.Ownership.validForWrite() ||
		e.Ownership.SourceAccount != srcAccount ||
		e.Ownership.SourceEventID != srcEventID {
		return nil, nil
	}
	return &e, nil
}

func (g *googleProvider) InsertBlock(ctx context.Context, calendarID, title string, start, end TimeSpan, own Ownership) (*Event, error) {
	// Never create a block we can't later identify and GC. This mirrors the
	// .coderabbit.yaml invariant for internal/sync and is enforced here so a
	// direct Provider caller can't bypass the bridge's check.
	if !own.validForWrite() {
		return nil, ErrNotOwned
	}
	// Idempotency: a retried insert (e.g. Google created the block but the
	// response was lost, then the call was retried) must not create a second
	// block. Check for an existing owned block for this source first and reuse
	// it. FindOwnedBlock already verifies ownership + source match.
	if existing, err := g.FindOwnedBlock(ctx, calendarID, own.SourceAccount, own.SourceEventID); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	block := &calendar.Event{
		Summary:      title,
		Start:        spanToGoogle(start),
		End:          spanToGoogle(end),
		Transparency: "opaque",
		Visibility:   "private",
		ExtendedProperties: &calendar.EventExtendedProperties{
			Private: map[string]string{
				ownerKey:          own.Owner,
				sourceAccountKey:  own.SourceAccount,
				sourceCalendarKey: own.SourceCalendarID,
				sourceEventKey:    own.SourceEventID,
			},
		},
	}
	created, err := g.client.InsertEvent(ctx, calendarID, block)
	if err != nil {
		// The failure may be ambiguous (e.g. a network timeout): the block
		// could have persisted server-side even though this call saw no
		// response. Reconcile with a lookup before surfacing the error, so a
		// caller that retries InsertBlock never creates a second block for
		// the same source event.
		if isAmbiguous(err) {
			if existing, findErr := g.FindOwnedBlock(ctx, calendarID, own.SourceAccount, own.SourceEventID); findErr == nil && existing != nil {
				return existing, nil
			}
		}
		return nil, err
	}
	if created == nil {
		return nil, fmt.Errorf("insert returned no event")
	}
	e := eventFromGoogle(created)
	return &e, nil
}

func (g *googleProvider) UpdateBlock(ctx context.Context, calendarID string, block Event, title string, start, end TimeSpan) (*Event, error) {
	// Google's Events.Update is a full replace: sending only the new times
	// would wipe the ownership extended properties, making the block
	// indistinguishable from a real event on the next pass. Re-find the full
	// owned block by its source identity, mutate a copy of it, and update —
	// preserving every other field.
	//
	// The write is conditional on the ETag from that re-read, so the
	// check-and-write is atomic: if the event changed in between — including
	// having its owner tag stripped — the update fails its precondition
	// instead of overwriting an event that may no longer be ours. On a 412 we
	// re-read, re-verify ownership, and retry once with the fresh ETag; a
	// still-owned block is a benign concurrent edit, a now-untagged one is
	// refused. This mirrors DeleteBlock, and exists for the same reason.
	own := block.Ownership
	// Refuse to update anything not provably ours with a complete source
	// identity: without it we can't safely locate the block and could target a
	// real event with empty filter values.
	if !own.validForWrite() {
		return nil, ErrNotOwned
	}

	const maxAttempts = 2
	for attempt := 0; attempt < maxAttempts; attempt++ {
		full, err := g.client.FindBlockBySource(ctx, calendarID, own.SourceAccount, own.SourceEventID)
		if err != nil {
			return nil, err
		}
		if full == nil {
			// The block vanished between list and update (deleted concurrently);
			// nothing to move. Report it via a nil result, no error.
			return nil, nil
		}
		// Don't trust the re-read result: verify it is genuinely our owned block
		// with a matching source identity before overwriting. A source-property
		// match that is untagged, incomplete, or points at a different source
		// must not be updated — that could clobber a real event.
		reread := ownershipFromGoogle(full)
		if !reread.validForWrite() ||
			reread.SourceAccount != own.SourceAccount ||
			reread.SourceEventID != own.SourceEventID {
			return nil, ErrNotOwned
		}

		// Mutate a COPY, not the event we just re-read: writing into `full`
		// would leave the caller's in-memory view showing the new span even
		// when the update fails.
		payload := *full
		payload.Start = spanToGoogle(start)
		payload.End = spanToGoogle(end)
		if title != "" {
			payload.Summary = title
		}

		updated, err := g.client.UpdateEvent(ctx, calendarID, full.Id, &payload, full.Etag)
		if err == nil {
			if updated == nil {
				return nil, fmt.Errorf("update returned no event")
			}
			e := eventFromGoogle(updated)
			return &e, nil
		}
		// A 412 means the event changed after our read; loop to re-read and
		// re-verify. Any other error is returned as-is.
		var apiErr *googleapi.Error
		if !errors.As(err, &apiErr) || apiErr.Code != http.StatusPreconditionFailed {
			return nil, err
		}
	}
	return nil, fmt.Errorf("update of the block for %s/%s kept failing its ownership precondition (event changing concurrently)",
		own.SourceAccount, own.SourceEventID)
}

func (g *googleProvider) DeleteBlock(ctx context.Context, calendarID, blockID string) error {
	// Verify the target is calendar-bridge-owned before deleting, and make the
	// check-and-delete atomic via an If-Match on the event's ETag: if the event
	// changed between the read and the delete, the conditional delete fails its
	// precondition rather than removing an event that may no longer be ours. On
	// a precondition failure we re-read, re-verify ownership, and retry once
	// with the fresh ETag; a still-owned block is a benign concurrent time
	// update, while a now-untagged event is refused. This upholds "never delete
	// an untagged event" even under concurrent modification.
	const maxAttempts = 2
	for attempt := 0; attempt < maxAttempts; attempt++ {
		ev, err := g.client.GetEvent(ctx, calendarID, blockID)
		if err != nil {
			return err
		}
		if ev == nil {
			return nil // already gone; nothing to do
		}
		if !isOwnedBlock(ev) {
			return ErrNotOwned
		}
		err = g.client.DeleteEvent(ctx, calendarID, blockID, ev.Etag)
		if err == nil {
			return nil
		}
		// A 412 precondition failure means the event changed after our read;
		// loop to re-read and re-verify. Any other error is returned as-is.
		var apiErr *googleapi.Error
		if !errors.As(err, &apiErr) || apiErr.Code != 412 {
			return err
		}
	}
	return fmt.Errorf("delete of block %s kept failing its ownership precondition (event changing concurrently)", blockID)
}
