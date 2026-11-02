package testkit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWaitUntilReady(t *testing.T) {
	tests := []struct {
		name     string
		probe    func(calls int) func(ctx context.Context) error
		wantErr  []string // substrings; nil means success
		minCalls int
	}{
		{
			name: "succeeds once the probe does",
			probe: func(calls int) func(context.Context) error {
				n := 0
				return func(context.Context) error {
					n++
					if n < calls {
						return errors.New("not yet")
					}
					return nil
				}
			},
			minCalls: 3,
		},
		{
			name: "a probe that never succeeds reports what and the last error",
			probe: func(int) func(context.Context) error {
				return func(context.Context) error { return errors.New("connection refused") }
			},
			wantErr: []string{"widget not ready after", "connection refused"},
		},
		{
			name: "a hanging probe is cancelled per attempt and retried",
			probe: func(calls int) func(context.Context) error {
				n := 0
				return func(ctx context.Context) error {
					n++
					if n < calls {
						<-ctx.Done()
						return ctx.Err()
					}
					return nil
				}
			},
			minCalls: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			probe := tt.probe(tt.minCalls)
			counted := func(ctx context.Context) error {
				calls++
				return probe(ctx)
			}
			err := waitUntilReady(context.Background(), "widget", 500*time.Millisecond, 10*time.Millisecond, counted)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("got %v, want nil", err)
				}
				if calls != tt.minCalls {
					t.Fatalf("probe called %d times, want %d", calls, tt.minCalls)
				}
				return
			}
			if err == nil {
				t.Fatal("got nil, want an error")
			}
			for _, s := range tt.wantErr {
				if !strings.Contains(err.Error(), s) {
					t.Fatalf("error %q doesn't mention %q", err, s)
				}
			}
		})
	}
}
