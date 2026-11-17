package domain_test

import (
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/domain"
)

// TestImportTransitions is data model §8's status table, every pair.
func TestImportTransitions(t *testing.T) {
	allowed := map[domain.ImportStatus][]domain.ImportStatus{
		domain.AwaitingUpload: {domain.Scanning, domain.Cancelled, domain.Expired},
		domain.Scanning:       {domain.Validating, domain.Failed},
		domain.Validating:     {domain.Processing, domain.Failed},
		domain.Processing:     {domain.Completed, domain.CompletedWithErrors, domain.Failed},
	}
	all := []domain.ImportStatus{domain.AwaitingUpload, domain.Scanning, domain.Validating, domain.Processing,
		domain.Completed, domain.CompletedWithErrors, domain.Failed, domain.Cancelled, domain.Expired}
	for _, from := range all {
		for _, to := range all {
			want := false
			for _, a := range allowed[from] {
				want = want || a == to
			}
			if got := domain.CanMoveImport(from, to); got != want {
				t.Errorf("%s → %s: %v, want %v", from, to, got, want)
			}
		}
		final := len(allowed[from]) == 0
		if got := from.Final(); got != final {
			t.Errorf("%s final: %v, want %v", from, got, final)
		}
	}
}
