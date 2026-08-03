package adjlist

// mvcc_window_owner_test.go — the commit window under CONCURRENT writers
// (rmp #2301, audit finding E4).
//
// # What was wrong
//
// The window was per-AdjList: an int depth and a []*adjShard dirty list, both
// mutated with no synchronisation, under an explicit single-writer contract that
// the exclusive visibility barrier supplied. With two writers that becomes a
// data race on both fields, and — worse than a race — a SEMANTIC error: one
// transaction's EndCommit froze the builders of shards the OTHER transaction was
// still writing, and two transactions touching one shard shared a builder.
//
// # What replaced it
//
// The builder is tagged with the token of the transaction that owns it. A
// builder is reused only by a write presenting that token, so ownership IS the
// transaction's identity. There is no shared counter, no shared list, and no
// freeze step: a finished transaction's token is never presented again, so its
// builder becomes unreachable for in-place mutation the moment it ends.
//
// The tests below pin the three properties that argument rests on.

import (
	"testing"
)

// slotArrayOf returns the shard's currently published slot array, for identity
// comparison.
func slotArrayOf(a *AdjList[string, float64], key string) (*adjShard[float64], *shardSlots, uint64) {
	id, _ := a.Mapper().Lookup(key)
	s := &a.shards[id&shardMask]
	return s, s.slotsRef.Load(), uint64(id) >> shardBits
}

// TestWindowOwner_SecondTransactionDoesNotAdoptAnothersBuilder is the property
// that makes the dedup safe without a barrier.
//
// Transaction A writes a shard twice, so it owns that shard's builder and is
// mutating it in place. Transaction B then writes the SAME shard. B must NOT
// mutate A's builder: it must clone the published array, which already carries
// A's writes.
//
// Against a build whose builder is untagged — the pre-change behaviour, and what
// two concurrent writers would have done — B mutates A's builder in place, so
// B's write lands in an array A may still publish over, and A's subsequent
// in-place writes land in an array B has published past. Either way one
// transaction's write is lost.
func TestWindowOwner_SecondTransactionDoesNotAdoptAnothersBuilder(t *testing.T) {
	a, clk := versionedList(t)
	for _, n := range []string{"a", "b", "c"} {
		if err := a.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	ws := a.WriteStampForTest()

	// A: two writes to node "a", so it owns the shard's builder.
	ws.Begin()
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("A first write: %v", err)
	}
	if err := a.AddEdge("a", "c", 2); err != nil {
		t.Fatalf("A second write: %v", err)
	}
	s, afterA, _ := slotArrayOf(a, "a")
	ownerA := s.buildingOwner
	if ownerA == 0 {
		t.Fatal("transaction A did not take ownership of the shard's builder, so the " +
			"clone-once dedup is not being exercised at all")
	}
	if s.building != afterA {
		t.Fatal("A's builder is not the published array: markDirtyAndBuild must publish it")
	}
	infoA, _ := ws.End()

	// B: a genuinely separate transaction writing the same shard.
	ws.Begin()
	if err := a.AddEdge("a", "b", 3); err != nil {
		t.Fatalf("B write: %v", err)
	}
	_, afterB, _ := slotArrayOf(a, "a")
	ownerB := s.buildingOwner
	infoB, _ := ws.End()

	if ownerB == ownerA {
		t.Fatalf("B took over A's builder under A's own token (%d): a builder must be "+
			"reusable only by the transaction that created it", ownerA)
	}
	if afterB == afterA {
		t.Fatal("B mutated A's builder IN PLACE instead of cloning: B's write would land " +
			"in an array A may still publish over, and A's later in-place writes would " +
			"land in an array B has published past — one of the two is lost either way")
	}
	// Commit both so the graph is in a consistent state to read.
	tsA := clk.NextCommitTS()
	infoA.Commit(tsA)
	clk.PublishCommitTS(tsA)
	tsB := clk.NextCommitTS()
	infoB.Commit(tsB)
	clk.PublishCommitTS(tsB)

	// Both transactions' writes survive: that is what "not lost" means.
	if !a.HasEdge("a", "c") {
		t.Fatal("A's second write was lost")
	}
	if !a.HasEdge("a", "b") {
		t.Fatal("B's write was lost")
	}
}

// TestWindowOwner_OneTransactionStillDedupes is the counterpart: the change must
// not have silently disabled the optimisation it reorganises.
//
// It asserts the dedup in BOTH shapes a transaction can take, because they
// differ by exactly one clone and the difference is worth stating rather than
// discovering:
//
//   - with an explicit [AdjList.BeginCommit], the window's token exists before
//     the first write, so the FIRST write already adopts the builder and every
//     later same-shard write mutates it in place. This is the engine's shape.
//   - with only a transaction open on the stamp, the shared commit record is
//     allocated LAZILY by the first version — deliberately, so that a bracket
//     which versions nothing allocates nothing — so the first write has no token
//     to own a builder with and clones. The dedup starts at the second write.
//
// Either way the cost stays O(distinct shards touched) rather than O(ops), which
// is the property that matters; the record-only shape just pays one extra clone
// per shard it touches.
func TestWindowOwner_OneTransactionStillDedupes(t *testing.T) {
	t.Run("explicit window, first write owns", func(t *testing.T) {
		a, _ := versionedList(t)
		for _, n := range []string{"a", "b", "c", "d"} {
			if err := a.AddNode(n); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		ws := a.WriteStampForTest()
		ws.Begin()
		a.BeginCommit()
		if err := a.AddEdge("a", "b", 1); err != nil {
			t.Fatalf("first write: %v", err)
		}
		_, first, _ := slotArrayOf(a, "a")
		if err := a.AddEdge("a", "c", 2); err != nil {
			t.Fatalf("second write: %v", err)
		}
		if err := a.AddEdge("a", "d", 3); err != nil {
			t.Fatalf("third write: %v", err)
		}
		_, last, _ := slotArrayOf(a, "a")
		a.EndCommit()
		ws.End()

		if last != first {
			t.Fatal("later same-shard writes published NEW slot arrays: the " +
				"clone-once-per-(shard, transaction) dedup is not happening, and a " +
				"multi-op commit is back to O(ops) copy-on-write instead of " +
				"O(distinct shards touched)")
		}
	})

	t.Run("record only, dedup from the second write", func(t *testing.T) {
		a, _ := versionedList(t)
		for _, n := range []string{"a", "b", "c", "d"} {
			if err := a.AddNode(n); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		ws := a.WriteStampForTest()
		ws.Begin()
		if err := a.AddEdge("a", "b", 1); err != nil {
			t.Fatalf("first write: %v", err)
		}
		if err := a.AddEdge("a", "c", 2); err != nil {
			t.Fatalf("second write: %v", err)
		}
		s, second, _ := slotArrayOf(a, "a")
		if s.buildingOwner == 0 {
			t.Fatal("no builder was adopted by the transaction's second write, so the " +
				"commit record is not being used as the owner token at all")
		}
		if err := a.AddEdge("a", "d", 3); err != nil {
			t.Fatalf("third write: %v", err)
		}
		_, third, _ := slotArrayOf(a, "a")
		ws.End()

		if third != second {
			t.Fatal("the third write published a NEW slot array: once a transaction owns " +
				"a shard's builder every later write to that shard must mutate it in place")
		}
	})
}

// TestWindowOwner_NoTransactionAlwaysClones pins the untransacted case: with no
// transaction and no bulk window there is no owner, so every write clones, which
// is the behaviour an unbracketed write has always had.
func TestWindowOwner_NoTransactionAlwaysClones(t *testing.T) {
	a, _ := versionedList(t)
	for _, n := range []string{"a", "b", "c"} {
		if err := a.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("first write: %v", err)
	}
	s, first, _ := slotArrayOf(a, "a")
	if s.building != nil {
		t.Fatal("an untransacted write adopted a builder: it has no transaction to own one")
	}
	if err := a.AddEdge("a", "c", 2); err != nil {
		t.Fatalf("second write: %v", err)
	}
	_, second, _ := slotArrayOf(a, "a")
	if second == first {
		t.Fatal("an untransacted write mutated a published array in place")
	}
}

// TestWindowOwner_BulkWindowDedupesWithoutATransaction covers the bulk paths —
// single-threaded WAL replay and bulk ingest — which write with no transaction
// open on the stamp and would otherwise lose the dedup entirely.
func TestWindowOwner_BulkWindowDedupesWithoutATransaction(t *testing.T) {
	a, _ := versionedList(t)
	for _, n := range []string{"a", "b", "c", "d"} {
		if err := a.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}

	a.BeginCommit()
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("first write: %v", err)
	}
	_, first, _ := slotArrayOf(a, "a")
	if err := a.AddEdge("a", "c", 2); err != nil {
		t.Fatalf("second write: %v", err)
	}
	_, second, _ := slotArrayOf(a, "a")
	if second != first {
		t.Fatal("a bulk window did not dedupe: WAL replay and bulk ingest would pay " +
			"O(ops) copy-on-write")
	}
	a.EndCommit()

	// After the window closes its token is retired, so the next write must
	// clone rather than mutate the retired window's builder in place — which
	// would publish into an array nothing points at.
	if err := a.AddEdge("a", "d", 3); err != nil {
		t.Fatalf("post-window write: %v", err)
	}
	_, third, _ := slotArrayOf(a, "a")
	if third == second {
		t.Fatal("a write after EndCommit mutated the closed window's builder in place: " +
			"the window's token was not retired")
	}
	if !a.HasEdge("a", "d") {
		t.Fatal("the post-window write is not visible")
	}
}

// TestWindowOwner_BulkWindowNests pins that the bulk window's depth still
// counts, so an inner Begin/End pair does not retire the outer window's token.
func TestWindowOwner_BulkWindowNests(t *testing.T) {
	a, _ := versionedList(t)
	for _, n := range []string{"a", "b", "c"} {
		if err := a.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	a.BeginCommit()
	outer := a.bulkOwner
	a.BeginCommit()
	a.EndCommit()
	if a.bulkOwner != outer {
		t.Fatalf("an inner EndCommit retired the outer window's token (%d -> %d)",
			outer, a.bulkOwner)
	}
	a.EndCommit()
	if a.bulkOwner != 0 {
		t.Fatal("the outermost EndCommit did not retire the window's token")
	}
}

// TestWindowOwner_TokenNeverCollidesWithATransactionID pins the one way the
// token scheme could go wrong: a bulk window whose token equals some
// transaction's id would let that transaction adopt the window's builder.
func TestWindowOwner_TokenNeverCollidesWithATransactionID(t *testing.T) {
	a, _ := versionedList(t)
	ws := a.WriteStampForTest()

	seen := make(map[uint64]string)
	for i := 0; i < 64; i++ {
		a.BeginCommit()
		if prev, ok := seen[a.bulkOwner]; ok {
			t.Fatalf("bulk token %d reused (previously %s)", a.bulkOwner, prev)
		}
		seen[a.bulkOwner] = "bulk"
		a.EndCommit()

		id := ws.Begin()
		if prev, ok := seen[id]; ok && prev == "bulk" {
			t.Fatalf("transaction id %d collides with a bulk window token: that "+
				"transaction could adopt the window's builder", id)
		}
		seen[id] = "tx"
		ws.End()
	}
}
