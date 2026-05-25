package server

import (
	"fmt"
	"strings"
	"testing"
)

// TestNextBookmark_10000StrictlyMonotone verifies AC1 of T741: 1e4 consecutive
// calls to NextBookmark produce strictly monotonically increasing bookmark
// values. "Strictly increasing" is checked both on the string representation
// (lexicographic order holds because the counter is zero-padded hex) and on the
// embedded counter value decoded from the bookmark suffix.
func TestNextBookmark_10000StrictlyMonotone(t *testing.T) {
	t.Parallel()

	const n = 10_000
	bookmarks := make([]string, n)
	for i := range n {
		bookmarks[i] = NextBookmark()
	}

	const prefix = "FB:k"
	var prevCounter uint64

	for i, bm := range bookmarks {
		// AC2 (also verified here): format must match "FB:k%08x".
		if !strings.HasPrefix(bm, prefix) {
			t.Fatalf("bookmarks[%d] = %q: missing prefix %q", i, bm, prefix)
		}
		suffix := bm[len(prefix):]
		if len(suffix) != 8 {
			t.Fatalf("bookmarks[%d] = %q: suffix len %d, want 8 hex digits", i, bm, len(suffix))
		}
		var counter uint64
		if _, err := fmt.Sscanf(suffix, "%x", &counter); err != nil {
			t.Fatalf("bookmarks[%d] = %q: parse counter: %v", i, bm, err)
		}

		if i > 0 && counter <= prevCounter {
			prev := bookmarks[i-1] //nolint:gosec // i>0 is checked above; index is always valid
			t.Fatalf("bookmarks[%d] = %q (counter=%d) is not strictly greater than "+
				"bookmarks[%d] = %q (counter=%d): monotonicity violated",
				i, bm, counter,
				i-1, prev, prevCounter)
		}
		prevCounter = counter
	}
}
