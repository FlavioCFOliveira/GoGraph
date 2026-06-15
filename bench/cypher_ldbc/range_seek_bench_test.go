package cypher_ldbc_test

// range_seek_bench_test.go — benchmark demonstrating the range-predicate
// B+tree index seek (#1505) win: a selective range over a large indexed label
// goes from a full NodeByLabelScan + Filter (O(N_label)) to a NodeByIndex
// RangeScan that touches only the in-range nodes (O(log d + matches)).
//
// The query selects a small ordered window (≈ 0.5% of the population) of a
// :RSPerson label on its indexed string "name" property. With the seek
// DISABLED the engine scans every :RSPerson node and filters; with it ENABLED
// it descends the bound btree and emits only the in-range nodes. The result
// multiset is identical (proven by cypher/range_seek_differential_test.go).
//
// Run:
//
//	go test -run='^$' -bench=BenchmarkRangeSeek -benchmem ./bench/cypher_ldbc/...

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// rsBenchPop is the :RSPerson population. Large enough that scanning all of it
// dominates a selective seek, and well above the seek's 1024-node floor.
const rsBenchPop = 50_000

// rsBenchQuery selects names in ["name00000", "name00250") — 250 of 50_000
// nodes (0.5% selectivity), comfortably inside the seek's 10% gate.
const rsBenchQuery = `MATCH (p:RSPerson) WHERE p.name >= "name00000" AND p.name < "name00250" RETURN p.name AS name`

// buildRangeSeekBenchEngine seeds a large :RSPerson label keyed by a sortable
// "name" string and creates the bound btree index via the engine's CREATE
// INDEX path (the only way to get a self-maintaining, backfilled btree).
func buildRangeSeekBenchEngine(b *testing.B, disableSeek bool) *cypher.Engine {
	b.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for i := 0; i < rsBenchPop; i++ {
		k := fmt.Sprintf("p%05d", i)
		_ = g.AddNode(k)
		_ = g.SetNodeLabel(k, "RSPerson")
		_ = g.SetNodeProperty(k, "name", lpg.StringValue(fmt.Sprintf("name%05d", i)))
	}
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{
		DisableRangeIndexSeek: disableSeek,
		MaxResultRows:         cypher.MaxResultRowsUnlimited,
	})
	if _, err := eng.Run(context.Background(),
		`CREATE INDEX FOR (n:RSPerson) ON (n.name) OPTIONS {indexType:'btree'}`, nil); err != nil {
		b.Fatalf("CREATE INDEX: %v", err)
	}
	return eng
}

func benchRangeSeek(b *testing.B, enabled bool) {
	b.Helper()
	eng := buildRangeSeekBenchEngine(b, !enabled)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.Run(ctx, rsBenchQuery, nil)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		for res.Next() {
		}
		if e := res.Err(); e != nil {
			b.Fatalf("Err: %v", e)
		}
		if err := res.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
	}
}

// BenchmarkRangeSeekSelective_IndexSeek measures the optimised range-seek plan.
func BenchmarkRangeSeekSelective_IndexSeek(b *testing.B) {
	benchRangeSeek(b, true)
}

// BenchmarkRangeSeekSelective_LabelScan measures the legacy NodeByLabelScan +
// Filter for the same query — the baseline the range seek replaces.
func BenchmarkRangeSeekSelective_LabelScan(b *testing.B) {
	benchRangeSeek(b, false)
}

// TestRangeSeekBench_ResultsMatch is a fast guard in the normal test layer: it
// confirms the benchmark query returns the identical row count under both
// plans, so the benchmark compares like with like.
func TestRangeSeekBench_ResultsMatch(t *testing.T) {
	build := func(disable bool) *cypher.Engine {
		g := lpg.New[string, float64](adjlist.Config{})
		for i := 0; i < 4000; i++ {
			k := fmt.Sprintf("p%05d", i)
			_ = g.AddNode(k)
			_ = g.SetNodeLabel(k, "RSPerson")
			_ = g.SetNodeProperty(k, "name", lpg.StringValue(fmt.Sprintf("name%05d", i)))
		}
		eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{
			DisableRangeIndexSeek: disable,
			MaxResultRows:         cypher.MaxResultRowsUnlimited,
		})
		if _, err := eng.Run(context.Background(),
			`CREATE INDEX FOR (n:RSPerson) ON (n.name) OPTIONS {indexType:'btree'}`, nil); err != nil {
			t.Fatalf("CREATE INDEX: %v", err)
		}
		return eng
	}
	count := func(e *cypher.Engine) int {
		res, err := e.Run(context.Background(), rsBenchQuery, nil)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for res.Next() {
			n++
		}
		if err := res.Err(); err != nil {
			t.Fatal(err)
		}
		_ = res.Close()
		return n
	}
	on := count(build(false))
	off := count(build(true))
	if on != off {
		t.Fatalf("row-count mismatch: seek=%d scan=%d", on, off)
	}
	if on != 250 {
		t.Fatalf("unexpected row count %d, want 250", on)
	}
}
