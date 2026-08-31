package sync

import (
	"testing"

	calendar "google.golang.org/api/calendar/v3"
)

// FuzzSourceIdentity attacks the ownership parsing with arbitrary extended
// property values. The invariants:
//
//   - it never panics;
//   - it never reports ok for an incomplete identity, since a block with a
//     partial identity cannot be matched back to a source and must never be
//     acted on;
//   - a round trip through the neutral model preserves the ownership decision
//     exactly, so the Provider seam can't quietly reclassify a real event as
//     one of ours (or vice versa).
func FuzzSourceIdentity(f *testing.F) {
	f.Add(ownerValue, "personal", "primary", "evt-1")
	f.Add("", "", "", "")
	f.Add(ownerValue, "", "primary", "evt-1")
	f.Add("calendar-bridge ", "personal", "primary", "evt-1") // trailing space
	f.Add("CALENDAR-BRIDGE", "personal", "primary", "evt-1")  // wrong case
	f.Add(ownerValue, "a|b", "primary", "c|d")                // separator injection
	f.Add(ownerValue, "personal", "primary", "")
	f.Add(ownerValue, "personal", "", "evt-1") // owner + account + event, no calendar
	f.Add("\x00", "\x00", "\x00", "\x00")

	f.Fuzz(func(t *testing.T, owner, srcAccount, srcCalendar, srcEvent string) {
		ev := &calendar.Event{
			Id: "some-id",
			ExtendedProperties: &calendar.EventExtendedProperties{Private: map[string]string{
				ownerKey:          owner,
				sourceAccountKey:  srcAccount,
				sourceCalendarKey: srcCalendar,
				sourceEventKey:    srcEvent,
			}},
		}

		owned := isOwnedBlock(ev)
		if owned != (owner == ownerValue) {
			t.Fatalf("isOwnedBlock = %v for owner %q; only the exact sentinel counts", owned, owner)
		}

		gotAcc, gotCal, gotEv, ok := sourceIdentity(ev)
		if ok && (gotAcc == "" || gotCal == "" || gotEv == "") {
			t.Fatalf("sourceIdentity reported ok with an incomplete identity (%q/%q/%q)", gotAcc, gotCal, gotEv)
		}
		if ok && (gotAcc != srcAccount || gotCal != srcCalendar || gotEv != srcEvent) {
			t.Fatalf("sourceIdentity round trip changed the values: got %q/%q/%q, want %q/%q/%q",
				gotAcc, gotCal, gotEv, srcAccount, srcCalendar, srcEvent)
		}

		// The neutral model must agree with the Google-typed check.
		own := ownershipFromGoogle(ev)
		if own.IsOwned() != owned {
			t.Fatalf("Ownership.IsOwned = %v but isOwnedBlock = %v — the Provider seam disagrees about ownership",
				own.IsOwned(), owned)
		}
		// validForWrite is what gates every write. It must never be true
		// without both the owner tag and a usable source identity.
		// The write gate must agree with sourceIdentity, which garbage
		// collection uses to match a block back to its source: if the gate were
		// looser, a block could be written that GC can never match, i.e. an
		// orphan this tool creates and cannot clean up.
		if own.validForWrite() && (!owned || srcAccount == "" || srcCalendar == "" || srcEvent == "") {
			t.Fatalf("validForWrite = true for owner=%q account=%q calendar=%q event=%q",
				owner, srcAccount, srcCalendar, srcEvent)
		}
		if _, _, _, idOK := sourceIdentity(ev); own.validForWrite() != (owned && idOK) {
			t.Fatalf("write gate and GC matching disagree: validForWrite=%v owned=%v sourceIdentity=%v",
				own.validForWrite(), owned, idOK)
		}

		// And a full round trip through the neutral Event must be stable.
		back := eventToGoogle(eventFromGoogle(ev))
		if isOwnedBlock(back) != owned {
			t.Fatalf("ownership flipped across a neutral-model round trip (%v -> %v)", owned, isOwnedBlock(back))
		}
		// Ownership alone is not enough. A conversion that dropped the source
		// calendar or source event tag would keep ownership stable and pass
		// the check above, while destroying the identity GC matches a block
		// back to its source by — producing exactly the uncollectable orphan
		// the write gate above exists to prevent.
		wantAcct, wantCal, wantEvt, wantOK := sourceIdentity(ev)
		gotAcct, gotCal, gotEvt, gotOK := sourceIdentity(back)
		if wantOK != gotOK || wantAcct != gotAcct || wantCal != gotCal || wantEvt != gotEvt {
			t.Fatalf("source identity changed across a neutral-model round trip:\n"+
				" before: account=%q calendar=%q event=%q ok=%v\n"+
				" after:  account=%q calendar=%q event=%q ok=%v",
				wantAcct, wantCal, wantEvt, wantOK, gotAcct, gotCal, gotEvt, gotOK)
		}
	})
}

// FuzzTimeSpanEqual checks the span comparison the update path depends on:
// it must be reflexive and symmetric, or a block would oscillate between
// "unchanged" and "needs updating" and rewrite itself forever.
func FuzzTimeSpanEqual(f *testing.F) {
	f.Add("2026-03-12T14:00:00Z", "", "UTC", "2026-03-12T14:00:00Z", "", "UTC")
	f.Add("", "2026-03-12", "", "", "2026-03-12", "")
	f.Add("2026-03-12T14:00:00+01:00", "", "Europe/Berlin", "2026-03-12T13:00:00Z", "", "UTC")

	f.Fuzz(func(t *testing.T, dtA, dA, tzA, dtB, dB, tzB string) {
		a := TimeSpan{DateTime: dtA, Date: dA, TimeZone: tzA}
		b := TimeSpan{DateTime: dtB, Date: dB, TimeZone: tzB}

		if !a.Equal(a) {
			t.Fatalf("Equal is not reflexive for %+v", a)
		}
		if a.Equal(b) != b.Equal(a) {
			t.Fatalf("Equal is not symmetric for %+v and %+v", a, b)
		}
		// Consistency with the Google-typed comparison the engine uses.
		if got, want := timesEqual(spanToGoogle(a), spanToGoogle(b)), a.Equal(b); got != want && a != (TimeSpan{}) && b != (TimeSpan{}) {
			t.Fatalf("timesEqual = %v but TimeSpan.Equal = %v for %+v / %+v", got, want, a, b)
		}
	})
}
