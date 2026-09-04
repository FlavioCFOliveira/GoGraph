package cypher

// create_index_pair_barrier_test.go — engine-level gates for the ONE-BARRIER
// registration of the CREATE INDEX (btree) index pair (rmp #2703).
//
// The deterministic operator-level gate lives in
// cypher/exec/create_index_pair_test.go and proves the property for
// exec.NewCreateIndexPairOp itself. This file proves the ENGINE actually gets
// it: that Engine.createBTreeIndexLocked registers the user btree and its
// internal numeric companion so a holder of the graph's SHARED visibility hold
// — which is exactly what a write transaction's index-change fan-out
// (exec.IndexBuffer.Commit → index.Manager.ApplyBatch) runs under — can never
// observe one without the other.
//
// The observers take that same shared hold via lpg.Graph.ApplyVersioned.
// lpg.Graph.ApplyAtomically excludes them for the duration of the barrier, so
// with ONE barrier every sample shows neither index or both.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newPairBarrierEngine builds an engine over a small Person graph with a
// numeric property, so a btree CREATE INDEX has real data to backfill.
func newPairBarrierEngine(t *testing.T, nodes int) (*Engine, *lpg.Graph[string, float64]) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := range nodes {
		key := fmt.Sprintf("p%04d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode %s: %v", key, err)
		}
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", key, err)
		}
		if err := g.SetNodeProperty(key, "age", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty %s: %v", key, err)
		}
	}
	return NewEngine(g), g
}

// TestCreateIndexPair_NeverObservedHalfRegistered drives a real btree
// CREATE INDEX while observers hold the graph's shared visibility gate and
// sample the index pair. A sample showing the user index without its numeric
// companion is precisely the state a concurrent index fan-out would act on: it
// would deliver the batch to the user btree and the companion — whose backfill
// snapshot predates it — would miss those entries permanently.
func TestCreateIndexPair_NeverObservedHalfRegistered(t *testing.T) {
	const (
		rounds    = 40
		observers = 8
		nodes     = 64
		maxSpin   = 1 << 20 // safety valve; never reached in practice
	)

	eng, g := newPairBarrierEngine(t, nodes)
	idxMgr := g.IndexManager()

	var halfSeen, bothSeen, noneSeen atomic.Int64

	for round := range rounds {
		// A fresh (index name, property) per round, so BOTH members of the pair
		// are genuinely registered by this round's statement rather than one of
		// them being absorbed as already-present.
		userName := fmt.Sprintf("idx_round_%d", round)
		prop := fmt.Sprintf("age%d", round)
		numName := numericBTreeName("Person", prop)

		sample := func() {
			// ApplyVersioned takes the graph's visibility gate in SHARED mode:
			// the same hold a write transaction's index fan-out runs under, and
			// the hold ApplyAtomically excludes.
			_ = g.ApplyVersioned(func(lpg.WriteTx) error {
				_, uerr := idxMgr.GetIndex(userName)
				_, nerr := idxMgr.GetIndex(numName)
				switch {
				case uerr == nil && nerr == nil:
					bothSeen.Add(1)
				case uerr == nil && nerr != nil:
					halfSeen.Add(1)
				default:
					noneSeen.Add(1)
				}
				return nil
			})
		}

		var (
			stop atomic.Bool
			wg   sync.WaitGroup
		)
		for range observers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for n := 0; n < maxSpin && !stop.Load(); n++ {
					sample()
				}
			}()
		}

		res, err := eng.Run(context.Background(), fmt.Sprintf(
			"CREATE INDEX %s FOR (n:Person) ON (n.%s) OPTIONS {indexType:'btree'}",
			userName, prop), nil)
		if err != nil {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("round %d: CREATE INDEX: %v", round, err)
		}
		_ = res.Close()

		// Sample once more from this goroutine so the settled state is observed
		// even if every worker happened to exit early.
		sample()
		stop.Store(true)
		wg.Wait()

		if _, gerr := idxMgr.GetIndex(userName); gerr != nil {
			t.Fatalf("round %d: user index %q not registered: %v", round, userName, gerr)
		}
		if _, gerr := idxMgr.GetIndex(numName); gerr != nil {
			t.Fatalf("round %d: numeric companion %q not registered: %v", round, numName, gerr)
		}
	}

	// NON-VACUITY. The oracle means nothing unless the observers actually ran on
	// both sides of the registration: only "none" would mean they never sampled
	// after the DDL, only "both" would mean they never sampled before it.
	if noneSeen.Load() == 0 {
		t.Fatal("vacuous oracle: no observer ever sampled BEFORE the pair was registered")
	}
	if bothSeen.Load() == 0 {
		t.Fatal("vacuous oracle: no observer ever sampled AFTER the pair was registered")
	}

	if half := halfSeen.Load(); half != 0 {
		t.Errorf("observed the index pair HALF-REGISTERED %d times (none=%d both=%d): "+
			"the user btree was visible to a holder of the shared visibility gate "+
			"while its numeric companion was not, so a concurrent index fan-out "+
			"would reach the user index and be missed permanently by the companion",
			half, noneSeen.Load(), bothSeen.Load())
	}
}

// TestCreateIndexPair_BTreePathRegistersPairAndInvalidatesPlanCache pins the two
// engine-side effects the operator now owns on the BTREE path: the pair is
// registered, and onSchemaChange — which the engine wires to
// Engine.ClearPlanCache — really ran, so a plan built before the index cannot
// survive it.
//
// The existing plan_cache_ddl_invalidation_test.go covers CREATE INDEX without
// OPTIONS, which is the HASH path (Engine.runCreateHashIndex). The btree path
// is a different function and was not covered there.
func TestCreateIndexPair_BTreePathRegistersPairAndInvalidatesPlanCache(t *testing.T) {
	t.Parallel()

	eng, g := newPairBarrierEngine(t, 16)

	// Populate the plan cache with a query planned before the index exists.
	res, err := eng.Run(context.Background(), "MATCH (n:Person) WHERE n.age > 4 RETURN n", nil)
	if err != nil {
		t.Fatal(err)
	}
	for res.Next() {
		_ = res.Record()
	}
	if cerr := res.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if got := eng.cache.Len(); got == 0 {
		t.Fatal("plan cache empty after a cacheable query: the invalidation assertion " +
			"below would be vacuous")
	}

	res, err = eng.Run(context.Background(),
		"CREATE INDEX idx_age FOR (n:Person) ON (n.age) OPTIONS {indexType:'btree'}", nil)
	if err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}
	if cerr := res.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	idxMgr := g.IndexManager()
	if _, gerr := idxMgr.GetIndex("idx_age"); gerr != nil {
		t.Errorf("user btree not registered: %v", gerr)
	}
	if _, gerr := idxMgr.GetIndex(numericBTreeName("Person", "age")); gerr != nil {
		t.Errorf("numeric companion not registered: %v", gerr)
	}
	if got := eng.cache.Len(); got != 0 {
		t.Errorf("plan cache holds %d entries after CREATE INDEX, want 0: onSchemaChange "+
			"did not invalidate plans built before the index existed", got)
	}
}

// TestCreateIndexPair_IfNotExistsAbsorbed_LeavesSchemaUntouched pins the
// engine-side no-op branch: a second CREATE INDEX IF NOT EXISTS on an existing
// name registers nothing new and does not invalidate the plan cache.
func TestCreateIndexPair_IfNotExistsAbsorbed_LeavesSchemaUntouched(t *testing.T) {
	t.Parallel()

	eng, g := newPairBarrierEngine(t, 16)
	idxMgr := g.IndexManager()

	res, err := eng.Run(context.Background(),
		"CREATE INDEX idx_age FOR (n:Person) ON (n.age) OPTIONS {indexType:'btree'}", nil)
	if err != nil {
		t.Fatalf("first CREATE INDEX: %v", err)
	}
	_ = res.Close()
	before := idxMgr.Count()

	// Re-plan something so the cache is non-empty going into the absorbed DDL.
	res, err = eng.Run(context.Background(), "MATCH (n:Person) WHERE n.age > 4 RETURN n", nil)
	if err != nil {
		t.Fatal(err)
	}
	for res.Next() {
		_ = res.Record()
	}
	_ = res.Close()
	cachedBefore := eng.cache.Len()
	if cachedBefore == 0 {
		t.Fatal("plan cache empty: the no-invalidation assertion below would be vacuous")
	}

	res, err = eng.Run(context.Background(),
		"CREATE INDEX idx_age IF NOT EXISTS FOR (n:Person) ON (n.age) OPTIONS {indexType:'btree'}", nil)
	if err != nil {
		t.Fatalf("IF NOT EXISTS must not error: %v", err)
	}
	_ = res.Close()

	if got := idxMgr.Count(); got != before {
		t.Errorf("index count %d after an absorbed IF NOT EXISTS, want %d", got, before)
	}
	if got := eng.cache.Len(); got != cachedBefore {
		t.Errorf("plan cache went from %d to %d entries on an absorbed IF NOT EXISTS: "+
			"a no-op must not report a schema change", cachedBefore, got)
	}
}
