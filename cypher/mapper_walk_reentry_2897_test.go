package cypher

// mapper_walk_reentry_2897_test.go — rmp #2897.
//
// [graph.Mapper.Walk] holds each shard's read lock for the whole of that
// shard's iteration, and its contract forbids the callback from re-entering the
// Mapper while a writer may run: once a writer's Intern queues on the shard's
// write lock, sync.RWMutex admits no new reader, so a nested read lock on the
// walked shard blocks forever, and with it the writer and every later operation
// on the shard. rmp #2896 fixed the pattern evaluator; #2897 audited every other
// caller and found three more that re-entered while writers run:
//
//   - the mutator adapters' WalkNodeIDs, whose consumers (the label-less MERGE
//     candidate walk and seedGlobalNodeCounter) resolve every id back through
//     the Mapper inside the callback;
//   - the planner-statistics scan, which resolves every walked node's labels and
//     properties by KEY — a Mapper Lookup on the walked shard;
//   - graphml.WriteWithPropsCtx (covered in graph/io/graphml).
//
// Each test drives the re-entering path while a writer interns fresh nodes, and
// fails on a deadline, with a goroutine dump, if it stops making progress.
//
// Layer: short. goleak-clean on success: every goroutine is joined before
// return. On a deadlock the stuck goroutines cannot be joined; the test fails.

import (
	"bytes"
	"context"
	"fmt"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const (
	reentry2897Run      = 3 * time.Second  // how long readers and the writer overlap
	reentry2897Deadline = 60 * time.Second // no-progress watchdog
	reentry2897Readers  = 4
)

// runWalkReentryStress runs reentry2897Readers goroutines calling read in a
// loop and one goroutine calling write(i) for i = 0, 1, … until write reports
// done, all for reentry2897Run. It fails the test when the set does not finish
// within reentry2897Deadline, attaching a goroutine dump, and when either side
// made no progress (a run that never overlapped proves nothing).
func runWalkReentryStress(t *testing.T, read func() error, write func(i int) (done bool, err error)) {
	t.Helper()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var reads, writes atomic.Uint64
	errs := make(chan error, reentry2897Readers+1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			done, err := write(i)
			if err != nil {
				errs <- fmt.Errorf("writer: %w", err)
				return
			}
			if done {
				return
			}
			writes.Add(1)
		}
	}()
	for range reentry2897Readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := read(); err != nil {
					errs <- fmt.Errorf("reader: %w", err)
					return
				}
				reads.Add(1)
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		time.Sleep(reentry2897Run)
		close(stop)
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(reentry2897Deadline):
		var dump bytes.Buffer
		_ = pprof.Lookup("goroutine").WriteTo(&dump, 1)
		t.Fatalf("no progress within %v after %d reads and %d writes: a Mapper.Walk callback "+
			"re-entered the Mapper and deadlocked against the writer\n%s",
			reentry2897Deadline, reads.Load(), writes.Load(), dump.String())
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if reads.Load() == 0 || writes.Load() == 0 {
		t.Fatalf("vacuous run: %d reads, %d writes", reads.Load(), writes.Load())
	}
	t.Logf("%d reads, %d writes overlapped", reads.Load(), writes.Load())
}

// reentry2897Graph returns a graph of n labelled nodes carrying a property, so
// every walked node costs the re-entering callbacks their full set of lookups.
func reentry2897Graph(t *testing.T, n int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for i := range n {
		k := fmt.Sprintf("n%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode %s: %v", k, err)
		}
		if err := g.SetNodeLabel(k, "L"); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", k, err)
		}
		if err := g.SetNodeProperty(k, "k", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty %s: %v", k, err)
		}
	}
	return g
}

// internFresh is the writer: every AddNode of a never-seen key takes a Mapper
// shard's write lock.
func internFresh(g *lpg.Graph[string, float64], limit int) func(i int) (bool, error) {
	return func(i int) (bool, error) {
		if i >= limit {
			return true, nil
		}
		return false, g.AddNode(fmt.Sprintf("w%d", i))
	}
}

// TestMutatorWalkNodeIDs_CallbackMayReenterMapper drives WalkNodeIDs on both
// mutator adapters with the callback shape its consumers use: resolve the id
// back to its key, and the key back to its id (merge_search.go and
// create_node.go seedGlobalNodeCounter).
func TestMutatorWalkNodeIDs_CallbackMayReenterMapper(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		mk   func(g *lpg.Graph[string, float64]) interface {
			WalkNodeIDs(func(graph.NodeID) bool)
			ResolveNodeLabel(graph.NodeID) (string, bool)
			ResolveNodeID(string) (graph.NodeID, bool)
		}
	}{
		{"lpgMutatorAdapter", func(g *lpg.Graph[string, float64]) interface {
			WalkNodeIDs(func(graph.NodeID) bool)
			ResolveNodeLabel(graph.NodeID) (string, bool)
			ResolveNodeID(string) (graph.NodeID, bool)
		} {
			return &lpgMutatorAdapter{g: g}
		}},
		{"walMutatorAdapter", func(g *lpg.Graph[string, float64]) interface {
			WalkNodeIDs(func(graph.NodeID) bool)
			ResolveNodeLabel(graph.NodeID) (string, bool)
			ResolveNodeID(string) (graph.NodeID, bool)
		} {
			return &walMutatorAdapter{g: g}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := reentry2897Graph(t, 4096)
			m := tc.mk(g)
			runWalkReentryStress(t, func() error {
				var err error
				visited := 0
				m.WalkNodeIDs(func(id graph.NodeID) bool {
					key, ok := m.ResolveNodeLabel(id)
					if !ok {
						err = fmt.Errorf("walked id %d does not resolve", id)
						return false
					}
					back, ok := m.ResolveNodeID(key)
					if !ok || back != id {
						err = fmt.Errorf("key %q of id %d resolves to %d (ok=%v)", key, id, back, ok)
						return false
					}
					visited++
					return true
				})
				if err == nil && visited < 4096 {
					err = fmt.Errorf("walk visited %d nodes, want at least 4096", visited)
				}
				return err
			}, internFresh(g, 400_000))
		})
	}
}

// TestMergeWithoutLabel_ConcurrentCreateDoesNotDeadlock is the end-to-end form:
// a label-less MERGE enumerates candidates through the mutator's WalkNodeIDs
// and resolves each one's key, labels and properties, while a concurrent write
// transaction interns fresh nodes. Concurrent writers are the engine's normal
// mode since rmp #2306.
func TestMergeWithoutLabel_ConcurrentCreateDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	g := reentry2897Graph(t, 4096)
	e := NewEngine(g)
	run := func(q string) error {
		res, err := e.RunInTx(context.Background(), q, nil)
		if err != nil {
			return err
		}
		for res.Next() { // intentional full drain
		}
		if err := res.Err(); err != nil {
			_ = res.Close()
			return err
		}
		return res.Close()
	}
	runWalkReentryStress(t, func() error {
		// n0 already carries k = 0, so the MERGE matches and creates nothing:
		// every iteration is a full candidate walk.
		return run(`MERGE (n {k: 0}) RETURN n`)
	}, func(i int) (bool, error) {
		if i >= 5_000 {
			return true, nil
		}
		return false, run(`UNWIND range(1, 16) AS x CREATE (:W)`)
	})
	var count expr.IntegerValue
	res, err := e.Run(context.Background(), `MATCH (n {k: 0}) RETURN count(n) AS c`, nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	for res.Next() {
		c, ok := res.Record()["c"].(expr.IntegerValue)
		if !ok {
			t.Fatalf("count: unexpected type %T", res.Record()["c"])
		}
		count = c
	}
	if err := res.Close(); err != nil {
		t.Fatalf("count close: %v", err)
	}
	if count != 1 {
		t.Fatalf("MERGE (n {k: 0}) left %d matching nodes, want exactly 1", count)
	}
}

// TestRefreshStatistics_ConcurrentWriterDoesNotDeadlock drives the planner
// statistics scan, which reads every walked node's labels and properties,
// while a writer interns fresh nodes.
func TestRefreshStatistics_ConcurrentWriterDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	g := reentry2897Graph(t, 4096)
	e := NewEngine(g)
	runWalkReentryStress(t, func() error {
		return e.RefreshStatistics(context.Background())
	}, internFresh(g, 400_000))
}
