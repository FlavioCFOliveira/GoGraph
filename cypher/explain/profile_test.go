package explain

import (
	"context"
	"sync"
	"testing"

	"gograph/cypher/exec"
	"gograph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// sliceSource is a test operator that emits a fixed number of rows.
// ─────────────────────────────────────────────────────────────────────────────

type sliceSource struct {
	total   int
	emitted int
}

func (s *sliceSource) Init(_ context.Context) error { return nil }
func (s *sliceSource) Next(out *exec.Row) (bool, error) {
	if s.emitted >= s.total {
		return false, nil
	}
	*out = exec.Row{expr.IntegerValue(s.emitted)}
	s.emitted++
	return true, nil
}
func (s *sliceSource) Close() error { return nil }

// ─────────────────────────────────────────────────────────────────────────────
// drainOperator drains op until EOS and returns the row count.
// ─────────────────────────────────────────────────────────────────────────────

func drainOperator(t *testing.T, op exec.Operator) int {
	t.Helper()
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() {
		if err := op.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()
	var row exec.Row
	count := 0
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

// ─────────────────────────────────────────────────────────────────────────────
// TestProfiledOperator_RowCount
// ─────────────────────────────────────────────────────────────────────────────

func TestProfiledOperator_RowCount(t *testing.T) {
	const total = 42
	src := &sliceSource{total: total}
	po := NewProfiledOperator(src, "SliceSource")

	n := drainOperator(t, po)
	if n != total {
		t.Fatalf("drained %d rows, want %d", n, total)
	}
	st := po.Stats()
	if st.Rows != uint64(total) {
		t.Fatalf("Stats.Rows = %d, want %d", st.Rows, total)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestProfiledOperator_ElapsedNs
// ─────────────────────────────────────────────────────────────────────────────

func TestProfiledOperator_ElapsedNs(t *testing.T) {
	const total = 10
	src := &sliceSource{total: total}
	po := NewProfiledOperator(src, "src")

	drainOperator(t, po)

	if po.Stats().ElapsedNs <= 0 {
		t.Fatal("expected ElapsedNs > 0 after draining rows")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestProfiledOperator_Chain
// ─────────────────────────────────────────────────────────────────────────────

// TestProfiledOperator_Chain builds a chain of two ProfiledOperators and
// verifies each records its individual stats.
func TestProfiledOperator_Chain(t *testing.T) {
	const total = 7
	src := &sliceSource{total: total}
	inner := NewProfiledOperator(src, "inner")
	outer := NewProfiledOperator(inner, "outer")

	drainOperator(t, outer)

	if inner.Stats().Rows != uint64(total) {
		t.Fatalf("inner.Rows = %d, want %d", inner.Stats().Rows, total)
	}
	if outer.Stats().Rows != uint64(total) {
		t.Fatalf("outer.Rows = %d, want %d", outer.Stats().Rows, total)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestProfiledOperator_Race
// ─────────────────────────────────────────────────────────────────────────────

// TestProfiledOperator_Race creates multiple independent operator chains and
// drains them concurrently. Each goroutine owns its own operator tree (per the
// Operator concurrency contract) so there is no shared state.
func TestProfiledOperator_Race(t *testing.T) {
	const goroutines = 20
	const rowsEach = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			src := &sliceSource{total: rowsEach}
			po := NewProfiledOperator(src, "race-src")
			if err := po.Init(context.Background()); err != nil {
				return
			}
			defer po.Close() //nolint:errcheck
			var row exec.Row
			for {
				ok, err := po.Next(&row)
				if err != nil || !ok {
					break
				}
			}
		}()
	}
	wg.Wait()
}

// ─────────────────────────────────────────────────────────────────────────────
// TestFormatReport
// ─────────────────────────────────────────────────────────────────────────────

func TestFormatReport(t *testing.T) {
	r := ProfileReport{
		Operators: []OperatorStats{
			{Name: "NodeByLabelScan", Rows: 100, DbHits: 100, ElapsedNs: 12000},
			{Name: "ProduceResults", Rows: 100, DbHits: 0, ElapsedNs: 1000},
		},
		TotalRows:   200,
		TotalDbHits: 100,
		ElapsedMs:   0.013,
	}
	out := FormatReport(r)
	for _, want := range []string{
		"NodeByLabelScan", "ProduceResults", "Total",
		"Rows", "DbHits", "Time (ms)",
	} {
		if !contains(out, want) {
			t.Errorf("FormatReport output missing %q\n%s", want, out)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || s != "" && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := range len(s) - len(sub) + 1 {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
