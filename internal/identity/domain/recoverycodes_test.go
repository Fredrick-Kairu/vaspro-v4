package domain

import "testing"

func TestGenerateRecoveryCodes_CountAndShape(t *testing.T) {
	codes, err := GenerateRecoveryCodes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(codes) != RecoveryCodeCount {
		t.Fatalf("got %d codes, want %d", len(codes), RecoveryCodeCount)
	}

	seen := make(map[string]bool, len(codes))
	for _, c := range codes {
		if len(c) != RecoveryCodeLength {
			t.Fatalf("code %q has length %d, want %d", c, len(c), RecoveryCodeLength)
		}
		if seen[c] {
			t.Fatalf("duplicate code %q in a single batch", c)
		}
		seen[c] = true
	}
}

func TestGenerateRecoveryCodes_DiffersAcrossCalls(t *testing.T) {
	first, err := GenerateRecoveryCodes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := GenerateRecoveryCodes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first[0] == second[0] {
		t.Fatal("expected two batches to produce different codes")
	}
}
