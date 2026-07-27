package cypher_test

// merge_access_path_bench_test.go — the measurement behind task #2217.
//
// The MERGE match phase walks EVERY interned node and filters afterwards
// (cypher/exec/merge_search.go, both the closure and the row-aware
// searchMergeNodes call mutator.WalkNodeIDs). Its cost therefore tracks the
// size of the whole graph rather than the population of the pattern's label,
// which makes the UNWIND-MERGE bulk-ingest idiom quadratic.
//
// THE DECISIVE SHAPE. Holding the matching label's population FIXED while
// growing an unrelated label separates the two hypotheses:
//
//	cost ∝ total nodes  → the walk examines decoys  (current behaviour)
//	cost ∝ label count  → the access path is label-restricted  (the fix)
//
// BenchmarkMergeMatch_DecoyGrowth is that experiment: :Hot stays at a constant
// 64 nodes and :Cold grows 0 → 16384. A label-restricted access path is FLAT
// across the sweep; the walk is linear in it. This mirrors the decomposition
// the round-4 audit used to isolate the load collapse, where a literal-key
// write was flat and a bound-key write was linear.
//
// BenchmarkMergeMatch_LabelGrowth is the control: it grows the MATCHING label,
// where cost is expected to grow under any correct implementation, because
// MERGE binds every match and so must enumerate them all.
//
// Run:
//
//	go test -run '^$' -bench 'BenchmarkMergeMatch' -benchmem ./cypher/

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newMergeBenchEngine seeds hot nodes labelled :Hot with a distinct id, and
// cold nodes labelled :Cold that can never match a :Hot pattern. Seeding goes
// through the Go API rather than Cypher, because a MATCH-based seed is itself
// quadratic and would dominate the measurement.
func newMergeBenchEngine(b *testing.B, hot, cold int) *cypher.Engine {
	b.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})

	for i := 0; i < hot; i++ {
		n := fmt.Sprintf("h%d", i)
		if err := g.AddNode(n); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(n, "Hot"); err != nil {
			b.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(n, "id", lpg.Int64Value(int64(i))); err != nil {
			b.Fatalf("SetNodeProperty: %v", err)
		}
	}
	for i := 0; i < cold; i++ {
		n := fmt.Sprintf("c%d", i)
		if err := g.AddNode(n); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(n, "Cold"); err != nil {
			b.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(n, "id", lpg.Int64Value(int64(i))); err != nil {
			b.Fatalf("SetNodeProperty: %v", err)
		}
	}
	return cypher.NewEngine(g)
}

// runMerge executes one MERGE that MATCHES an existing :Hot node, so the
// measurement is of the match phase and never of node creation.
func runMerge(b *testing.B, eng *cypher.Engine, query string) {
	b.Helper()
	res, err := eng.RunInTx(context.Background(), query, nil)
	if err != nil {
		b.Fatalf("MERGE: %v", err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		b.Fatalf("MERGE iterate: %v", err)
	}
	_ = res.Close()
}

// BenchmarkMergeMatch_DecoyGrowth holds the matching label's population fixed
// and grows an unrelated label. Flat means the access path is label-restricted;
// linear means it walks the whole graph.
func BenchmarkMergeMatch_DecoyGrowth(b *testing.B) {
	const hot = 64
	for _, cold := range []int{0, 1024, 4096, 16384} {
		b.Run(fmt.Sprintf("hot=%d/cold=%d", hot, cold), func(b *testing.B) {
			eng := newMergeBenchEngine(b, hot, cold)
			const q = `MERGE (n:Hot {id: 7}) RETURN n`
			runMerge(b, eng, q) // warm the plan cache
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				runMerge(b, eng, q)
			}
		})
	}
}

// BenchmarkMergeMatch_LabelGrowth is the control: growing the MATCHING label
// must cost more under any correct implementation, since MERGE binds every
// match and therefore has to enumerate them.
func BenchmarkMergeMatch_LabelGrowth(b *testing.B) {
	for _, hot := range []int{64, 1024, 4096, 16384} {
		b.Run(fmt.Sprintf("hot=%d/cold=0", hot), func(b *testing.B) {
			eng := newMergeBenchEngine(b, hot, 0)
			const q = `MERGE (n:Hot {id: 7}) RETURN n`
			runMerge(b, eng, q)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				runMerge(b, eng, q)
			}
		})
	}
}

// BenchmarkMergeMatch_LabelsOnly measures the pattern with no property
// predicate, where the label posting list alone determines the candidate set.
func BenchmarkMergeMatch_LabelsOnly(b *testing.B) {
	const hot = 8
	for _, cold := range []int{0, 4096, 16384} {
		b.Run(fmt.Sprintf("hot=%d/cold=%d", hot, cold), func(b *testing.B) {
			eng := newMergeBenchEngine(b, hot, cold)
			const q = `MERGE (n:Hot) RETURN count(n) AS c`
			runMerge(b, eng, q)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				runMerge(b, eng, q)
			}
		})
	}
}

// BenchmarkMergeUnwindBatch measures the idiom the task is actually about:
// UNWIND $rows AS r MERGE (p:Hot {id: r.id}). This is the row-aware path, where
// the merge search runs once PER ROW, so a per-row cost multiplies by the batch
// size. It is the shape a driver's bulk-ingest documentation teaches.
//
// It also guards a risk the single-MERGE benchmarks cannot see: the label
// posting list is resolved per invocation, so if materialising it were
// expensive relative to the walk it would show up here multiplied by B.
func BenchmarkMergeUnwindBatch(b *testing.B) {
	const batch = 200
	rows := make([]any, batch)
	for i := range rows {
		rows[i] = map[string]any{"id": int64(i)}
	}
	for _, cold := range []int{0, 4096, 16384} {
		b.Run(fmt.Sprintf("batch=%d/cold=%d", batch, cold), func(b *testing.B) {
			eng := newMergeBenchEngine(b, batch, cold)
			const q = `UNWIND $rows AS r MERGE (p:Hot {id: r.id}) RETURN count(p) AS c`
			params := map[string]any{"rows": rows}
			// Warm: every row MATCHES, so the measurement is of the match phase.
			if res, err := eng.RunInTxAny(context.Background(), q, params); err != nil {
				b.Fatalf("warm: %v", err)
			} else {
				for res.Next() {
				}
				_ = res.Close()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res, err := eng.RunInTxAny(context.Background(), q, params)
				if err != nil {
					b.Fatalf("MERGE batch: %v", err)
				}
				for res.Next() {
				}
				if err := res.Err(); err != nil {
					b.Fatalf("MERGE batch iterate: %v", err)
				}
				_ = res.Close()
			}
		})
	}
}
