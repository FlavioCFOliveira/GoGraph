package exec

// top_stable_prefix_property_test.go — #2509
//
// THE correctness crux for fusing ORDER BY … SKIP s LIMIT k into Skip(s) over
// Top(s+k): the fused plan is only equivalent to Sort → Skip → Limit if
//
//	Top(n)  ==  Sort truncated to n
//
// as a SEQUENCE, not merely as a set. Today's fusion is literal-only and
// SKIP-free, so the window Top serves always starts at row 0 and a divergence in
// the ORDER of rows that tie on the sort key is invisible to the caller who
// asked only for "the best k". The moment a SKIP is fused, the window moves into
// the middle of the ordered stream, and any transposition of tied rows inside
// Top's output changes WHICH rows the page contains — a different answer to the
// same query, with no error and no failing test anywhere in the tree.
//
// The existing coverage cannot reach this. sort_ties_test.go orders 3 nodes and
// order_tie_aggregation_test.go 2 groups, both under total orders with no actual
// tie; the openCypher TCK's whole ORDER BY/SKIP/LIMIT surface is six scenarios
// over 5–16 rows with distinct keys. So this file generates the shape those
// cannot: many rows, few distinct key values, hence long runs of ties, with the
// window boundary landing inside a run most of the time.
//
// The oracle is the [Sort] operator itself, driven over the same rows with the
// same keys, truncated to n. Sort is stable by construction (see
// [Sort.sortDecorated]), so "stable Sort truncated to n" is not a re-derivation
// of the expected answer in the test — it is the engine's own definition of the
// ordered stream that the unfused plan would have produced.

import (
	"context"
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/internal/sortseam"
)

// topPropTB is the subset of the testing surface both *testing.T and *rapid.T
// provide, so the drain helper below serves the property body and the
// deterministic regressions alike.
type topPropTB interface {
	Helper()
	Fatalf(format string, args ...any)
	Errorf(format string, args ...any)
	Logf(format string, args ...any)
}

// drainTB runs an operator to completion and returns copies of the emitted rows.
// It duplicates sort_decoration_test.go's drain only in that it accepts the
// narrower [topPropTB], which *rapid.T satisfies and *testing.T does too.
func drainTB(t topPropTB, op Operator) []Row {
	t.Helper()
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var out []Row
	var row Row
	for {
		ok, err := op.Next(&row)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		cp := make(Row, len(row))
		copy(cp, row)
		out = append(out, cp)
	}
	if err := op.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Fixture
// ─────────────────────────────────────────────────────────────────────────────

// topPropFixture is one generated input: the rows, the ORDER BY key sequence to
// apply to them, and the human-readable shape for failure messages.
type topPropFixture struct {
	rows  []Row
	keys  []SortKey
	shape string
}

// Column layout of a generated row:
//
//	col 0            — the arrival ordinal, unique, never a sort key. This is the
//	                   row's IDENTITY: two rows that tie on every sort key are
//	                   distinguishable only here, so a transposition of tied rows
//	                   is visible in the emitted sequence and nowhere else.
//	col 1 … col K    — the key sources.
const topPropIDCol = 0

// genFixture builds rows whose sort keys collide heavily.
//
// The distinct-value count per key column is drawn far below the row count, so
// the expected run length of a tie is rows/distinct — with the defaults that is
// between 2 and 60 rows sharing a key, and a window boundary at a uniformly
// drawn n lands inside such a run most of the time. That is the whole point of
// the generator: a fixture with distinct keys can never fail this property.
//
// NULLs are injected into the key source at a low rate so the NULL-ordering
// branch of [expr.Compare] participates (NULLs last in ASC, first in DESC) and
// so a tie can be a tie BETWEEN NULLS, which compares equal but is not the same
// value as an integer tie.
func genFixture(t *rapid.T) topPropFixture {
	nRows := rapid.IntRange(1, 120).Draw(t, "rows")
	nKeys := rapid.IntRange(1, 3).Draw(t, "keys")

	// Distinct values per key column: 1 forces EVERY row to tie on that key.
	distinct := make([]int, nKeys)
	asc := make([]bool, nKeys)
	viaEval := make([]bool, nKeys)
	for j := 0; j < nKeys; j++ {
		distinct[j] = rapid.IntRange(1, 8).Draw(t, fmt.Sprintf("distinct%d", j))
		asc[j] = rapid.Bool().Draw(t, fmt.Sprintf("asc%d", j))
		// An evaluator-backed key is the shape ORDER BY n.age takes after
		// RETURN n; a ColIdx key is the projected-column shape. Both reach the
		// same comparator, but only the evaluator path allocates a key value,
		// so both must be exercised.
		viaEval[j] = rapid.Bool().Draw(t, fmt.Sprintf("eval%d", j))
	}
	nullRate := rapid.IntRange(0, 4).Draw(t, "nullRate") // in 16ths

	rows := make([]Row, nRows)
	for i := 0; i < nRows; i++ {
		// The payload is >= 256 so boxing it into an expr.Value allocates
		// rather than aliasing the runtime's small-integer table; that keeps
		// this fixture consistent with the allocation-sensitive ones next door.
		row := make(Row, 1+nKeys)
		row[topPropIDCol] = expr.IntegerValue(int64(1_000_000 + i))
		for j := 0; j < nKeys; j++ {
			if rapid.IntRange(0, 15).Draw(t, fmt.Sprintf("null%d_%d", i, j)) < nullRate {
				row[1+j] = expr.Null
				continue
			}
			v := rapid.IntRange(0, distinct[j]-1).Draw(t, fmt.Sprintf("v%d_%d", i, j))
			row[1+j] = expr.IntegerValue(int64(256 + v))
		}
		rows[i] = row
	}

	keys := make([]SortKey, nKeys)
	for j := 0; j < nKeys; j++ {
		col := 1 + j
		if viaEval[j] {
			keys[j] = SortKey{
				Ascending: asc[j],
				Eval: func(r Row) (expr.Value, error) {
					if col < len(r) {
						return r[col], nil
					}
					return expr.Null, nil
				},
			}
			continue
		}
		keys[j] = SortKey{ColIdx: col, Ascending: asc[j]}
	}

	return topPropFixture{
		rows: rows,
		keys: keys,
		shape: fmt.Sprintf("rows=%d keys=%d distinct=%v asc=%v eval=%v nullRate=%d/16",
			nRows, nKeys, distinct, asc, viaEval, nullRate),
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The property
// ─────────────────────────────────────────────────────────────────────────────

// TestTopEqualsStableSortPrefix is acceptance criterion 2 of rmp #2509.
//
// For every generated input and every n, the sequence [Top] emits must be
// IDENTICAL — row for row, including the relative order of rows that tie on
// every sort key — to the sequence [Sort] emits, truncated to n.
//
// Equality of the SET is not enough and is deliberately not what is asserted.
// Skip(s) over Top(s+k) hands the caller rows [s, s+k) of Top's output, so two
// orderings that agree as sets but disagree in position produce two different
// pages for the same query.
func TestTopEqualsStableSortPrefix(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		f := genFixture(rt)
		n := rapid.IntRange(0, len(f.rows)+2).Draw(rt, "n")

		sortOp, err := NewSort(&sliceSource{rows: copyRows(f.rows)}, f.keys, 0)
		if err != nil {
			rt.Fatalf("NewSort: %v", err)
		}
		sorted := drainTB(rt, sortOp)

		topOp, err := NewTop(&sliceSource{rows: copyRows(f.rows)}, f.keys, n, 0)
		if err != nil {
			rt.Fatalf("NewTop: %v", err)
		}
		got := drainTB(rt, topOp)

		want := sorted
		if n < len(want) {
			want = want[:n]
		}
		compareTopAgainstSortPrefix(rt, f, n, got, want)
	})
}

// compareTopAgainstSortPrefix reports the first positional divergence, naming
// the identity column of both rows, and — when the two sequences hold the same
// identities in a different order — says so explicitly, because a permutation of
// tied rows and a genuinely wrong row set are two different defects with two
// different fixes.
func compareTopAgainstSortPrefix(t topPropTB, f topPropFixture, n int, got, want []Row) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("Top(%d) emitted %d rows, stable Sort prefix has %d [%s]",
			n, len(got), len(want), f.shape)
	}
	for i := range want {
		if expr.Equivalent(got[i][topPropIDCol], want[i][topPropIDCol]) {
			continue
		}
		samSet := sameIdentitySet(got, want)
		t.Fatalf("Top(%d) diverges from the stable Sort prefix at position %d: "+
			"Top=%v, Sort=%v; identity sets %s [%s]\n  Top  ids: %v\n  Sort ids: %v",
			n, i, got[i][topPropIDCol], want[i][topPropIDCol],
			map[bool]string{true: "MATCH (a tie-order transposition)", false: "DIFFER (a wrong row set)"}[samSet],
			f.shape, identities(got), identities(want))
	}
}

// identities projects the identity column, for failure messages.
func identities(rows []Row) []int64 {
	out := make([]int64, len(rows))
	for i, r := range rows {
		if iv, ok := r[topPropIDCol].(expr.IntegerValue); ok {
			out[i] = int64(iv) - 1_000_000
		}
	}
	return out
}

// sameIdentitySet reports whether the two sequences hold the same identities,
// ignoring order.
func sameIdentitySet(a, b []Row) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[int64]int, len(a))
	for _, id := range identities(a) {
		seen[id]++
	}
	for _, id := range identities(b) {
		seen[id]--
		if seen[id] < 0 {
			return false
		}
	}
	return true
}

// ─────────────────────────────────────────────────────────────────────────────
// Deterministic minimal witness
// ─────────────────────────────────────────────────────────────────────────────

// TestTopTieOrderMinimalWitness is the hand-written minimum of the property
// above, kept as a fast, seed-independent regression that names the defect
// precisely: five rows, ALL tied on the single sort key, and a limit that
// retains three of them. A bounded heap that carries no arrival ordinal emits
// those three in the order its sift sequence happens to leave them; a stable
// Sort emits them in arrival order.
//
// This is the exact shape a fused SKIP lands on: with SKIP 1 LIMIT 2 the caller
// receives positions 1 and 2 of this sequence, so a transposition here changes
// which rows the page contains, not merely how they are arranged.
func TestTopTieOrderMinimalWitness(t *testing.T) {
	const rowCount = 5
	const limit = 3

	build := func() []Row {
		rows := make([]Row, rowCount)
		for i := range rows {
			rows[i] = Row{
				expr.IntegerValue(int64(1_000_000 + i)), // identity
				expr.IntegerValue(int64(300)),           // every row ties here
			}
		}
		return rows
	}
	keys := []SortKey{{ColIdx: 1, Ascending: true}}

	sortOp, err := NewSort(&sliceSource{rows: build()}, keys, 0)
	if err != nil {
		t.Fatalf("NewSort: %v", err)
	}
	sorted := drainTB(t, sortOp)

	topOp, err := NewTop(&sliceSource{rows: build()}, keys, limit, 0)
	if err != nil {
		t.Fatalf("NewTop: %v", err)
	}
	got := drainTB(t, topOp)

	f := topPropFixture{shape: fmt.Sprintf("rows=%d all-tied limit=%d", rowCount, limit)}
	compareTopAgainstSortPrefix(t, f, limit, got, sorted[:limit])
}

// ─────────────────────────────────────────────────────────────────────────────
// Regime coverage
// ─────────────────────────────────────────────────────────────────────────────

// TestTopRegimesBothMatchSortPrefix drives the property across the boundary
// between [Top]'s two internal regimes and, crucially, ASSERTS WHICH ONE RAN.
//
// Top accumulates rows until the buffer reaches 2n and only then builds a heap,
// so the same bound produces two structurally different executions depending on
// the input size, and they arrive at the emission order by different routes: the
// accumulate-only regime sorts arrivals that are still in arrival order, while
// the heap regime sorts entries the sift sequence has already scrambled and can
// only put back with the arrival ordinal. A regime sweep that silently ran the
// same path at every point would look like coverage and be none, so
// [Top.usedHeap] is checked at every point.
//
// The interesting rows are n == M/2, where the threshold lands exactly on the
// last input row so the DEFERRED heapify never fires, and n == M/2 - 1, where it
// fires with two rows left to process.
func TestTopRegimesBothMatchSortPrefix(t *testing.T) {
	const rowCount = 100

	// Ties everywhere: six distinct keys over a hundred rows, so every window
	// boundary lands inside a run of equal keys.
	build := func() []Row {
		rows := make([]Row, rowCount)
		for i := range rows {
			rows[i] = Row{
				expr.IntegerValue(int64(1_000_000 + i)),
				expr.IntegerValue(int64(300 + (i*7)%6)),
			}
		}
		return rows
	}
	keys := []SortKey{{ColIdx: 1, Ascending: true}}

	sortOp, err := NewSort(&sliceSource{rows: build()}, keys, 0)
	if err != nil {
		t.Fatalf("NewSort: %v", err)
	}
	sorted := drainTB(t, sortOp)

	for _, tc := range []struct {
		n        int
		wantHeap bool
	}{
		{0, false},  // admits nothing, drains the child, never heapifies
		{1, true},   // threshold 2, reached on the second row
		{7, true},   // threshold 14
		{49, true},  // threshold 98, reached two rows before the end
		{50, false}, // threshold 100, reached on the LAST row: nothing arrives
		//                after it, so the deferred heapify never happens
		{51, false},  // threshold 102 — never reached
		{100, false}, // bound equals the input: accumulate everything
		{150, false}, // bound above the input
	} {
		tc := tc
		t.Run(fmt.Sprintf("n=%d", tc.n), func(t *testing.T) {
			op, err := NewTop(&sliceSource{rows: build()}, keys, tc.n, 0)
			if err != nil {
				t.Fatalf("NewTop: %v", err)
			}
			got := drainTB(t, op)
			if op.usedHeap != tc.wantHeap {
				t.Fatalf("n=%d ran the %s regime, want the %s one: the sweep is not "+
					"covering both routes to the emission order",
					tc.n, regimeName(op.usedHeap), regimeName(tc.wantHeap))
			}
			want := sorted
			if tc.n < len(want) {
				want = want[:tc.n]
			}
			f := topPropFixture{shape: fmt.Sprintf("rows=%d 6-value key n=%d heap=%v",
				rowCount, tc.n, op.usedHeap)}
			compareTopAgainstSortPrefix(t, f, tc.n, got, want)
		})
	}
}

func regimeName(heap bool) string {
	if heap {
		return "bounded-heap"
	}
	return "accumulate-only"
}

// TestTopLegacyArmMatchesSortPrefix runs the same property on the LEGACY
// comparator arm, which compares by re-evaluating both operands instead of
// reading decorated keys ([sortseam.SetKeyDecorationDisabled]).
//
// It is not redundant with the differential test next door. That one proves the
// two arms agree WITH EACH OTHER; this one proves the legacy arm agrees with
// Sort's prefix, so the pair cannot both be wrong in the same direction — which
// is exactly what would happen if the arrival ordinal were threaded into only
// one of the two comparators.
//
// It is NOT t.Parallel: it writes the process-global control.
func TestTopLegacyArmMatchesSortPrefix(t *testing.T) {
	const rowCount = 60
	build := func() []Row {
		rows := make([]Row, rowCount)
		for i := range rows {
			rows[i] = Row{
				expr.IntegerValue(int64(1_000_000 + i)),
				expr.IntegerValue(int64(300 + (i*11)%4)),
			}
		}
		return rows
	}
	keys := []SortKey{{ColIdx: 1, Ascending: false}}

	restore := sortseam.SetKeyDecorationDisabled(true)
	defer restore()

	sortOp, err := NewSort(&sliceSource{rows: build()}, keys, 0)
	if err != nil {
		t.Fatalf("NewSort: %v", err)
	}
	sorted := drainTB(t, sortOp)

	for _, n := range []int{1, 5, 29, 30, 31, 60, 70} {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			op, err := NewTop(&sliceSource{rows: build()}, keys, n, 0)
			if err != nil {
				t.Fatalf("NewTop: %v", err)
			}
			if op.h.decorated {
				t.Fatal("the seam did not select the legacy arm, so this test is " +
					"comparing the decorated path against Sort a second time")
			}
			got := drainTB(t, op)
			want := sorted
			if n < len(want) {
				want = want[:n]
			}
			f := topPropFixture{shape: fmt.Sprintf("LEGACY arm rows=%d n=%d", rowCount, n)}
			compareTopAgainstSortPrefix(t, f, n, got, want)
		})
	}
}
