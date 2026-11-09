package domain_test

import (
	"testing"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

func TestParseTimeOfDay(t *testing.T) {
	for in, want := range map[string]time.Duration{"21:30": 21*time.Hour + 30*time.Minute, "00:00": 0, "23:59": 23*time.Hour + 59*time.Minute} {
		got, err := domain.ParseTimeOfDay(in)
		if err != nil || got.Offset() != want {
			t.Fatalf("%q: %v, %v; want %v", in, got.Offset(), err, want)
		}
	}
	for _, bad := range []string{"", "24:00", "21:60", "9:5", "21.30", "21:30:00", "-1:00"} {
		if _, err := domain.ParseTimeOfDay(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

// NextRebuild: the next run at or after now, and its date (the UTC date
// of the run), on the grid every replica shares.
func TestNextRebuild(t *testing.T) {
	at, err := domain.ParseTimeOfDay("21:30")
	if err != nil {
		t.Fatal(err)
	}
	day := func(d, h, m int) time.Time { return time.Date(2026, 11, d, h, m, 0, 0, time.UTC) }
	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"earlier the same day", day(9, 10, 0), day(9, 21, 30)},
		{"exactly at the time", day(9, 21, 30), day(9, 21, 30)},
		{"just after: tomorrow", day(9, 21, 31), day(10, 21, 30)},
		{"after midnight", day(10, 0, 5), day(10, 21, 30)},
		{"another time zone is read as UTC", day(9, 22, 0).In(time.FixedZone("EAT", 3*3600)), day(10, 21, 30)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			next, date := domain.NextRebuild(tc.now, at)
			if !next.Equal(tc.want) {
				t.Fatalf("next %s, want %s", next, tc.want)
			}
			if wantDate := tc.want.Format(time.DateOnly); date.Format(time.DateOnly) != wantDate {
				t.Fatalf("run date %s, want %s", date.Format(time.DateOnly), wantDate)
			}
		})
	}
}
