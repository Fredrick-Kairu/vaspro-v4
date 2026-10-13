// Package clock is the only source of the current time: production code
// never calls time.Now() directly (.claude/rules/go.md), so a test can
// swap Real for Fake and control time explicitly.
package clock

import (
	"sync"
	"time"
)

// Clock returns the current time.
type Clock interface {
	Now() time.Time
}

// Real is the production Clock: time.Now(), stored in UTC (data model §1).
type Real struct{}

func (Real) Now() time.Time { return time.Now().UTC() }

// Fake is a Clock a test controls explicitly — no sleeps, no drift.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake returns a Fake starting at t.
func NewFake(t time.Time) *Fake {
	return &Fake{now: t}
}

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Set moves the fake clock to exactly t.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t
}

// Advance moves the fake clock forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}
