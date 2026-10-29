package domain

import (
	"strings"
	"testing"
)

func TestGenerateSMPPAccountPassword_LengthAndAlphabet(t *testing.T) {
	password, err := GenerateSMPPAccountPassword()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(password) != SMPPAccountPasswordLength {
		t.Fatalf("got length %d, want %d", len(password), SMPPAccountPasswordLength)
	}
	for _, c := range password {
		if !strings.ContainsRune(smppAccountPasswordAlphabet, c) {
			t.Fatalf("password %q contains a character outside the SMPP-safe alphabet", password)
		}
	}
}

func TestGenerateSMPPAccountPassword_DiffersAcrossCalls(t *testing.T) {
	first, err := GenerateSMPPAccountPassword()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := GenerateSMPPAccountPassword()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first == second {
		t.Fatal("expected two calls to produce different passwords")
	}
}
