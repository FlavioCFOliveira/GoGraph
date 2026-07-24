package main

import (
	"bytes"
	"context"
	"testing"

	"go.uber.org/goleak"
)

// TestMain doubles the package as a goroutine-leak check: the example — including
// the columnar exercise, which builds a bounded sub-graph and drives three engines
// — must terminate every goroutine it starts before the process exits. A leak here
// fails the whole package, catching a query pipeline or worker pool that outlives
// its Result.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestColumnarExercise pins the columnar-execution observability section (#2121).
// Its lines are telemetry (prefixed "# "), so they are absent from the
// deterministic fact-line set; this test reads them by name and asserts the
// DIRECTION of each columnar win (the exact allocation figures are volatile), plus
// the two invariants that prove the columnar path actually engaged rather than
// silently falling back:
//
//   - the filter's columnar-filter batch counter is > 0 for the columnar query and
//     exactly 0 for the coalesce() row-mode twin (a direct, module-emitted signal);
//   - the aggregation and hash-join columnar variants allocate strictly fewer times
//     than their result-identical row-mode / nested-loop baselines.
//
// It also checks the results the exercise compared internally are the expected
// shape (12 country groups covering every user; a positive homonym-pair count),
// so a query that stopped grouping or joining could not pass by allocating less.
func TestColumnarExercise(t *testing.T) {
	var buf bytes.Buffer
	cfg := testConfig()
	if err := columnarExercise(context.Background(), cfg, &buf); err != nil {
		t.Fatalf("columnarExercise: %v", err)
	}
	out := buf.String()

	// Working set: capped at colScaleUsers/colScaleArticles, but testConfig is
	// below both caps, so the exercise runs at the test scale.
	if got := telemetryInt(t, out, "columnar.scale.users"); got != int64(min(cfg.users, colScaleUsers)) {
		t.Errorf("columnar.scale.users = %d, want %d", got, min(cfg.users, colScaleUsers))
	}
	if got := telemetryInt(t, out, "columnar.scale.articles"); got != int64(min(cfg.articles, colScaleArticles)) {
		t.Errorf("columnar.scale.articles = %d, want %d", got, min(cfg.articles, colScaleArticles))
	}

	// (1) Columnar aggregation. Every user carries one of the fixed countries, so
	// the grouping covers all users across exactly len(countries) groups. The
	// columnar de-box (unboxed grouping-key hash) must allocate strictly fewer
	// times and fewer bytes than the coalesce() row-mode twin.
	wantUsers := int64(min(cfg.users, colScaleUsers))
	if got := telemetryInt(t, out, "columnar.agg.groups"); got != int64(len(countries)) {
		t.Errorf("columnar.agg.groups = %d, want %d", got, len(countries))
	}
	if got := telemetryInt(t, out, "columnar.agg.members_total"); got != wantUsers {
		t.Errorf("columnar.agg.members_total = %d, want %d (every user counted once)", got, wantUsers)
	}
	aggCol := telemetryInt(t, out, "columnar.agg.col_mallocs")
	aggRow := telemetryInt(t, out, "columnar.agg.row_mallocs")
	if aggCol <= 0 || aggCol >= aggRow {
		t.Errorf("columnar aggregation did not win: col_mallocs=%d, row_mallocs=%d (want 0 < col < row)", aggCol, aggRow)
	}
	if col, row := telemetryInt(t, out, "columnar.agg.col_bytes"), telemetryInt(t, out, "columnar.agg.row_bytes"); col >= row {
		t.Errorf("columnar aggregation bytes did not win: col_bytes=%d, row_bytes=%d", col, row)
	}

	// (2) Columnar Expand + Filter. The batch counter is the direct engagement
	// proof: > 0 for the columnar query, exactly 0 for the coalesce() twin. Both
	// return the identical row count (the exercise fails the run otherwise), and
	// the columnar path allocates strictly fewer times.
	if got := telemetryInt(t, out, "columnar.filter.col_batches"); got <= 0 {
		t.Errorf("columnar.filter.col_batches = %d, want > 0 (columnar Expand+Filter must engage)", got)
	}
	if got := telemetryInt(t, out, "columnar.filter.row_batches"); got != 0 {
		t.Errorf("columnar.filter.row_batches = %d, want 0 (the coalesce twin must stay on the row path)", got)
	}
	if got := telemetryInt(t, out, "columnar.filter.rows"); got <= 0 {
		t.Errorf("columnar.filter.rows = %d, want > 0", got)
	}
	filCol := telemetryInt(t, out, "columnar.filter.col_mallocs")
	filRow := telemetryInt(t, out, "columnar.filter.row_mallocs")
	if filCol <= 0 || filCol >= filRow {
		t.Errorf("columnar filter did not win: col_mallocs=%d, row_mallocs=%d (want 0 < col < row)", filCol, filRow)
	}

	// (3) Columnar hash join. A positive homonym-pair count confirms the join ran,
	// and the columnar hash join must allocate strictly fewer times than the
	// hash-join-disabled nested-loop baseline for the identical result.
	if got := telemetryInt(t, out, "columnar.hashjoin.pairs"); got <= 0 {
		t.Errorf("columnar.hashjoin.pairs = %d, want > 0 (the equi-join must find homonym pairs)", got)
	}
	hjCol := telemetryInt(t, out, "columnar.hashjoin.col_mallocs")
	hjNested := telemetryInt(t, out, "columnar.hashjoin.nested_mallocs")
	if hjCol <= 0 || hjCol >= hjNested {
		t.Errorf("columnar hash join did not win: col_mallocs=%d, nested_mallocs=%d (want 0 < col < nested)", hjCol, hjNested)
	}
}
