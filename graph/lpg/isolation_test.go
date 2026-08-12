package lpg

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// TestIsolation_CrossSubstructure_EdgeImpliesLabels proves the barrier flips a
// transaction's writes across DIFFERENT substructures (adjacency + node labels)
// atomically. Each transaction toggles between two consistent states —
// {edge u→v present, u:Hot, v:Hot} and {no edge, no labels} — so the invariant
// "HasEdge(u,v) ⇔ HasNodeLabel(u,Hot) ⇔ HasNodeLabel(v,Hot)" must hold on every
// pinned read. A reader observing the edge without a label (or vice versa)
// would have seen a partial transaction across substructures. Run under -race.
func TestIsolation_CrossSubstructure_EdgeImpliesLabels(t *testing.T) {
	t.Parallel()

	g := New[string, int64](adjlist.Config{Directed: true})
	// Intern u, v up front so the toggling only adds/removes the edge + labels.
	if err := g.AddNode("u"); err != nil {
		t.Fatalf("AddNode u: %v", err)
	}
	if err := g.AddNode("v"); err != nil {
		t.Fatalf("AddNode v: %v", err)
	}

	const (
		toggles = 40000
		readers = 8
	)
	var (
		wg        sync.WaitGroup
		done      atomic.Bool
		violation atomic.Int64
		reads     atomic.Int64
		// The first violating observation, so a failure names which pair
		// disagreed rather than only how many did (rmp #2378).
		firstSeen atomic.Bool
		firstEdge atomic.Bool
		firstLU   atomic.Bool
		firstLV   atomic.Bool
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer done.Store(true)
		present := false
		for i := 0; i < toggles; i++ {
			want := !present
			// THE WRITES MUST SHARE ONE TRANSACTION RECORD, or they are not
			// atomically visible to a snapshot reader — which is the whole
			// subject of this test (rmp #2378).
			//
			// This used to call the bare autocommit methods inside
			// [Graph.ApplyAtomically]. That bracket holds visGate exclusively, so
			// it excludes other WRITERS, but each bare call passes a nil
			// transaction and [Graph.deltaStamp] answers a nil record with
			// `g.stamp.Stamp()` — A FRESH COMMIT INSTANT PER WRITE. The edge
			// therefore committed at one instant and each label at a later one,
			// and a reader whose startTS landed between them saw the edge without
			// the labels. Measured under the gate's own environment (a full
			// `go test -race ./...` as peer load): 5 violating runs in 40, with
			// the signature `edge=true label(u)=false label(v)=false` — the ADD
			// path, since the remove path takes the edge down first.
			//
			// A shared record makes deltaStamp return that record for every write,
			// so all three land at one instant. The module states the requirement
			// itself, on [Graph.ApplyInsideLockedTx]: "the statement must share the
			// record the enclosing LockBarrier opened, or the explicit transaction
			// is not atomically visible."
			//
			// THREADING ONE [WriteTx] IS NECESSARY AND NOT SUFFICIENT — the tear
			// SURVIVES it, and rmp #2378 stays open.
			//
			// Measured under the same recipe with the writes threaded as below:
			// 4 failing runs in 100, in BOTH pairings and BOTH directions —
			// `label(u) != label(v)`, `edge=false label(u)=true label(v)=true`, and
			// `edge=true label(u)=false label(v)=false`. So the fix above removes
			// the three-separate-instants cause and something else remains.
			//
			// TWO DRAFTS OF THIS COMMENT WERE WRONG, both from too few samples:
			// "0 violations in 60 runs" (from the first 20), then "the edge-vs-label
			// signature is GONE" (from 3 samples that happened to be label-vs-label).
			// Do not restate a signature from fewer than ~100 runs at this rate.
			//
			// What the instrumentation EXCLUDED: at a violation both nodes had live
			// delta chains (`sh.d != nil` and `sh.d[id] != nil` for u and v, in
			// different shards), so labelBagAsOfLocked's per-node present-time
			// fallback is NOT the mechanism, and neither is the reclaim dropping a
			// chain that was still needed.
			_ = g.ApplyAtomicallyTx(func(tx WriteTx) error {
				wv := g.Writer(tx)
				if want {
					_ = wv.AddEdge("u", "v", 0)
					_ = wv.SetNodeLabel("u", "Hot")
					_ = wv.SetNodeLabel("v", "Hot")
				} else {
					wv.RemoveEdge("u", "v")
					wv.RemoveNodeLabel("u", "Hot")
					wv.RemoveNodeLabel("v", "Hot")
				}
				return nil
			})
			present = want
		}
	}()

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !done.Load() {
				// A SNAPSHOT, which since rmp #2344 is the only thing that
				// provides atomic visibility: Graph.View is gone, and it never
				// gave this guarantee anyway once ordinary writes held the
				// barrier shared. Every read below resolves at one instant.
				func() {
					snap := g.BeginRead()
					defer g.EndRead(snap)
					view := g.ReadAt(snap)
					_, e := g.EdgeWeightAsOf("u", "v", snap)
					lu := view.HasNodeLabel("u", "Hot")
					lv := view.HasNodeLabel("v", "Hot")
					reads.Add(1)
					if e != lu || e != lv {
						violation.Add(1)
						// WHICH PAIR disagreed, and what was actually seen (rmp #2378).
						//
						// This fired once under `make ci` on 2026-08-09 and has not
						// reproduced in 24 attempts since, so the next occurrence may be
						// the only other one for a long time and it must arrive
						// diagnostic rather than as a bare count. The old message named
						// "edge/label" for every case, but `e != lu || e != lv` also trips
						// when the two LABEL reads disagree with EACH OTHER — u and v can
						// hash to different label shards, which is a materially different
						// suspect from edge-versus-label.
						//
						// Only the FIRST observation is kept, via CompareAndSwap: it is the
						// one whose interleaving is least perturbed by the recording.
						if firstSeen.CompareAndSwap(false, true) {
							firstEdge.Store(e)
							firstLU.Store(lu)
							firstLV.Store(lv)
						}
					}
				}()
			}
		}()
	}

	wg.Wait()
	if v := violation.Load(); v != 0 {
		e, lu, lv := firstEdge.Load(), firstLU.Load(), firstLV.Load()
		which := "edge disagrees with both labels"
		switch {
		case lu != lv:
			which = "THE TWO LABELS DISAGREE WITH EACH OTHER (u and v may be in different label shards)"
		case e != lu:
			which = "the edge disagrees with the labels, which agree with each other"
		}
		t.Fatalf("observed %d cross-substructure violations inside a pinned SNAPSHOT "+
			"(rmp #2378). First observation: edge=%v label(u)=%v label(v)=%v — %s. "+
			"%d reads were taken",
			v, e, lu, lv, which, reads.Load())
	}
	if reads.Load() == 0 {
		t.Fatal("readers never read; test did not exercise the invariant")
	}
}

// TestIsolation_ApplyAtomically_View_NoPartialReads stress-tests the F3
// transaction-visibility barrier (docs/isolation-design.md) directly on the
// lpg mechanism, with no WAL/I/O so it can run many iterations.
//
// A writer repeatedly sets node "a".v and node "b".v to the SAME value
// inside one ApplyAtomically call. Readers inside View read both and assert
// equality. The barrier guarantees a reader observes either none or all of a
// transaction's writes, so a.v == b.v must hold on every pinned read; a
// partial transaction (new a.v, old b.v) would trip the counter. Run under
// -race (the per-shard locks already prevent data races, so the gap proven
// closed here is the logical partial-transaction visibility).
func TestIsolation_ApplyAtomically_View_NoPartialReads(t *testing.T) {
	t.Parallel()

	g := New[string, int64](adjlist.Config{Directed: true})

	// Seed both nodes.
	if err := g.ApplyAtomicallyTx(func(tx WriteTx) error {
		wv := g.Writer(tx)
		if err := wv.SetNodeProperty("a", "v", Int64Value(0)); err != nil {
			return err
		}
		return wv.SetNodeProperty("b", "v", Int64Value(0))
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const (
		iterations = 50000
		readers    = 8
	)
	var (
		wg        sync.WaitGroup
		done      atomic.Bool
		violation atomic.Int64
		reads     atomic.Int64
		writeErr  atomic.Int64
		// The first violating observation, so a failure names WHAT was seen
		// rather than only how many times. The sibling oracle
		// TestIsolation_CrossSubstructure_EdgeImpliesLabels was given this
		// treatment at rmp #2378; this one was not, and when it fired it could
		// say nothing about its own cause.
		//
		// The captured values are self-describing: the writer sets BOTH
		// properties to the loop counter, so each value IS the iteration that
		// wrote it and their difference is how many transactions apart the two
		// reads landed. A difference of one is a single transaction observed
		// half-applied; anything larger is a different suspect.
		firstSeen atomic.Bool
		firstIA   atomic.Int64
		firstIB   atomic.Int64
		// The same two properties re-read from the SAME pinned snapshot straight
		// after the violation, plus that snapshot's start instant.
		firstReA     atomic.Int64
		firstReB     atomic.Int64
		firstStartTS atomic.Uint64
		// The per-shard substrate state, sampled AT the violation. This is the
		// question the values alone cannot answer: a snapshot read falls through to
		// the CURRENT bag when the shard holds no history (snapshot_read.go, the
		// `sh.d != nil` gate) or when this node has no chain (propBagAsOfLocked),
		// and a present-time read is indistinguishable from a correct one until you
		// look. The previous cycle established for LABELS that both nodes held live
		// chains at the violation; nothing has ever established it for PROPERTIES,
		// which is this arm.
		firstAFacts atomic.Value // string
		firstBFacts atomic.Value // string
		firstShared atomic.Value // string
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer done.Store(true)
		for i := int64(1); i <= iterations; i++ {
			// One shared transaction record, for the reason spelled out in
			// TestIsolation_CrossSubstructure_EdgeImpliesLabels: the bare
			// autocommit writes take a FRESH commit instant each
			// ([Graph.deltaStamp] on a nil record), so a snapshot reader can land
			// between a.v and b.v. This test was written for the Graph.View era
			// and its READER was migrated to snapshots (rmp #2344) while its
			// writer was not — the same latent defect as rmp #2378, in the same
			// file (found while fixing that one).
			if err := g.ApplyAtomicallyTx(func(tx WriteTx) error {
				wv := g.Writer(tx)
				if err := wv.SetNodeProperty("a", "v", Int64Value(i)); err != nil {
					return err
				}
				return wv.SetNodeProperty("b", "v", Int64Value(i))
			}); err != nil {
				writeErr.Add(1)
				return
			}
		}
	}()

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !done.Load() {
				stop := false
				func() {
					snap := g.BeginRead()
					defer g.EndRead(snap)
					view := g.ReadAt(snap)
					va, oka := view.GetNodeProperty("a", "v")
					vb, okb := view.GetNodeProperty("b", "v")
					if !oka || !okb {
						stop = true
						return
					}
					ia, _ := va.Int64()
					ib, _ := vb.Int64()
					reads.Add(1)
					if ia != ib {
						violation.Add(1)
						// Only the FIRST observation is kept, via CompareAndSwap:
						// it is the one whose interleaving is least perturbed by
						// the recording.
						if firstSeen.CompareAndSwap(false, true) {
							firstIA.Store(ia)
							firstIB.Store(ib)
							// RE-READ THE SAME PINNED SNAPSHOT. This separates the two
							// possibilities that the values alone cannot: if the same
							// snapshot now AGREES, the tear was a transient window and
							// something the read consulted changed between the two
							// reads; if it still DISAGREES, this snapshot resolves the
							// two nodes inconsistently as a matter of state, and that
							// state can then be dissected at leisure instead of chased.
							ra, _ := mustInt64(view.GetNodeProperty("a", "v"))
							rb, _ := mustInt64(view.GetNodeProperty("b", "v"))
							firstReA.Store(ra)
							firstReB.Store(rb)
							firstStartTS.Store(snap.startTS)
							_, sa := propChainFacts(g, "a")
							_, sb := propChainFacts(g, "b")
							firstAFacts.Store(sa)
							firstBFacts.Store(sb)
							firstShared.Store(headsShareRecord(g, "a", "b"))
						}
					}
				}()
				if stop {
					return
				}
			}
		}()
	}

	wg.Wait()
	if writeErr.Load() != 0 {
		t.Fatalf("writer hit %d errors", writeErr.Load())
	}
	if v := violation.Load(); v != 0 {
		ia, ib := firstIA.Load(), firstIB.Load()
		ra, rb := firstReA.Load(), firstReB.Load()
		persistence := "TRANSIENT — the same pinned snapshot AGREED on re-read, so something the read consulted changed between the two reads"
		if ra != rb {
			persistence = "PERSISTENT — the same pinned snapshot STILL disagreed on re-read, so this snapshot resolves the two nodes inconsistently as a matter of state"
		}
		t.Fatalf("observed %d partial-transaction violations (a.v != b.v inside a pinned SNAPSHOT); "+
			"first observation a.v=%d b.v=%d (delta %d, %s); re-read of the SAME snapshot (startTS=%d) gave a.v=%d b.v=%d — %s; "+
			"substrate at the violation: a: %s | b: %s | %s",
			v, ia, ib, ia-ib, tearDirection(ia, ib),
			firstStartTS.Load(), ra, rb, persistence,
			firstAFacts.Load(), firstBFacts.Load(), firstShared.Load())
	}
	if reads.Load() == 0 {
		t.Fatal("readers never observed both properties; test did not exercise the invariant")
	}
}

// propChainFacts encodes, for one node key, the per-shard substrate state a
// snapshot read of it would have consulted. It is packed into a uint64 so the
// capture is a single atomic store on a path that must perturb the interleaving
// as little as possible.
//
// Layout: bit 0 set when the SHARD holds no history at all (the `sh.d != nil`
// gate declines and the read returns the current bag); bit 1 set when the shard
// has history but THIS NODE has no chain (propBagAsOfLocked returns the current
// bag); the remaining bits carry the head record's stamp, which is 0 when there
// is no head.
//
// Either of the first two bits being set at a violation would mean the read was a
// present-time read rather than an as-of-snapshot one, which is a materially
// different defect from a mis-ordered visibility decision.
// It returns the stamp UNMODIFIED, and reports the two absent cases through a
// separate return value rather than through reserved bits of the stamp.
//
// # An earlier version packed both into one word, and that destroyed the signal
//
// It returned `stampTS() << 2` with the low two bits as flags. A stamp is not a
// small number: an IN-FLIGHT record carries the transaction id, which is
// [mvcc.TxIDBase] + k = 2^63 + k, so the shift OVERFLOWED and
// (2^63+k)<<2 mod 2^64 == 4k. The decoder's >>2 then printed k — a plausible
// small integer that looked exactly like a commit timestamp. The first captured
// violation reported "head stamped 85468", which was never a timestamp at all but
// the sequence number of a transaction that was still IN FLIGHT, and the one fact
// that mattered — that this node's head was uncommitted — was the fact the
// encoding erased.
//
// Hence: no packing, no reserved bits, and the in-flight case named explicitly.
func propChainFacts(g *Graph[string, int64], key string) (stamp uint64, state string) {
	id, ok := g.adj.Mapper().Lookup(key)
	if !ok {
		return 0, "NO SUCH NODE"
	}
	sh := g.nodePropShardFor(id)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	if sh.d == nil {
		return 0, "SHARD HELD NO HISTORY — the read returned the CURRENT bag, not an as-of-snapshot one"
	}
	d := sh.d[id]
	if d == nil {
		return 0, "NODE HAD NO CHAIN — the read returned the CURRENT bag, not an as-of-snapshot one"
	}
	ts := d.stampTS()
	switch {
	case ts == mvcc.AbortedTS:
		return ts, "live chain, head ABORTED"
	case ts >= mvcc.TxIDBase:
		return ts, fmt.Sprintf("live chain, head IN FLIGHT as transaction %d", ts-mvcc.TxIDBase)
	default:
		return ts, fmt.Sprintf("live chain, head COMMITTED at %d", ts)
	}
}

// headsShareRecord reports whether the two nodes' chain heads carry the SAME
// commit record. That is the premise the whole isolation guarantee rests on here:
// both properties are written inside ONE ApplyAtomicallyTx, so their deltas must
// share one *commitInfo and therefore commit at one instant. If they ever do not,
// the two writes commit separately and a snapshot landing between them sees a
// partial transaction — which is exactly the failure mode this file's other test
// documents for the bare autocommit path.
//
// It is sampled at the violation rather than asserted up front because a
// quiescent check already passes; the question is whether it still holds under
// the interleaving that produces the tear.
func headsShareRecord(g *Graph[string, int64], ka, kb string) string {
	ia, oka := g.adj.Mapper().Lookup(ka)
	ib, okb := g.adj.Mapper().Lookup(kb)
	if !oka || !okb {
		return "record comparison unavailable (a node was absent)"
	}
	ra, oka := headRecord(g, ia)
	rb, okb := headRecord(g, ib)
	switch {
	case !oka || !okb:
		return "record comparison unavailable (a chain was absent)"
	case ra == nil || rb == nil:
		return fmt.Sprintf("AT LEAST ONE HEAD CARRIES NO RECORD (a=%v b=%v) — that write took a fresh instant of its own", ra != nil, rb != nil)
	case ra == rb:
		return "both heads share ONE commit record, as the transaction requires"
	default:
		return "THE TWO HEADS CARRY DIFFERENT COMMIT RECORDS — the two writes of one transaction did not commit together"
	}
}

// headRecord returns the commit record of id's chain head.
func headRecord(g *Graph[string, int64], id graph.NodeID) (*commitInfo, bool) {
	sh := g.nodePropShardFor(id)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	if sh.d == nil {
		return nil, false
	}
	d := sh.d[id]
	if d == nil {
		return nil, false
	}
	return d.info, true
}

// mustInt64 unwraps a property read for the diagnostic re-read, reporting -1 for
// an absent key so a missing property is distinguishable from a real value
// rather than silently reading as zero.
func mustInt64(v PropertyValue, ok bool) (int64, bool) {
	if !ok {
		return -1, false
	}
	i, iok := v.Int64()
	if !iok {
		return -2, false
	}
	return i, true
}

// tearDirection classifies a partial-transaction observation by which of the two
// reads saw the newer transaction. The reader reads "a" and then "b" from one
// pinned snapshot, and the writer sets both to the same loop counter, so the
// values order the two reads against the writer's timeline.
//
// The two directions have different suspects, which is the whole point of
// recording it: a later-read-is-newer tear means the visibility basis moved
// FORWARD between the two reads (a version the snapshot still needed stopped
// being resolved as-of that snapshot), whereas an earlier-read-is-newer tear
// means the second read undid more than it should have.
func tearDirection(ia, ib int64) string {
	switch {
	case ia < ib:
		return "the LATER read (b) saw the newer transaction — the visibility basis moved FORWARD between the two reads"
	case ia > ib:
		return "the EARLIER read (a) saw the newer transaction — the later read undid a change its snapshot should have kept"
	default:
		return "values equal — the capture did not record the violating pair"
	}
}

// TestIsolation_DirectReadObservesPartialTransaction characterises the
// documented OPT-IN nature of the visibility barrier (#1283): the
// no-partial-transaction guarantee holds ONLY for reads routed through
// [Graph.View]. A direct public read (here g.AdjList().HasEdge and
// g.HasNodeLabel called WITHOUT View) takes only its own shard locks, not
// visMu, so it can observe a multi-op transaction half-applied — the edge of
// an edge-plus-labels write before the endpoint labels exist.
//
// This is a CONTRACT/characterization test, not a bug fix: it locks the
// currently-documented behaviour. It proves two halves of the same coin under
// a deterministic handshake (no flaky timing):
//
//   - a reader reading DIRECTLY mid-transaction observes violation > 0
//     (the opt-in hole is real and documented), while
//   - the same reader, wrapped in [Graph.View], observes ZERO violations
//     (View closes the window).
//
// The writer pins the partial state open across a barrier so the direct read
// is guaranteed to land inside the transaction; the reader never requests
// visMu (only shard locks), so the handshake cannot deadlock against the
// writer that holds visMu via [Graph.ApplyAtomically]. Run under -race: the
// per-shard locks make every access data-race-free; the gap proven OPEN here
// is the logical partial-transaction visibility, not a memory race.
func TestIsolation_DirectReadObservesPartialTransaction(t *testing.T) {
	t.Parallel()

	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("u"); err != nil {
		t.Fatalf("AddNode u: %v", err)
	}
	if err := g.AddNode("v"); err != nil {
		t.Fatalf("AddNode v: %v", err)
	}

	// readEdgeImpliesLabels evaluates the cross-substructure invariant
	// "HasEdge(u,v) ⇔ HasNodeLabel(u,Hot) ⇔ HasNodeLabel(v,Hot)" with three
	// direct public reads and reports whether it is currently violated.
	readEdgeImpliesLabels := func() bool {
		e := g.AdjList().HasEdge("u", "v")
		lu := g.HasNodeLabel("u", "Hot")
		lv := g.HasNodeLabel("v", "Hot")
		return e != lu || e != lv
	}

	// snapshotEdgeImpliesLabels evaluates the SAME invariant through a pinned
	// MVCC snapshot. Since rmp #2344 this is what closes the window — Graph.View
	// is gone, and it had stopped closing it anyway once ordinary writes took the
	// barrier shared.
	snapshotEdgeImpliesLabels := func() bool {
		snap := g.BeginRead()
		defer g.EndRead(snap)
		view := g.ReadAt(snap)
		_, e := g.EdgeWeightAsOf("u", "v", snap)
		lu := view.HasNodeLabel("u", "Hot")
		lv := view.HasNodeLabel("v", "Hot")
		return e != lu || e != lv
	}

	// Half 1 — direct read, NO View. A writer opens a transaction, adds the
	// edge, then blocks BEFORE setting the labels until the reader has read.
	// The reader, reading directly, must observe {edge present, labels absent}
	// — a half-applied transaction — proving the opt-in hole.
	var directViolation atomic.Int64
	{
		readNow := make(chan struct{})  // writer -> reader: edge added, labels not yet
		readDone := make(chan struct{}) // reader -> writer: read taken, finish the txn
		writeDone := make(chan struct{})

		go func() {
			defer close(writeDone)
			_ = g.ApplyAtomically(func() error {
				_ = g.AddEdge("u", "v", 0)
				close(readNow) // partial state is now established
				<-readDone     // hold the transaction open across the direct read
				_ = g.SetNodeLabel("u", "Hot")
				_ = g.SetNodeLabel("v", "Hot")
				return nil
			})
		}()

		<-readNow
		if readEdgeImpliesLabels() {
			directViolation.Add(1)
		}
		close(readDone)
		<-writeDone
	}

	if directViolation.Load() == 0 {
		t.Fatalf("direct (unpinned) read did not observe the documented partial-transaction hole; " +
			"expected violation > 0")
	}

	// Reset to the clean, fully-applied state for half 2.
	if err := g.ApplyAtomically(func() error {
		g.AdjList().RemoveEdge("u", "v")
		g.RemoveNodeLabel("u", "Hot")
		g.RemoveNodeLabel("v", "Hot")
		return nil
	}); err != nil {
		t.Fatalf("reset: %v", err)
	}

	// Half 2 — the SAME invariant read through a pinned SNAPSHOT. The snapshot
	// resolves every read at one instant, so it can only ever observe the
	// fully-applied state or the pre-transaction state, never a mixture: zero
	// violations. The writer's start is synchronised with the reader so the
	// snapshot genuinely contends with a multi-op apply.
	var viewViolation atomic.Int64
	var viewReads atomic.Int64
	{
		var wg sync.WaitGroup
		startWrite := make(chan struct{})

		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startWrite
			_ = g.ApplyAtomically(func() error {
				_ = g.AddEdge("u", "v", 0)
				runtime.Gosched() // widen the partial window the View must mask
				_ = g.SetNodeLabel("u", "Hot")
				_ = g.SetNodeLabel("v", "Hot")
				return nil
			})
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			close(startWrite)
			// Read repeatedly through a snapshot while the writer applies. Every
			// pinned read must see the invariant hold.
			for i := 0; i < 2000; i++ {
				viewReads.Add(1)
				if snapshotEdgeImpliesLabels() {
					viewViolation.Add(1)
				}
			}
		}()

		wg.Wait()
	}

	if v := viewViolation.Load(); v != 0 {
		t.Fatalf("snapshot read observed %d partial-transaction violations; the snapshot must close the window", v)
	}
	if viewReads.Load() == 0 {
		t.Fatal("snapshot readers never read; half 2 did not exercise the invariant")
	}

	t.Logf("characterized opt-in barrier: direct reads observed %d violation(s); "+
		"snapshot reads observed 0 across %d reads",
		directViolation.Load(), viewReads.Load())
}
