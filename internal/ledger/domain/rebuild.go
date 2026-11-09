package domain

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// TimeOfDay is a UTC time of day the nightly rebuild runs at (task 3.8).
type TimeOfDay struct{ offset time.Duration }

// Offset is the time since midnight UTC.
func (t TimeOfDay) Offset() time.Duration { return t.offset }

var timeOfDay = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

// ParseTimeOfDay reads "HH:MM", 00:00 to 23:59.
func ParseTimeOfDay(s string) (TimeOfDay, error) {
	m := timeOfDay.FindStringSubmatch(s)
	if m == nil {
		return TimeOfDay{}, fmt.Errorf("ledger: %q is not a time of day (HH:MM, UTC)", s)
	}
	h, _ := strconv.Atoi(m[1])
	mins, _ := strconv.Atoi(m[2])
	return TimeOfDay{offset: time.Duration(h)*time.Hour + time.Duration(mins)*time.Minute}, nil
}

// NextRebuild is the next rebuild at or after now, and the date it runs
// for: the UTC date of that time, so every replica claims the same
// date (OQ-19).
func NextRebuild(now time.Time, at TimeOfDay) (next, runDate time.Time) {
	now = now.UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	next = midnight.Add(at.offset)
	if next.Before(now) {
		midnight = midnight.AddDate(0, 0, 1)
		next = midnight.Add(at.offset)
	}
	return next, midnight
}
