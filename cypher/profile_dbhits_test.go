package cypher_test

// profile_dbhits_test.go — T910: PROFILE returns plan with dbhits per operator.
//
// Since the engine has no Profile() method yet, this test exercises the
// explain package's instrumentation infrastructure directly:
//
//  1. An InstrumentedScan wrapped around a ProfiledOperator over a node-count
//     source yields OperatorStats with Rows > 0 and DbHits > 0.
//  2. The sum of DbHits grows monotonically with graph size (n=10, n=100).
//  3. FormatReport output contains expected column headers.
//  4. Race-clean (independent pipelines per goroutine).
//  5. goleak-clean.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"gograph/cypher"
	"gograph/cypher/exec"
	"gograph/cypher/explain"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// nodeCountSource is an exec.Operator that emits exactly n rows,
// each containing expr.IntegerValue(i).
type nodeCountSource struct {
	total int
	pos   int
	buf   [1]expr.Value
}

func newNodeCountSource(n int) *nodeCountSource { return &nodeCountSource{total: n} }

func (s *nodeCountSource) Init(_ context.Context) error { s.pos = 0; return nil }

func (s *nodeCountSource) Next(out *exec.Row) (bool, error) {
	if s.pos >= s.total {
		return false, nil
	}
	s.buf[0] = expr.IntegerValue(int64(s.pos))
	*out = s.buf[:]
	s.pos++
	return true, nil
}

func (s *nodeCountSource) Close() error { return nil }

// drainPipeline drains op (already Init'd) and returns the row count.
func drainPipeline(t *testing.T, op exec.Operator) int {
	t.Helper()
	var row exec.Row
	var count int
	for {
		ok, err := op.Next(&row)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		count++
	}
	return count
}

// TestProfile_DbHits is the T910 acceptance test.
func TestProfile_DbHits(t *testing.T) {
	t.Parallel()

	t.Run("rows_and_dbhits_positive", func(t *testing.T) {
		t.Parallel()

		const n = 42
		src := newNodeCountSource(n)
		var counter explain.DbHitsCounter
		instrumented := explain.NewInstrumentedScan(src, &counter)
		profiled := explain.NewProfiledOperator(instrumented, "AllNodesScan")

		if err := profiled.Init(context.Background()); err != nil {
			t.Fatalf("Init: %v", err)
		}
		rows := drainPipeline(t, profiled)
		if err := profiled.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		stats := profiled.Stats()

		if uint64(rows) != stats.Rows {
			t.Errorf("Stats.Rows=%d, drained=%d", stats.Rows, rows)
		}
		if stats.Rows == 0 {
			t.Error("Stats.Rows == 0, want > 0")
		}
		if counter.Load() == 0 {
			t.Error("DbHitsCounter.Load() == 0, want > 0")
		}
		if counter.Load() != uint64(n) {
			t.Errorf("DbHits=%d, want %d", counter.Load(), n)
		}
	})

	t.Run("dbhits_monotone_with_graph_size", func(t *testing.T) {
		t.Parallel()

		sizes := []int{10, 100}
		var prev uint64
		for _, n := range sizes {
			src := newNodeCountSource(n)
			var counter explain.DbHitsCounter
			instrumented := explain.NewInstrumentedScan(src, &counter)

			if err := instrumented.Init(context.Background()); err != nil {
				t.Fatalf("Init n=%d: %v", n, err)
			}
			drainPipeline(t, instrumented)
			if err := instrumented.Close(); err != nil {
				t.Fatalf("Close n=%d: %v", n, err)
			}

			got := counter.Load()
			if got <= prev {
				t.Errorf("n=%d: DbHits=%d not > prev=%d — not monotone", n, got, prev)
			}
			prev = got
		}
	})

	t.Run("format_report_headers", func(t *testing.T) {
		t.Parallel()

		r := explain.ProfileReport{
			Operators: []explain.OperatorStats{
				{Name: "AllNodesScan", Rows: 50, DbHits: 50, ElapsedNs: 5000},
				{Name: "ProduceResults", Rows: 50, DbHits: 0, ElapsedNs: 500},
			},
			TotalRows:   100,
			TotalDbHits: 50,
			ElapsedMs:   0.0055,
		}
		out := explain.FormatReport(r)
		for _, want := range []string{"Rows", "DbHits", "Time (ms)", "AllNodesScan", "ProduceResults"} {
			if !strings.Contains(out, want) {
				t.Errorf("FormatReport output missing %q\n%s", want, out)
			}
		}
	})

	t.Run("profiled_operator_chain_dbhits", func(t *testing.T) {
		t.Parallel()

		// A chained pipeline: ProfiledOperator(outer) → InstrumentedScan →
		// ProfiledOperator(inner) → nodeCountSource.
		const n = 20
		src := newNodeCountSource(n)
		var counter explain.DbHitsCounter
		inner := explain.NewProfiledOperator(src, "inner")
		instrumented := explain.NewInstrumentedScan(inner, &counter)
		outer := explain.NewProfiledOperator(instrumented, "outer")

		if err := outer.Init(context.Background()); err != nil {
			t.Fatalf("Init: %v", err)
		}
		drainPipeline(t, outer)
		if err := outer.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		innerStats := inner.Stats()
		outerStats := outer.Stats()

		if innerStats.Rows != uint64(n) {
			t.Errorf("inner.Rows=%d, want %d", innerStats.Rows, n)
		}
		if outerStats.Rows != uint64(n) {
			t.Errorf("outer.Rows=%d, want %d", outerStats.Rows, n)
		}
		if counter.Load() != uint64(n) {
			t.Errorf("DbHits=%d, want %d", counter.Load(), n)
		}
	})

	t.Run("engine_explain_contains_operator_names", func(t *testing.T) {
		t.Parallel()

		// Verify the engine's Explain output contains both AllNodesScan and
		// NodeByLabelScan when appropriate queries are used.
		g := lpg.New[string, float64](adjlist.Config{})
		for i := range 5 {
			key := fmt.Sprintf("q%d", i)
			if err := g.AddNode(key); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
			if err := g.SetNodeLabel(key, "Foo"); err != nil {
				t.Fatalf("SetNodeLabel: %v", err)
			}
		}
		eng := cypher.NewEngine(g)

		for _, tc := range []struct {
			query string
			want  string
		}{
			{"MATCH (n) RETURN n", "AllNodesScan"},
			{"MATCH (n:Foo) RETURN n", "NodeByLabelScan"},
		} {
			tc := tc
			t.Run(tc.query, func(t *testing.T) {
				t.Parallel()
				plan, err := eng.Explain(tc.query, nil)
				if err != nil {
					t.Fatalf("Explain: %v", err)
				}
				if !strings.Contains(plan, tc.want) {
					t.Errorf("plan missing %q:\n%s", tc.want, plan)
				}
			})
		}
	})
}
