//go:build integration

package adapters_test

import (
	"context"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/clamd"
)

// TestClamdScanner_AgainstRealClamAV is the one test that runs real
// ClamAV (task 2.8): the adapter's protocol is ClamAV's, not just the
// fake's.
func TestClamdScanner_AgainstRealClamAV(t *testing.T) {
	scanner := adapters.NewClamdScanner(testkit.StartClamAV(t))
	ctx := context.Background()

	clean, err := scanner.Scan(ctx, []byte("%PDF-1.4\n% a harmless test document\n"))
	if err != nil || !clean.Clean {
		t.Fatalf("a clean file: %+v, %v", clean, err)
	}
	infected, err := scanner.Scan(ctx, clamd.EICAR())
	if err != nil || infected.Clean || infected.Signature == "" {
		t.Fatalf("the EICAR test file: %+v, %v; want a signature", infected, err)
	}
	t.Logf("ClamAV named it %q", infected.Signature)
}
