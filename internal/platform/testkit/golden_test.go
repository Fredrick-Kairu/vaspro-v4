package testkit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAssertGolden_MatchingContentPasses(t *testing.T) {
	AssertGolden(t, "sample.golden", []byte("hello golden\n"))
}

func TestCompareGolden_MismatchReportsNotOK(t *testing.T) {
	ok, want, err := compareGolden("sample.golden", []byte("not what's on disk\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected a content mismatch to report ok=false")
	}
	if string(want) != "hello golden\n" {
		t.Fatalf("want %q, got %q", "hello golden\n", want)
	}
}

func TestCompareGolden_MissingFileErrors(t *testing.T) {
	if _, _, err := compareGolden("does-not-exist.golden", []byte("x")); err == nil {
		t.Fatal("expected a missing golden file to error")
	}
}

func TestAssertGolden_UpdateFlagWritesFile(t *testing.T) {
	path := filepath.Join("testdata", "tmp-update-test.golden")
	t.Cleanup(func() { _ = os.Remove(path) })

	*update = true
	t.Cleanup(func() { *update = false })

	AssertGolden(t, "tmp-update-test.golden", []byte("written by -update\n"))

	got, err := os.ReadFile(path) //nolint:gosec // path is this test's own fixed testdata fixture name, not external input
	if err != nil {
		t.Fatalf("expected the golden file to have been written: %v", err)
	}
	if string(got) != "written by -update\n" {
		t.Fatalf("got %q, want %q", got, "written by -update\n")
	}
}
