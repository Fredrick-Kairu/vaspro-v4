package testkit

import (
	"testing"
	"time"
)

func TestNewClock_StartsAtGivenTime(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewClock(start)

	if got := c.Now(); !got.Equal(start) {
		t.Fatalf("got %v, want %v", got, start)
	}
}

func TestNewClock_AdvanceMovesItForward(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewClock(start)

	c.Advance(time.Hour)

	want := start.Add(time.Hour)
	if got := c.Now(); !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
