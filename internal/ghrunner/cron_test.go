package ghrunner

import (
	"testing"
	"time"
)

func TestCronNext(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	tests := []struct{ expr, from, want string }{
		{"30 5 * * 1,3", "2026-10-31 12:00", "2026-11-02 05:30"}, // Saturday -> Monday
		{"0 0 * * *", "2026-10-31 00:00", "2026-11-01 00:00"},    // strictly after
		{"*/15 * * * *", "2026-10-31 12:07", "2026-10-31 12:15"},
		{"0 9-17/4 * * 1-5", "2026-10-30 10:00", "2026-10-30 13:00"}, // Friday 13:00
		{"0 9-17/4 * * 1-5", "2026-10-30 18:00", "2026-11-02 09:00"},
		{"5 4 1 * *", "2026-10-31 12:00", "2026-11-01 04:05"},
		{"0 0 29 2 *", "2026-10-31 12:00", "2028-02-29 00:00"},  // leap day
		{"0 12 15 * 0", "2026-10-31 00:00", "2026-11-01 12:00"}, // day-of-month OR weekday: Sunday 1 Nov
		{"0 0 * * 7", "2026-10-31 12:00", "2026-11-01 00:00"},   // 7 is Sunday
	}
	for _, tt := range tests {
		c, err := ParseCron(tt.expr)
		if err != nil {
			t.Fatalf("%s: %v", tt.expr, err)
		}
		if got := c.Next(at(tt.from)); !got.Equal(at(tt.want)) {
			t.Errorf("%q after %s: %s, want %s", tt.expr, tt.from, got.Format("2006-01-02 15:04"), tt.want)
		}
	}
	for _, bad := range []string{"* * * *", "61 * * * *", "* 24 * * *", "*/0 * * * *", "a * * * *", "5-2 * * * *"} {
		if _, err := ParseCron(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
