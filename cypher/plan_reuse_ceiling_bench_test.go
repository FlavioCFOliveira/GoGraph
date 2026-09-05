package cypher

// plan_reuse_ceiling_bench_test.go — rmp #2693 CEILING PROBE.
//
// # What this bounds, and what it is not
//
// #2693 records that the PHYSICAL operator tree is rebuilt on every execution,
// even on a plan-cache hit, and that an -memprofilerate=1 profile puts 59.52%
// of allocated objects in [Engine.buildReadPhysical]. Before designing anything,
// this file bounds what removing that build could possibly buy, so the design is
// argued against a number rather than against a profile share.
//
// Two instruments, deliberately different in kind:
//
//   - BenchmarkPlanReusePhases drives the CUMULATIVE PREFIXES of the read path
//     ([Engine.runReadPrefix], which already exists for rmp #2292) on the exact
//     workload #2693 names: bench/contention's cypher-read-label-small, a
//     2000-node label graph under "MATCH (n:N) RETURN count(n)". Differencing
//     3build against 2snapshot gives the build's cost per read; differencing
//     4full against 3build gives the execution's. The ceiling on any
//     build-elimination fix is then 4full/(4full-build), and it is an OPTIMISTIC
//     ceiling because no replacement is free.
//
//   - BenchmarkPrebuiltTreeCeiling is the DIRECT probe: one physical tree,
//     built once outside the timer, re-executed per iteration. It is NOT a
//     candidate implementation — it pins ONE snapshot for the whole run, which
//     is wrong for a read (see the design note in the task) — but it measures
//     execution with the build actually absent rather than arithmetically
//     removed, which is the only way to catch a build cost that is partly
//     PAID BACK at execution time (a pre-sized buffer, a resolved gate).
//
// Both arms assert the answer every iteration. A probe that reuses operator
// state is exactly the thing that can silently return a stale or empty answer,
// so an unasserted probe here would be worthless.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ceilNodes and ceilQuery reproduce bench/contention's cypher-read-label-small
// exactly: 2000 labelled nodes below the parallel-scan threshold, counted by
// label. Drifting from that population or that query would measure a workload
// none of #2693's evidence was gathered on.
const (
	ceilNodes = 2000
	ceilQuery = "MATCH (n:N) RETURN count(n)"
)

// newCeilRig builds bench/contention's seedGraph population through the same
// raw-graph calls it uses, so the label registry, the property-key registry and
// the adjacency all end up in the same shape.
func newCeilRig(tb testing.TB) *Engine {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < ceilNodes; i++ {
		id := fmt.Sprintf("n%d", i)
		if err := g.AddNode(id); err != nil {
			tb.Fatalf("AddNode %s: %v", id, err)
		}
		if err := g.SetNodeLabel(id, "N"); err != nil {
			tb.Fatalf("SetNodeLabel %s: %v", id, err)
		}
		if err := g.SetNodeProperty(id, "v", lpg.Int64Value(int64(i))); err != nil {
			tb.Fatalf("SetNodeProperty %s: %v", id, err)
		}
	}
	for i := 0; i+1 < ceilNodes; i++ {
		if err := g.AddEdge(fmt.Sprintf("n%d", i), fmt.Sprintf("n%d", i+1), 1); err != nil {
			tb.Fatalf("AddEdge %d: %v", i, err)
		}
	}
	return NewEngine(g)
}

// BenchmarkPlanReusePhases measures each cumulative prefix of the read path on
// cypher-read-label-small, serially and in parallel. Run it with -benchmem: the
// allocs/op column is what #2693's 33 allocs/op claim has to be checked against,
// and the per-phase difference is what attributes those allocations.
func BenchmarkPlanReusePhases(b *testing.B) {
	phases := []struct {
		name string
		p    readPhase
	}{
		{"1parse", phaseParse},
		{"2snapshot", phaseSnapshot},
		{"3build", phaseBuild},
		{"4full", phaseFull},
	}
	for _, ph := range phases {
		b.Run(ph.name, func(b *testing.B) {
			eng := newCeilRig(b)
			ctx := context.Background()
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := eng.runReadPrefix(ctx, ph.p, ceilQuery, nil); err != nil {
					b.Fatalf("phase %s: %v", ph.name, err)
				}
			}
		})
	}
}

// BenchmarkPlanReusePhasesParallel is the same decomposition under -cpu
// concurrency, because #2693's evidence is a CONCURRENCY result: GOGC=1000 buys
// +40-49% at level 4, where a serial allocation cost need not dominate. Each
// goroutine drives its own prefix; the engine is shared, exactly as the
// contention sweep shares it.
func BenchmarkPlanReusePhasesParallel(b *testing.B) {
	phases := []struct {
		name string
		p    readPhase
	}{
		{"1parse", phaseParse},
		{"2snapshot", phaseSnapshot},
		{"3build", phaseBuild},
		{"4full", phaseFull},
	}
	for _, ph := range phases {
		b.Run(ph.name, func(b *testing.B) {
			eng := newCeilRig(b)
			ctx := context.Background()
			b.ResetTimer()
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if err := eng.runReadPrefix(ctx, ph.p, ceilQuery, nil); err != nil {
						b.Fatalf("phase %s: %v", ph.name, err)
					}
				}
			})
		})
	}
}

// drainCeil executes one already-built tree end to end and returns the single
// count it produces. It is the assertion hook: a reused tree whose Init failed
// to reset its aggregation state returns a different number, and a reused tree
// whose scan cursor was not rewound returns zero rows.
func drainCeil(ctx context.Context, e *Engine, op exec.Operator, cols []string) (int64, error) {
	rs := exec.Run(ctx, op, cols)
	r := newResultWithLimit(rs, cols, nil, nil, nil, e.maxResultRows, e.maxResultBytes)
	r.globalMem = e.globalMem
	r.materialize()
	var (
		got  int64
		rows int
	)
	for r.Next() {
		rows++
		v := r.Record()["count(n)"]
		iv, ok := v.(expr.IntegerValue)
		if !ok {
			return 0, fmt.Errorf("count column is %T(%v), not an expr.IntegerValue", v, v)
		}
		got = int64(iv)
	}
	if err := r.Err(); err != nil {
		return 0, err
	}
	if rows != 1 {
		return 0, fmt.Errorf("got %d rows, want exactly 1", rows)
	}
	return got, nil
}

// BenchmarkPrebuiltTreeCeiling re-executes ONE physical tree. Read its ns/op as
// the floor a build-elimination fix could reach on this workload, and its
// allocs/op as the floor for allocation.
//
// It is a probe and not a design: the snapshot is pinned for the whole run, so
// a concurrent writer's commits would be invisible to every iteration after the
// first. That is precisely the correctness risk the task names, and it is why
// this arm lives in a benchmark and not in api.go.
func BenchmarkPrebuiltTreeCeiling(b *testing.B) {
	eng := newCeilRig(b)
	ctx := context.Background()
	entry, _, err := eng.parseAndAnalyse(ceilQuery)
	if err != nil {
		b.Fatalf("parseAndAnalyse: %v", err)
	}
	snap := eng.g.BeginRead()
	defer eng.g.EndRead(snap)
	queryReg := newNowAwareRegistry(eng.reg, time.Now())
	op, cols, err := eng.buildReadPhysical(ctx, entry, entry.plan, nil, queryReg, nil, snap)
	if err != nil {
		b.Fatalf("buildReadPhysical: %v", err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		got, derr := drainCeil(ctx, eng, op, cols)
		if derr != nil {
			b.Fatalf("iteration %d: %v", i, derr)
		}
		if got != ceilNodes {
			b.Fatalf("iteration %d: count=%d, want %d — the reused tree did not reset",
				i, got, ceilNodes)
		}
	}
}

// BenchmarkPrebuiltTreeCeilingParallel is the parallel counterpart, with ONE
// TREE PER GOROUTINE. Per-goroutine (or pooled) trees are a plausible shape for
// a real fix, so unlike the serial arm this one is not structurally impossible —
// it is only incorrect in that each goroutine pins its snapshot for the whole
// run. It bounds what a pooled-tree design could reach at concurrency.
func BenchmarkPrebuiltTreeCeilingParallel(b *testing.B) {
	eng := newCeilRig(b)
	ctx := context.Background()
	entry, _, err := eng.parseAndAnalyse(ceilQuery)
	if err != nil {
		b.Fatalf("parseAndAnalyse: %v", err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		snap := eng.g.BeginRead()
		defer eng.g.EndRead(snap)
		queryReg := newNowAwareRegistry(eng.reg, time.Now())
		op, cols, berr := eng.buildReadPhysical(ctx, entry, entry.plan, nil, queryReg, nil, snap)
		if berr != nil {
			b.Fatalf("buildReadPhysical: %v", berr)
		}
		for pb.Next() {
			got, derr := drainCeil(ctx, eng, op, cols)
			if derr != nil {
				b.Fatalf("%v", derr)
			}
			if got != ceilNodes {
				b.Fatalf("count=%d, want %d — the reused tree did not reset", got, ceilNodes)
			}
		}
	})
}
