package snapshot

import (
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestSnapshotCollectors_NoMapperReentryDeadlock is the #1648 regression gate.
//
// The four snapshot collectors (collectNodeLabelRecords, collectEdgeLabelRecords,
// collectNodePropertyRecords, collectEdgePropertyRecords) used to re-enter the
// graph Mapper from inside Mapper.Walk's callback — via NodeLabels/NodeProperties
// (Lookup) and Resolve. The Mapper holds a shard RLock across the Walk callback,
// and its documented contract (graph/mapper.go:337-345) forbids re-entry while a
// writer may run: once a writer's internSlow queues on a shard's write lock,
// sync.RWMutex admits no new readers (writer anti-starvation), so the nested
// RLock deadlocks the callback, the writer, and every future operation on the
// shard.
//
// The non-blocking checkpoint runs these collectors in its lock-free phase 2
// (store/checkpoint/checkpoint.go), holding neither the commit lock nor
// Graph.View, so a concurrent committer that interns a brand-new node/label/
// property key on a shard a collector is walking triggers the deadlock — observed
// in example 17 at 50k accounts / 1M transfers (audit 2026-06-21, reproduced
// 3/3). The pre-existing checkpoint stall test commits a node with no labels or
// properties, so its collector callbacks return immediately and never force the
// window.
//
// This test forces the same collision deterministically and densely: collector
// goroutines repeatedly serialise the labels and properties of a populated graph
// while writer goroutines hammer fresh interns across every Mapper shard. Under
// the buggy code the collectors deadlock and the watchdog fires; under the fix
// (collect bare NodeIDs inside Walk, resolve via the lock-free *ByID accessors
// after Walk returns) the collectors complete promptly. Runs under -race in CI.
func TestSnapshotCollectors_NoMapperReentryDeadlock(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})

	// Populate enough nodes to spread across the Mapper's 256 shards, each
	// carrying a label and a property, with a labelled+propertied edge to its
	// successor so all four collectors do real per-node work in their callbacks
	// (every callback re-enters the Mapper under the buggy code).
	const seedNodes = 4096
	for i := 0; i < seedNodes; i++ {
		n := "n" + strconv.Itoa(i)
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%s): %v", n, err)
		}
		if err := g.SetNodeLabel(n, "Seed"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(n, "idx", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	for i := 0; i+1 < seedNodes; i++ {
		src, dst := "n"+strconv.Itoa(i), "n"+strconv.Itoa(i+1)
		if err := g.AddEdgeLabeledWithProperty(src, dst, 1, "NEXT", "w", lpg.Int64Value(1)); err != nil {
			t.Fatalf("AddEdgeLabeledWithProperty: %v", err)
		}
	}

	const collectors = 3
	const iterations = 30
	stop := make(chan struct{})
	done := make(chan struct{})

	var collectorWG sync.WaitGroup
	collectorWG.Add(collectors)
	for c := 0; c < collectors; c++ {
		go func() {
			defer collectorWG.Done()
			for it := 0; it < iterations; it++ {
				if _, _, err := WriteLabels(io.Discard, g); err != nil {
					t.Errorf("WriteLabels: %v", err)
					return
				}
				if _, _, err := WriteProperties(io.Discard, g); err != nil {
					t.Errorf("WriteProperties: %v", err)
					return
				}
			}
		}()
	}
	go func() {
		collectorWG.Wait()
		close(done)
	}()

	// Writer goroutines: intern brand-new node keys, forcing internSlow
	// write-locks across the very shards the collectors walk. A queued write lock
	// on a walked shard is what arms the RWMutex anti-starvation that deadlocks
	// the buggy re-entrant callback.
	//
	// The fresh-intern budget is bounded: it is large enough to densely cover all
	// 256 Mapper shards many times over — so a collector mid-Walk reliably
	// overlaps a queued internSlow and the buggy code deadlocks within the first
	// ~1k interns — but bounded so the graph stays small and the fixed collectors
	// finish promptly instead of chasing an unbounded, ever-growing node set.
	const maxFreshInterns = 60000
	var writers sync.WaitGroup
	var interned int64
	for w := 0; w < 3; w++ {
		writers.Add(1)
		go func(base int) {
			defer writers.Done()
			i := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				if atomic.AddInt64(&interned, 1) > maxFreshInterns {
					return
				}
				_ = g.AddNode("w" + strconv.Itoa(base) + "_" + strconv.Itoa(i))
				i++
			}
		}(w)
	}

	// Watchdog generous relative to the ~4s fixed-code runtime under -race, so a
	// pathologically slow or contended CI runner cannot false-fail; a genuine
	// re-entry regression hangs forever and still surfaces here.
	const watchdog = 45 * time.Second
	select {
	case <-done:
	case <-time.After(watchdog):
		close(stop)
		t.Fatalf("snapshot collectors deadlocked: did not finish %d collector runs within %v "+
			"(interned %d fresh keys) — Mapper.Walk re-entry regression (#1648)",
			collectors*iterations, watchdog, atomic.LoadInt64(&interned))
	}
	close(stop)
	writers.Wait()
}
