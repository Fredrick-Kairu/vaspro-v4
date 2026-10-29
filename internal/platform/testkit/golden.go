package testkit

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "update golden files in testdata/")

// AssertGolden compares got against testdata/<name> (relative to the
// calling test's package directory), failing the test on a mismatch.
// Run `go test -update ./...` to write or refresh the golden file —
// review the diff before committing it (.claude/rules/tests.md).
func AssertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	ok, want, err := compareGolden(name, got)
	if err != nil {
		t.Fatalf("testkit: %v", err)
	}
	if !ok {
		t.Fatalf("testkit: %s does not match golden file; run with -update to review and accept the diff\n--- got ---\n%s\n--- want ---\n%s",
			filepath.Join("testdata", name), got, want)
	}
}

// compareGolden holds AssertGolden's logic without any *testing.T
// dependency, so it's directly unit-testable (a subtest's t.Fatalf
// unavoidably marks its parent failed too, which makes "assert this
// call fails" awkward to test any other way).
func compareGolden(name string, got []byte) (ok bool, want []byte, err error) {
	path := filepath.Join("testdata", name)

	if *update {
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o750); mkErr != nil {
			return false, nil, fmt.Errorf("creating testdata dir for %s: %w", path, mkErr)
		}
		if wErr := os.WriteFile(path, got, 0o644); wErr != nil { //nolint:gosec // golden fixtures are non-sensitive test data
			return false, nil, fmt.Errorf("writing golden file %s: %w", path, wErr)
		}
		return true, got, nil
	}

	want, err = os.ReadFile(path) //nolint:gosec // path is built from a caller-supplied golden file name, not untrusted input
	if err != nil {
		return false, nil, fmt.Errorf("reading golden file %s (run with -update to create it): %w", path, err)
	}
	return string(got) == string(want), want, nil
}
