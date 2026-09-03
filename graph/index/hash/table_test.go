package hash

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// The tests in this file gate the OPEN-ADDRESSED SHARD TABLE that replaced the
// map-behind-an-RWMutex spine in rmp #2699. They exist because that structure
// introduces failure modes the semantic tests elsewhere in this package cannot
// see: every one of them still passes against a table whose probe chains have
// been severed, because a severed chain loses keys silently rather than
// reporting an error.
//
// The hazard is specific and it is the classic one for linear probing. A probe
// stops at the first nil slot, so a key K published at slot p is only reachable
// while every slot between hash(K) and p stays non-nil. Clearing a slot in the
// middle of that run — the obvious way to implement a delete — strands K, and K
// then reads as absent although nothing ever deleted it. [tombstone] is why that
// cannot happen here, and TestTable_ProbeChainSurvivesReap is what proves it.

// TestTable_ProbeChainSurvivesReap deletes half of a densely packed key space
// and asserts that every SURVIVING key is still reachable.
//
// It is the regression gate for severed probe chains, and it was VERIFIED to
// fail against the defect it guards rather than merely assumed to: replacing the
// tombstone store in [hashShard.reap] with a nil store makes this test report
// "1917 surviving keys became unreachable after reaping their neighbours" out of
// the 10 000 that survive. The same mutant fails four of the other five tests in
// this file. Two further mutants were run and caught (rmp #2699):
//
//   - making [hashShard.slotFor] probe PAST a tombstone that matches its key,
//     so a revive takes a fresh slot, fails TestTable_ReviveReusesTheSameSlot
//     and TestTable_ConcurrentChurnIsRaceFreeAndConsistent — the latter through
//     duplicateKeyCount, because the key then occupies two slots;
//   - making [hashShard.rehash] size the new table from used rather than from
//     the live count, and carry tombstones across, fails
//     TestTable_ChurnDoesNotGrowTheTableWithoutBound.
func TestTable_ProbeChainSurvivesReap(t *testing.T) {
	t.Parallel()
	const n = 20000

	idx := New[int64]()
	for i := range n {
		idx.Insert(int64(i), graph.NodeID(uint64(i)))
	}

	// Delete every other key. Each of these empties its entry, so each drives a
	// real reap and leaves a real tombstone.
	for i := 0; i < n; i += 2 {
		idx.Delete(int64(i), graph.NodeID(uint64(i)))
	}

	var stranded []int64
	for i := 1; i < n; i += 2 {
		if got := idx.Cardinality(int64(i)); got != 1 {
			stranded = append(stranded, int64(i))
		}
	}
	if len(stranded) != 0 {
		t.Fatalf("%d surviving keys became unreachable after reaping their neighbours; first few: %v",
			len(stranded), stranded[:min(len(stranded), 8)])
	}
	// The deleted half must be genuinely gone, or the test above would pass
	// against a reap that does nothing at all.
	for i := 0; i < n; i += 2 {
		if got := idx.Cardinality(int64(i)); got != 0 {
			t.Fatalf("Cardinality(%d) = %d after Delete, want 0", i, got)
		}
		if idx.keyPresent(int64(i)) {
			t.Fatalf("key %d still published after Delete", i)
		}
	}
	if dup := idx.duplicateKeyCount(); dup != 0 {
		t.Fatalf("duplicateKeyCount = %d, want 0", dup)
	}
	if bad := idx.loadFactorViolations(); bad != 0 {
		t.Fatalf("loadFactorViolations = %d, want 0", bad)
	}
	if bad, detail := idx.countingMismatches(); bad != 0 {
		t.Fatalf("used/tombs accounting wrong in %d shards: %s", bad, detail)
	}
}

// TestTable_ProbeChainSurvivesReap_StringKeys repeats the same proof for a
// non-numeric key, because a string key is stored by a two-word header and is
// the shape a torn key write would corrupt.
func TestTable_ProbeChainSurvivesReap_StringKeys(t *testing.T) {
	t.Parallel()
	const n = 8000

	idx := New[string]()
	key := func(i int) string { return fmt.Sprintf("value-%06d", i) }
	for i := range n {
		idx.Insert(key(i), graph.NodeID(uint64(i)))
	}
	for i := 0; i < n; i += 3 {
		idx.Delete(key(i), graph.NodeID(uint64(i)))
	}
	for i := range n {
		want := uint64(1)
		if i%3 == 0 {
			want = 0
		}
		if got := idx.Cardinality(key(i)); got != want {
			t.Fatalf("Cardinality(%q) = %d, want %d", key(i), got, want)
		}
	}
	if dup := idx.duplicateKeyCount(); dup != 0 {
		t.Fatalf("duplicateKeyCount = %d, want 0", dup)
	}
}

// TestTable_ReviveReusesTheSameSlot pins the rule that keeps a slot's key
// write-once: re-inserting a reaped key must revive that key's OWN tombstone
// rather than take a fresh slot.
//
// If a revive took a fresh slot instead, the key would occupy two slots and the
// tombstoned one would keep the key alive for ever; if a tombstone were handed
// to a DIFFERENT key, that key's write would race every reader holding the slot.
func TestTable_ReviveReusesTheSameSlot(t *testing.T) {
	t.Parallel()
	idx := New[int64]()
	const seeded = 8000
	for i := range seeded {
		idx.Insert(int64(i), graph.NodeID(uint64(i))) //nolint:gosec // G115: bounded loop index
	}

	// The key is SEARCHED FOR, not assumed. Two things have to hold for this
	// test to be measuring the revive path at all, and neither is a property of
	// any particular key: the shard must hold enough live keys that reaping one
	// does not trip [reclaimTrigger] — which would legitimately drop the
	// tombstone and leave nothing to revive — and it must therefore be found at
	// run time, because the shard seed is drawn per PROCESS
	// (maphash.MakeSeed), so key-to-shard assignment differs between runs.
	//
	// Asserting a fixed key here is exactly how this test broke when the
	// reclamation trigger landed: key 7's shard held two keys, the reap
	// reclaimed, and the failure looked like a defect in the revive path rather
	// than a stale premise in the test.
	k := int64(-1)
	for cand := range seeded {
		if _, used, tombs := idx.tableStats(int64(cand)); used-tombs >= 2*reclaimTrigger+1 {
			k = int64(cand)
			break
		}
	}
	if k < 0 {
		t.Fatalf("no shard held enough live keys to exercise a revive without tripping "+
			"reclaimTrigger=%d; seed %d keys was not enough", reclaimTrigger, seeded)
	}

	before, count := idx.slotIndexOf(int64(k))
	if count != 1 {
		t.Fatalf("key %d occupies %d slots before delete, want 1", k, count)
	}
	_, usedBefore, tombsBefore := idx.tableStats(int64(k))

	idx.Delete(k, graph.NodeID(uint64(k))) //nolint:gosec // G115: k is a bounded index
	_, usedAfterDel, tombsAfterDel := idx.tableStats(int64(k))
	if usedAfterDel != usedBefore {
		t.Fatalf("used = %d after reap, want it unchanged at %d: a tombstone still occupies its slot",
			usedAfterDel, usedBefore)
	}
	if tombsAfterDel != tombsBefore+1 {
		t.Fatalf("tombs = %d after reap, want %d", tombsAfterDel, tombsBefore+1)
	}

	idx.Insert(k, graph.NodeID(999))
	after, count := idx.slotIndexOf(int64(k))
	if count != 1 {
		t.Fatalf("key %d occupies %d slots after revive, want 1", k, count)
	}
	if after != before {
		t.Fatalf("revive moved key %d from slot %d to slot %d; it must reuse its own tombstone",
			k, before, after)
	}
	_, usedAfter, tombsAfter := idx.tableStats(int64(k))
	if usedAfter != usedBefore {
		t.Fatalf("used = %d after revive, want %d: reviving must not consume a second slot",
			usedAfter, usedBefore)
	}
	if tombsAfter != tombsBefore {
		t.Fatalf("tombs = %d after revive, want %d", tombsAfter, tombsBefore)
	}
	if got := idx.Cardinality(int64(k)); got != 1 {
		t.Fatalf("Cardinality(%d) = %d after revive, want 1", k, got)
	}
}

// TestTable_ChurnDoesNotGrowTheTableWithoutBound proves that tombstones are
// RECLAIMED. A table that only ever grew would turn a steady insert/delete
// workload — the normal life of a property index — into an unbounded memory
// leak, which Compliance Mandate 4 forbids outright.
func TestTable_ChurnDoesNotGrowTheTableWithoutBound(t *testing.T) {
	t.Parallel()
	const (
		live   = 2000
		rounds = 40
	)
	idx := New[int64]()
	for i := range live {
		idx.Insert(int64(i), graph.NodeID(uint64(i)))
	}
	settled := idx.maxTableSlots()

	// Each round retires the whole live set and replaces it with a disjoint
	// one, so every key ever used is distinct and every retirement tombstones.
	for r := 1; r <= rounds; r++ {
		base := int64(r * live)
		for i := range live {
			idx.Insert(base+int64(i), graph.NodeID(uint64(i)))
		}
		prev := base - live
		for i := range live {
			idx.Delete(prev+int64(i), graph.NodeID(uint64(i)))
		}
	}

	if got := idx.maxTableSlots(); got > settled*4 {
		t.Fatalf("largest table grew to %d slots after %d churn rounds over a %d-key live set "+
			"(settled at %d); tombstones are not being reclaimed",
			got, rounds, live, settled)
	}
	if dup := idx.duplicateKeyCount(); dup != 0 {
		t.Fatalf("duplicateKeyCount = %d, want 0", dup)
	}
	if bad := idx.loadFactorViolations(); bad != 0 {
		t.Fatalf("loadFactorViolations = %d, want 0", bad)
	}
	if bad, detail := idx.countingMismatches(); bad != 0 {
		t.Fatalf("used/tombs accounting wrong in %d shards: %s", bad, detail)
	}
	// The final live set must be intact and the retired ones gone.
	base := int64(rounds * live)
	for i := range live {
		if got := idx.Cardinality(base + int64(i)); got != 1 {
			t.Fatalf("Cardinality(%d) = %d, want 1", base+int64(i), got)
		}
		if got := idx.Cardinality(base - live + int64(i)); got != 0 {
			t.Fatalf("Cardinality(%d) = %d, want 0", base-live+int64(i), got)
		}
	}
}

// TestTable_GrowthPreservesEveryKey drives enough keys through one index to
// force many rehashes in every shard, and asserts the rehash loses nothing.
func TestTable_GrowthPreservesEveryKey(t *testing.T) {
	t.Parallel()
	const n = 60000
	idx := New[int64]()
	for i := range n {
		idx.Insert(int64(i), graph.NodeID(uint64(i)))
	}
	for i := range n {
		if got := idx.Cardinality(int64(i)); got != 1 {
			t.Fatalf("Cardinality(%d) = %d after growth, want 1", i, got)
		}
	}
	if got := idx.DistinctValues(); got != n {
		t.Fatalf("DistinctValues = %d, want %d", got, n)
	}
	if dup := idx.duplicateKeyCount(); dup != 0 {
		t.Fatalf("duplicateKeyCount = %d, want 0", dup)
	}
	if bad := idx.loadFactorViolations(); bad != 0 {
		t.Fatalf("loadFactorViolations = %d, want 0", bad)
	}
}

// TestTable_ConcurrentChurnIsRaceFreeAndConsistent runs readers against writers
// that create and reap keys, with STRING keys so that a torn key write — the
// hazard the write-once rule on [slot] exists to exclude — is visible to the
// race detector rather than merely improbable.
//
// Run it with -race; without the detector it still checks that no reader ever
// observes a key that no writer ever published.
func TestTable_ConcurrentChurnIsRaceFreeAndConsistent(t *testing.T) {
	t.Parallel()
	const (
		writers = 4
		readers = 4
		keys    = 3000
		rounds  = 40
	)
	idx := New[string]()
	key := func(w, i int) string { return fmt.Sprintf("w%02d-k%05d", w, i) }

	var writersWG, readersWG sync.WaitGroup
	stop := make(chan struct{})

	for w := range writers {
		writersWG.Add(1)
		go func(w int) {
			defer writersWG.Done()
			for r := range rounds {
				for i := range keys {
					idx.Insert(key(w, i), graph.NodeID(uint64(w*keys+i))) //nolint:gosec // G115: bounded loop indices
				}
				for i := range keys {
					if (i+r)%2 == 0 {
						idx.Delete(key(w, i), graph.NodeID(uint64(w*keys+i))) //nolint:gosec // G115: bounded loop indices
					}
				}
			}
		}(w)
	}

	var bogus atomic.Int64
	for g := range readers {
		readersWG.Add(1)
		go func(g int) {
			defer readersWG.Done()
			rng := rand.New(rand.NewPCG(uint64(g), 2))
			for {
				select {
				case <-stop:
					return
				default:
				}
				w := int(rng.UintN(writers))
				i := int(rng.UintN(keys))
				k := key(w, i)
				// Cardinality is 0 or 1 for these keys and nothing else: each
				// key is published by exactly one writer with exactly one node.
				if c := idx.Cardinality(k); c > 1 {
					bogus.Add(1)
				}
				// A key that reads present must read back its OWN node id. A
				// severed probe chain or a slot handed to another key would
				// answer with a different writer's id here.
				if bm := idx.Lookup(k); bm.GetCardinality() == 1 {
					if !bm.Contains(uint64(w*keys + i)) { //nolint:gosec // G115: bounded loop indices
						bogus.Add(1)
					}
				}
			}
		}(g)
	}

	writersWG.Wait()
	close(stop)
	readersWG.Wait()

	if n := bogus.Load(); n != 0 {
		t.Fatalf("%d impossible observations by concurrent readers", n)
	}
	if dup := idx.duplicateKeyCount(); dup != 0 {
		t.Fatalf("duplicateKeyCount = %d, want 0", dup)
	}
	if bad := idx.loadFactorViolations(); bad != 0 {
		t.Fatalf("loadFactorViolations = %d, want 0", bad)
	}
}

// TestTable_DeserializeOverTombstonesKeepsAccountingExact closes the coverage
// hole a mutation test walked straight through.
//
// Every other assertion in this file runs on an index built by Insert and
// Delete. [Index.Deserialize] REPLACES a shard's table wholesale and restates
// used and tombs by assignment rather than by increment, so it is the one place
// the accounting can be set to an arbitrary wrong value in a single line — and
// mutating its `s.used = len(fresh[k])` to `s.used = 0` left the whole package
// suite green before this test existed. The undercount then stops
// [hashShard.slotFor] rehashing, fills the table, and spins [hashShard.find] for
// ever while holding the shard's writer lock.
//
// The source index is deliberately CHURNED first, so the displaced tables carry
// real tombstones and the swap is not exercised against a pristine one.
func TestTable_DeserializeOverTombstonesKeepsAccountingExact(t *testing.T) {
	t.Parallel()
	const n = 4000

	src := New[int64]()
	for i := range n {
		src.Insert(int64(i), graph.NodeID(uint64(i))) //nolint:gosec // G115: bounded loop index
	}

	// Churn the DESTINATION so its tables are full of tombstones when the swap
	// lands on them.
	dst := New[int64]()
	for i := range n {
		dst.Insert(int64(i+n), graph.NodeID(uint64(i))) //nolint:gosec // G115: bounded loop index
	}
	for i := range n {
		if i%2 == 0 {
			dst.Delete(int64(i+n), graph.NodeID(uint64(i))) //nolint:gosec // G115: bounded loop index
		}
	}
	if bad, detail := dst.countingMismatches(); bad != 0 {
		t.Fatalf("pre-condition: accounting already wrong in %d shards: %s", bad, detail)
	}

	var buf bytes.Buffer
	if err := src.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	if err := dst.Deserialize(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}

	if bad, detail := dst.countingMismatches(); bad != 0 {
		t.Fatalf("used/tombs accounting wrong after Deserialize in %d shards: %s", bad, detail)
	}
	if bad := dst.loadFactorViolations(); bad != 0 {
		t.Fatalf("loadFactorViolations = %d, want 0", bad)
	}
	if dup := dst.duplicateKeyCount(); dup != 0 {
		t.Fatalf("duplicateKeyCount = %d, want 0", dup)
	}
	if got := dst.DistinctValues(); got != n {
		t.Fatalf("DistinctValues = %d, want %d", got, n)
	}

	// Keep inserting AFTER the swap. An undercounted used only becomes a spin
	// once the table it lies about actually fills, so the assertions above
	// cannot catch it on their own — this is what turns the lie into a hang,
	// and the test times out rather than failing if the accounting is wrong.
	for i := range n {
		dst.Insert(int64(i+2*n), graph.NodeID(uint64(i))) //nolint:gosec // G115: bounded loop index
	}
	if bad, detail := dst.countingMismatches(); bad != 0 {
		t.Fatalf("used/tombs accounting wrong after post-swap inserts in %d shards: %s", bad, detail)
	}
	if bad := dst.loadFactorViolations(); bad != 0 {
		t.Fatalf("loadFactorViolations = %d, want 0", bad)
	}
	for i := range n {
		if got := dst.Cardinality(int64(i)); got != 1 {
			t.Fatalf("Cardinality(%d) = %d after Deserialize, want 1", i, got)
		}
	}
}

// TestTable_HydrationDoesNotAllocateEveryShard is a MEMORY gate, under
// Compliance Mandate 4.
//
// Hydration is the path by which every index in the engine is born
// (cypher/index_hydration.go), and a real index is far narrower than
// shardCount. [Index.Deserialize] allocated a table unconditionally per shard
// until rmp #2699 measured it: a TWO-KEY index came back from the wire holding
// 2048 slots across all 256 shards — 32 KB for an int64 key, and more for a
// wider one — where 8 slots in one shard were called for.
func TestTable_HydrationDoesNotAllocateEveryShard(t *testing.T) {
	t.Parallel()
	src := New[int64]()
	src.Insert(1, graph.NodeID(1))
	src.Insert(2, graph.NodeID(2))

	var buf bytes.Buffer
	if err := src.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	dst := New[int64]()
	if err := dst.Deserialize(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}

	tables, slots := dst.tablesAllocated()
	// Two keys can land in one shard or two, never more.
	if tables > 2 {
		t.Fatalf("hydrating a 2-key index allocated %d shard tables (%d slots); "+
			"want at most 2 — Deserialize must publish nil for a shard it gives no key",
			tables, slots)
	}
	if slots > 2*minTableSlots {
		t.Fatalf("hydrating a 2-key index allocated %d slots, want at most %d", slots, 2*minTableSlots)
	}
	if got := dst.DistinctValues(); got != 2 {
		t.Fatalf("DistinctValues = %d, want 2", got)
	}
	if got := dst.Cardinality(1); got != 1 {
		t.Fatalf("Cardinality(1) = %d, want 1", got)
	}
	if got := dst.Cardinality(2); got != 1 {
		t.Fatalf("Cardinality(2) = %d, want 1", got)
	}
	// A shard with no table must still answer, and must still accept a key.
	if got := dst.Cardinality(999999); got != 0 {
		t.Fatalf("Cardinality of an absent key in a table-less shard = %d, want 0", got)
	}
	dst.Insert(999999, graph.NodeID(7))
	if got := dst.Cardinality(999999); got != 1 {
		t.Fatalf("Cardinality after inserting into a table-less shard = %d, want 1", got)
	}
}

// TestTable_ShardAndSlotSizesArePinned guards the widths the spine's documented
// decisions are argued from.
//
// The shardCount block reasons from a 10 240-byte shard array staying resident
// in L1, and [slot]'s geometry is argued from eight int64 slots filling one
// 128-byte line. Both are load-bearing numbers and both are silently breakable
// by adding one field, so they are asserted rather than trusted. This is the
// same guard TestEntrySizeClass provides for the entry.
func TestTable_ShardAndSlotSizesArePinned(t *testing.T) {
	t.Parallel()
	if got := unsafe.Sizeof(hashShard[int64]{}); got != 40 {
		t.Errorf("unsafe.Sizeof(hashShard[int64]) = %d, want 40; the shardCount "+
			"rationale is argued from a %d-byte shard array", got, 40*shardCount)
	}
	if got := unsafe.Sizeof(hashShard[string]{}); got != 40 {
		t.Errorf("unsafe.Sizeof(hashShard[string]) = %d, want 40 (the key type "+
			"lives in the table, not the shard)", got)
	}
	if got := unsafe.Sizeof(slot[int64]{}); got != 16 {
		t.Errorf("unsafe.Sizeof(slot[int64]) = %d, want 16; eight per 128-byte "+
			"line is the probe-locality argument on [slot]", got)
	}
	if got := minTableSlots * unsafe.Sizeof(slot[int64]{}); got != 128 {
		t.Errorf("smallest int64 table = %d bytes, want 128 (one cache line)", got)
	}
	// The load-factor ceiling must leave at least one nil slot in the smallest
	// table, or find's probe loop could not terminate in it.
	if minTableSlots*loadNum/loadDen >= minTableSlots {
		t.Fatalf("load factor %d/%d leaves no free slot in a %d-slot table",
			loadNum, loadDen, minTableSlots)
	}
}

// TestTable_EmptiedIndexReleasesItsSlots is the regression gate for the
// tombstone-retention defect rmp #2699 introduced and closed in the same cycle.
//
// The map spine this replaced freed a key the instant Delete removed the last
// NodeID: `delete(s.entries, value)` dropped the map slot and the key with it.
// The open-addressed table cannot do that — clearing a slot severs the probe
// chains running through it (see [tombstone]) — so a reap leaves a tombstone
// that still holds a reference to the key.
//
// Reclaiming those tombstones was, at first, the job of [hashShard.rehash]
// alone, and rehash has exactly ONE caller: [hashShard.slotFor], reached only on
// the Insert path. A shard that is emptied and never written to again therefore
// retained EVERY key for the life of the index — unbounded in time, which
// Compliance Mandate 4 forbids. String keys make the retention concrete; the
// probe-length degradation it also caused is measured in [hashShard.reap].
//
// Verified to FAIL before the fix: without the reclamation in reap this reports
// 4000 slots still occupied by 4000 tombstones and 256 tables still allocated.
func TestTable_EmptiedIndexReleasesItsSlots(t *testing.T) {
	t.Parallel()
	const n = 4000

	idx := New[string]()
	keys := make([]string, n)
	for i := range n {
		// A wide key, so that retaining it retains something worth measuring.
		keys[i] = strings.Repeat("x", 64) + fmt.Sprintf("%06d", i)
	}
	for i := range n {
		idx.Insert(keys[i], graph.NodeID(uint64(i))) //nolint:gosec // G115: bounded loop index
	}
	if used, _ := idx.occupancy(); used != n {
		t.Fatalf("pre-condition: occupancy used=%d, want %d", used, n)
	}

	for i := range n {
		idx.Delete(keys[i], graph.NodeID(uint64(i))) //nolint:gosec // G115: bounded loop index
	}

	if got := idx.DistinctValues(); got != 0 {
		t.Fatalf("DistinctValues = %d after emptying, want 0", got)
	}
	used, tombs := idx.occupancy()
	if used != 0 || tombs != 0 {
		t.Errorf("after emptying the index, %d slots are still occupied (%d of them tombstones); "+
			"every key they carry is retained for the life of the index", used, tombs)
	}
	if tables, slots := idx.tablesAllocated(); tables != 0 {
		t.Errorf("after emptying the index, %d shard tables (%d slots) are still allocated, want 0",
			tables, slots)
	}
	if bad, detail := idx.countingMismatches(); bad != 0 {
		t.Fatalf("used/tombs accounting wrong in %d shards: %s", bad, detail)
	}
	// The index must remain fully usable afterwards.
	idx.Insert(keys[0], graph.NodeID(1))
	if got := idx.Cardinality(keys[0]); got != 1 {
		t.Fatalf("Cardinality after re-insert into a released shard = %d, want 1", got)
	}
}

// TestTable_FindPanicsRatherThanSpinningOnACorruptTable proves the READ path
// fails STOP rather than fail-silent or, worse, not at all.
//
// [hashShard.find] probes until it meets a nil slot, and the load factor
// guarantees one exists. If the maintained used count ever drifts below the
// table's true occupancy the table can fill completely, and the unbounded loop
// this replaced then spun at 100% CPU for ever — demonstrated, not supposed:
// with every slot occupied, a probe for an absent key in that shard did not
// return within two seconds.
//
// Returning "not found" instead would be worse than the panic, not better: it
// answers ABSENT for a key that is present, which is fail-silent — forbidden by
// name in CLAUDE.md — and a Consistency violation under Compliance Mandate 2
// that no caller can detect.
//
// Verified to FAIL without the bound: with the probe limit removed from find,
// this test does not fail, it HANGS, and the package run dies on its -timeout.
func TestTable_FindPanicsRatherThanSpinningOnACorruptTable(t *testing.T) {
	t.Parallel()
	idx := New[int64]()
	idx.Insert(1, graph.NodeID(1))
	s, _ := idx.locate(int64(1))
	tb := s.tbl.Load()

	// Fill EVERY slot, exactly as an undercounted used eventually would. The
	// counters are deliberately left alone: the drift between them and the
	// structure IS the corruption being simulated.
	for i := range tb.slots {
		if tb.slots[i].e.Load() == nil {
			tb.slots[i].key = int64(-1000 - i)
			tb.slots[i].e.Store(tombstone)
		}
	}
	for i := range tb.slots {
		if tb.slots[i].e.Load() == nil {
			t.Fatalf("pre-condition: slot %d is still free; the table was not filled", i)
		}
	}

	// An ABSENT key that maps to the FILLED shard. Probing any other shard
	// would terminate immediately and prove nothing, which is how the first
	// version of this experiment fooled itself.
	target := int64(-1)
	for c := int64(1000); c < 2_000_000; c++ {
		if cs, _ := idx.locate(c); cs == s {
			target = c
			break
		}
	}
	if target < 0 {
		t.Fatal("no candidate key mapped to the filled shard")
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Errorf("Cardinality returned normally on a table with no free slot; "+
				"find must panic rather than answer, or spin (target=%d)", target)
			return
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "shard table corrupt") {
			t.Errorf("panicked with %#v, want a message naming the corrupt shard table", r)
		}
		if !strings.Contains(msg, "used=") || !strings.Contains(msg, "tombs=") {
			t.Errorf("panic message %q does not name used/tombs; it is the only diagnostic "+
				"an operator gets for this failure", msg)
		}
	}()
	_ = idx.Cardinality(target)
}
