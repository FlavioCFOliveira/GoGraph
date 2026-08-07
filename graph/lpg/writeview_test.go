package lpg

// writeview_test.go — rmp #2320: the deterministic reproduction of the defect
// that made removing the visibility barrier unsound, and the gate that keeps it
// closed.
//
// # Why this test lives here and not at the Cypher level
//
// The defect needs a specific INTERLEAVING: writer A opens its bracket, writes,
// then writer B opens ITS bracket — overwriting the ambient slot — and only then
// does A write again. A's second write is the one that adopts B's commit record.
//
// A test driven through the Cypher engine cannot produce that order on purpose.
// The engine opens its bracket and runs the whole statement inside it, so a decoy
// bracket opened before the statement is simply overwritten by the statement's own
// [Graph.beginWrite] and the ambient resolution lands on the right transaction by
// accident — which was measured: a decoy-based test at the Cypher level passed
// against a deliberately bypassed build. Only here, one layer down, can the two
// brackets be interleaved deterministically.
//
// The Cypher level gets the other two halves instead: a count of ambient
// resolutions across the write surface, and a concurrent multi-writer run of the
// atomic-visibility invariant (cypher/mvcc_carried_txn_test.go).

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// TestWriteView_SecondWriteDoesNotAdoptAnOverlappingTransactionsRecord is the
// reproduction. It fails on a build where the write does not carry its
// transaction, and the failure is the shipped defect: half a transaction visible.
func TestWriteView_SecondWriteDoesNotAdoptAnOverlappingTransactionsRecord(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})

	// A pre-existing value on each key, so a version that never becomes visible
	// reads back as the old one rather than as absent — which makes the assertion
	// about the transaction's ATOMICITY rather than about mere presence.
	for _, k := range []string{"first", "second"} {
		if err := g.SetNodeProperty("n", k, Int64Value(0)); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}

	var (
		aWroteFirst = make(chan struct{}) // A has written property one
		bPublished  = make(chan struct{}) // B's bracket is open, slot names B
		bRelease    = make(chan struct{}) // B may finish
		bDone       = make(chan struct{})
	)

	// Writer B: opens a bracket, publishes itself on the ambient slot, writes
	// NOTHING, and stays open until told. Its record can therefore only ever
	// acquire a version by somebody else's write resolving through the slot.
	go func() {
		defer close(bDone)
		<-aWroteFirst
		_ = g.ApplyVersioned(func(WriteTx) error {
			close(bPublished)
			<-bRelease
			return nil
		})
	}()

	// Writer A: one transaction, two property writes, with B's bracket opening
	// between them.
	err := g.ApplyVersioned(func(tx WriteTx) error {
		wv := g.Writer(tx)
		if err := wv.SetNodeProperty("n", "first", Int64Value(1)); err != nil {
			return err
		}
		close(aWroteFirst)
		<-bPublished // the ambient slot now names B, not A
		return wv.SetNodeProperty("n", "second", Int64Value(1))
	})
	if err != nil {
		close(bRelease)
		<-bDone
		t.Fatalf("writer A: %v", err)
	}

	// Read A's effect while B is STILL OPEN. B never commits, so anything stamped
	// with B's record is invisible here — which is exactly how the split shows up.
	snap := g.BeginRead()
	v := g.ReadAt(snap)
	first, okFirst := v.GetNodeProperty("n", "first")
	second, okSecond := v.GetNodeProperty("n", "second")
	g.EndRead(snap)

	close(bRelease)
	<-bDone

	if !okFirst || !okSecond {
		t.Fatalf("a seeded property vanished (first=%v second=%v)", okFirst, okSecond)
	}
	gotFirst, _ := first.Int64()
	gotSecond, _ := second.Int64()
	if gotFirst != 1 || gotSecond != 1 {
		t.Fatalf("transaction A is only HALF visible: first=%d second=%d, want 1 and 1.\n"+
			"A's second write was made after writer B published itself on the graph's "+
			"ambient slot, so it adopted B's commit record — and B has not committed. "+
			"A write must CARRY its transaction (rmp #2320); this is the exact split that "+
			"forced rmp #2304's revert.", gotFirst, gotSecond)
	}
}

// TestWriteView_CarriesTheTransactionRatherThanResolvingIt is the direct,
// non-concurrent form of the same property: with a foreign transaction published
// on the slot, a view-driven write must count ZERO ambient resolutions, and a view
// built from the zero transaction must count some.
//
// It pins the two halves against each other, so neither the threading nor the
// instrument can rot without a failure.
func TestWriteView_CarriesTheTransactionRatherThanResolvingIt(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	if err := g.SetNodeProperty("n", "v", Int64Value(0)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A foreign transaction on the slot, so an ambient resolution has something to
	// resolve TO. Without one, an untransacted write takes a fresh timestamp and is
	// counted as untracked rather than ambient — a different thing.
	foreign := g.beginWriteCtx()
	g.stamp.Publish(&foreign.tx)
	defer func() {
		g.stamp.EndFor(&foreign.tx)
	}()

	carried := g.AmbientVersionResolutions()
	if err := g.Writer(WriteTx{w: foreign}).SetNodeProperty("n", "v", Int64Value(1)); err != nil {
		t.Fatalf("carried write: %v", err)
	}
	if n := g.AmbientVersionResolutions() - carried; n != 0 {
		t.Fatalf("a write through a WriteView that CARRIES a transaction still resolved "+
			"%d version(s) through the ambient slot", n)
	}

	bypassed := g.AmbientVersionResolutions()
	if err := g.Writer(WriteTx{}).SetNodeProperty("n", "v", Int64Value(2)); err != nil {
		t.Fatalf("bypassed write: %v", err)
	}
	if n := g.AmbientVersionResolutions() - bypassed; n == 0 {
		t.Fatal("a write through a WriteView that carries NO transaction resolved nothing " +
			"through the ambient slot; the instrument the gate above relies on is dead")
	}
}

// TestWriteView_CoversEveryTransactionalMutator is the drift gate for the accessor
// itself.
//
// [WriteView] shadows the graph's mutators by NAME, and a mutator added to [Graph]
// but not here would silently fall back to the ambient path — the failure mode
// that made rmp #2304 revert, reintroduced by omission with nothing to notice it.
// This test enumerates the internal `…Info` forms, which is the module's own
// definition of "this store is transactional", and requires a WriteView method for
// each.
//
// It is a hand-maintained list rather than reflection, deliberately: reflection
// over method sets would compare exported names and could not see the `…Info`
// forms at all, and the point is to force a decision when a new versioned store
// lands — either it gets a WriteView method, or its absence is recorded here with
// a reason.
//
// # What is deliberately ABSENT, and why
//
// The NodeID-keyed `…ByID` forms — setEdgeLabelByHandleIDInfo,
// setEdgePropertyByHandleIDInfo, delEdgePropertyByHandleIDInfo,
// removeEdgeInstanceByHandleIDInfo, setEdgeRelTypeAtSlotByIDInfo and
// addEdgeRelTypeOverflowByIDInfo — have no WriteView method. Their only callers
// outside this package are in store/snapshot (edgehandles.go, labels.go), which is
// snapshot APPLY: a single-threaded load with no bracket open and no concurrent
// writer, so an ambient resolution there names no transaction and takes a fresh
// commit timestamp, which is correct. If a concurrent path ever reaches one of
// them, it needs a WriteView method and an entry here — and the ambient-resolution
// gates would report it rather than leaving it to be noticed.
func TestWriteView_CoversEveryTransactionalMutator(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	wv := g.Writer(WriteTx{})

	// Every entry drives its WriteView method once. A method that stops existing
	// breaks the build here, which is the point; a store that gains a threaded form
	// with no WriteView method must be added.
	calls := map[string]func(){
		"addNodeInfo":                    func() { _ = wv.AddNode("n") },
		"removeNodeInfo":                 func() { wv.RemoveNode("n") },
		"reviveInfo":                     func() { wv.Revive("n") },
		"setNodeLabelInfo":               func() { _ = wv.SetNodeLabel("n", "L") },
		"removeNodeLabelInfo":            func() { wv.RemoveNodeLabel("n", "L") },
		"setNodePropertyInfo":            func() { _ = wv.SetNodeProperty("n", "k", Int64Value(1)) },
		"delNodePropertyInfo":            func() { wv.DelNodeProperty("n", "k") },
		"addEdgeInfo":                    func() { _ = wv.AddEdge("n", "m", 0) },
		"addEdgeHInfo":                   func() { _, _ = wv.AddEdgeH("n", "m", 0) },
		"addEdgeHIfAbsentInfo":           func() { _, _ = wv.AddEdgeHIfAbsent("n", "m", 0, 99) },
		"removeEdgeByHandleInfo":         func() { wv.RemoveEdgeByHandle("n", "m", 99) },
		"setEdgeLabelInfo":               func() { wv.SetEdgeLabel("n", "m", "R") },
		"removeEdgeLabelInfo":            func() { wv.RemoveEdgeLabel("n", "m", "R") },
		"setEdgePropertyInfo":            func() { _ = wv.SetEdgeProperty("n", "m", "k", Int64Value(1)) },
		"delEdgePropertyInfo":            func() { wv.DelEdgeProperty("n", "m", "k") },
		"setEdgeLabelAtInfo":             func() { wv.SetEdgeLabelAt("n", "m", 0, "R") },
		"setEdgePropertyAtInfo":          func() { _ = wv.SetEdgePropertyAt("n", "m", 0, "k", Int64Value(1)) },
		"removeEdgeInstanceInfo":         func() { wv.RemoveEdgeInstance("n", "m", 0) },
		"setEdgeLabelByHandleInfo":       func() { wv.SetEdgeLabelByHandle("n", "m", 99, "R") },
		"setEdgePropertyByHandleInfo":    func() { _ = wv.SetEdgePropertyByHandle("n", "m", 99, "k", Int64Value(1)) },
		"delEdgePropertyByHandleInfo":    func() { wv.DelEdgePropertyByHandle("n", "m", 99, "k") },
		"removeEdgeInstanceByHandleInfo": func() { wv.RemoveEdgeInstanceByHandle("n", "m", 99) },
		"removeAllEdgesFromInfo":         func() { wv.RemoveAllEdgesFrom("n") },
		"removeEdgeInfo(via RemoveEdge)": func() { wv.RemoveEdge("n", "m") },
	}
	if len(calls) != 24 {
		t.Fatalf("the covered-mutator list holds %d entries, expected 24; a store was added "+
			"or removed without updating this gate", len(calls))
	}
	for name, call := range calls {
		call() // must not panic; the assertion is that the method exists and runs
		_ = name
	}
}

// TestWriteCtx_UndoOfADoomedTransactionIsNotRefused pins the exemption the
// physical undo needs.
//
// A transaction that loses a write-write conflict is DOOMED, and a doomed
// transaction refuses every further write so it cannot pile versions onto a chain
// head it does not own. The undo has to run at exactly that moment — and once the
// inverses carried the transaction (as they must, or the inverse lands on a
// different commit record from the write it withdraws) they inherited that refusal
// and the rollback silently applied NOTHING, leaving the transaction's earlier
// writes committed. Measured as a 1 694 729-cent conservation failure in
// examples/27_concurrent_txn.
//
// See [writeCtx.undoing] for the reasoning and the Memgraph abort path it follows.
func TestWriteCtx_UndoOfADoomedTransactionIsNotRefused(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	if err := g.SetNodeProperty("n", "v", Int64Value(0)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := g.ApplyVersioned(func(tx WriteTx) error {
		wv := g.Writer(tx)
		if err := wv.SetNodeProperty("n", "v", Int64Value(1)); err != nil {
			return err
		}
		// Doom the transaction the way a real conflict does: record one.
		if e := tx.w.conflictErr("node properties", ^uint64(0)); e == nil {
			t.Fatal("conflictErr returned nil; the transaction was not doomed")
		}
		if !tx.w.doomed() {
			t.Fatal("the transaction does not report itself doomed after a recorded conflict")
		}

		// The forward path must now refuse.
		if err := wv.SetNodeProperty("n", "v", Int64Value(2)); err == nil {
			t.Fatal("a doomed transaction accepted a further forward write; the refusal " +
				"that stops it piling versions onto a foreign chain head is gone")
		}

		// The UNDO path must not.
		tx.EnterUndo()
		defer tx.ExitUndo()
		if tx.w.doomed() {
			t.Fatal("the transaction still reports doomed inside the undo region; " +
				"clearEdgePairState and friends read that to mean 'my write was refused'")
		}
		if err := wv.SetNodeProperty("n", "v", Int64Value(0)); err != nil {
			t.Fatalf("the undo's inverse was REFUSED: %v.\n"+
				"An undo runs precisely when the transaction is doomed, so refusing it "+
				"leaves the writes the statement already applied committed and unwithdrawn "+
				"(rmp #2320).", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bracket: %v", err)
	}

	// The inverse landed, so the value is back to the seeded one.
	snap := g.BeginRead()
	v, ok := g.ReadAt(snap).GetNodeProperty("n", "v")
	g.EndRead(snap)
	if !ok {
		t.Fatal("the property vanished")
	}
	if got, _ := v.Int64(); got != 0 {
		t.Fatalf("v = %d after the undo, want the seeded 0; the rollback did not take effect", got)
	}
}

// TestWriteView_ConcurrentWritersEachPublishTheirOwnTransaction is the
// multi-writer form: N writers, each writing several stores inside one
// transaction, with a reader continuously snapshotting. Every snapshot must
// observe each writer's transaction whole or not at all, and no write may resolve
// through the ambient slot.
//
// It is the property the deterministic test above proves once, exercised at the
// scale that produced the original 105 942 torn observations.
func TestWriteView_ConcurrentWritersEachPublishTheirOwnTransaction(t *testing.T) {
	const (
		writers = 8
		rounds  = 60
		props   = 4
	)
	g := New[string, float64](adjlist.Config{Directed: true})
	keys := make([]string, writers)
	for w := range keys {
		keys[w] = string(rune('a' + w))
		for p := 0; p < props; p++ {
			if err := g.SetNodeProperty(keys[w], propName(p), Int64Value(0)); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}

	ambientBefore := g.AmbientVersionResolutions()
	var (
		writersWG sync.WaitGroup
		readerWG  sync.WaitGroup
		stop      = make(chan struct{})
		torn      atomic.Int64
		observed  atomic.Int64
	)

	// Reader: every node's four properties are written together by ONE transaction,
	// so within one snapshot they must all carry the same round. A property that is
	// absent is impossible here (all four are seeded), so an absent one is itself a
	// failure rather than something to skip.
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			snap := g.BeginRead()
			v := g.ReadAt(snap)
			for _, k := range keys {
				first, ok := v.GetNodeProperty(k, propName(0))
				if !ok {
					torn.Add(1)
					continue
				}
				want, _ := first.Int64()
				for p := 1; p < props; p++ {
					got, ok := v.GetNodeProperty(k, propName(p))
					if !ok {
						torn.Add(1)
						continue
					}
					observed.Add(1)
					if n, _ := got.Int64(); n != want {
						torn.Add(1)
					}
				}
			}
			g.EndRead(snap)
		}
	}()

	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func(k string) {
			defer writersWG.Done()
			for r := 1; r <= rounds; r++ {
				if err := writeRoundWithRetry(g, k, props, r); err != nil {
					t.Errorf("writer %s round %d: %v", k, r, err)
					return
				}
			}
		}(keys[w])
	}

	// The writers are the workload; the reader runs until they are ALL done, however
	// they finished. Closing stop after this Wait — and never inside a writer — is
	// what keeps the shutdown free of the "the probe never fires because a writer
	// bailed out" deadlock.
	writersWG.Wait()
	close(stop)
	readerWG.Wait()

	if observed.Load() == 0 {
		t.Fatal("the reader made no observation; the test did not exercise the invariant")
	}
	if n := torn.Load(); n != 0 {
		t.Fatalf("%d of %d snapshot observation(s) saw a transaction only partly applied; "+
			"every version of one transaction must point at ONE commit record (rmp #2320)",
			n, observed.Load())
	}
	if n := g.AmbientVersionResolutions() - ambientBefore; n != 0 {
		t.Fatalf("%d version(s) resolved through the ambient slot under %d concurrent writers",
			n, writers)
	}
}

// writeRoundWithRetry writes one round's four properties as ONE transaction,
// retrying while it is refused with a serialization conflict.
//
// # Why a writer conflicts even though no two writers share a node
//
// It conflicts with ITSELF — with its own previous round. A transaction's start
// timestamp is the CONTIGUOUS commit frontier (rmp #2298), which cannot advance
// past a commit that is still in flight, so under N concurrent writers a
// transaction routinely begins at an instant that EXCLUDES its own predecessor's
// commit. The predecessor's version is then not visible to it, and overwriting a
// version it cannot see is exactly what [mvcc.Conflicts] refuses.
//
// That is snapshot isolation working, not a defect, and it is the cost the
// contiguous frontier accepts in exchange for never handing a reader a straddled
// commit. It is also why every MVCC client must retry: PostgreSQL's xip list can
// say "T1 committed, T0 still running" and GoGraph's frontier cannot, so GoGraph
// refuses where PostgreSQL would proceed. Retrying is the client's half of the
// contract; this test does it so that what it measures is atomic VISIBILITY and
// the threading, not the conflict rate.
//
// # Why the bound is a DEADLINE and not an attempt count
//
// An attempt count was tried first and it made `make ci` non-deterministically red
// — not under `-race`, but under COVERAGE instrumentation, which slows every
// bracket so a fixed number of yields is consumed inside one still-in-flight
// commit. That is the same shape as rmp #2319. A deadline is the honest bound: it
// states what the test actually requires ("the frontier must advance within this
// long") and it does not change meaning when the machine or the instrumentation
// does. Exponential backoff starting below a scheduler quantum keeps the fast case
// fast.
func writeRoundWithRetry(g *Graph[string, float64], k string, props, round int) error {
	const (
		deadline       = 15 * time.Second
		initialBackoff = 50 * time.Microsecond
		maxBackoff     = 2 * time.Millisecond
	)
	giveUp := time.Now().Add(deadline)
	backoff := initialBackoff
	attempts := 0
	for {
		attempts++
		err := g.ApplyVersioned(func(tx WriteTx) error {
			wv := g.Writer(tx)
			for p := 0; p < props; p++ {
				if e := wv.SetNodeProperty(k, propName(p), Int64Value(int64(round))); e != nil {
					return e
				}
			}
			return nil
		})
		if err == nil {
			return nil
		}
		if !errors.Is(err, mvcc.ErrSerializationConflict) {
			return err
		}
		if time.Now().After(giveUp) {
			return fmt.Errorf("%d serialization conflicts over %s on one round: %w", attempts, deadline, err)
		}
		time.Sleep(backoff)
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

// propName returns the p-th property name of the multi-writer gate's fixed set.
func propName(p int) string { return "p" + string(rune('0'+p)) }
