package grpc

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

// TestToStatus pins every app error's gRPC code and VASPRO code (system
// design §8: BAL-4002 is "no tokens left for this service").
func TestToStatus(t *testing.T) {
	s := &Server{log: discard()}
	cases := []struct {
		err    error
		code   codes.Code
		reason string
	}{
		{app.ErrValidation, codes.InvalidArgument, "VAL-4000"},
		{app.ErrNotFound, codes.NotFound, "VAL-4003"},
		{app.ErrIdempotencyConflict, codes.AlreadyExists, "VAL-4001"},
		{app.ErrInsufficientBalance, codes.FailedPrecondition, "BAL-4001"},
		{app.ErrInsufficientTokens, codes.FailedPrecondition, "BAL-4002"},
		{domain.ErrHoldState, codes.FailedPrecondition, "BAL-4003"},
		{domain.ErrAllocationFinal, codes.InvalidArgument, "BAL-4004"},
		{app.ErrNoPrice, codes.FailedPrecondition, "BAL-4005"},
		{app.ErrAccountNotOpen, codes.FailedPrecondition, "BAL-4006"},
		{domain.ErrReversalNotCovered, codes.FailedPrecondition, "BAL-4007"},
		{domain.ErrNotAwaitingPayback, codes.FailedPrecondition, "VAL-4016"},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			st, _ := status.FromError(s.toStatus(fmt.Errorf("wrapped: %w", tc.err)))
			if st.Code() != tc.code {
				t.Fatalf("code %s, want %s", st.Code(), tc.code)
			}
			var reason string
			for _, d := range st.Details() {
				if info, ok := d.(*errdetails.ErrorInfo); ok {
					reason = info.GetReason()
				}
			}
			if reason != tc.reason {
				t.Fatalf("reason %q, want %q", reason, tc.reason)
			}
		})
	}
	t.Run("anything else is INTERNAL", func(t *testing.T) {
		st, _ := status.FromError((&Server{log: discard()}).toStatus(errors.New("boom")))
		if st.Code() != codes.Internal {
			t.Fatalf("code %s", st.Code())
		}
	})
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
