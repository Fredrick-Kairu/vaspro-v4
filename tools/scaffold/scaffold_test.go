package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateName_AcceptsAGoodName(t *testing.T) {
	root := t.TempDir()
	if err := ValidateName(root, "identity"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateName_RejectsBadCharacters(t *testing.T) {
	root := t.TempDir()
	cases := []string{"Identity", "identity-service", "123identity", "", "identity service", "identity!"}
	for _, name := range cases {
		if err := ValidateName(root, name); err == nil {
			t.Errorf("expected %q to be rejected", name)
		}
	}
}

func TestValidateName_RejectsReservedWords(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"platform", "all", "vaspro_all"} {
		if err := ValidateName(root, name); err == nil {
			t.Errorf("expected %q to be rejected as reserved", name)
		}
	}
}

func TestValidateName_RejectsAlreadyExistingService(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal", "identity"), 0o750); err != nil {
		t.Fatalf("failed to set up fixture: %v", err)
	}

	if err := ValidateName(root, "identity"); err == nil {
		t.Fatal("expected an already-existing service directory to be rejected")
	}
}

func TestInsertAfter_InsertsRightAfterTheSentinelLine(t *testing.T) {
	content := "line one\n// sentinel\nline two\n"
	got, err := insertAfter(content, "// sentinel", "inserted")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "line one\n// sentinel\ninserted\nline two\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestInsertAfter_MissingSentinelErrors(t *testing.T) {
	if _, err := insertAfter("no sentinel here\n", "// sentinel", "inserted"); err == nil {
		t.Fatal("expected a missing sentinel to error")
	}
}
