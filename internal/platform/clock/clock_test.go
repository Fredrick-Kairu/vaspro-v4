package clock_test

import (
	"testing"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
)

func TestReal_NowIsCloseToTimeNowAndUTC(t *testing.T) {
	before := time.Now()
	got := clock.Real{}.Now()
	after := time.Now()

	if got.Location() != time.UTC {
		t.Fatalf("expected UTC, got location %v", got.Location())
	}
	if got.Before(before.Add(-time.Second)) || got.After(after.Add(time.Second)) {
		t.Fatalf("Now() = %v, expected it to be between %v and %v", got, before, after)
	}
}

func TestFake_NowReturnsWhatItWasSetTo(t *testing.T) {
	want := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	f := clock.NewFake(want)

	if got := f.Now(); !got.Equal(want) {
		t.Fatalf("Now() = %v, want %v", got, want)
	}
}

func TestFake_Set(t *testing.T) {
	f := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	next := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)
	f.Set(next)

	if got := f.Now(); !got.Equal(next) {
		t.Fatalf("after Set, Now() = %v, want %v", got, next)
	}
}

func TestFake_Advance(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f := clock.NewFake(start)

	f.Advance(90 * time.Minute)

	want := start.Add(90 * time.Minute)
	if got := f.Now(); !got.Equal(want) {
		t.Fatalf("after Advance, Now() = %v, want %v", got, want)
	}
}

func TestFake_NeverDependsOnRealTime(t *testing.T) {
	// A fake set far in the past must stay there, with no sleeps and no
	// drift — this is the whole point of a fake clock (.claude/rules/tests.md).
	f := clock.NewFake(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	for range 1000 {
		_ = f.Now()
	}
	want := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := f.Now(); !got.Equal(want) {
		t.Fatalf("Now() drifted to %v, want %v", got, want)
	}
}

func TestFake_ImplementsClockInterface(t *testing.T) {
	var _ clock.Clock = clock.NewFake(time.Now())
	var _ clock.Clock = clock.Real{}
}
