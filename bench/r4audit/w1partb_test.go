//go:build r4audit

package r4audit

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestW1PartB_BoundKeyWriteFlatInN is the acceptance measurement for rmp #2225
// part B: admitting the hash join for a WRITING statement.
//
// # The shape
//
// `UNWIND $rows AS r MATCH (a:P {sid: r.ss}) …` is the bulk-load idiom. Its key
// varies per row, so it never reaches an index on either the read or the write
// path (#2182's correlated-seek pass only substitutes a ROW-INVARIANT binding).
// Both forms therefore SCAN — but the read was served by the #1506 hash join and
// the write was not, because [buildPlanWithMutatorFull] left the gate off. The
// read cost O(N+B), the write O(N·B).
//
// # What is measured
//
// Each write case against its own read control at the same N, so the residual is
// the write clause and nothing else. A write that is within a small constant of
// its read control has the join; a write that tracks the read's cost multiplied
// by the batch size does not.
//
// Round-4 figures, before part B (batch 500, indexed :P):
//
//	bound-key read    4.319ms   2.691ms   5.099ms    9.797ms   (N = 2k/4k/8k/16k)
//	bound-key write  226.77ms 454.647ms 923.972ms  1.86036s
func TestW1PartB_BoundKeyWriteFlatInN(t *testing.T) {
	const batch = 500
	sizes := []int{2000, 4000, 8000, 16000}

	cases := []struct{ name, stmt string }{
		{"bound-key read   (control)", `UNWIND $rows AS r MATCH (a:P {sid: r.ss}) RETURN a.sid`},
		{"bound-key write  CREATE rel", `UNWIND $rows AS r MATCH (a:P {sid: r.ss}) CREATE (a)-[:K]->(:Z)`},
		{"bound-key write  SET", `UNWIND $rows AS r MATCH (a:P {sid: r.ss}) SET a.t = true`},
		{"bound-key write  two joins", `UNWIND $rows AS r MATCH (a:P {sid: r.ss}), (b:P {sid: r.ts}) CREATE (a)-[:K]->(b)`},
	}

	fmt.Printf("%-30s", "case")
	for _, n := range sizes {
		fmt.Printf("%14s", fmt.Sprintf("N=%d", n))
	}
	fmt.Printf("   growth (8x N)\n")

	for _, c := range cases {
		fmt.Printf("%-30s", c.name)
		var first, last time.Duration
		for i, n := range sizes {
			// A fresh engine per repeat: a write mutates the graph, so re-running
			// the same statement on the same engine would measure a different
			// (growing) graph each time.
			best := time.Hour
			for k := 0; k < 5; k++ {
				eng, rows := seedForLoadOpt(t, n, batch, true)
				st := time.Now()
				res, err := eng.RunAny(context.Background(), c.stmt, map[string]any{"rows": rows})
				if err != nil {
					t.Fatalf("%s: %v", c.name, err)
				}
				for res.Next() {
				}
				if err := res.Err(); err != nil {
					t.Fatalf("%s: %v", c.name, err)
				}
				_ = res.Close()
				if d := time.Since(st); d < best {
					best = d
				}
			}
			fmt.Printf("%14s", best.Round(time.Microsecond))
			if i == 0 {
				first = best
			}
			last = best
		}
		fmt.Printf("   %.1fx\n", float64(last)/float64(first))
	}
}

// seedPartB mirrors seedForLoadOpt but lets the caller choose whether the Engine
// has the hash join disabled, which seedForLoadOpt cannot express (it owns the
// Engine construction and there is no exported accessor for the graph).
func seedPartB(tb testing.TB, n, batch int, disableHashJoin bool) (*cypher.Engine, []any) {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			tb.Fatal(err)
		}
		if err := g.SetNodeLabel(key, "P"); err != nil {
			tb.Fatal(err)
		}
		if err := g.SetNodeProperty(key, "sid", lpg.StringValue(fmt.Sprintf("s%d", i))); err != nil {
			tb.Fatal(err)
		}
	}
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableHashJoin: disableHashJoin})
	if _, err := eng.RunAny(context.Background(), `CREATE INDEX p_sid FOR (x:P) ON (x.sid)`, nil); err != nil {
		tb.Fatalf("create index: %v", err)
	}
	rows := make([]any, 0, batch)
	for i := 0; i < batch; i++ {
		rows = append(rows, map[string]any{
			"ss": fmt.Sprintf("s%d", i%n),
			"ts": fmt.Sprintf("s%d", (i*7+1)%n),
		})
	}
	return eng, rows
}

// TestW1PartB_HashJoinIsWhatChanged proves the win comes from the hash join and
// not from some other difference, by running the SAME write statement on an
// engine with DisableHashJoin set — the supported way to get the pre-part-B plan.
func TestW1PartB_HashJoinIsWhatChanged(t *testing.T) {
	const batch = 500
	const n = 8000
	const stmt = `UNWIND $rows AS r MATCH (a:P {sid: r.ss}) CREATE (a)-[:K]->(:Z)`

	measure := func(disable bool) time.Duration {
		best := time.Hour
		for k := 0; k < 3; k++ {
			eng, rows := seedPartB(t, n, batch, disable)
			st := time.Now()
			res, err := eng.RunAny(context.Background(), stmt, map[string]any{"rows": rows})
			if err != nil {
				t.Fatal(err)
			}
			for res.Next() {
			}
			_ = res.Close()
			if d := time.Since(st); d < best {
				best = d
			}
		}
		return best
	}

	on := measure(false)
	off := measure(true)
	fmt.Printf("N=%d batch=%d  hash join ON %v   OFF %v   speed-up %.1fx\n",
		n, batch, on.Round(time.Microsecond), off.Round(time.Microsecond), float64(off)/float64(on))
	if off <= on {
		t.Errorf("the hash join did not make the write faster (on=%v off=%v); part B is inert", on, off)
	}
}
