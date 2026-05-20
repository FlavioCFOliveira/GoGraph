package server

import (
	"strings"
	"sync"
	"testing"

	"gograph/bolt/packstream"
)

// ─────────────────────────────────────────────────────────────────────────────
// Task 315: Bookmark tests
// ─────────────────────────────────────────────────────────────────────────────

// TestNextBookmark_MonotonicallyIncreasing verifies that successive calls to
// NextBookmark return strictly increasing values even under concurrent access.
// The race detector is relied upon to catch any data race.
func TestNextBookmark_MonotonicallyIncreasing(t *testing.T) {
	t.Parallel()

	const goroutines = 64
	const perGoroutine = 100

	bookmarks := make([]string, goroutines*perGoroutine)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for g := range goroutines {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			local := make([]string, perGoroutine)
			for i := range perGoroutine {
				local[i] = NextBookmark()
			}
			mu.Lock()
			copy(bookmarks[gid*perGoroutine:], local)
			mu.Unlock()
		}(g)
	}
	wg.Wait()

	// Verify all bookmarks are non-empty and have the expected prefix.
	seen := make(map[string]struct{}, len(bookmarks))
	for _, bm := range bookmarks {
		if bm == "" {
			t.Fatal("NextBookmark returned empty string")
		}
		if !strings.HasPrefix(bm, "FB:k") {
			t.Errorf("bookmark %q does not start with FB:k", bm)
		}
		if _, dup := seen[bm]; dup {
			t.Errorf("duplicate bookmark: %q", bm)
		}
		seen[bm] = struct{}{}
	}
}

// TestExtractBookmarks verifies extraction of bookmark lists from metadata maps.
func TestExtractBookmarks(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		extra map[string]packstream.Value
		want  []string
	}{
		{
			name:  "absent key",
			extra: map[string]packstream.Value{},
			want:  nil,
		},
		{
			name:  "nil value",
			extra: map[string]packstream.Value{"bookmarks": nil},
			want:  nil,
		},
		{
			name: "single bookmark",
			extra: map[string]packstream.Value{
				"bookmarks": []packstream.Value{"FB:k00000001"},
			},
			want: []string{"FB:k00000001"},
		},
		{
			name: "multiple bookmarks",
			extra: map[string]packstream.Value{
				"bookmarks": []packstream.Value{"FB:k00000001", "FB:k00000002"},
			},
			want: []string{"FB:k00000001", "FB:k00000002"},
		},
		{
			name: "non-string entries filtered",
			extra: map[string]packstream.Value{
				"bookmarks": []packstream.Value{int64(42), "FB:k00000003"},
			},
			want: []string{"FB:k00000003"},
		},
		{
			name: "all non-string entries",
			extra: map[string]packstream.Value{
				"bookmarks": []packstream.Value{int64(1), int64(2)},
			},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ExtractBookmarks(tc.extra)
			if len(got) != len(tc.want) {
				t.Fatalf("ExtractBookmarks len: got %d, want %d (got=%v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("ExtractBookmarks[%d]: got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
