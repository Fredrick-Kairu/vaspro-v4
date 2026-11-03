package adapters_test

import (
	"context"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/clamd"
)

// TestClamdScanner_AgainstTheFake runs the real adapter against the
// fake clamd's copy of the protocol; scanner_integration_test.go runs
// it against real ClamAV.
func TestClamdScanner_AgainstTheFake(t *testing.T) {
	fake, err := clamd.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	scanner := adapters.NewClamdScanner(fake.Addr())
	ctx := context.Background()

	if err := scanner.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	clean, err := scanner.Scan(ctx, []byte("%PDF-1.4 a harmless document"))
	if err != nil || !clean.Clean {
		t.Fatalf("a clean file: %+v, %v", clean, err)
	}
	big := make([]byte, 3<<20) // several INSTREAM chunks
	copy(big[len(big)-200:], clamd.EICAR())
	infected, err := scanner.Scan(ctx, big)
	if err != nil || infected.Clean || infected.Signature != "Eicar-Test-Signature" {
		t.Fatalf("EICAR at the end of 3 MB: %+v, %v", infected, err)
	}

	fake.Close()
	if _, err := scanner.Scan(ctx, []byte("anything")); err == nil {
		t.Fatal("a scan with clamd down succeeded; it must fail closed")
	}
}
