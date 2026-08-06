package generation_test

// mvcc_agreement_test.go — rmp #2311 item (d): a generation and an MVCC read of the
// SAME instant agree, and a generation built from the present does not.
//
// Layer: short.
//
// # What this pins
//
// The package doc states the one rule relating the two: a generation is only
// meaningful as of the instant it was built at. This is that rule as a test. It is
// worth having because the failure is silent — a generation built from the present
// while writers commit still answers every query, it simply answers for no instant at
// all, and nothing downstream re-checks visibility.
//
// # WHAT THIS TEST DOES AND DOES NOT CATCH — measured, not claimed
//
// It was validated by injection, and the result is worth recording because it is
// PARTLY NEGATIVE. Replacing the as-of build with a present-time one does NOT fail it:
// at the moment the generation is built the present and the instant COINCIDE, because
// the later transactions have not committed yet. So this test cannot detect a
// present-time build on its own.
//
// What it does establish, and what the assertions below are actually for:
//   - a generation built as of an instant AGREES with an independently computed MVCC
//     read of that same instant;
//   - it keeps agreeing after later transactions commit, so it is genuinely pinned;
//   - and it then DIFFERS from a present-time build, which is the discriminating
//     assertion — without it the first two would hold trivially on a graph that never
//     moved.
//
// Catching a present-time build at its build site needs a writer committing DURING the
// build, which is the torn-read shape and is not deterministic here. That gap is
// stated rather than papered over.

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/generation"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestGeneration_BuiltAtAnInstantAgreesWithAnMVCCReadOfIt asserts that a generation
// built from a snapshot describes that snapshot's graph, and keeps describing it while
// later transactions commit.
func TestGeneration_BuiltAtAnInstantAgreesWithAnMVCCReadOfIt(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	defer func() { _ = g.Close() }()

	const before = 6
	for i := 0; i < before; i++ {
		if err := g.ApplyVersioned(func(tx lpg.WriteTx) error {
			return g.Writer(tx).AddEdge(fmt.Sprintf("a%d", i), fmt.Sprintf("b%d", i), 1)
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	at := g.BeginRead()
	defer g.EndRead(at)

	// The generation, built AS OF that instant, and published.
	live := func(id graph.NodeID) bool { return g.NodeExistsAsOf(id, at) }
	gen := csr.BuildFromAdjListAsOf(g.AdjList(), live, at.StartTS(), at.TxID())
	pub := generation.New(gen)
	heldGen := pub.Acquire()
	defer pub.Release(heldGen)
	held := heldGen.CSR()

	// Transactions commit AFTER the instant. The held generation must not change.
	const after = 5
	for i := 0; i < after; i++ {
		if err := g.ApplyVersioned(func(tx lpg.WriteTx) error {
			return g.Writer(tx).AddEdge(fmt.Sprintf("x%d", i), fmt.Sprintf("y%d", i), 1)
		}); err != nil {
			t.Fatalf("later %d: %v", i, err)
		}
	}

	// An MVCC read of the same instant, computed independently of the generation.
	var mvccEdges uint64
	g.AdjList().Mapper().Walk(func(id graph.NodeID, _ string) bool {
		mvccEdges += uint64(len(g.AdjList().EntryViewAsOf(id, at.StartTS(), at.TxID()).Neighbours))
		return true
	})

	if held.Size() != mvccEdges {
		t.Errorf("the generation holds %d edges and an MVCC read of the same instant sees "+
			"%d: a generation built from a snapshot must describe that snapshot's graph",
			held.Size(), mvccEdges)
	}
	if mvccEdges != before {
		t.Fatalf("an MVCC read of the instant sees %d edges, want %d: the fixture did not "+
			"commit what this test assumes", mvccEdges, before)
	}

	// THE CONTROL. A generation built from the PRESENT now sees the later commits, so
	// the window this test runs in is real and the agreement above was not vacuous.
	nowGen := csr.BuildFromAdjList(g.AdjList())
	if nowGen.Size() != before+after {
		t.Fatalf("a present-time build sees %d edges, want %d: nothing committed during "+
			"the window, so the assertion above proved nothing", nowGen.Size(), before+after)
	}
	if held.Size() == nowGen.Size() {
		t.Error("the generation held across the later commits reports the same size as a " +
			"present-time build: it is not pinned to the instant it was built at")
	}
}
