package ledger_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger"
)

// A bad LEDGER_REBUILD_AT stops startup before anything connects or
// listens (task 3.8).
func TestRun_RefusesABadRebuildTime(t *testing.T) {
	for _, bad := range []string{"24:00", "9:30", "noon"} {
		svc := ledger.New(ledger.Config{RebuildAt: bad}, nil, nil, nil, nil)
		err := svc.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "LEDGER_REBUILD_AT") {
			t.Fatalf("%q: got %v, want a LEDGER_REBUILD_AT error", bad, err)
		}
	}
}
