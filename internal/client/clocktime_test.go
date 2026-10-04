package client

import (
	"testing"
	"time"
)

// A wake time shows as the clock time on the same day and with the date on
// any other, measured against the now the caller passes.
func TestClockTime(t *testing.T) {
	woke := time.Date(2026, 10, 3, 16, 40, 0, 0, time.Local)
	for _, tc := range []struct {
		now  time.Time
		want string
	}{
		{time.Date(2026, 10, 3, 23, 59, 0, 0, time.Local), "16:40"},
		{time.Date(2026, 10, 4, 0, 1, 0, 0, time.Local), "Oct 3 16:40"},
		{time.Date(2027, 10, 3, 16, 40, 0, 0, time.Local), "Oct 3 16:40"},
	} {
		if got := clockTime(woke, tc.now); got != tc.want {
			t.Errorf("clockTime(%v, %v) = %q, want %q", woke, tc.now, got, tc.want)
		}
	}
}
