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

func TestStartWithRetries(t *testing.T) {
	ctx := context.Background()
	logf := func(string, ...any) {}

	t.Run("a failed start is torn down and a fresh one tried", func(t *testing.T) {
		attempts, terminated := 0, 0
		got, terminate, err := startWithRetries(ctx, logf, "widget", 3, func(context.Context) (int, func(), error) {
			attempts++
			stop := func() { terminated++ }
			if attempts < 3 {
				return 0, stop, errors.New("port never came up")
			}
			return 42, stop, nil
		})
		if err != nil || got != 42 || attempts != 3 {
			t.Fatalf("got %d, %v after %d attempts", got, err, attempts)
		}
		if terminated != 2 {
			t.Fatalf("%d failed containers torn down, want 2", terminated)
		}
		terminate()
		if terminated != 3 {
			t.Fatal("the returned terminate doesn't stop the working container")
		}
	})
	t.Run("after the last attempt the error names what and how many tries", func(t *testing.T) {
		_, _, err := startWithRetries(ctx, logf, "widget", 2, func(context.Context) (int, func(), error) {
			return 0, nil, errors.New("port never came up")
		})
		if err == nil || !strings.Contains(err.Error(), "widget") || !strings.Contains(err.Error(), "2 attempts") ||
			!strings.Contains(err.Error(), "port never came up") {
			t.Fatalf("got %v", err)
		}
	})
}
