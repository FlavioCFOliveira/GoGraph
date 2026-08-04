package cypher_test

// mvcc_concurrent_merge_test.go — rmp #2306: what MERGE guarantees once nothing
// serialises writers.
//
// Several operators under cypher/exec documented a "single-writer guarantee" as
// the reason concurrent MERGE callers could not both create the same node. That
// guarantee was the engine's writer mutex and the store's capacity-one semaphore,
// and rmp #2306 retired both — so the claim had to be re-established by
// measurement rather than repeated.
//
// These tests record what is actually true now. They are characterisation tests:
// each asserts the guarantee the engine DOES provide, and names the one it does
// not, so a future reader is not misled by a comment describing a retired lock.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// countNodes returns how many :Acct nodes carry the given id.
func countMergedNodes(t *testing.T, eng *cypher.Engine, id string) int64 {
	t.Helper()
	r, err := eng.Run(context.Background(),
		`MATCH (a:Acct) WHERE a.id = '`+id+`' RETURN count(a) AS n`, nil)
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	var got int64
	for r.Next() {
		v, ok := r.Record()["n"]
		if !ok {
			t.Fatal("count query returned no n column")
		}
		n, ok := v.(expr.IntegerValue)
		if !ok {
			t.Fatalf("count column has type %T, want expr.IntegerValue", v)
		}
		got = int64(n)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("count drain: %v", err)
	}
	_ = r.Close()
	return got
}

// TestConcurrentMerge_WithoutAConstraintMayCreateDuplicates records the guarantee
// the engine does NOT provide, and why that is correct rather than a defect.
//
// Two MERGE statements on the same pattern can both find no match and both
// create, because under MVCC two CREATEs of two distinct new nodes are not a
// write-write conflict: there is no shared object for first-updater-wins to
// arbitrate. Preventing it would require serialising writers or taking a
// pattern-scoped lock, which is exactly what this sprint removed.
//
// This matches the reference implementations rather than diverging from them:
// Neo4j documents that concurrent MERGE can create duplicates unless a uniqueness
// constraint backs the pattern, for the same structural reason. The remedy is the
// constraint, which the companion test exercises.
//
// The assertion is therefore a RANGE, not an equality: at least one node exists
// (MERGE created it) and no more than the number of writers. Asserting exactly 1
// would encode a guarantee the engine does not make and would be flaky; asserting
// exactly N would encode one it does not make either, since the writers may well
// serialise by chance.
func TestConcurrentMerge_WithoutAConstraintMayCreateDuplicates(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)

	const writers = 8
	var wg sync.WaitGroup
	var failures atomic.Int64
	wg.Add(writers)
	for range writers {
		go func() {
			defer wg.Done()
			r, err := eng.RunAny(context.Background(),
				`MERGE (a:Acct {id:'shared'}) RETURN a`, nil)
			if err == nil {
				err = drain(r)
			}
			if err != nil {
				failures.Add(1)
				t.Errorf("MERGE: %v", err)
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatal("a MERGE failed; the duplication question is moot")
	}

	got := countMergedNodes(t, eng, "shared")
	if got < 1 {
		t.Fatalf("MERGE created nothing: got %d nodes, want at least 1", got)
	}
	if got > writers {
		t.Fatalf("got %d nodes from %d concurrent MERGE statements, which is more than "+
			"one per writer — each writer may create at most one node", got, writers)
	}
	t.Logf("%d concurrent MERGE statements on one pattern produced %d node(s). "+
		"Values above 1 are CORRECT and expected without a uniqueness constraint: "+
		"two CREATEs of two distinct new nodes are not a write-write conflict, so "+
		"MVCC has nothing to arbitrate. Use a UNIQUE constraint to make the pattern "+
		"unique (see TestConcurrentMerge_AUniqueConstraintCollapsesTheDuplicates).",
		writers, got)
}

// TestConcurrentMerge_AUniqueConstraintCollapsesTheDuplicates is the guarantee the
// engine DOES provide, and it is the one a caller should rely on: with a
// uniqueness constraint on the merged property, concurrent MERGE statements on the
// same pattern leave exactly ONE node, because the constraint's reservation is
// atomic and refuses the losers.
//
// This is the test that would catch a real defect. If retiring the writer
// serialisers had left constraint enforcement non-atomic, this would report more
// than one node — a Consistency violation under the ACID mandate, not a
// documented MERGE nuance.
func TestConcurrentMerge_AUniqueConstraintCollapsesTheDuplicates(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	r, err := eng.RunAny(ctx,
		`CREATE CONSTRAINT acct_id FOR (a:Acct) REQUIRE a.id IS UNIQUE`, nil)
	if err != nil {
		t.Fatalf("CREATE CONSTRAINT: %v", err)
	}
	if err := drain(r); err != nil {
		t.Fatalf("CREATE CONSTRAINT drain: %v", err)
	}

	const writers = 8
	var wg sync.WaitGroup
	var refused atomic.Int64
	wg.Add(writers)
	for range writers {
		go func() {
			defer wg.Done()
			res, rerr := eng.RunAny(ctx, `MERGE (a:Acct {id:'shared'}) RETURN a`, nil)
			if rerr == nil {
				rerr = drain(res)
			}
			if rerr != nil {
				// A refusal is a legitimate outcome: the constraint or the conflict
				// predicate rejected this writer. What must NOT happen is a second
				// node existing.
				refused.Add(1)
			}
		}()
	}
	wg.Wait()

	got := countMergedNodes(t, eng, "shared")
	if got != 1 {
		t.Fatalf("got %d :Acct nodes with id='shared' from %d concurrent MERGE "+
			"statements under a UNIQUE constraint, want exactly 1 (%d writers were "+
			"refused).\nA uniqueness constraint must hold whatever the write "+
			"concurrency: rmp #2306 retired every writer serialiser, so this is the "+
			"gate proving enforcement is atomic rather than a side effect of "+
			"one-writer-at-a-time.", got, writers, refused.Load())
	}
	t.Logf("%d concurrent MERGE statements under a UNIQUE constraint left exactly 1 "+
		"node; %d writer(s) were refused", writers, refused.Load())
}

// TestConcurrentCreate_PerStatementCountersAreNotShared checks the other stale
// claim: cypher/exec's counter structs use plain fields rather than atomics, and
// documented the single-writer write path as the reason.
//
// The reason is wrong but the code is safe, for a better reason: each statement's
// mutator adapter owns its counters inline (its own countersStore), so there is no
// shared counter for concurrent statements to race on. This test asserts the
// observable consequence — every statement reports ITS OWN write effects, not a
// total polluted by its peers — which is what a shared counter would break, and
// which `-race` would also catch.
func TestConcurrentCreate_PerStatementCountersAreNotShared(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)

	const (
		writers        = 8
		nodesPerWriter = 3
	)
	var wg sync.WaitGroup
	wrong := make([]string, writers)
	wg.Add(writers)
	for w := range writers {
		go func(w int) {
			defer wg.Done()
			// One statement creating exactly nodesPerWriter nodes, all keyed to this
			// writer so nothing collides. The list is inline rather than a parameter
			// to keep the helper free of the expr.Value plumbing.
			ids := make([]string, 0, nodesPerWriter)
			for i := range nodesPerWriter {
				ids = append(ids, fmt.Sprintf("'w%d-%d'", w, i))
			}
			q := fmt.Sprintf(`UNWIND [%s] AS i CREATE (:N {id:i})`, strings.Join(ids, ","))
			r, err := eng.RunAny(context.Background(), q, nil)
			if err != nil {
				wrong[w] = fmt.Sprintf("run: %v", err)
				return
			}
			if err := drain(r); err != nil {
				wrong[w] = fmt.Sprintf("drain: %v", err)
				return
			}
			if got := r.Counters().NodesCreated; got != nodesPerWriter {
				wrong[w] = fmt.Sprintf("NodesCreated = %d, want %d", got, nodesPerWriter)
			}
		}(w)
	}
	wg.Wait()
	for w, msg := range wrong {
		if msg != "" {
			t.Errorf("writer %d: %s.\nEach statement must report only its own write "+
				"effects. A counter shared across concurrent statements would inflate "+
				"this; rmp #2306 removed the serialisation that used to hide it.", w, msg)
		}
	}
}
