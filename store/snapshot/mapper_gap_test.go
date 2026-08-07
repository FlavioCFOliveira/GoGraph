package snapshot

// mapper_gap_test.go — rmp #2310: the concurrent capture filters the mapper, and
// [graph.Mapper.LoadFrom] requires the intra-shard indexes it is handed to be
// CONTIGUOUS.
//
// Layer: short.
//
// # The precondition, and what happens when it does not hold
//
// A NodeID is packed as (intra << shardBits) | shard, and intra is assigned when the
// key is INTERNED. LoadFrom groups the entries it is given by shard, sorts them by
// intra, and rejects the whole snapshot with ErrMapperEntryCorrupted unless the
// sequence is exactly 0..N-1 (graph/mapper_restore.go, precondition 3).
//
// Before rmp #2310 the capture emitted every interned id, so the sequence was
// complete by construction. The concurrent capture emits only the ids interned AS OF
// its instant — which it must, or the recovered graph would hold nodes that did not
// exist then. Dropping ids is safe only while the dropped set is a per-shard SUFFIX,
// and that holds exactly when no write transaction is open when the instant is taken:
// interning is monotone within a shard, so the only invisible ids are then the ones
// interned afterwards.
//
// An id interned by a STILL-OPEN transaction breaks it — it sits below ids that later
// transactions have already interned and committed. Measured before the guard: the
// capture produced a mapper whose shard 114 held intra 1 and not intra 0, and
// LoadFrom rejected it with "shard 114 intra-index gap: got 1 at slot 0". A snapshot
// that cannot be loaded is a checkpoint that has already destroyed the WAL prefix it
// folded, so the capture must refuse to produce one.

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// sameShardKeys returns two distinct keys that intern into the SAME mapper shard,
// found by probing a throwaway mapper — the shard is a pure function of the key, so
// the placement carries over to any other mapper.
func sameShardKeys(t *testing.T) (string, string) {
	t.Helper()
	m := graph.NewMapper[string]()
	seen := make(map[uint64]string)
	for i := 0; i < 200000; i++ {
		k := fmt.Sprintf("gap-key-%06d", i)
		s := graph.MapperShardOf(m.Intern(k))
		if prev, ok := seen[s]; ok {
			return prev, k
		}
		seen[s] = k
	}
	t.Fatal("no two probe keys landed in the same mapper shard")
	return "", ""
}

// TestCapture_FilteredMapperKeepsIntraIndexesContiguous drives the exact ordering the
// precondition is vulnerable to — a key interned FIRST and committed SECOND — and
// asserts the captured mapper still loads.
//
// # Why it is deterministic
//
// The ordering is constructed, not raced for: transaction A interns its key and is
// held open; transaction B then interns the neighbouring slot in the same shard and
// commits; the instant is taken between the two. A is therefore guaranteed to be the
// filtered-out entry and guaranteed to sit BELOW B in the shard's intra sequence, so
// the hole is in the middle every time this test runs.
func TestCapture_FilteredMapperKeepsIntraIndexesContiguous(t *testing.T) {
	keyA, keyB := sameShardKeys(t)

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()

	// A interns first and stays OPEN.
	txA := g.BeginVersionedTx()
	if err := g.Writer(txA).AddNode(keyA); err != nil {
		g.EndVersionedTx(txA)
		t.Fatalf("A AddNode(%q): %v", keyA, err)
	}
	// B interns the next slot in the same shard and COMMITS.
	if err := g.ApplyVersioned(func(tx lpg.WriteTx) error {
		return g.Writer(tx).AddNode(keyB)
	}); err != nil {
		g.EndVersionedTx(txA)
		t.Fatalf("B AddNode(%q): %v", keyB, err)
	}

	idA, okA := g.AdjList().Mapper().Lookup(keyA)
	idB, okB := g.AdjList().Mapper().Lookup(keyB)
	if !okA || !okB {
		g.EndVersionedTx(txA)
		t.Fatalf("both keys must be interned: %q=%v %q=%v", keyA, okA, keyB, okB)
	}
	if graph.MapperShardOf(idA) != graph.MapperShardOf(idB) {
		g.EndVersionedTx(txA)
		t.Fatalf("probe keys landed in different shards (%d, %d): the test's premise is gone",
			graph.MapperShardOf(idA), graph.MapperShardOf(idB))
	}
	if uint64(idA) >= uint64(idB) {
		g.EndVersionedTx(txA)
		t.Fatalf("A must have interned BEFORE B for the hole to be in the middle: idA=%d idB=%d",
			uint64(idA), uint64(idB))
	}

	// The instant: B has committed, A has not.
	at := g.BeginRead()
	if g.NodeInternedAsOf(idA, at) {
		g.EndRead(at)
		g.EndVersionedTx(txA)
		t.Fatal("the uncommitted transaction's node is visible at the instant: the capture " +
			"would not filter it and this test could not produce the hole it exists to test")
	}
	if !g.NodeInternedAsOf(idB, at) {
		g.EndRead(at)
		g.EndVersionedTx(txA)
		t.Fatal("the committed transaction's node is NOT visible at the instant")
	}

	cs := csr.BuildFromAdjListAsOf(g.AdjList(),
		func(id graph.NodeID) bool { return g.NodeExistsAsOf(id, at) },
		at.StartTS(), at.TxID())
	capt, err := CaptureGraph[string, float64](g, cs, nil, at)
	g.EndRead(at)
	g.EndVersionedTx(txA)

	// THE CONTRACT: the capture must REFUSE. Producing an image here is the defect —
	// there is no correct one, and the alternative to refusing is a snapshot recovery
	// rejects after the WAL prefix that could have repaired it has been truncated.
	if err == nil {
		t.Fatalf("CaptureGraph accepted an instant taken while transaction A was still open "+
			"and produced an image of %d node(s). Node %d (%q) was interned before node %d "+
			"(%q) in shard %d but is not visible at the instant, so the image has a hole in "+
			"the middle of that shard's intra sequence",
			capt.Order(), uint64(idA), keyA, uint64(idB), keyB, graph.MapperShardOf(idA))
	}
	if !errors.Is(err, ErrCaptureNotQuiesced) {
		t.Fatalf("CaptureGraph failed with %v, want %v", err, ErrCaptureNotQuiesced)
	}
	if capt != nil {
		t.Error("CaptureGraph returned both an error and a capture; a refused capture must " +
			"yield nothing a caller could publish by mistake")
	}

	// The failure must be REACHABLE, not merely declared: confirm the image the guard
	// prevented really would have been unloadable, by handing LoadFrom the entry set
	// the filter would have produced.
	entries := []graph.MapperEntry[string]{{ID: idB, Key: keyB}}
	if lerr := graph.NewMapper[string]().LoadFrom(entries); !errors.Is(lerr, graph.ErrMapperEntryCorrupted) {
		t.Errorf("LoadFrom accepted the holed entry set (%v): the precondition the guard "+
			"enforces is not actually enforced downstream, so the guard may be unnecessary "+
			"or may be guarding the wrong thing", lerr)
	}
}

// TestCapture_QuiescedInstantEmitsEveryVisibleNode is the positive arm: with no write
// transaction open when the instant is taken — the state the commit serialiser's
// drain guarantees the checkpointer — the capture succeeds and carries every
// committed node.
//
// Without it, TestCapture_FilteredMapperKeepsIntraIndexesContiguous would be
// satisfied by a guard that rejected every concurrent capture.
func TestCapture_QuiescedInstantEmitsEveryVisibleNode(t *testing.T) {
	keyA, keyB := sameShardKeys(t)

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()

	// Both transactions COMMIT before the instant, in interning order.
	for _, k := range []string{keyA, keyB} {
		key := k
		if err := g.ApplyVersioned(func(tx lpg.WriteTx) error {
			return g.Writer(tx).AddNode(key)
		}); err != nil {
			t.Fatalf("AddNode(%q): %v", key, err)
		}
	}

	at := g.BeginRead()
	cs := csr.BuildFromAdjListAsOf(g.AdjList(),
		func(id graph.NodeID) bool { return g.NodeExistsAsOf(id, at) },
		at.StartTS(), at.TxID())
	capt, err := CaptureGraph[string, float64](g, cs, nil, at)
	g.EndRead(at)
	if err != nil {
		t.Fatalf("CaptureGraph on a quiesced instant: %v", err)
	}

	rb, err := ReadMapperString(bytes.NewReader(capt.mapper.bytes))
	if err != nil {
		t.Fatalf("ReadMapperString: %v", err)
	}
	entries := make([]graph.MapperEntry[string], 0, len(rb.Pairs))
	for _, p := range rb.Pairs {
		entries = append(entries, graph.MapperEntry[string]{ID: p.ID, Key: p.Key})
	}
	fresh := graph.NewMapper[string]()
	if lerr := fresh.LoadFrom(entries); lerr != nil {
		t.Fatalf("the captured mapper does not load back: %v", lerr)
	}
	for _, k := range []string{keyA, keyB} {
		if _, ok := fresh.Lookup(k); !ok {
			t.Errorf("committed key %q is missing from the image", k)
		}
	}
	if got, want := capt.Order(), uint64(2); got != want {
		t.Errorf("image carries %d nodes, want %d", got, want)
	}
}

// TestCapture_TombstoneIDsAreAscending pins tombstones.bin's documented input
// contract for the INSTANT path.
//
// # Why it needs a test of its own
//
// The present-time path gets ascending ids for free: it reads the roaring bitmap,
// whose ToArray is ordered. The instant path cannot use the bitmap — the bitmap
// answers "removed NOW" and keeps no history — so it derives the set from a mapper
// walk instead, and that walk is NOT ascending. A NodeID packs as
// (intra << shardBits) | shard and Walk is shard-major, so a graph with more than one
// node per shard walks 0, 256, 512, 768, 1, 257, …
//
// The graph here is deliberately larger than the 256 shards. Below that every shard
// holds a single node, ids equal shard indexes, and the walk IS ascending — which is
// exactly why the wrong claim survived being written down, and why a small fixture
// would pass against the unsorted code.
func TestCapture_TombstoneIDsAreAscending(t *testing.T) {
	const n = 2000

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()

	for i := 0; i < n; i++ {
		if err := g.AddNode(fmt.Sprintf("tomb-%05d", i)); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	// Remove every third node, so the dead set spans many shards at several intra
	// indexes and the walk order is thoroughly non-monotonic.
	var removed int
	for i := 0; i < n; i += 3 {
		if err := g.ApplyVersioned(func(tx lpg.WriteTx) error {
			g.Writer(tx).RemoveNode(fmt.Sprintf("tomb-%05d", i))
			return nil
		}); err != nil {
			t.Fatalf("RemoveNode: %v", err)
		}
		removed++
	}

	at := g.BeginRead()
	got := g.TombstonedIDsAsOf(at)
	g.EndRead(at)

	if len(got) != removed {
		t.Fatalf("TombstonedIDsAsOf returned %d ids, want %d", len(got), removed)
	}
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Fatalf("TombstonedIDsAsOf is not ascending at index %d: %d follows %d. "+
				"tombstones.bin documents ascending ids as its input contract, and the "+
				"present-time form satisfies it via the roaring bitmap's ToArray — the "+
				"instant form derives the set from a shard-major mapper walk and must sort",
				i, uint64(got[i]), uint64(got[i-1]))
		}
	}
	// The fixture must actually exercise the non-monotonic case, or a sort-free
	// implementation would pass. Confirm the underlying walk really is out of order.
	var walked []graph.NodeID
	g.AdjList().Mapper().Walk(func(id graph.NodeID, _ string) bool {
		walked = append(walked, id)
		return true
	})
	monotonic := true
	for i := 1; i < len(walked); i++ {
		if walked[i] < walked[i-1] {
			monotonic = false
			break
		}
	}
	if monotonic {
		t.Fatal("the mapper walk happened to be ascending on this fixture, so the sort was " +
			"never exercised and this test proves nothing — enlarge the graph past the " +
			"shard count")
	}
}
