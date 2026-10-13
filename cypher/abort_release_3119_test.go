package cypher_test

// abort_release_3119_test.go — regression gates for rmp #3119 and #3120: a write
// transaction that is cut, fails, is refused or is rolled back — an autocommit
// statement or an explicit transaction — leaves no memory behind for the nodes
// it created.
//
// # What was wrong
//
// Every node a CREATE makes is interned in the graph's mapper under a synthetic
// key the moment it is written. When the statement's transaction aborted, the
// abort left each such id tombstoned and marked unborn and its key bound, for
// the life of the graph. Measured on this branch's base (81ed1eae, v0.17.0)
// with the reporter's workload — 600 committed nodes, then
// `MATCH (a), (b), (c) CREATE ()` cut by a deadline:
//
//   - 1 s deadline: 1,565,861 keys interned at the cut; after the rollback and a
//     forced GC the live heap was 315.2 MB against 2.3 MB before, and still
//     161.3 MB once a later write and a vacuum pass had run (about 200 B per
//     issued id at first, about 100 B permanently);
//   - 5 s deadline: 7,256,212 keys and 1847.5 MB live after the rollback.
//
// Besides the mapper (keys, forward map, reverse table), the abort kept the
// unborn set, the recycled transaction state's created list and side write set,
// and one adjacency conflict stamp per created node.
//
// # What the gate asserts
//
// After the cut statement and runtime.GC, (i) the live heap is within
// heapSlack + bytesPerIssuedID x issued of the level before it, and (ii) the
// mapper holds exactly the committed nodes. issued is the number of ids the
// statement drew, read from the mapper's high-water marks, which the release
// never lowers. bytesPerIssuedID = 8 is 25 times below the defect's permanent
// cost and 25 times below its cost right after the rollback, so the gate fails
// on the defect with a wide margin; heapSlack covers what one statement
// legitimately leaves cached (its plan) and allocator noise. Both hold under
// -race, which changes allocation counts but not what stays reachable.
//
// The cut is a real context deadline. Its length is doubled until the statement
// has drawn minIssued ids, so the gate costs the same on a fast host and under
// -race and never passes for want of work.
//
// Layer: short.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

const (
	// minIssued is how many ids the cut statement must draw before the gate
	// judges it: enough that the defect's retention (≥ 100 B per id) is far above
	// heapSlack.
	minIssued = 50_000
	// bytesPerIssuedID is the heap the gate tolerates per id the cut statement
	// drew; see the file comment.
	bytesPerIssuedID = 8
	// heapSlack is the fixed heap the gate tolerates; see the file comment.
	heapSlack = 2 << 20
)

// liveHeap returns the live heap after two full collections, so objects freed
// by the first (finalizers, sync.Pool victims) are gone too.
func liveHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// issuedIDs is the number of ids the mapper has ever issued, holes included:
// the sum of its shards' high-water marks.
func issuedIDs(m *graph.Mapper[string]) uint64 {
	var n uint64
	for _, v := range m.Watermark().Next() {
		n += v
	}
	return n
}

// mustRun3119 runs q on eng, drains it and fails the test on any error.
func mustRun3119(t *testing.T, eng *cypher.Engine, q string, params map[string]any) {
	t.Helper()
	res, err := eng.RunInTxAny(context.Background(), q, params)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		t.Fatalf("%s: %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("%s: close: %v", q, err)
	}
}

// runExpectFailure runs q on eng and returns the statement's error, failing the
// test when it succeeds.
func runExpectFailure(ctx context.Context, t *testing.T, eng *cypher.Engine, q string) error {
	t.Helper()
	res, err := eng.RunInTxAny(ctx, q, nil)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	err = res.Err()
	_ = res.Close()
	if err == nil {
		t.Fatalf("%s: succeeded, want a failure", q)
	}
	return err
}

// countNodes3119 returns count(n) over the whole graph as the engine sees it.
func countNodes3119(t *testing.T, eng *cypher.Engine) int64 {
	t.Helper()
	res, err := eng.Run(context.Background(), `MATCH (n) RETURN count(n) AS c`, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Close() }()
	if !res.Next() {
		t.Fatalf("count returned no row: %v", res.Err())
	}
	c, err := strconv.ParseInt(res.ValueAt(0).String(), 10, 64)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return c
}

// cutUntilIssued runs cut — a statement that must end on its context's deadline
// — under deadlines doubling from 50 ms until the statements have drawn at least
// atLeast ids from m, and returns how many they drew.
func cutUntilIssued(t *testing.T, m *graph.Mapper[string], atLeast uint64, cut func(context.Context) error) uint64 {
	t.Helper()
	issued0 := issuedIDs(m)
	for d := 50 * time.Millisecond; ; d *= 2 {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		err := cut(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the cut statement ended with %v, want the deadline", err)
		}
		issued := issuedIDs(m) - issued0
		if issued >= atLeast {
			return issued
		}
		if d > 30*time.Second {
			t.Fatalf("only %d ids drawn within %v; the gate needs %d", issued, d, atLeast)
		}
	}
}

// assertReleased checks the two invariants every gate in this file shares: the
// mapper holds exactly the committed nodes, and the live heap grew by no more
// than bound since before.
func assertReleased(t *testing.T, eng *cypher.Engine, g *lpg.Graph[string, float64], committed int, before, after, bound, issued uint64) {
	t.Helper()
	if got := g.AdjList().Mapper().Len(); got != committed {
		t.Errorf("mapper holds %d keys, want the %d committed nodes", got, committed)
	}
	if got := countNodes3119(t, eng); got != int64(committed) {
		t.Errorf("count(n) = %d, want %d", got, committed)
	}
	if got := g.TombstoneCountStored(); got != 0 {
		t.Errorf("%d tombstones left by a rolled-back creation", got)
	}
	grew := int64(after) - int64(before)
	t.Logf("issued %d ids; live heap %d -> %d bytes (%+d), bound %d", issued, before, after, grew, bound)
	if grew > int64(bound) {
		t.Errorf("live heap grew by %d bytes over %d issued ids (%.2f B/id), bound %d", grew, issued, float64(grew)/float64(issued), bound)
	}
}

// TestCutCartesianCreateLeavesNoMemory_3119 is the reporter's reproduction at
// minimum cost; see the file comment for the bound.
func TestCutCartesianCreateLeavesNoMemory_3119(t *testing.T) {
	// Not parallel: the gate reads the process heap.
	g := lpg.New[string, float64](adjlist.Config{})
	defer func() { _ = g.Close() }()
	eng := cypher.NewEngine(g)
	const committed = 600
	mustRun3119(t, eng, `UNWIND range(1, 600) AS i CREATE (:N {i: i})`, nil)
	m := g.AdjList().Mapper()

	before := liveHeap()
	issued := cutUntilIssued(t, m, minIssued, func(ctx context.Context) error {
		return runExpectFailure(ctx, t, eng, `MATCH (a), (b), (c) CREATE ()`)
	})
	after := liveHeap()

	if got := m.Len(); got != committed {
		t.Errorf("mapper holds %d keys after the rollback, want the %d committed nodes", got, committed)
	}
	if got := countNodes3119(t, eng); got != committed {
		t.Errorf("count(n) = %d after the rollback, want %d", got, committed)
	}
	if got := g.TombstoneCountStored(); got != 0 {
		t.Errorf("%d tombstones left by a rolled-back creation", got)
	}
	if got := g.MVCCStats().AdjConflictStamps; got != 0 {
		t.Errorf("%d adjacency conflict stamps left by a rolled-back creation", got)
	}
	bound := uint64(heapSlack) + bytesPerIssuedID*issued
	grew := int64(after) - int64(before)
	t.Logf("issued %d ids; live heap %d -> %d bytes (%+d), bound %d", issued, before, after, grew, bound)
	if grew > int64(bound) {
		t.Errorf("live heap grew by %d bytes over %d issued ids (%.1f B/id), bound %d", grew, issued, float64(grew)/float64(issued), bound)
	}
}

// abortReleaseWirings runs f over the store-less engine and the WAL-backed one.
// The WAL-backed graph has an id reserver installed, which the release must not
// disturb.
func abortReleaseWirings(t *testing.T, f func(t *testing.T, eng *cypher.Engine, g *lpg.Graph[string, float64])) {
	t.Run("InMemory", func(t *testing.T) {
		t.Parallel()
		g := lpg.New[string, float64](adjlist.Config{})
		t.Cleanup(func() { _ = g.Close() })
		f(t, cypher.NewEngine(g), g)
	})
	t.Run("WALStore", func(t *testing.T) {
		t.Parallel()
		o, err := store.Open(t.TempDir(), openedOptions())
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		t.Cleanup(func() {
			if err := o.Close(); err != nil && !errors.Is(err, wal.ErrWriterClosed) {
				t.Errorf("store.Close: %v", err)
			}
		})
		f(t, cypher.NewEngineWithOpened(o), o.Graph())
	})
}

// TestAbortedStatementMintedKeysAreReleased_3119 fails statements that created
// nodes through CREATE and through MERGE and requires: no key the statements
// minted resolves, every id they drew is a hole, nothing is tombstoned, the
// committed graph is intact, and a later creation is issued an id above every
// id the failed statements drew.
func TestAbortedStatementMintedKeysAreReleased_3119(t *testing.T) {
	t.Parallel()
	abortReleaseWirings(t, func(t *testing.T, eng *cypher.Engine, g *lpg.Graph[string, float64]) {
		ctx := context.Background()
		mustRun3119(t, eng, `CREATE (:Seed {k: 1}), (:Seed {k: 2})`, nil)
		m := g.AdjList().Mapper()
		seq0 := g.KeySequence().Load()
		wm0 := m.Watermark()
		for _, q := range []string{
			// Setting a node as a property value is refused (InvalidPropertyType)
			// after the creations have been applied.
			`UNWIND range(1, 40) AS i CREATE (n:T {i: i}) WITH collect(n) AS ns MATCH (s:Seed {k: 1}) SET s.bad = ns[0]`,
			`MERGE (n:Mx {k: 7}) WITH n MATCH (s:Seed {k: 2}) SET s.bad = n`,
			`CREATE (a:T)-[:R]->(b:T) WITH a MATCH (s:Seed {k: 1}) SET s.bad = a`,
		} {
			if err := runExpectFailure(ctx, t, eng, q); err == nil {
				t.Fatalf("%s did not fail", q)
			}
		}
		seq1 := g.KeySequence().Load()
		wm1 := m.Watermark()
		if seq1 <= seq0 {
			t.Fatal("setup: the failed statements minted no key")
		}
		for v := seq0 + 1; v <= seq1; v++ {
			for _, k := range []string{"__cx_" + strconv.FormatUint(v, 16), "__cx_merge_" + strconv.FormatUint(v, 16)} {
				if id, ok := m.Lookup(k); ok {
					t.Fatalf("minted key %q of a failed statement still resolves to %d", k, id)
				}
			}
		}
		next0, next1 := wm0.Next(), wm1.Next()
		drawn := 0
		for s := range next1 {
			for idx := next0[s]; idx < next1[s]; idx++ {
				id := graph.NodeID(idx<<8 | uint64(s))
				drawn++
				if k, ok := m.Resolve(id); ok {
					t.Fatalf("id %d drawn by a failed statement resolves to %q, want a hole", id, k)
				}
				if g.IsTombstonedStored(id) {
					t.Fatalf("id %d drawn by a failed statement is tombstoned, want a hole", id)
				}
			}
		}
		if drawn < 43 {
			t.Fatalf("the failed statements drew %d ids, want at least 43", drawn)
		}
		if got := m.Len(); got != 2 {
			t.Fatalf("mapper holds %d keys, want the 2 seeds", got)
		}
		if got := countNodes3119(t, eng); got != 2 {
			t.Fatalf("count(n) = %d, want 2", got)
		}
		mustRun3119(t, eng, `CREATE (:Later)`, nil)
		if got := countNodes3119(t, eng); got != 3 {
			t.Fatalf("count(n) = %d after a later CREATE, want 3", got)
		}
		var later graph.NodeID
		found := false
		m.Walk(func(id graph.NodeID, _ string) bool {
			if g.HasNodeLabelByID(id, "Later") {
				later, found = id, true
				return false
			}
			return true
		})
		if !found {
			t.Fatal("the later node is not in the mapper")
		}
		if wm1.Covers(later) {
			t.Fatalf("the later node got id %d, below the high-water mark of the failed statements: an id was issued twice", later)
		}
	})
}

// TestCommittedWithdrawnCreationKeepsBinding_3119: a creation withdrawn inside a
// transaction that COMMITS is named by the commit marker, so its key stays
// bound and its id is a committed-dead node, not a hole.
func TestCommittedWithdrawnCreationKeepsBinding_3119(t *testing.T) {
	t.Parallel()
	abortReleaseWirings(t, func(t *testing.T, eng *cypher.Engine, g *lpg.Graph[string, float64]) {
		mustRun3119(t, eng, `CREATE (:Seed)`, nil)
		m := g.AdjList().Mapper()
		mustRun3119(t, eng, `CREATE (n:Gone) DELETE n`, nil)
		if got := m.Len(); got != 2 {
			t.Fatalf("mapper holds %d keys, want 2: the committed-dead node must stay bound", got)
		}
		if got := g.TombstoneCountStored(); got != 1 {
			t.Fatalf("%d tombstones, want 1 for the committed-dead node", got)
		}
		if got := countNodes3119(t, eng); got != 1 {
			t.Fatalf("count(n) = %d, want 1", got)
		}
	})
}

// TestAbortReleaseLeavesConcurrentCommitsIntact_3119 runs committing writers
// beside statements that create nodes and fail. Every committed node must stay
// bound and counted, and no key of a failed statement may remain. Under -race it
// is also the data-race gate for the release path.
func TestAbortReleaseLeavesConcurrentCommitsIntact_3119(t *testing.T) {
	t.Parallel()
	abortReleaseWirings(t, func(t *testing.T, eng *cypher.Engine, g *lpg.Graph[string, float64]) {
		const committers, perCommitter, aborters, perAborter = 4, 25, 2, 10
		ctx := context.Background()
		var wg sync.WaitGroup
		errs := make(chan error, committers*perCommitter+aborters*perAborter)
		for w := 0; w < committers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < perCommitter; i++ {
					res, err := eng.RunInTxAny(ctx, `CREATE (:C {w: $w, i: $i})`, map[string]any{"w": w, "i": i})
					if err == nil {
						for res.Next() {
						}
						err = res.Err()
						_ = res.Close()
					}
					if err != nil {
						errs <- fmt.Errorf("committer %d/%d: %w", w, i, err)
					}
				}
			}()
		}
		for a := 0; a < aborters; a++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < perAborter; i++ {
					res, err := eng.RunInTxAny(ctx,
						`UNWIND range(1, 20) AS j CREATE (n:A {j: j}) WITH collect(n) AS ns UNWIND ns AS x SET x.bad = ns[0]`, nil)
					if err == nil {
						for res.Next() {
						}
						err = res.Err()
						_ = res.Close()
					}
					if err == nil {
						errs <- fmt.Errorf("aborter %d/%d: the failing statement succeeded", a, i)
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		m := g.AdjList().Mapper()
		const want = committers * perCommitter
		if got := m.Len(); got != want {
			t.Fatalf("mapper holds %d keys, want the %d committed nodes", got, want)
		}
		if got := countNodes3119(t, eng); got != want {
			t.Fatalf("count(n) = %d, want %d", got, want)
		}
		if got := g.TombstoneCountStored(); got != 0 {
			t.Fatalf("%d tombstones left, want 0", got)
		}
		seen := map[string]bool{}
		m.Walk(func(id graph.NodeID, _ string) bool {
			props := g.NodePropertiesByID(id)
			pw, _ := props["w"].Int64()
			pi, _ := props["i"].Int64()
			key := fmt.Sprintf("%d/%d", pw, pi)
			if seen[key] {
				t.Errorf("committed node %s appears twice", key)
			}
			seen[key] = true
			return true
		})
		if len(seen) != want {
			t.Fatalf("%d distinct committed nodes walked, want %d", len(seen), want)
		}
	})
}

// TestAbortReleaseSurvivesReopen_3119 releases a failed statement's ids on a
// durable store, commits more nodes, and reopens it: recovery must rebuild the
// committed nodes under the ids they had, and the drawn ids must stay holes.
func TestAbortReleaseSurvivesReopen_3119(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	o, err := store.Open(dir, openedOptions())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	eng := cypher.NewEngineWithOpened(o)
	ctx := context.Background()
	mustRun3119(t, eng, `CREATE (:P {k: 1})`, nil)
	if err := runExpectFailure(ctx, t, eng,
		`UNWIND range(1, 30) AS i CREATE (n:T {i: i}) WITH collect(n) AS ns MATCH (p:P) SET p.bad = ns[0]`); err == nil {
		t.Fatal("the failing statement succeeded")
	}
	mustRun3119(t, eng, `CREATE (:P {k: 2})`, nil)
	ids := map[string]graph.NodeID{}
	o.Graph().AdjList().Mapper().Walk(func(id graph.NodeID, k string) bool { ids[k] = id; return true })
	if len(ids) != 2 {
		t.Fatalf("%d keys bound before reopen, want 2", len(ids))
	}
	wm := o.Graph().AdjList().Mapper().Watermark()
	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	o2, err := store.Open(dir, openedOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() {
		if err := o2.Close(); err != nil && !errors.Is(err, wal.ErrWriterClosed) {
			t.Errorf("Close: %v", err)
		}
	}()
	m2 := o2.Graph().AdjList().Mapper()
	if got := m2.Len(); got != 2 {
		t.Fatalf("%d keys after reopen, want 2", got)
	}
	for k, id := range ids {
		if got, ok := m2.Lookup(k); !ok || got != id {
			t.Fatalf("key %q is bound to (%d,%v) after reopen, want %d", k, got, ok, id)
		}
	}
	// A creation after the reopen must not reuse an id the failed statement drew.
	eng2 := cypher.NewEngineWithOpened(o2)
	mustRun3119(t, eng2, `CREATE (:P {k: 3})`, nil)
	m2.Walk(func(id graph.NodeID, k string) bool {
		if _, old := ids[k]; !old && wm.Covers(id) {
			t.Errorf("a node created after the reopen got id %d, below the pre-reopen high-water mark", id)
		}
		return true
	})
}

// TestExplicitTxRollbackLeavesNoMemory_3119 is the gate for an EXPLICIT
// transaction: the Cartesian CREATE runs inside a transaction opened with a
// deadline, the deadline cuts it, and the transaction is rolled back as a
// whole. The same two invariants and bound as the autocommit gate apply. A
// second transaction that creates nodes successfully and is then rolled back
// must release them too.
func TestExplicitTxRollbackLeavesNoMemory_3119(t *testing.T) {
	// Not parallel: the gate reads the process heap.
	g := lpg.New[string, float64](adjlist.Config{})
	defer func() { _ = g.Close() }()
	eng := cypher.NewEngine(g)
	const committed = 600
	mustRun3119(t, eng, `UNWIND range(1, 600) AS i CREATE (:N {i: i})`, nil)
	m := g.AdjList().Mapper()

	before := liveHeap()
	issued := cutUntilIssued(t, m, minIssued, func(ctx context.Context) error {
		tx, err := eng.BeginTx(ctx)
		if err != nil {
			return err
		}
		res, err := tx.Exec(`MATCH (a), (b), (c) CREATE ()`, nil)
		if err == nil {
			err = res.Err()
			_ = res.Close()
		}
		if rerr := tx.Rollback(); rerr != nil {
			t.Fatalf("Rollback: %v", rerr)
		}
		return err
	})
	tx, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	res, err := tx.Exec(`UNWIND range(1, 2000) AS i CREATE (:Gone {i: i})`, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	_ = res.Close()
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	after := liveHeap()
	assertReleased(t, eng, g, committed, before, after, uint64(heapSlack)+bytesPerIssuedID*(issued+2000), issued+2000)
}
