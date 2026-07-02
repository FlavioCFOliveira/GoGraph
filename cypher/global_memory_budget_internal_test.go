package cypher

// global_memory_budget_internal_test.go — white-box guard for the policy mapping
// of EngineOptions.GlobalMaxResultBytes (#1842): the zero value derives the
// ceiling from the Go soft memory limit (GOMEMLIMIT), the unlimited sentinel
// disables it, and a positive value is used verbatim.

import (
	"math"
	"runtime/debug"
	"testing"
)

func TestResolveGlobalMaxResultBytes_Policy(t *testing.T) {
	// Save and restore the process soft memory limit so this test never leaks a
	// GOMEMLIMIT change into other tests.
	orig := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(orig) })

	t.Run("explicit_positive_verbatim", func(t *testing.T) {
		if got := resolveGlobalMaxResultBytes(12345); got != 12345 {
			t.Fatalf("positive value: got %d, want 12345 (verbatim)", got)
		}
	})

	t.Run("unlimited_sentinel", func(t *testing.T) {
		if got := resolveGlobalMaxResultBytes(GlobalMaxResultBytesUnlimited); got != 0 {
			t.Fatalf("unlimited sentinel: got %d, want 0 (no ceiling)", got)
		}
	})

	t.Run("default_derives_half_of_gomemlimit", func(t *testing.T) {
		debug.SetMemoryLimit(1 << 30) // 1 GiB soft limit
		if got := resolveGlobalMaxResultBytes(0); got != 1<<29 {
			t.Fatalf("default with GOMEMLIMIT=1 GiB: got %d, want %d (half)", got, int64(1<<29))
		}
	})

	t.Run("default_unlimited_when_no_gomemlimit", func(t *testing.T) {
		debug.SetMemoryLimit(math.MaxInt64) // the "no soft limit" value
		if got := resolveGlobalMaxResultBytes(0); got != 0 {
			t.Fatalf("default with no GOMEMLIMIT: got %d, want 0 (unlimited)", got)
		}
	})
}
