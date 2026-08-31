package sync

import (
	"testing"

	calendar "google.golang.org/api/calendar/v3"
)

func TestIsOwnedBlock(t *testing.T) {
	cases := []struct {
		name string
		ev   *calendar.Event
		want bool
	}{
		{
			name: "nil extended properties",
			ev:   &calendar.Event{},
			want: false,
		},
		{
			name: "no private map",
			ev:   &calendar.Event{ExtendedProperties: &calendar.EventExtendedProperties{}},
			want: false,
		},
		{
			name: "unrelated private key",
			ev: &calendar.Event{ExtendedProperties: &calendar.EventExtendedProperties{
				Private: map[string]string{"someOtherApp": "yes"},
			}},
			want: false,
		},
		{
			name: "owned block",
			ev: &calendar.Event{ExtendedProperties: &calendar.EventExtendedProperties{
				Private: map[string]string{ownerKey: ownerValue},
			}},
			want: true,
		},
		{
			name: "wrong owner value",
			ev: &calendar.Event{ExtendedProperties: &calendar.EventExtendedProperties{
				Private: map[string]string{ownerKey: "some-other-tool"},
			}},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isOwnedBlock(tc.ev)
			if got != tc.want {
				t.Errorf("isOwnedBlock() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSourceIdentity(t *testing.T) {
	t.Run("fully tagged block", func(t *testing.T) {
		ev := &calendar.Event{ExtendedProperties: &calendar.EventExtendedProperties{
			Private: map[string]string{
				sourceAccountKey:  "personal",
				sourceCalendarKey: "primary",
				sourceEventKey:    "abc123",
			},
		}}
		account, calID, eventID, ok := sourceIdentity(ev)
		if !ok || account != "personal" || calID != "primary" || eventID != "abc123" {
			t.Errorf("sourceIdentity() = (%q, %q, %q, %v), want (personal, primary, abc123, true)", account, calID, eventID, ok)
		}
	})

	t.Run("missing fields", func(t *testing.T) {
		ev := &calendar.Event{ExtendedProperties: &calendar.EventExtendedProperties{
			Private: map[string]string{sourceAccountKey: "personal"},
		}}
		_, _, _, ok := sourceIdentity(ev)
		if ok {
			t.Error("sourceIdentity() ok = true, want false for partially-tagged event")
		}
	})

	t.Run("real event, no tags at all", func(t *testing.T) {
		ev := &calendar.Event{Summary: "Dentist"}
		_, _, _, ok := sourceIdentity(ev)
		if ok {
			t.Error("sourceIdentity() ok = true, want false for untagged real event")
		}
	})
}

func TestTimesEqual(t *testing.T) {
	dt := func(datetime string) *calendar.EventDateTime {
		return &calendar.EventDateTime{DateTime: datetime, TimeZone: "UTC"}
	}

	cases := []struct {
		name string
		a, b *calendar.EventDateTime
		want bool
	}{
		{"both nil", nil, nil, true},
		{"one nil", dt("2026-01-01T10:00:00Z"), nil, false},
		{"equal", dt("2026-01-01T10:00:00Z"), dt("2026-01-01T10:00:00Z"), true},
		{"different time", dt("2026-01-01T10:00:00Z"), dt("2026-01-01T11:00:00Z"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := timesEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("timesEqual() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAccountIsHealthy(t *testing.T) {
	healthy := []Account{{Name: "a"}, {Name: "b"}}

	if !accountIsHealthy(healthy, "a") {
		t.Error("accountIsHealthy(a) = false, want true")
	}
	if accountIsHealthy(healthy, "c") {
		t.Error("accountIsHealthy(c) = true, want false (not in healthy set)")
	}
	if accountIsHealthy(nil, "a") {
		t.Error("accountIsHealthy on nil slice should be false")
	}
}
