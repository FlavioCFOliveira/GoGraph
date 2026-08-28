package exec

// sort_decoration_test.go — #2652
//
// The operator-level oracles for decorate-sort-undecorate in [Sort] and [Top]:
//
//   - the emitted order is byte-identical to the legacy per-comparison sort,
//     TIE ORDER INCLUDED, for both operators;
//   - the comparator allocates nothing;
//   - the cycle-following permutation applies the permutation it is given;
//   - the sort-key evaluator runs once per row rather than once per comparison.
//
// Every differential test here drives BOTH arms in one process through
// [sortseam.SetKeyDecorationDisabled] and asserts, independently of the results,
// that the two arms really did take different paths — a comparison of two runs of
// the same code would pass no matter what the rewrite did.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/internal/sortseam"
)

// decorationRowCount is large enough that sort.SliceStable's insertion-sort
// threshold (blocks of 20) is exceeded and the merge path runs, which is where a
// stability bug would live.
const decorationRowCount = 500

// keyModulus is deliberately far smaller than decorationRowCount so most rows
// TIE on the sort key. Tie order is the property the rewrite is most likely to
// break and the one a golden file cannot pin, so the fixture is built to make
// ties the common case rather than an edge case.
const keyModulus = 7

// decorationFixture builds rows whose sort key must be produced by an EVALUATOR
// rather than read from a column, which is the shape #2652 is about.
//
// Column 0 is a unique payload identifying the row, so any reordering of tied
// rows is visible in the emitted sequence. Column 1 holds the key SOURCE, and
// the returned SortKey reads it through Eval, not through ColIdx — that is what
// puts the evaluation inside the comparator on the legacy arm.
//
// Every value is >= 256. Below that the Go runtime's convT64 hands back a
// pointer into staticuint64s and boxing an integer into an interface does not
// allocate, which would turn TestSortComparatorIsAllocationFree into a false
// pass. Confirmed empirically in this sprint: n=255 measured 0.00 allocs/op,
// n=256 measured 1.00.
func decorationFixture(n int, ascending bool) (rows []Row, key SortKey, evals *int) {
	rows = make([]Row, n)
	for i := 0; i < n; i++ {
		rows[i] = Row{
			expr.IntegerValue(int64(1_000_000 + i)), // unique payload, >= 256
			expr.IntegerValue(int64(256 + i%keyModulus)),
		}
	}
	count := 0
	key = SortKey{
		Ascending: ascending,
		Eval: func(row Row) (expr.Value, error) {
			count++
			return row[1], nil
		},
	}
	return rows, key, &count
}

// copyRows deep-copies a row slice so an operator that sorts in place cannot
// disturb a second arm's input.
func copyRows(rows []Row) []Row {
	out := make([]Row, len(rows))
	for i, r := range rows {
		cp := make(Row, len(r))
		copy(cp, r)
		out[i] = cp
	}
	return out
}

// drain runs an operator to completion and returns the emitted rows.
func drain(t *testing.T, op Operator) []Row {
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

// assertSameSequence fails unless got and want are the same rows in the same
// order, compared value by value. It reports the FIRST divergence with its
// index, because a tie-order regression typically shows as a single transposed
// pair deep inside an otherwise correct sequence.
func assertSameSequence(t *testing.T, what string, got, want []Row) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: emitted %d rows, legacy emitted %d", what, len(got), len(want))
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("%s: row %d width %d, legacy width %d", what, i, len(got[i]), len(want[i]))
		}
		for c := range want[i] {
			if !expr.Equivalent(got[i][c], want[i][c]) {
				t.Fatalf("%s: row %d col %d = %v, legacy = %v (first divergence of %d rows)",
					what, i, c, got[i][c], want[i][c], len(want))
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Sort
// ─────────────────────────────────────────────────────────────────────────────

// TestSortDecorationStabilityMatchesLegacy is acceptance criterion 3 for [Sort]:
// the decorated sort emits byte-identical rows in byte-identical order,
// including the order of rows that tie on every key.
//
// It is NOT t.Parallel: it flips a process-global control.
func TestSortDecorationStabilityMatchesLegacy(t *testing.T) {
	for _, ascending := range []bool{true, false} {
		name := "asc"
		if !ascending {
			name = "desc"
		}
		t.Run(name, func(t *testing.T) {
			base, _, _ := decorationFixture(decorationRowCount, ascending)

			// Legacy arm.
			legacyRows, legacyKey, legacyEvals := decorationFixture(decorationRowCount, ascending)
			restore := sortseam.SetKeyDecorationDisabled(true)
			op, err := NewSort(&sliceSource{rows: copyRows(legacyRows)}, []SortKey{legacyKey}, 0)
			if err != nil {
				t.Fatalf("NewSort: %v", err)
			}
			legacyOut := drain(t, op)
			restore()

			// Decorated arm.
			decRows, decKey, decEvals := decorationFixture(decorationRowCount, ascending)
			restoreDec := sortseam.SetKeyDecorationDisabled(false)
			op2, err := NewSort(&sliceSource{rows: copyRows(decRows)}, []SortKey{decKey}, 0)
			if err != nil {
				t.Fatalf("NewSort: %v", err)
			}
			decOut := drain(t, op2)
			restoreDec()

			// NON-VACUITY: prove the seam moved the execution, not just the
			// clock. A comparison of two identical paths proves nothing about
			// the rewrite. The legacy arm must have evaluated the key far more
			// than once per row; the decorated arm exactly once per row.
			if *decEvals != decorationRowCount {
				t.Fatalf("decorated arm evaluated the key %d times, want exactly %d (one per row)",
					*decEvals, decorationRowCount)
			}
			if *legacyEvals <= decorationRowCount {
				t.Fatalf("legacy arm evaluated the key %d times, i.e. no more than the "+
					"decorated arm's %d: the seam did not select the legacy path, so this "+
					"test compared the decorated path against itself",
					*legacyEvals, decorationRowCount)
			}
			t.Logf("%s: key evaluations legacy=%d decorated=%d (%d rows, %.1fx)",
				name, *legacyEvals, *decEvals, decorationRowCount,
				float64(*legacyEvals)/float64(*decEvals))

			assertSameSequence(t, "decorated Sort", decOut, legacyOut)

			// And an independent oracle, so the test does not rest solely on the
			// legacy arm being right: a straight stable sort of the input.
			want := copyRows(base)
			sort.SliceStable(want, func(i, j int) bool {
				c := expr.Compare(want[i][1], want[j][1])
				if !ascending {
					c = -c
				}
				return c < 0
			})
			assertSameSequence(t, "decorated Sort vs reference", decOut, want)
		})
	}
}

// TestSortComparatorIsAllocationFree is acceptance criterion 2: the comparator
// the decorated sort runs performs NO allocation.
//
// The falsification arm matters as much as the assertion. [keysLess] reading two
// precomputed values must measure 0; the legacy [Sort.rowLess] over the same key
// must measure MORE than 0, because it evaluates and — in the real engine —
// builds a RowContext. If the legacy arm ever measured 0 too, the assertion would
// be pinning a property the code had regardless of #2652.
func TestSortComparatorIsAllocationFree(t *testing.T) {
	rows, key, _ := decorationFixture(64, true)
	keys := []SortKey{key}

	// Decorated comparator: two precomputed values from the flat key buffer.
	kv := make([]expr.Value, len(rows))
	for i, r := range rows {
		kv[i] = sortKeyValue(key, r)
	}
	var sink bool
	got := testing.AllocsPerRun(2000, func() {
		sink = keysLess(keys, kv[3:], kv[11:])
	})
	if got != 0 {
		t.Errorf("keysLess allocated %.2f objects/op, want 0 (sink=%v)", got, sink)
	}
	t.Logf("keysLess: %.2f allocs/op", got)

	// Falsification: the legacy comparator over the same rows must allocate.
	// Sort.rowLess itself only calls Eval, so the fixture's Eval closure is
	// wrapped in one that boxes an integer >= 256 — the same cost class the real
	// engine pays, and the reason the fixture's values start at 256.
	boxing := SortKey{
		Ascending: true,
		Eval: func(row Row) (expr.Value, error) {
			iv, _ := row[1].(expr.IntegerValue)
			return expr.IntegerValue(int64(iv) + 1), nil // >= 257, escapes -> boxes
		},
	}
	legacy := &Sort{keys: []SortKey{boxing}}
	legacyAllocs := testing.AllocsPerRun(2000, func() {
		sink = legacy.rowLess(rows[3], rows[11])
	})
	if legacyAllocs == 0 {
		t.Errorf("legacy rowLess allocated %.2f objects/op: the 0-allocation assertion "+
			"above is vacuous, because the comparator it replaced did not allocate either",
			legacyAllocs)
	}
	t.Logf("legacy rowLess: %.2f allocs/op (falsification arm)", legacyAllocs)
}

// TestPermuteRows checks the cycle-following permutation against the obvious
// buffered implementation over many random permutations, including the identity
// and full-reversal edge cases.
func TestPermuteRows(t *testing.T) {
	rng := rand.New(rand.NewPCG(0x2652, 0x2652))
	for _, n := range []int{0, 1, 2, 3, 7, 64, 501} {
		for trial := 0; trial < 20; trial++ {
			perm := make([]int, n)
			for i := range perm {
				perm[i] = i
			}
			switch trial {
			case 0: // identity
			case 1: // full reversal
				for i := range perm {
					perm[i] = n - 1 - i
				}
			default:
				rng.Shuffle(n, func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
			}

			rows := make([]Row, n)
			for i := range rows {
				rows[i] = Row{expr.IntegerValue(int64(1000 + i))}
			}
			want := make([]Row, n)
			for i, p := range perm {
				want[i] = rows[p]
			}

			permCopy := make([]int, n)
			copy(permCopy, perm)
			permuteRows(rows, permCopy)

			for i := range want {
				if !expr.Equivalent(rows[i][0], want[i][0]) {
					t.Fatalf("n=%d trial=%d: rows[%d]=%v, want %v (perm=%v)",
						n, trial, i, rows[i][0], want[i][0], perm)
				}
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Top
// ─────────────────────────────────────────────────────────────────────────────

// TestTopDecorationMatchesLegacy is acceptance criterion 3 for [Top], the twin
// site the task's scope statement omitted. Top reached the per-comparison
// comparator from three places (heap admission, the heap's own Less, and the
// final result sort), so its tie ordering is a function of the heap's sift
// sequence and is exactly what a careless rewrite would move.
//
// The limits sweep the boundaries: 0 (admits nothing, must still drain), 1, a
// limit under the input size, exactly the input size, and above it.
func TestTopDecorationMatchesLegacy(t *testing.T) {
	const n = 200
	for _, limit := range []int{0, 1, 3, 17, n, n + 5} {
		for _, ascending := range []bool{true, false} {
			t.Run(fmt.Sprintf("limit=%d/asc=%v", limit, ascending), func(t *testing.T) {
				legacyRows, legacyKey, legacyEvals := decorationFixture(n, ascending)
				restore := sortseam.SetKeyDecorationDisabled(true)
				op, err := NewTop(&sliceSource{rows: copyRows(legacyRows)}, []SortKey{legacyKey}, limit)
				if err != nil {
					t.Fatalf("NewTop: %v", err)
				}
				legacyOut := drain(t, op)
				restore()

				decRows, decKey, decEvals := decorationFixture(n, ascending)
				restoreDec := sortseam.SetKeyDecorationDisabled(false)
				op2, err := NewTop(&sliceSource{rows: copyRows(decRows)}, []SortKey{decKey}, limit)
				if err != nil {
					t.Fatalf("NewTop: %v", err)
				}
				decOut := drain(t, op2)
				restoreDec()

				want := limit
				if want > n {
					want = n
				}
				if len(decOut) != want {
					t.Fatalf("decorated Top emitted %d rows, want %d", len(decOut), want)
				}
				assertSameSequence(t, "decorated Top", decOut, legacyOut)

				if limit == 0 {
					// n == 0 admits nothing, so neither arm may evaluate a key.
					if *legacyEvals != 0 || *decEvals != 0 {
						t.Errorf("limit=0 evaluated keys: legacy=%d decorated=%d, want 0 each",
							*legacyEvals, *decEvals)
					}
					return
				}
				if *decEvals != n {
					t.Errorf("decorated Top evaluated the key %d times, want exactly %d "+
						"(one per input row)", *decEvals, n)
				}
				t.Logf("key evaluations legacy=%d decorated=%d over %d input rows",
					*legacyEvals, *decEvals, n)
			})
		}
	}
}

// TestTopDecorationSeamIsEffective is the non-vacuity guard for the Top
// differential above. It is separate because Top's legacy evaluation count
// depends on the limit, and a limit that admits every row (limit >= n) performs
// no replacement at all — so only a limit strictly between 1 and n proves the
// seam changed the execution.
func TestTopDecorationSeamIsEffective(t *testing.T) {
	const n = 200
	const limit = 17

	_, legacyKey, legacyEvals := decorationFixture(n, true)
	legacyRows, _, _ := decorationFixture(n, true)
	restore := sortseam.SetKeyDecorationDisabled(true)
	op, err := NewTop(&sliceSource{rows: copyRows(legacyRows)}, []SortKey{legacyKey}, limit)
	if err != nil {
		t.Fatalf("NewTop: %v", err)
	}
	drain(t, op)
	restore()

	if *legacyEvals <= n {
		t.Fatalf("legacy Top evaluated the key %d times over %d rows with limit %d: "+
			"no more than one per row, so the seam did not select the legacy path and "+
			"TestTopDecorationMatchesLegacy compared the decorated path against itself",
			*legacyEvals, n, limit)
	}
	t.Logf("legacy Top: %d key evaluations over %d rows (limit %d), %.1fx the decorated %d",
		*legacyEvals, n, limit, float64(*legacyEvals)/float64(n), n)
}
