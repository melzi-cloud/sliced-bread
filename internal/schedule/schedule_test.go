package schedule

import (
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestLastSlot(t *testing.T) {
	cases := []struct {
		b         Backup
		now, want string
	}{
		{Default(), "2026-11-07T02:59:59Z", "2026-11-06T03:00:00Z"},
		{Default(), "2026-11-07T03:00:00Z", "2026-11-07T03:00:00Z"},
		{Default(), "2026-11-07T23:10:00Z", "2026-11-07T03:00:00Z"},
		{Backup{Hour: 3}, "2026-11-07T04:00:00Z", "2026-11-07T03:00:00Z"}, // stored before frequencies
		{Backup{Hour: 3, Every: 6}, "2026-11-07T02:00:00Z", "2026-11-06T21:00:00Z"},
		{Backup{Hour: 3, Every: 6}, "2026-11-07T10:30:00Z", "2026-11-07T09:00:00Z"},
		{Backup{Hour: 21, Every: 6}, "2026-11-07T10:30:00Z", "2026-11-07T09:00:00Z"},
		{Backup{Hour: 20, Every: 12}, "2026-11-07T10:30:00Z", "2026-11-07T08:00:00Z"},
		{Backup{Hour: 20, Every: 12}, "2026-11-07T21:00:00Z", "2026-11-07T20:00:00Z"},
		// 2026-11-07 is a Saturday.
		{Backup{Hour: 3, Every: Weekly, Weekday: time.Sunday}, "2026-11-07T10:00:00Z", "2026-11-01T03:00:00Z"},
		{Backup{Hour: 3, Every: Weekly, Weekday: time.Sunday}, "2026-11-08T03:00:00Z", "2026-11-08T03:00:00Z"},
		{Backup{Hour: 3, Every: Weekly, Weekday: time.Saturday}, "2026-11-07T02:00:00Z", "2026-10-31T03:00:00Z"},
		{Backup{Hour: 0, Every: Weekly, Weekday: time.Thursday}, "2026-11-05T00:00:00Z", "2026-11-05T00:00:00Z"},
	}
	for _, c := range cases {
		got := c.b.LastSlot(at(c.now))
		if !got.Equal(at(c.want)) {
			t.Errorf("%s at %s: last slot %s, want %s", c.b, c.now, got.Format(time.RFC3339), c.want)
		}
		if next := c.b.NextSlot(at(c.now)); !next.After(at(c.now)) {
			t.Errorf("%s at %s: next slot %s not after now", c.b, c.now, next)
		}
	}
}

func TestMaintenanceHour(t *testing.T) {
	cases := []struct {
		b    Backup
		want int
	}{
		{Default(), 4},
		{Backup{Hour: 3, Every: 6}, 4},
		{Backup{Hour: 20, Every: 6}, 3},
		{Backup{Hour: 20, Every: 12}, 9},
		{Backup{Hour: 23, Every: 24}, 0},
		{Backup{Hour: 23, Every: Weekly, Weekday: time.Monday}, 0},
	}
	for _, c := range cases {
		if got := c.b.MaintenanceHour(); got != c.want {
			t.Errorf("%s: maintenance at %d, want %d", c.b, got, c.want)
		}
	}
}

func TestStringsRoundTrip(t *testing.T) {
	for _, b := range []Backup{Default(), {Hour: 5, Every: 6}, {Hour: 22, Every: Weekly, Weekday: time.Wednesday}} {
		h, e, w := b.Strings()
		if got := FromStrings(h, e, w); got != b.Normalize() {
			t.Errorf("round trip %+v -> %+v", b, got)
		}
	}
	if got := FromStrings("7", "", ""); got != (Backup{Hour: 7, Every: 24}) {
		t.Errorf("old secret with only an hour: %+v", got)
	}
	if got := FromStrings("x", "5", "9"); got != Default() {
		t.Errorf("bad values: %+v", got)
	}
	if s := (Backup{Hour: 3, Every: 6}).String(); s != "every 6 hours (03:00, 09:00, 15:00, 21:00 UTC)" {
		t.Error(s)
	}
	if (Backup{Hour: 3, Every: 8}).Validate() == nil || (Backup{Hour: 24, Every: 6}).Validate() == nil {
		t.Error("accepted a bad schedule")
	}
}
