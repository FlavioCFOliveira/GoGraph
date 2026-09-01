package exec

// parallel_scan_project_budget_internal_test.go — STRUCTURAL bound on the
// batched result-budget overshoot (#2649).
//
// # Why this file exists alongside the external budget tests
//
// The external tests (parallel_scan_project_budget_test.go) run at maxRows=100
// with W=10, where clamp(maxRows/(16*W), 1, morselSize) pins the per-worker
// batch to 1 — byte-for-byte the pre-#2649 per-row behaviour. They therefore
// prove the small-cap guarantee is unchanged, which is exactly what they are
// for; but they never enter the batching regime at all. Every one of them would
// still pass with flushBudget broken for any batch larger than a single row.
//
// This file covers what they cannot, and does so from INSIDE the package so it
// can read the operator's OWN threshold and worker count rather than restating
// the sizing formula in the assertion — a test that recomputed the formula would
// agree with a wrong implementation as readily as with a right one.
//
// It asserts three things:
//
//   - the end-to-end row and byte overshoot at a cap large enough that the batch
//     exceeds one row, expressed as maxRows + W*Fr with W and Fr read off the
//     operator after the run;
//   - budgetChunk / setBudgetThresholds over a sweep of caps and worker counts,
//     against the bound WithResultBudget's godoc states; and
//   - that chargeBudget charges BOTH dimensions for every row, which is the one
//     way this design can silently under-count bytes.

import (
	"context"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// budgetIDWalker is the in-package twin of the external tests' staticNodeWalker:
// a walker over NodeIDs [0, n).
type budgetIDWalker struct{ ids []graph.NodeID }

func (w *budgetIDWalker) WalkNodeIDs(fn func(graph.NodeID) bool) {
	for _, id := range w.ids {
		if !fn(id) {
			return
		}
	}
}

func newBudgetIDWalker(n int) *budgetIDWalker {
	ids := make([]graph.NodeID, n)
	for i := range ids {
		ids[i] = graph.NodeID(i)
	}
	return &budgetIDWalker{ids: ids}
}

// budgetEvenFactory builds a morsel sub-plan that keeps even NodeIDs and
// projects the id through unchanged: exactly one row per two morsel nodes, so
// the produced-row count is a known function of the scan size.
func budgetEvenFactory(morsel []graph.NodeID) (Operator, error) {
	scan := NewAllNodesScan(&budgetIDWalker{ids: morsel})
	filt := NewFilter(scan, func(row Row) (expr.Value, error) {
		iv, ok := row[0].(expr.IntegerValue)
		if !ok {
			return expr.Null, nil
		}
		return expr.BoolValue(int64(iv)%2 == 0), nil
	})
	return NewProject(filt, []ProjectionItem{{
		Alias: "x",
		Eval:  func(row Row) (expr.Value, error) { return row[0], nil },
	}})
}

// TestParallelScanProject_BatchedRowOvershootStructural pins the row half of the
// contract in WithResultBudget's godoc — rows <= maxRows + W*Fr — at a cap large
// enough for the batch to exceed a single row, so the batching path is genuinely
// exercised. W and Fr come from the operator itself; no bound in this test is a
// literal.
func TestParallelScanProject_BatchedRowOvershootStructural(t *testing.T) {
	const (
		n        = 40_000
		allEven  = n / 2
		maxRows  = 3_200 // >= 32*W for every plausible W, so Fr >= 2
		morselSz = 1024
	)

	op := NewParallelScanProject(newBudgetIDWalker(n), budgetEvenFactory, morselSz, nil).
		WithResultBudget(maxRows, 0, nil)
	rows, err := Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	w := int64(len(op.results)) // the worker count Init actually settled on
	fr := op.flushRows          // the batch Init actually sized
	got := int64(len(rows))

	// Non-vacuity: if the batch were 1 this test would be measuring the
	// pre-#2649 behaviour and proving nothing about the batching.
	if fr <= 1 {
		t.Fatalf("flushRows=%d with maxRows=%d and W=%d: the batching regime was not "+
			"entered, so this test is vacuous", fr, maxRows, w)
	}

	// The emitted prefix must EXCEED the cap: the drain layer's exact count is
	// what produces the canonical cap error, and it can only trip on a prefix
	// larger than the cap. This is the assertion that fails if the post-charge
	// design is ever "simplified" into a pre-charged reservation.
	if got <= maxRows {
		t.Fatalf("emitted %d rows with maxRows=%d: the prefix does not exceed the cap, "+
			"so the drain's exact count can never trip ErrResultRowsExceeded", got, maxRows)
	}

	// The structural bound, stated exactly as the godoc states it.
	if bound := int64(maxRows) + w*fr; got > bound {
		t.Fatalf("emitted %d rows; bound is maxRows + W*Fr = %d + %d*%d = %d",
			got, maxRows, w, fr, bound)
	}

	// And it must still be a bounded PREFIX, not the whole set.
	if got >= allEven {
		t.Fatalf("emitted %d rows; the budget did not bound materialisation (full set is %d)", got, allEven)
	}
	t.Logf("rows=%d bound=maxRows+W*Fr=%d+%d*%d=%d (full set %d, GOMAXPROCS=%d)",
		got, maxRows, w, fr, int64(maxRows)+w*fr, allEven, runtime.GOMAXPROCS(0))
}

// TestParallelScanProject_BatchedByteOvershootStructural is the byte half of the
// same contract: bytes <= maxBytes + W*Fb + W*S_max. With a constant per-row
// estimate, S_max is that constant.
func TestParallelScanProject_BatchedByteOvershootStructural(t *testing.T) {
	const (
		n           = 40_000
		allEven     = n / 2
		bytesPerRow = 100 // constant estimate, so S_max == bytesPerRow
		maxBytes    = 320_000
		morselSz    = 1024
	)

	op := NewParallelScanProject(newBudgetIDWalker(n), budgetEvenFactory, morselSz, nil).
		WithResultBudget(0, maxBytes, func(Row) int64 { return bytesPerRow })
	rows, err := Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	w := int64(len(op.results))
	fb := op.flushBytes
	gotBytes := int64(len(rows)) * bytesPerRow

	if fb <= bytesPerRow {
		t.Fatalf("flushBytes=%d with maxBytes=%d and W=%d: the batch is a single row or "+
			"less, so this test is vacuous", fb, maxBytes, w)
	}
	if gotBytes <= maxBytes {
		t.Fatalf("emitted %d bytes with maxBytes=%d: the prefix does not exceed the cap, "+
			"so the drain's exact count can never trip ErrResultBytesExceeded", gotBytes, maxBytes)
	}
	if bound := int64(maxBytes) + w*fb + w*bytesPerRow; gotBytes > bound {
		t.Fatalf("emitted %d bytes; bound is maxBytes + W*Fb + W*S_max = %d + %d*%d + %d*%d = %d",
			gotBytes, maxBytes, w, fb, w, bytesPerRow, bound)
	}
	if int64(len(rows)) >= allEven {
		t.Fatalf("emitted %d rows; the byte budget did not bound materialisation (full set is %d)",
			len(rows), allEven)
	}
	t.Logf("bytes=%d bound=%d (rows=%d, W=%d, Fb=%d)",
		gotBytes, int64(maxBytes)+w*fb+w*bytesPerRow, len(rows), w, fb)
}

// TestBudgetThresholdsAreCapRelative sweeps setBudgetThresholds over caps and
// worker counts and checks every property WithResultBudget's godoc promises.
// The critical one is the LAST: an absolute chunk would silently loosen the
// guarantee for a caller who deliberately set a small budget, so a cap below
// 16*W must pin the batch to 1 and reproduce the pre-#2649 behaviour exactly.
func TestBudgetThresholdsAreCapRelative(t *testing.T) {
	const morselSz = 1024
	estimator := func(Row) int64 { return 1 }

	for _, w := range []int{1, 2, 4, 8, 10, 64, 1024} {
		for _, cap := range []int64{0, 1, 10, 100, 1_000, 16_000, 10_000_000, 1 << 30} {
			op := NewParallelScanProject(newBudgetIDWalker(0), budgetEvenFactory, morselSz, nil).
				WithResultBudget(cap, cap, estimator)
			op.setBudgetThresholds(w)
			fr, fb := op.flushRows, op.flushBytes
			wi := int64(w)

			if cap <= 0 {
				if fr != 0 || fb != 0 {
					t.Fatalf("W=%d cap=%d: unbounded dimension must yield a zero batch, got Fr=%d Fb=%d", w, cap, fr, fb)
				}
				continue
			}
			if fr < 1 || fb < 1 {
				t.Fatalf("W=%d cap=%d: a bounded dimension must yield a batch of at least 1, got Fr=%d Fb=%d", w, cap, fr, fb)
			}
			if fr > morselSz {
				t.Fatalf("W=%d cap=%d: Fr=%d exceeds the morselSize ceiling %d", w, cap, fr, morselSz)
			}
			if fb > execMemChargeChunk {
				t.Fatalf("W=%d cap=%d: Fb=%d exceeds the execMemChargeChunk ceiling %d", w, cap, fb, execMemChargeChunk)
			}
			// The documented slack: W*F <= max(W, cap/16), never more.
			if lim := max(wi, cap/budgetFlushDivisor); wi*fr > lim || wi*fb > lim {
				t.Fatalf("W=%d cap=%d: slack W*Fr=%d W*Fb=%d exceeds max(W, cap/16)=%d", w, cap, wi*fr, wi*fb, lim)
			}
			// The 6.25% claim, in the regime the godoc claims it for.
			if cap >= budgetFlushDivisor*wi {
				if wi*fr > cap/budgetFlushDivisor || wi*fb > cap/budgetFlushDivisor {
					t.Fatalf("W=%d cap=%d: slack exceeds 6.25%% of the cap (Fr=%d Fb=%d)", w, cap, fr, fb)
				}
			} else if fr != 1 || fb != 1 {
				// Cap-relative, never absolute: below 16*W the batch collapses to
				// one row, which is the pre-#2649 per-row behaviour.
				t.Fatalf("W=%d cap=%d (< 16*W): batch must collapse to 1, got Fr=%d Fb=%d", w, cap, fr, fb)
			}
		}
	}
}

// TestBudgetThresholdsByteDimensionOffWithoutEstimator pins that the byte batch
// is zero — the dimension disabled — when no estimator was injected, however
// large maxBytes is. Without this, a nil estimator would be dereferenced.
func TestBudgetThresholdsByteDimensionOffWithoutEstimator(t *testing.T) {
	op := NewParallelScanProject(newBudgetIDWalker(0), budgetEvenFactory, 1024, nil).
		WithResultBudget(1_000_000, 1_000_000, nil)
	op.setBudgetThresholds(8)
	if op.flushBytes != 0 {
		t.Fatalf("flushBytes=%d with a nil estimator; want 0 (dimension off)", op.flushBytes)
	}
	if op.newBudgetTally().estimateRow != nil {
		t.Fatal("tally captured an estimator when the byte dimension is off")
	}
}

// TestChargeBudgetChargesBothDimensions is the regression test for the one
// silent-corruption mistake this design invites: returning as soon as the ROW
// threshold fires would leave that row's bytes uncharged forever, making the
// shared byte total a permanent under-count that grows with every batch.
func TestChargeBudgetChargesBothDimensions(t *testing.T) {
	t.Run("row threshold fires first", func(t *testing.T) {
		tally := budgetTally{
			flushRows:   1, // fires on the very first row
			flushBytes:  1 << 30,
			estimateRow: func(Row) int64 { return 7 },
		}
		if !tally.chargeBudget(nil) {
			t.Fatal("chargeBudget did not report the row threshold")
		}
		if tally.bytes != 7 {
			t.Fatalf("bytes=%d; want 7 — the row threshold short-circuited the byte charge", tally.bytes)
		}
	})

	t.Run("byte threshold fires first", func(t *testing.T) {
		tally := budgetTally{
			flushRows:   1 << 30,
			flushBytes:  4,
			estimateRow: func(Row) int64 { return 5 },
		}
		if !tally.chargeBudget(nil) {
			t.Fatal("chargeBudget did not report the byte threshold")
		}
		if tally.rows != 1 {
			t.Fatalf("rows=%d; want 1 — the row charge was skipped", tally.rows)
		}
	})

	t.Run("both dimensions off", func(t *testing.T) {
		var tally budgetTally
		if tally.chargeBudget(nil) {
			t.Fatal("chargeBudget reported a flush with both dimensions off")
		}
		if tally.rows != 0 || tally.bytes != 0 {
			t.Fatalf("tally moved with both dimensions off: rows=%d bytes=%d", tally.rows, tally.bytes)
		}
	})
}

// TestFlushBudgetPublishesAndResets pins flushBudget's two jobs: it must publish
// the private tally to the shared totals ONCE and reset it, so no row is charged
// twice (which would make the operator stop early — the same silent-truncation
// failure a pre-charged reservation would cause).
func TestFlushBudgetPublishesAndResets(t *testing.T) {
	op := NewParallelScanProject(newBudgetIDWalker(0), budgetEvenFactory, 1024, nil).
		WithResultBudget(100, 1000, func(Row) int64 { return 1 })
	tally := budgetTally{rows: 30, bytes: 300}

	if over := op.flushBudget(&tally); over {
		t.Fatal("flushBudget reported over-budget at 30/100 rows and 300/1000 bytes")
	}
	if op.sharedRows.Load() != 30 || op.sharedBytes.Load() != 300 {
		t.Fatalf("shared totals = %d rows / %d bytes; want 30 / 300",
			op.sharedRows.Load(), op.sharedBytes.Load())
	}
	if tally.rows != 0 || tally.bytes != 0 {
		t.Fatalf("tally not reset after flush: rows=%d bytes=%d", tally.rows, tally.bytes)
	}
	// A second flush of an empty tally must be a no-op, not a re-publish.
	if over := op.flushBudget(&tally); over {
		t.Fatal("flushing an empty tally reported over-budget")
	}
	if op.sharedRows.Load() != 30 || op.sharedBytes.Load() != 300 {
		t.Fatalf("flushing an empty tally moved the shared totals to %d / %d",
			op.sharedRows.Load(), op.sharedBytes.Load())
	}
	// Crossing the cap must be reported.
	tally.rows = 71
	if over := op.flushBudget(&tally); !over {
		t.Fatalf("flushBudget did not report over-budget at %d rows against maxRows=100",
			op.sharedRows.Load())
	}
}
