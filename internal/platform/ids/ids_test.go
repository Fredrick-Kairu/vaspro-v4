package ids_test

import (
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
)

func TestNew_SortsByCreationTime(t *testing.T) {
	const n = 100
	generated := make([]string, n)
	for i := range n {
		id, err := ids.New()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		generated[i] = id.String()
	}

	for i := 1; i < n; i++ {
		if generated[i] <= generated[i-1] {
			t.Fatalf("id[%d] = %q is not strictly greater than id[%d] = %q", i, generated[i], i-1, generated[i-1])
		}
	}
}

func TestMustNew_ReturnsAValidUUID(t *testing.T) {
	id := ids.MustNew()
	if id.String() == "" {
		t.Fatal("expected a non-empty UUID")
	}
}

func TestNewPrefixed_ParsePrefixed_RoundTrip(t *testing.T) {
	// Build the prefixed string from a known UUID directly, rather than
	// through NewPrefixed, so this actually checks ParsePrefixed extracts
	// the same value that went in — not just that the two functions agree
	// with each other.
	want := ids.MustNew()
	public := "msg_" + want.String()

	got, err := ids.ParsePrefixed("msg", public)
	if err != nil {
		t.Fatalf("unexpected error parsing %q: %v", public, err)
	}
	if got != want {
		t.Fatalf("ParsePrefixed(%q) = %v, want %v", public, got, want)
	}
}

func TestNewPrefixed_ThenParsePrefixed_Succeeds(t *testing.T) {
	public, err := ids.NewPrefixed("msg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ids.ParsePrefixed("msg", public); err != nil {
		t.Fatalf("NewPrefixed's own output didn't parse: %v", err)
	}
}

func TestMustNewPrefixed_HasTheRightShape(t *testing.T) {
	public := ids.MustNewPrefixed("inv")
	if len(public) < len("inv_")+36 {
		t.Fatalf("expected %q to look like inv_<uuid>", public)
	}
	if _, err := ids.ParsePrefixed("inv", public); err != nil {
		t.Fatalf("MustNewPrefixed's own output didn't parse: %v", err)
	}
}

func TestParsePrefixed_RejectsWrongPrefix(t *testing.T) {
	public := ids.MustNewPrefixed("msg")
	if _, err := ids.ParsePrefixed("inv", public); err == nil {
		t.Fatal("expected parsing with the wrong prefix to fail")
	}
}

func TestParsePrefixed_RejectsMalformedUUID(t *testing.T) {
	if _, err := ids.ParsePrefixed("msg", "msg_not-a-uuid"); err == nil {
		t.Fatal("expected a malformed UUID tail to fail")
	}
}

func TestParsePrefixed_RejectsMissingSeparator(t *testing.T) {
	id := ids.MustNew()
	if _, err := ids.ParsePrefixed("msg", "msg"+id.String()); err == nil {
		t.Fatal("expected a missing separator to fail")
	}
}
