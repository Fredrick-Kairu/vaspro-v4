package testkit

import (
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
)

// Clock is platform/clock's Fake, re-exported here so a test needs only
// one import (testkit) for every helper this package names.
type Clock = clock.Fake

// NewClock returns a Clock starting at t.
func NewClock(t time.Time) *Clock {
	return clock.NewFake(t)
}
