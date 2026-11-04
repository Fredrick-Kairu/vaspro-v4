package domain

import (
	"errors"
	"fmt"
)

// HoldStatus is a hold's status (data model §5).
type HoldStatus string

const (
	HoldActive   HoldStatus = "active"
	HoldCaptured HoldStatus = "captured"
	HoldReleased HoldStatus = "released"
	HoldExpired  HoldStatus = "expired"
)

// ErrHoldState: the hold isn't in a state that allows this request, or
// the capture is larger than the hold (BAL-4003).
var ErrHoldState = errors.New("ledger: hold can't change this way")

// ErrInvalidHold covers a malformed hold request.
var ErrInvalidHold = errors.New("ledger: invalid hold request")

// Hold is the part of a hold its rules need. Captured is 0 unless
// captured; Settled means settled_at is set.
type Hold struct {
	Status   HoldStatus
	Amount   int64
	Captured int64
	Settled  bool
}

// HoldChange is what a request does to a hold. Replay means nothing
// changes; otherwise the hold takes Status (and Captured),
// Release leaves the account's held_minor now, and Settle sets
// settled_at.
type HoldChange struct {
	Replay   bool
	Status   HoldStatus
	Captured int64
	Release  int64
	Settle   bool
}

// CaptureHold applies a capture of used (OQ-14). Without a posting the
// unused part is released and the used part stays held until
// settlement (3.5) posts it. With a posting the whole hold leaves
// held_minor and the hold is settled, since the posting in the same
// transaction takes the used part.
//
// A repeated capture of the same amount is a replay. For a replay with
// a posting the caller must still find that posting by its key: a hold
// captured without one and settled later would also look like this.
func CaptureHold(h Hold, used int64, withPosting bool) (HoldChange, error) {
	if used <= 0 {
		return HoldChange{}, fmt.Errorf("%w: a capture must use a positive amount; release a hold nothing used", ErrInvalidHold)
	}
	switch h.Status {
	case HoldActive:
		if used > h.Amount {
			return HoldChange{}, fmt.Errorf("%w: capture of %d exceeds the hold's %d", ErrHoldState, used, h.Amount)
		}
		if withPosting {
			return HoldChange{Status: HoldCaptured, Captured: used, Release: h.Amount, Settle: true}, nil
		}
		return HoldChange{Status: HoldCaptured, Captured: used, Release: h.Amount - used}, nil
	case HoldCaptured:
		if used == h.Captured && (!withPosting || h.Settled) {
			return HoldChange{Replay: true}, nil
		}
		return HoldChange{}, fmt.Errorf("%w: already captured %d", ErrHoldState, h.Captured)
	default:
		return HoldChange{}, fmt.Errorf("%w: hold is %s", ErrHoldState, h.Status)
	}
}

// ReleaseHold gives an active hold's whole amount back. Releasing a
// released or expired hold is a replay: its money is already back.
func ReleaseHold(h Hold) (HoldChange, error) {
	switch h.Status {
	case HoldActive:
		return HoldChange{Status: HoldReleased, Release: h.Amount, Settle: true}, nil
	case HoldReleased, HoldExpired:
		return HoldChange{Replay: true}, nil
	default:
		return HoldChange{}, fmt.Errorf("%w: hold is %s", ErrHoldState, h.Status)
	}
}

// HoldContribution is what a hold adds to its account's held amount:
// the whole amount while active, the captured part until it's settled,
// nothing after.
func HoldContribution(h Hold) int64 {
	switch {
	case h.Status == HoldActive:
		return h.Amount
	case h.Status == HoldCaptured && !h.Settled:
		return h.Captured
	default:
		return 0
	}
}
