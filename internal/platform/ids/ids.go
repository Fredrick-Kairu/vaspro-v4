// Package ids generates primary keys and the prefixed public IDs built
// from them (data model §1, D-04): UUIDv7 internally, "<prefix>_<uuid>"
// at the API boundary.
package ids

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// New returns a fresh UUIDv7. google/uuid's NewV7 guards a process-wide
// last-generated timestamp, so even two calls in the same nanosecond come
// out strictly increasing — IDs sort by creation time, not just by
// millisecond.
func New() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("ids: generating uuidv7: %w", err)
	}
	return id, nil
}

// MustNew is New, panicking on error. crypto/rand failing is not a
// request-time condition worth plumbing an error return through every
// caller for; use New directly wherever that matters.
func MustNew() uuid.UUID {
	id, err := New()
	if err != nil {
		panic(err)
	}
	return id
}

// NewPrefixed returns "<prefix>_<uuidv7>" — the public-facing form data
// model §1 calls out (e.g. "msg_…", "inv_…"). The design fixes the
// concept, not a byte encoding; a plain prefixed canonical UUID string is
// the simplest choice and the cheapest to change later if that turns out
// to matter.
func NewPrefixed(prefix string) (string, error) {
	id, err := New()
	if err != nil {
		return "", err
	}
	return prefix + "_" + id.String(), nil
}

// MustNewPrefixed is NewPrefixed, panicking on error.
func MustNewPrefixed(prefix string) string {
	id, err := NewPrefixed(prefix)
	if err != nil {
		panic(err)
	}
	return id
}

// ParsePrefixed validates that s is "<prefix>_<uuid>" and returns the
// uuid — the inverse of NewPrefixed.
func ParsePrefixed(prefix, s string) (uuid.UUID, error) {
	want := prefix + "_"
	rest, ok := strings.CutPrefix(s, want)
	if !ok {
		return uuid.UUID{}, fmt.Errorf("ids: %q does not have the prefix %q", s, want)
	}
	id, err := uuid.Parse(rest)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("ids: %q is not a valid uuid: %w", rest, err)
	}
	return id, nil
}
