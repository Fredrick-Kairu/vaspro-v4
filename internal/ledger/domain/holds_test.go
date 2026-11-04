package domain_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

func active(amount int64) domain.Hold {
	return domain.Hold{Status: domain.HoldActive, AmountMinor: amount}
}

// TestCaptureHold covers every hold state against a capture, with and
// without a posting (OQ-14).
func TestCaptureHold(t *testing.T) {
	captured := func(amount, used int64, settled bool) domain.Hold {
		return domain.Hold{Status: domain.HoldCaptured, AmountMinor: amount, CapturedMinor: used, Settled: settled}
	}
	cases := []struct {
		name        string
		hold        domain.Hold
		used        int64
		withPosting bool
		want        domain.HoldChange
		err         error
	}{
		{"worked example: 304.00 of 320.00 used, 16.00 released now, 304.00 stays held", active(32000), 30400, false,
			domain.HoldChange{Status: domain.HoldCaptured, CapturedMinor: 30400, ReleaseMinor: 1600}, nil},
		{"all of it used", active(32000), 32000, false,
			domain.HoldChange{Status: domain.HoldCaptured, CapturedMinor: 32000, ReleaseMinor: 0}, nil},
		{"with a posting the whole hold is released and settled", active(32000), 30400, true,
			domain.HoldChange{Status: domain.HoldCaptured, CapturedMinor: 30400, ReleaseMinor: 32000, Settle: true}, nil},
		{"more than held", active(32000), 32001, false, domain.HoldChange{}, domain.ErrHoldState},
		{"nothing used is a release, not a capture", active(32000), 0, false, domain.HoldChange{}, domain.ErrInvalidHold},
		{"a negative amount", active(32000), -1, false, domain.HoldChange{}, domain.ErrInvalidHold},

		{"the same capture again is a replay", captured(32000, 30400, false), 30400, false, domain.HoldChange{Replay: true}, nil},
		{"the same capture after settlement is still a replay", captured(32000, 30400, true), 30400, false, domain.HoldChange{Replay: true}, nil},
		{"the same capture with a posting again is a replay", captured(32000, 30400, true), 30400, true, domain.HoldChange{Replay: true}, nil},
		{"a capture with a posting after a plain one that isn't settled", captured(32000, 30400, false), 30400, true, domain.HoldChange{}, domain.ErrHoldState},
		{"another amount after a capture", captured(32000, 30400, false), 30000, false, domain.HoldChange{}, domain.ErrHoldState},

		{"after a release", domain.Hold{Status: domain.HoldReleased, AmountMinor: 32000, Settled: true}, 30400, false, domain.HoldChange{}, domain.ErrHoldState},
		{"after expiry", domain.Hold{Status: domain.HoldExpired, AmountMinor: 32000, Settled: true}, 30400, false, domain.HoldChange{}, domain.ErrHoldState},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.CaptureHold(tc.hold, tc.used, tc.withPosting)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("got %v, want %v", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestReleaseHold(t *testing.T) {
	cases := []struct {
		name string
		hold domain.Hold
		want domain.HoldChange
		err  error
	}{
		{"an active hold gives everything back", active(32000), domain.HoldChange{Status: domain.HoldReleased, ReleaseMinor: 32000, Settle: true}, nil},
		{"released again is a replay", domain.Hold{Status: domain.HoldReleased, AmountMinor: 32000, Settled: true}, domain.HoldChange{Replay: true}, nil},
		{"an expired hold is already given back", domain.Hold{Status: domain.HoldExpired, AmountMinor: 32000, Settled: true}, domain.HoldChange{Replay: true}, nil},
		{"a captured hold can't be released", domain.Hold{Status: domain.HoldCaptured, AmountMinor: 32000, CapturedMinor: 1}, domain.HoldChange{}, domain.ErrHoldState},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.ReleaseHold(tc.hold)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("got %v, want %v", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestHoldContribution: what a hold adds to held_minor, the invariant
// the property test and the nightly rebuild (3.8) check.
func TestHoldContribution(t *testing.T) {
	cases := []struct {
		hold domain.Hold
		want int64
	}{
		{active(32000), 32000},
		{domain.Hold{Status: domain.HoldCaptured, AmountMinor: 32000, CapturedMinor: 30400}, 30400},
		{domain.Hold{Status: domain.HoldCaptured, AmountMinor: 32000, CapturedMinor: 30400, Settled: true}, 0},
		{domain.Hold{Status: domain.HoldReleased, AmountMinor: 32000, Settled: true}, 0},
		{domain.Hold{Status: domain.HoldExpired, AmountMinor: 32000, Settled: true}, 0},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%+v", tc.hold), func(t *testing.T) {
			if got := domain.HoldContribution(tc.hold); got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestCaptureThenContribution: a capture's release plus what stays
// held always adds up to the hold's amount, so no money is lost or
// counted twice.
func TestCaptureThenContribution(t *testing.T) {
	for _, withPosting := range []bool{false, true} {
		for _, used := range []int64{1, 15999, 16000, 31999, 32000} {
			h := active(32000)
			ch, err := domain.CaptureHold(h, used, withPosting)
			if err != nil {
				t.Fatal(err)
			}
			after := domain.Hold{Status: ch.Status, AmountMinor: h.AmountMinor, CapturedMinor: ch.CapturedMinor, Settled: ch.Settle}
			if got := ch.ReleaseMinor + domain.HoldContribution(after); got != 32000 {
				t.Fatalf("used %d, posting %v: released %d + still held %d = %d, want 32000",
					used, withPosting, ch.ReleaseMinor, domain.HoldContribution(after), got)
			}
		}
	}
}
