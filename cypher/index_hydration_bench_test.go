package cypher

// index_hydration_bench_test.go — the measured justification for rmp #2490.
//
// CLAUDE.md requires a change that spends one resource vector to save another to
// be measured IN THIS PROJECT rather than argued. Hydration spends disk (the
// indexes/<name>.bin payloads a checkpoint already wrote) and I/O (reading them
// back) to save CPU and allocations at reopen, so the two arms below are the A/B:
//
//	arm A "backfill" — the pre-#2490 path: registerRecoveredIndexes /
//	                   registerRecoveredConstraints scan the recovered graph.
//	arm B "hydrate"  — the payload source is wired, so each index is loaded from
//	                   its snapshot image.
//
// Both arms run over the IDENTICAL recovered graph and the identical index set,
// and both are checked for identical index CONTENTS before the timing loop: a
// benchmark of an arm that produces the wrong answer measures nothing.
//
// # What is deliberately NOT in the timed region
//
// recovery.Open. It is byte-identical work in both arms — it neither reads nor
// hydrates an index — and it is O(snapshot + WAL), so including it would dilute
// the delta with a constant an order of magnitude larger. The loop therefore
// resets the graph's index.Manager and rebuilds the Engine, which is exactly the
// step the two arms differ in.
//
// The existing BenchmarkIndexesRecoveryVsRebuild in store/recovery is NOT the
// baseline here and must not be quoted as one: its "rebuild" arm re-inserts into
// an index in memory, which is not the engine backfill path — that path walks the
// WHOLE mapper (one nodeRef per node in the graph, not per node carrying the
// indexed label), twice per user index (the bound index plus its numeric
// companion) and twice per UNIQUE constraint.
//
// # Honest scope limit
//
// The win materialises only when the WAL suffix is EMPTY or index-irrelevant,
// because otherwise the staleness gate refuses the payload and arm B degrades
// into arm A plus a wasted lookup. In this repository that means
// store.WithFinalCheckpoint (store/db.go, used only by internal/sim) and
// post-checkpoint crashes. An embedder that never checkpoints —
// examples/25_software_house_api is one — always rebuilds, and this measurement
// says nothing about it.

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// benchHydrationNodes is the fixture size. It is above
// backfillParallelMinNodes (8192) so arm A takes the PARALLEL backfill path —
// the production one for a graph of this size — rather than the serial fallback,
// and above the planner's range-seek population floor so the fixture is a
// realistic reopen rather than a toy.
const benchHydrationNodes = 50_000

// buildHydrationFixture creates a checkpointed store directory holding
// benchHydrationNodes labelled, propertied nodes plus one hash index and one
// btree index (so four registered indexes once each numeric companion is
// derived), with the WAL prefix truncated so the suffix is empty and every
// payload is hydratable.
func buildHydrationFixture(tb testing.TB) string {
	tb.Helper()
	dir := tb.TempDir()
	s := hydOpen(tb, dir)
	s.write(`CREATE INDEX person_name FOR (n:Person) ON (n.name)`)
	s.write(`CREATE INDEX person_age FOR (n:Person) ON (n.age) OPTIONS {indexType: 'btree'}`)
	s.write(fmt.Sprintf(
		`UNWIND range(1, %d) AS i CREATE (:Person {name: toString(i), age: i})`, benchHydrationNodes))
	s.checkpoint()
	s.close()
	if m := hydManifest(tb, dir); m.IndexesCommitTS == 0 || len(m.Indexes) == 0 {
		tb.Fatalf("fixture is not hydratable: indexes_commit_ts=%d payloads=%d",
			m.IndexesCommitTS, len(m.Indexes))
	}
	return dir
}

// BenchmarkRecoveredIndexPopulation measures the engine-construction step of a
// reopen with and without the snapshot payloads, over one recovered graph.
//
// Run it as:
//
//	go test -run=^$ -bench=BenchmarkRecoveredIndexPopulation -benchmem -count=10 ./cypher/ > new.txt
//	benchstat new.txt
func BenchmarkRecoveredIndexPopulation(b *testing.B) {
	dir := buildHydrationFixture(b)

	res, err := recovery.Open[string, float64](dir, hydRecOpts())
	if err != nil {
		b.Fatalf("recovery.Open: %v", err)
	}
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		b.Fatalf("wal.Open: %v", err)
	}
	b.Cleanup(func() {
		if cerr := w.Close(); cerr != nil {
			b.Errorf("wal.Close: %v", cerr)
		}
	})
	st := res.NewStore(w, hydStoreOpts())
	g := st.Graph()

	if res.SnapshotIndexes == 0 {
		b.Fatal("no hydratable payload in the fixture: arm B would silently be arm A")
	}

	// build constructs one Engine over g with a fresh index.Manager, so each
	// iteration populates every index from scratch exactly as a reopen does.
	// src == nil is arm A (backfill), src == res is arm B (hydrate).
	build := func(src IndexPayloadSource) *Engine {
		g.SetIndexManager(index.NewManager())
		return NewEngineWithOptions(g, EngineOptions{
			Store:                  st,
			RecoveredConstraints:   ConstraintDefsFromRecovery(res.Constraints),
			RecoveredIndexes:       IndexDefsFromRecovery(res.Indexes),
			RecoveredIndexPayloads: src,
		})
	}

	// EQUIVALENCE CHECK, before any timing: the two arms must produce identical
	// index contents, or the comparison is between a correct implementation and a
	// broken one.
	engA := build(nil)
	if engA.recoveredIdx.hydrated != 0 || engA.recoveredIdx.rebuilt == 0 {
		b.Fatalf("arm A did not backfill: hydrated=%d rebuilt=%d",
			engA.recoveredIdx.hydrated, engA.recoveredIdx.rebuilt)
	}
	gotA := benchIndexKeys(b, g, "person_name", "1", "500", fmt.Sprint(benchHydrationNodes))

	engB := build(res)
	if engB.recoveredIdx.hydrated == 0 || engB.recoveredIdx.rebuilt != 0 {
		b.Fatalf("arm B did not hydrate: hydrated=%d rebuilt=%d",
			engB.recoveredIdx.hydrated, engB.recoveredIdx.rebuilt)
	}
	gotB := benchIndexKeys(b, g, "person_name", "1", "500", fmt.Sprint(benchHydrationNodes))

	if !reflect.DeepEqual(gotA, gotB) {
		b.Fatalf("the two arms disagree, so the timings below compare different results:\n"+
			"backfill=%v\nhydrate=%v", gotA, gotB)
	}
	if len(gotA) == 0 {
		b.Fatal("the equivalence probe found nothing, so it would hold for two empty indexes")
	}

	b.Run("backfill", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = build(nil)
		}
	})
	b.Run("hydrate", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = build(res)
		}
	})
}

// benchIndexKeys reads the id lists the named hash index holds for each of
// values, flattened, so the two arms can be compared for exact equality.
func benchIndexKeys(b *testing.B, g *lpg.Graph[string, float64], name string, values ...string) []uint64 {
	b.Helper()
	sub, err := g.IndexManager().GetIndex(name)
	if err != nil {
		b.Fatalf("GetIndex(%q): %v", name, err)
	}
	idx, ok := sub.(interface {
		LookupAppend(string, []uint64) []uint64
	})
	if !ok {
		b.Fatalf("index %q (%T) does not expose LookupAppend", name, sub)
	}
	var out []uint64
	for _, v := range values {
		out = idx.LookupAppend(v, out)
	}
	return out
}
