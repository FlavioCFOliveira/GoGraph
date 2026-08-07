package lpg

// mvcc_observability_test.go — rmp #2312 AC 1 and AC 5: the write-side series and the
// retained-chain distribution are proved to MOVE, and to move by the right amount.
//
// Layer: short.
//
// # Why every assertion here is an exact number
//
// A counter nobody has watched increment is a counter that does not work, and this
// project has caught three of its own instruments reporting a number that could only
// ever have been zero. But "it moved" is the weaker claim: for each of these series
// there is a plausible WRONG placement that also moves, and only the count
// distinguishes them —
//
//   - commits counted in [Graph.endWrite]'s versioned-nothing branch would exceed the
//     number of instants the clock allocated, and the conflict rate's denominator
//     would include transactions that never committed;
//   - the writer gauge decremented in endWrite rather than in
//     [Graph.releaseWriterSnapshot] would LEAK on every bracket that versioned
//     nothing, because endWrite returns early for those;
//   - a chain-depth histogram that counted the records it FREED rather than the ones
//     it retained would report a growing distribution for a graph whose chains are
//     all being collected.
//
// So each test below asserts the count, and each says which wrong placement it rules
// out.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// TestMVCCStats_WriterGaugeRisesInsideABracketAndReturnsToZero asserts the writer
// gauge is POSITIVE while a write bracket is open and zero once it closes.
//
// The observation is taken from INSIDE the bracket, because the property is about the
// open interval and a sample taken after it would read zero whether the counter works
// or not — the negative-oracle trap. It also opens a bracket that writes NOTHING,
// which is the case that distinguishes the correct decrement site from the plausible
// one: [Graph.endWrite] returns early when a transaction versioned nothing, so a
// decrement placed there would leak a writer per empty bracket for the life of the
// process.
func TestMVCCStats_WriterGaugeRisesInsideABracketAndReturnsToZero(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()

	if got := g.MVCCStats().Write.Writers; got != 0 {
		t.Fatalf("a fresh graph reports %d writers, want 0", got)
	}

	var inside int64
	if err := g.ApplyVersioned(func(tx WriteTx) error {
		inside = g.MVCCStats().Write.Writers
		return g.Writer(tx).AddNode("a")
	}); err != nil {
		t.Fatalf("ApplyVersioned: %v", err)
	}
	if inside != 1 {
		t.Errorf("the writer gauge read %d from inside an open write bracket, want 1: "+
			"nothing else in this test can observe the interval, so a zero here means the "+
			"gauge cannot report a writer at all", inside)
	}
	if got := g.MVCCStats().Write.Writers; got != 0 {
		t.Errorf("the writer gauge is %d after the bracket closed, want 0", got)
	}

	// THE EMPTY BRACKET. It takes a horizon slot and a transaction id and versions
	// nothing, so endWrite returns before it publishes anything.
	for i := 0; i < 50; i++ {
		if err := g.ApplyVersioned(func(WriteTx) error { return nil }); err != nil {
			t.Fatalf("empty ApplyVersioned: %v", err)
		}
	}
	if got := g.MVCCStats().Write.Writers; got != 0 {
		t.Errorf("the writer gauge is %d after 50 brackets that versioned nothing, want 0: "+
			"the decrement is on a path those brackets do not reach", got)
	}
}

// TestMVCCStats_CommitsCountPublishedInstantsOnly asserts a bracket that versioned
// nothing is not counted as a commit, and one that wrote is.
//
// The rule is that a commit IS a published instant, and it matters because the commit
// count is the denominator an operator divides the conflict count by. Counting empty
// brackets would put commits above the number of instants the clock ever allocated and
// silently deflate every rate computed from it.
func TestMVCCStats_CommitsCountPublishedInstantsOnly(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()

	for i := 0; i < 20; i++ {
		if err := g.ApplyVersioned(func(WriteTx) error { return nil }); err != nil {
			t.Fatalf("empty ApplyVersioned: %v", err)
		}
	}
	if got := g.MVCCStats().Write.Commits; got != 0 {
		t.Errorf("20 brackets that versioned nothing produced %d commits, want 0", got)
	}

	const writes = 7
	for i := 0; i < writes; i++ {
		if err := g.ApplyVersioned(func(tx WriteTx) error {
			return g.Writer(tx).AddNode(nodeName(i))
		}); err != nil {
			t.Fatalf("ApplyVersioned: %v", err)
		}
	}
	if got := g.MVCCStats().Write.Commits; got != writes {
		t.Errorf("%d writing transactions produced %d commits, want %d", writes, got, writes)
	}
	if got := g.MVCCStats().Write.Aborts; got != 0 {
		t.Errorf("no transaction conflicted but %d aborts were counted", got)
	}
}

// TestMVCCStats_ConflictIsCountedAsBothAConflictAndAnAbort asserts the two write-side
// outcomes partition the transactions that reached a decision.
//
// A doomed transaction is counted once where its conflict was detected and once, as
// the abort it became, where its record is marked. Both are needed and they are not
// the same number in general: a transaction can abort without conflicting.
func TestMVCCStats_ConflictIsCountedAsBothAConflictAndAnAbort(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	// The construction the conflict suite uses: A removes the node and stays in
	// flight, so its death record is a chain head B cannot displace.
	txA := g.beginLabelTx()
	txA.removeNode("a")

	// B revives it, which records a birth on the same chain. addNode reaches the node
	// existence store through a VOID primitive, so the refusal is recorded on the
	// transaction rather than returned here, and commit is where it surfaces.
	txB := g.beginLabelTx()
	if err := txB.addNode("a"); err != nil {
		t.Fatalf("B addNode: %v", err)
	}
	if _, err := txB.commit(); err == nil {
		t.Fatalf("B's overlapping revival was ACCEPTED, so no conflict occurred and this " +
			"test proves nothing about the counters — fix the construction, do not relax it")
	}

	s := g.MVCCStats()
	if s.Write.Conflicts != 1 {
		t.Errorf("conflicts = %d, want 1", s.Write.Conflicts)
	}
	if s.Write.Aborts != 1 {
		t.Errorf("aborts = %d, want 1: a doomed transaction becomes an abort, and a "+
			"conflict count with no abort beside it cannot be read as an outcome", s.Write.Aborts)
	}
	idx := mvcc.ConflictStoreIndex(mvcc.StoreNodeExistence)
	if s.Write.ByStore[idx] != 1 {
		t.Errorf("the node-existence bucket holds %d, want 1: the conflict was attributed "+
			"to the wrong store, so an operator cannot tell which structure contended",
			s.Write.ByStore[idx])
	}
	if rate := s.Write.ConflictRate(); rate <= 0 {
		t.Errorf("ConflictRate() = %v with one conflict recorded, want above zero", rate)
	}

	if _, err := txA.commit(); err != nil {
		t.Fatalf("A was refused too (%v)", err)
	}
	if got := g.MVCCStats().Write.Conflicts; got != 1 {
		t.Errorf("the winner's commit moved the conflict count to %d, want 1", got)
	}
}

// TestMVCCStats_ChainDepthReportsRetainedDepth asserts the distribution counts the
// records a reader would still have to walk, and not the ones the sweep freed.
//
// A reader is held open across the writes so the versions CANNOT be reclaimed, which
// is what makes a chain deep in the first place. Without the reader every chain
// collapses to nothing and the histogram is empty — which is also this test's
// negative control, asserted at the end.
func TestMVCCStats_ChainDepthReportsRetainedDepth(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	// The reader pins the watermark below every version written after it.
	snap := g.BeginRead()

	const depth = 6
	for i := 0; i < depth; i++ {
		if err := g.SetNodeProperty("a", "v", Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	// A synchronous sweep, so the measurement does not depend on the background
	// vacuum having run: the histogram is filled by the reclaimer either way.
	g.ReclaimNow()

	d := g.ChainDepthsOf(int(depthNodeProps))
	if d.Chains() == 0 {
		t.Fatalf("no property chain was measured at all: the reclaimer walked no chain, so "+
			"the distribution cannot describe read cost (deepest=%d)", d.Deepest)
	}
	if d.Deepest < depth {
		t.Errorf("the deepest retained property chain is %d, want at least %d: a reader "+
			"pinned every one of those versions, so a smaller number means the histogram is "+
			"counting the records the sweep FREED rather than the ones it kept", d.Deepest, depth)
	}
	// depth=6 falls in the 4-7 bucket; assert it is not being reported as depth 1.
	if d.Buckets[0] != 0 {
		t.Errorf("a chain landed in the depth-1 bucket while a reader held %d versions "+
			"back: buckets=%v", depth, d.Buckets)
	}

	// THE NEGATIVE CONTROL, and it is the instrument's own validation: with the reader
	// gone the watermark advances past every version, the sweep frees them all, and the
	// distribution must go EMPTY. A histogram that stayed populated here would be
	// accumulating over the life of the process rather than describing the present.
	g.EndRead(snap)
	g.ReclaimNow()
	if after := g.ChainDepthsOf(int(depthNodeProps)); after.Chains() != 0 {
		t.Errorf("the property distribution still holds %d chains after every version "+
			"became reclaimable: buckets=%v deepest=%d", after.Chains(), after.Buckets, after.Deepest)
	}
}

// TestMVCCStats_ActiveReadersExcludesWriters asserts the reader count is the snapshot
// count MINUS the writers, so a graph with one writer and no reader does not report a
// reader.
//
// That is exactly what it did report before rmp #2312: the horizon holds a writer's
// snapshot too (rmp #2299), and the field was called ActiveReaders.
func TestMVCCStats_ActiveReadersExcludesWriters(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()

	var snapshots, readers, writers int
	if err := g.ApplyVersioned(func(tx WriteTx) error {
		s := g.MVCCStats()
		snapshots, readers, writers = s.ActiveSnapshots, s.ActiveReaders(), int(s.Write.Writers)
		return g.Writer(tx).AddNode("a")
	}); err != nil {
		t.Fatalf("ApplyVersioned: %v", err)
	}
	if snapshots != 1 || writers != 1 {
		t.Fatalf("inside one write bracket: snapshots=%d writers=%d, want 1 and 1", snapshots, writers)
	}
	if readers != 0 {
		t.Errorf("ActiveReaders() = %d inside a write bracket with no reader open, want 0", readers)
	}

	// And with a genuine reader open beside no writer, it must be 1.
	snap := g.BeginRead()
	s := g.MVCCStats()
	if s.ActiveReaders() != 1 {
		t.Errorf("ActiveReaders() = %d with one open read snapshot, want 1 "+
			"(snapshots=%d writers=%d)", s.ActiveReaders(), s.ActiveSnapshots, s.Write.Writers)
	}
	g.EndRead(snap)
}

// nodeName gives each test write a distinct key without importing fmt for it.
func nodeName(i int) string { return string(rune('a' + i%26)) }
