package clamav_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clamav"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/clamd"
)

func startFake(t *testing.T) *clamd.Server {
	t.Helper()
	fake, err := clamd.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	return fake
}

// TestClient_AgainstTheFake runs the client against the fake's copy of
// clamd's protocol; clamav_integration_test.go runs it against ClamAV.
func TestClient_AgainstTheFake(t *testing.T) {
	ctx := context.Background()
	fake := startFake(t)
	client := clamav.New(fake.Addr())

	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	t.Run("a clean file", func(t *testing.T) {
		res, err := client.Scan(ctx, bytes.NewReader([]byte("name,city\nTest,Nairobi\n")))
		if err != nil || !res.Clean {
			t.Fatalf("%+v, %v", res, err)
		}
	})

	t.Run("an empty file is clean", func(t *testing.T) {
		res, err := client.Scan(ctx, bytes.NewReader(nil))
		if err != nil || !res.Clean {
			t.Fatalf("%+v, %v", res, err)
		}
	})

	// EICAR at the end of a stream sent in many chunks, and straddling
	// every chunk boundary the client could use.
	for _, at := range []int{0, 1 << 16, 1<<16 - 30, 1<<20 - 30, 3<<20 - 68} {
		t.Run("EICAR found wherever it sits", func(t *testing.T) {
			data := make([]byte, 3<<20)
			copy(data[at:], clamd.EICAR())
			res, err := client.Scan(ctx, bytes.NewReader(data))
			if err != nil || res.Clean || res.Signature != "Eicar-Test-Signature" {
				t.Fatalf("EICAR at %d: %+v, %v", at, res, err)
			}
		})
	}

	t.Run("a stream over clamd's limit is ErrTooLarge, not clean", func(t *testing.T) {
		fake.SetStreamMaxLength(1 << 20)
		defer fake.SetStreamMaxLength(clamd.DefaultStreamMaxLength)
		res, err := client.Scan(ctx, io.LimitReader(zeros{}, 3<<20))
		if !errors.Is(err, clamav.ErrTooLarge) || res.Clean {
			t.Fatalf("%+v, %v; want ErrTooLarge", res, err)
		}
	})

	t.Run("a reader that fails is an error, not a verdict", func(t *testing.T) {
		res, err := client.Scan(ctx, io.MultiReader(bytes.NewReader([]byte("abc")), failing{}))
		if err == nil || res.Clean {
			t.Fatalf("%+v, %v", res, err)
		}
	})

	t.Run("clamd down fails closed", func(t *testing.T) {
		down := startFake(t)
		addr := down.Addr()
		down.Close()
		res, err := clamav.New(addr).Scan(ctx, bytes.NewReader([]byte("anything")))
		if err == nil || res.Clean {
			t.Fatalf("%+v, %v", res, err)
		}
		if err := clamav.New(addr).Ping(ctx); err == nil {
			t.Fatal("Ping with clamd down succeeded")
		}
	})
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type failing struct{}

func (failing) Read([]byte) (int, error) { return 0, errors.New("storage went away") }
