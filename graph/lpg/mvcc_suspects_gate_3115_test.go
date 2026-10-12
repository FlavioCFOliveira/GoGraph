package lpg

// mvcc_suspects_gate_3115_test.go — rmp #3115: appendSuspects skips a shard
// without its read lock when the shard's lock-free flag says it holds no label
// delta (hasD) or no life record (hasRec). The skip is sound only while a false
// flag is never stale; these tests pin that invariant and the end-to-end result
// it protects.
//
// Layer: short.

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// lockedSuspects is the pre-#3115 appendSuspects: it takes every shard's read
// lock and ignores the per-shard flags. It is the oracle the gated walk must
// reproduce exactly in a quiescent graph.
func lockedSuspects[N comparable, W any](g *Graph[N, W]) []graph.NodeID {
	var out []graph.NodeID
	if g.labelDeltaActive.Load() != 0 {
		for i := range g.nodeLabelShards {
			sh := &g.nodeLabelShards[i]
			sh.mu.RLock()
			for id := range sh.d {
				out = append(out, id)
			}
			sh.mu.RUnlock()
		}
	}
	if g.nodeLifeActive.Load() != 0 {
		for i := range g.nodeLifeShards {
			sh := &g.nodeLifeShards[i]
			sh.mu.RLock()
			for id := range sh.born {
				out = append(out, id)
			}
			for id := range sh.died {
				out = append(out, id)
			}
			sh.mu.RUnlock()
		}
	}
	if g.idxPendingActive.Load() != 0 {
		g.idxDeferred.mu.Lock()
		for k := range g.idxDeferred.pending {
			out = append(out, k.id)
		}
		g.idxDeferred.mu.Unlock()
	}
	slices.Sort(out)
	return out
}

// checkSuspectFlags reports a shard whose map is non-nil while its flag is
// false: the one state in which the gated walk would lose a suspect.
func checkSuspectFlags[N comparable, W any](g *Graph[N, W]) error {
	for i := range g.nodeLabelShards {
		sh := &g.nodeLabelShards[i]
		sh.mu.RLock()
		bad := sh.d != nil && !sh.hasD.Load()
		sh.mu.RUnlock()
		if bad {
			return fmt.Errorf("label shard %d holds a delta map but hasD is false", i)
		}
	}
	for i := range g.nodeLifeShards {
		sh := &g.nodeLifeShards[i]
		sh.mu.RLock()
		bad := (sh.born != nil || sh.died != nil) && !sh.hasRec.Load()
		sh.mu.RUnlock()
		if bad {
			return fmt.Errorf("life shard %d holds a life record map but hasRec is false", i)
		}
	}
	return nil
}

var errAbort3115 = errors.New("deliberate abort")

// TestSuspectGate_FlagsFollowTheSideMapsThroughCommitAbortAndReclaim drives one
// graph through every transition of the two side maps — first delta and first
// record, abort withdrawal, reclaim to empty — and at each step requires the
// gated walk to equal the fully locked oracle and the flags to be exact.
func TestSuspectGate_FlagsFollowTheSideMapsThroughCommitAbortAndReclaim(t *testing.T) {
	g := New[string, float64](adjlist.Config{})
	defer func() { _ = g.Close() }()

	const nodes = 600 // > 2 × propMapShards, so every shard is reached
	names := make([]string, nodes)
	for i := range names {
		names[i] = fmt.Sprintf("n%d", i)
		if err := g.AddNode(names[i]); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	g.ReclaimNow()

	same := func(step string) {
		t.Helper()
		got := g.appendSuspects(nil)
		slices.Sort(got)
		if want := lockedSuspects(g); !slices.Equal(got, want) {
			t.Fatalf("%s: gated walk returned %d suspects, the locked oracle %d", step, len(got), len(want))
		}
		if err := checkSuspectFlags(g); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	same("drained")

	// A pinned reader holds every delta and record written after it.
	pin := g.BeginRead()
	for i := 0; i < nodes; i += 3 {
		if err := g.ApplyAtomicallyTx(func(tx WriteTx) error { return g.Writer(tx).SetNodeLabel(names[i], "L") }); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
	}
	for i := 0; i < 300; i++ {
		if err := g.AddNode(fmt.Sprintf("born%d", i)); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	if g.labelDeltaActive.Load() == 0 || g.nodeLifeActive.Load() == 0 {
		t.Fatal("precondition: no label delta or no life record is live, so a gated walk is untested")
	}
	same("committed deltas under a pinned reader")

	// Aborted writes are withdrawn; their shards may empty and lower the flag.
	for i := 1; i < nodes; i += 7 {
		err := g.ApplyAtomicallyTx(func(tx WriteTx) error {
			if err := g.Writer(tx).SetNodeLabel(names[i], "M"); err != nil {
				return err
			}
			return errAbort3115
		})
		if !errors.Is(err, errAbort3115) {
			t.Fatalf("aborted tx returned %v", err)
		}
	}
	same("after aborts")

	g.EndRead(pin)
	g.ReclaimNow()
	same("reclaimed")
	for i := range g.nodeLabelShards {
		if sh := &g.nodeLabelShards[i]; sh.d == nil && sh.hasD.Load() {
			t.Fatalf("label shard %d: d is nil after reclaim but hasD is still raised; the gate never re-closes", i)
		}
	}
}

// TestSuspectGate_ConcurrentWritersAbortsAndVacuumLoseNoSuspect runs committing
// and aborting writers, the reclaimer, a flag checker, and readers together.
// Every reader compares the corrected label bitmap at its pinned snapshot with
// the per-node truth at the same snapshot: a suspect the gate skipped wrongly
// leaves an uncommitted or withdrawn member in the bitmap, or drops a member.
func TestSuspectGate_ConcurrentWritersAbortsAndVacuumLoseNoSuspect(t *testing.T) {
	g := New[string, float64](adjlist.Config{})
	defer func() { _ = g.Close() }()

	const (
		nodes   = 512
		writers = 4
		opsPer  = 150
		readers = 2
	)
	names := make([]string, nodes)
	ids := make([]graph.NodeID, nodes)
	for i := range names {
		names[i] = fmt.Sprintf("c%d", i)
		if err := g.AddNode(names[i]); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		id, ok := g.adj.Mapper().Lookup(names[i])
		if !ok {
			t.Fatalf("node %s not found", names[i])
		}
		ids[i] = id
	}
	lid := g.reg.intern("L")

	var (
		wg, side sync.WaitGroup
		stop     atomic.Bool
		failMu   sync.Mutex
		failure  error
		compared atomic.Int64 // reader comparisons made while history was live
	)
	fail := func(err error) {
		failMu.Lock()
		if failure == nil {
			failure = err
		}
		failMu.Unlock()
		stop.Store(true)
	}

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for k := 0; k < opsPer && !stop.Load(); k++ {
				name := names[(w*131+k*17)%nodes]
				_ = g.ApplyAtomicallyTx(func(tx WriteTx) error {
					var err error
					if k%2 == 0 {
						err = g.Writer(tx).SetNodeLabel(name, "L")
					} else {
						err = g.Writer(tx).RemoveNodeLabel(name, "L")
					}
					if err != nil {
						return err
					}
					if k%3 == 0 {
						return errAbort3115
					}
					return nil
				})
			}
		}(w)
	}
	side.Add(2 + readers)
	go func() { // vacuum
		defer side.Done()
		for !stop.Load() {
			g.ReclaimNow()
		}
	}()
	go func() { // flag invariant, checked under each shard's lock
		defer side.Done()
		for !stop.Load() {
			if err := checkSuspectFlags(g); err != nil {
				fail(err)
			}
		}
	}()
	has := func(bag labelBag) bool { return bag.has(lid) }
	for r := 0; r < readers; r++ {
		go func() {
			defer side.Done()
			for !stop.Load() {
				s := g.BeginRead()
				live := g.labelDeltaActive.Load() != 0 || g.nodeLifeActive.Load() != 0
				bm := g.LabelBitmapAsOf(lid, s)
				for _, id := range ids {
					want := g.labelBagTest(id, s, has) && g.NodeExistsAsOf(id, s)
					if got := bm.Contains(uint64(id)); got != want {
						fail(fmt.Errorf("node %d at snapshot: bitmap member=%v, per-node truth=%v", id, got, want))
						break
					}
				}
				g.EndRead(s)
				if live {
					compared.Add(1)
				}
			}
		}()
	}

	wg.Wait()
	stop.Store(true)
	side.Wait()
	if failure != nil {
		t.Fatal(failure)
	}
	if compared.Load() == 0 {
		t.Fatal("no reader comparison ran while history was live: the gate was never exercised")
	}
	g.ReclaimNow()
	if err := checkSuspectFlags(g); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d reader comparisons with live history", compared.Load())
}
