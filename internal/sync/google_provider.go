package sync

import (
	"context"
	"time"

	calendar "google.golang.org/api/calendar/v3"
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
	return Event{
		ID:        ev.Id,
		Start:     spanFromGoogle(ev.Start),
		End:       spanFromGoogle(ev.End),
		Cancelled: ev.Status == "cancelled",
		Ownership: ownershipFromGoogle(ev),
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
		return nil, err
	}
	e := eventFromGoogle(created)
	return &e, nil
}

func (g *googleProvider) UpdateBlockTime(ctx context.Context, calendarID string, block Event, start, end TimeSpan) (*Event, error) {
	// Google's Events.Update is a full replace: sending only the new times
	// would wipe the ownership extended properties and the block title,
	// making the block indistinguishable from a real event on the next pass.
	// Re-find the full owned block by its source identity, mutate only its
	// times, and update — preserving every other field. This is exactly the
	// invariant the engine's original ensureBlock relied on.
	own := block.Ownership
	// Refuse to update anything not provably ours with a complete source
	// identity: without it we can't safely locate the block and could target a
	// real event with empty filter values.
	if !own.validForWrite() {
		return nil, ErrNotOwned
	}
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
	// with a matching source identity before overwriting its times. A
	// source-property match that is untagged, incomplete, or points at a
	// different source must not be updated — that could clobber a real event.
	reread := ownershipFromGoogle(full)
	if !reread.validForWrite() ||
		reread.SourceAccount != own.SourceAccount ||
		reread.SourceEventID != own.SourceEventID {
		return nil, ErrNotOwned
	}
	full.Start = spanToGoogle(start)
	full.End = spanToGoogle(end)
	updated, err := g.client.UpdateEvent(ctx, calendarID, full.Id, full)
	if err != nil {
		return nil, err
	}
	e := eventFromGoogle(updated)
	return &e, nil
}

func (g *googleProvider) DeleteBlock(ctx context.Context, calendarID, blockID string) error {
	// Re-read the target and verify it is calendar-bridge-owned before
	// deleting, so an untagged real event can never be removed through this
	// path — even if a caller passes the wrong ID. Enforced here rather than
	// trusting caller discipline.
	//
	// There is a narrow theoretical TOCTOU window between this read and the
	// delete: a concurrent actor could strip the ownership tag in between, and
	// the delete would still fire. Closing it fully needs an If-Match/ETag
	// conditional delete, which the neutral Provider model doesn't carry. We
	// accept the window deliberately: the only writer of these blocks is
	// calendar-bridge itself (a single-writer daemon), the production sync path
	// deletes via the plain Google client on blocks it just listed rather than
	// through this adapter, and the re-check already defeats the realistic
	// failure mode (a stale/incorrect ID pointing at a real event).
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
	return g.client.DeleteEvent(ctx, calendarID, blockID)
}
