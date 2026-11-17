//go:build integration

package clamav_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clamav"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/clamd"
)

// TestClient_AgainstRealClamAV checks the streamed protocol against
// ClamAV itself. It needs about 1 GB of memory and half a minute.
func TestClient_AgainstRealClamAV(t *testing.T) {
	ctx := context.Background()
	client := clamav.New(testkit.StartClamAV(t))
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	res, err := client.Scan(ctx, bytes.NewReader(bytes.Repeat([]byte("name,city\n"), 200_000)))
	if err != nil || !res.Clean {
		t.Fatalf("a 2 MB clean file: %+v, %v", res, err)
	}
	res, err = client.Scan(ctx, bytes.NewReader(clamd.EICAR()))
	if err != nil || res.Clean || res.Signature == "" {
		t.Fatalf("EICAR: %+v, %v", res, err)
	}
}
