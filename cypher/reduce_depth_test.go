package cypher_test

// reduce_depth_test.go — regression tests for the value-nesting-depth blocker
// (2026-07-13 production-readiness audit, security finding F1).
//
// reduce() can add one nesting level to its accumulator per iteration while
// charging only one element against the element budget, so a short query such
// as `RETURN reduce(acc=[0], x IN range(1,4900000) | [acc])` builds a value
// millions of levels deep inside the 10,000,000-element ceiling. Downstream
// recursive value walkers (result accounting, PackStream encoding,
// String/Hash/Equal) then overflow the goroutine stack — a fatal,
// unrecoverable crash that recover() cannot catch and that takes down every
// other connection on a shared server. The evaluator now caps constructed value
// depth (expr.MaxValueDepth) and rejects an over-deep or over-large accumulator
// with a typed error. These tests completing at all — no fatal stack overflow —
// is itself part of what they assert.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// runReduceQueryExpectErr runs q and returns the error surfaced either by Run or
// by the lazy result stream (Volcano evaluation defers the eval error to the
// first pull). It fails the test if the query completes without any error.
func runReduceQueryExpectErr(tb testing.TB, q string) error {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngineWithRegistry(g, funcs.DefaultRegistry)
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		return err
	}
	defer res.Close()
	for res.Next() { // drain the stream to surface the lazy error
	}
	if e := res.Err(); e != nil {
		return e
	}
	tb.Fatalf("Run(%q): expected an error, got none", q)
	return nil
}

// TestReduce_DeepNestingRejected is the core regression: a reduce that would
// build a value deeper than expr.MaxValueDepth is rejected with a typed error
// instead of crashing. range(1,6000) exceeds both the depth cap and the
// in-loop stride check.
func TestReduce_DeepNestingRejected(t *testing.T) {
	t.Parallel()
	err := runReduceQueryExpectErr(t, "RETURN reduce(acc = [0], x IN range(1, 6000) | [acc])")
	if !strings.Contains(err.Error(), "nested deeper") {
		t.Fatalf("error = %v, want a value-too-deep error", err)
	}
}

// TestReduce_AliasedExplosionRejected covers the aliasing variant: [acc, acc]
// keeps depth linear but doubles the logical node count each iteration (a shared
// accumulator under two slots). The depth walk's visit cap rejects it rather
// than exploring an exponential structure or letting a downstream walker do so.
func TestReduce_AliasedExplosionRejected(t *testing.T) {
	t.Parallel()
	err := runReduceQueryExpectErr(t, "RETURN reduce(acc = [0], x IN range(1, 40) | [acc, acc])")
	if !strings.Contains(err.Error(), "nested deeper") {
		t.Fatalf("error = %v, want a value-too-deep error", err)
	}
}

// TestReduce_ModerateNestingAccepted guards against a false positive: a reduce
// that nests well within expr.MaxValueDepth must still succeed. This pins the
// cap high enough not to reject legitimate use.
func TestReduce_ModerateNestingAccepted(t *testing.T) {
	t.Parallel()
	// 500 levels deep — comfortably under the 1000 cap.
	got := runReduceQuery(t, "RETURN reduce(acc = [0], x IN range(1, 500) | [acc])")
	if got == nil {
		t.Fatalf("moderate-depth reduce returned nil, want a nested list value")
	}
}
