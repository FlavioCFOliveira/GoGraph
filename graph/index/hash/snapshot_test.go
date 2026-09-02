package hash

import (
	"bytes"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// TestInlineTierMaxMatchesNodeSet pins [inlineTierMax] against index's own
// inline/bitmap threshold through the EXPORTED constructors.
//
// The hash package derives [snapshot.shared] from the cardinality rather than
// from index.NodeSet's state tag, which is unexported (see [inlineTierMax] for
// why asking would cost more than the read it guards). That derivation is only
// correct while the two thresholds agree. Nothing in the compiler enforces the
// agreement: nodeset.go's smallSetMax could move and this package would keep
// labelling bitmap-tier sets shared == false, handing a MUTABLE roaring bitmap
// to a reader that takes no lock — a data race the type system cannot see.
//
// So the correspondence is asserted, in both directions, from behaviour rather
// than from a shared constant.
func TestInlineTierMaxMatchesNodeSet(t *testing.T) {
	t.Parallel()

	ids := make([]uint64, 0, inlineTierMax+1)
	for i := range inlineTierMax + 1 {
		ids = append(ids, uint64(i)*10)
	}

	at := index.NodeSetFromSorted(ids[:inlineTierMax])
	if _, shared := at.Bitmap(); shared {
		t.Fatalf("a NodeSet of exactly %d ids is ALREADY on the bitmap tier: "+
			"inlineTierMax is too high, so every key at that cardinality is "+
			"mis-labelled immutable and read with no lock while a writer mutates it",
			inlineTierMax)
	}
	over := index.NodeSetFromSorted(ids)
	if _, shared := over.Bitmap(); !shared {
		t.Fatalf("a NodeSet of %d ids is NOT on the bitmap tier: inlineTierMax is "+
			"too low, so keys are needlessly sent through the entry read lock",
			inlineTierMax+1)
	}
}

// TestSnapshotSharedMatchesTier sweeps a key's cardinality across the promotion
// boundary and asserts that the flag the read path trusts — [snapshot.shared] —
// equals the set's ACTUAL tier at every step.
//
// This is the invariant the whole lock-free read path rests on. A snapshot that
// claims shared == false while its set aliases a live *roaring64.Bitmap is read
// with no synchronisation at all, concurrently with a writer mutating that
// bitmap in place. The sweep covers the boundary from both sides, so a
// threshold that is off by one in either direction fails here rather than
// showing up as an intermittent race in a consumer package.
func TestSnapshotSharedMatchesTier(t *testing.T) {
	t.Parallel()

	for card := 1; card <= inlineTierMax+4; card++ {
		t.Run(fmt.Sprintf("card=%d", card), func(t *testing.T) {
			t.Parallel()

			idx := New[int64]()
			const value = int64(1)
			for n := range card {
				idx.Insert(value, graph.NodeID(uint64(n)+1))
			}
			e, ok := idx.entryFor(value)
			if !ok {
				t.Fatal("entryFor found no published entry")
			}
			sn, viaSnapshot := e.loadImage()
			// Round three: the two cheapest tiers are carried by the meta word
			// ALONE, so a snapshot at cardinality 0 or 1 is the memory
			// regression this design exists to remove, and the absence of one
			// above the bound would mean the image was lost entirely.
			if want := card > 1; viaSnapshot != want {
				t.Fatalf("cardinality %d publishes through a snapshot = %v, want %v",
					card, viaSnapshot, want)
			}
			if got := sn.set.Cardinality(); got != uint64(card) {
				t.Fatalf("published cardinality = %d, want %d", got, card)
			}
			// The ground truth: Bitmap's shared return is true only on the
			// bitmap tier. It allocates for the inline tiers, which is exactly
			// why the read path may not call it — but a test may.
			_, actuallyShared := sn.set.Bitmap()
			if sn.shared != actuallyShared {
				t.Fatalf("snapshot.shared = %v at cardinality %d but the set's real "+
					"tier is shared = %v: shared==false on a bitmap-tier set hands a "+
					"MUTABLE bitmap to a reader that takes no lock",
					sn.shared, card, actuallyShared)
			}
			if want := card > inlineTierMax; sn.shared != want {
				t.Fatalf("snapshot.shared = %v at cardinality %d, want %v",
					sn.shared, card, want)
			}
		})
	}
}

// TestPromotionKeepsStaleInlineSnapshotIntact is tier-transition 1 on [entry]:
// INLINE -> BITMAP while a reader still holds the old pointer.
//
// The promoting writer must build the new bitmap from a COPY and leave the old
// inline image alone, because a reader that loaded the old pointer is reading
// that image RIGHT NOW with no lock at all — Go's GC keeps it alive for exactly
// as long as that reader. The reader's answer is then slightly stale, which is
// the same staleness the per-entry RWMutex of round one already permitted for a
// read completing just before a concurrent insert; nothing here is a new
// weakening.
//
// The window is CONSTRUCTED rather than raced for: the test holds the pointer a
// mid-flight reader would be holding, so the assertion is arithmetic instead of
// scheduler-dependent.
func TestPromotionKeepsStaleInlineSnapshotIntact(t *testing.T) {
	t.Parallel()

	idx := New[int64]()
	const value = int64(1)
	for n := range inlineTierMax {
		idx.Insert(value, graph.NodeID(uint64(n)+1))
	}
	e, ok := idx.entryFor(value)
	if !ok {
		t.Fatal("entryFor found no published entry")
	}
	stale, _ := e.loadImage()
	if stale.shared {
		t.Fatalf("setup: a %d-id list is already shared, so this test would not "+
			"exercise a promotion at all", inlineTierMax)
	}
	before := stale.set.AppendTo(nil)
	if len(before) != inlineTierMax {
		t.Fatalf("setup: stale image holds %d ids, want %d", len(before), inlineTierMax)
	}

	// Cross the bound: this is the promotion.
	idx.Insert(value, graph.NodeID(uint64(inlineTierMax)+1))

	fresh, _ := e.loadImage()
	if fresh == stale {
		t.Fatal("the promotion published no new image: either the write mutated " +
			"the published image in place, or this test is not seeing the transition " +
			"it exists to check")
	}
	if !fresh.shared {
		t.Fatalf("the promoted snapshot at cardinality %d is not marked shared: a "+
			"reader will read its live bitmap with no lock", fresh.set.Cardinality())
	}
	if after := stale.set.AppendTo(nil); !slices.Equal(before, after) {
		t.Fatalf("the promotion MUTATED the image a reader still holds: %v -> %v; "+
			"copy-on-write is what makes the lock-free read path safe", before, after)
	}
	if got := stale.set.Cardinality(); got != uint64(inlineTierMax) {
		t.Fatalf("stale image cardinality moved to %d, want %d", got, inlineTierMax)
	}
	if got := idx.Cardinality(value); got != uint64(inlineTierMax)+1 {
		t.Fatalf("Cardinality after the promotion = %d, want %d", got, inlineTierMax+1)
	}
}

// TestDemotionPublishesInlineSnapshotAndFreezesOldBitmap is tier-transition 2 on
// [entry]: BITMAP -> INLINE, when a [Index.Delete] falls back to
// [inlineTierMax].
//
// Two things must hold. The demotion must actually happen — index.NodeSet never
// demotes on its own, so without it a churned key keeps a bitmap-tier set for
// ever, stays on the locked read path, and [entry.publish]'s cardinality -> tier
// derivation stops being total. And the bitmap the demotion abandons must be
// FROZEN from then on, because a reader that loaded the pre-demotion snapshot is
// still reaching that bitmap through the pointer it holds.
func TestDemotionPublishesInlineSnapshotAndFreezesOldBitmap(t *testing.T) {
	t.Parallel()

	idx := New[int64]()
	const value = int64(2)
	const total = inlineTierMax + 1 // the smallest bitmap-tier list
	for n := range total {
		idx.Insert(value, graph.NodeID(uint64(n)+1))
	}
	e, ok := idx.entryFor(value)
	if !ok {
		t.Fatal("entryFor found no published entry")
	}
	sharedSnap, _ := e.loadImage()
	if !sharedSnap.shared {
		t.Fatalf("setup: a %d-id list is not on the bitmap tier, so there is no "+
			"demotion to observe", total)
	}
	bm, isShared := sharedSnap.set.Bitmap()
	if !isShared {
		t.Fatal("setup: the snapshot claims shared but Bitmap disagrees")
	}
	// Captured BEFORE the demoting delete, so the expected post-delete contents
	// are derived rather than read back out of the object under test. Capturing
	// afterwards would blind this test to a write the demoting delete itself
	// makes to the bitmap it is abandoning.
	beforeDelete := bm.ToArray()

	// One removal falls back to the bound: this is the demotion.
	idx.Delete(value, graph.NodeID(uint64(total)))

	// The demoting delete removes exactly the one id, and must do nothing else
	// to the bitmap it then abandons.
	wantFrozen := slices.DeleteFunc(slices.Clone(beforeDelete),
		func(id uint64) bool { return id == uint64(total) })
	frozen := bm.ToArray()
	if !slices.Equal(wantFrozen, frozen) {
		t.Fatalf("the demoting delete left the bitmap it abandons holding %v, want %v: "+
			"it touched that bitmap after publishing the inline snapshot, and a reader "+
			"inside mu.RLock() on the pre-demotion snapshot is still reading it",
			frozen, wantFrozen)
	}

	demoted, viaSnapshot := e.loadImage()
	if demoted == sharedSnap {
		t.Fatal("the delete published no new image: the list stayed on the " +
			"bitmap tier at inlineTierMax, so the key never regains the lock-free " +
			"read path and publish's tier derivation is no longer total")
	}
	if !viaSnapshot {
		t.Fatalf("a %d-id list was published inline: only cardinality 0 and 1 fit "+
			"the meta word", inlineTierMax)
	}
	if demoted.shared {
		t.Fatalf("the demoted snapshot at cardinality %d is still marked shared",
			demoted.set.Cardinality())
	}
	if got := demoted.set.Cardinality(); got != uint64(inlineTierMax) {
		t.Fatalf("demoted cardinality = %d, want %d", got, inlineTierMax)
	}

	// And it must not move again, however much the key churns.
	idx.Insert(value, graph.NodeID(uint64(total)))   // promotes again, new bitmap
	idx.Delete(value, graph.NodeID(uint64(total)))   // demotes again
	idx.Insert(value, graph.NodeID(uint64(total)+1)) // promotes again
	if now := bm.ToArray(); !slices.Equal(frozen, now) {
		t.Fatalf("a writer kept mutating the bitmap the demotion abandoned: %v -> %v; "+
			"a reader inside mu.RLock() on the pre-demotion snapshot is still reading it",
			frozen, now)
	}
}

// TestStaleSnapshotSurvivesReap is tier-transition 4 on [entry]: a reader
// holding a non-shared snapshot concurrent with a reap.
//
// [hashShard.reap] marks the entry dead and unlinks it from the shard map, but
// it must not disturb the entry's published image: a reader that already
// resolved the image is reading it with no lock, and a reader that loaded the
// ENTRY but not yet its image resolves it AFTER the reap. So the pre-reap
// contents must still be there on both routes — through the image a reader
// already holds, and through the entry a late reader is only now resolving.
func TestStaleSnapshotSurvivesReap(t *testing.T) {
	t.Parallel()

	idx := New[int64]()
	const value = int64(3)
	idx.Insert(value, graph.NodeID(7))
	e, ok := idx.entryFor(value)
	if !ok {
		t.Fatal("entryFor found no published entry")
	}
	stale, viaSnapshot := e.loadImage()
	if stale.shared {
		t.Fatal("setup: a singleton list is not supposed to be on the bitmap tier")
	}
	if viaSnapshot {
		t.Fatal("setup: a singleton published a snapshot, so this test is not " +
			"exercising the inline image round three put it on")
	}

	idx.Delete(value, graph.NodeID(7)) // empties the list, so the key is reaped

	if idx.keyPresent(value) {
		t.Fatal("Delete did not reap the emptied key: the reap this test observes " +
			"never ran, so the test is vacuous")
	}
	if !e.isDead() {
		t.Fatal("the reaped entry was not marked dead")
	}
	// The LATE reader's route: resolving the entry now, after the reap, must
	// TERMINATE and must answer exactly as an absent value does. The emptying
	// Delete published the metaEmpty tag and released the snapshot, so there is
	// no pointer left for such a reader to dereference — and no pointer tag
	// left for it to resolve against a snapshot that is gone, which is the
	// state that would park it for ever.
	post, postViaSnapshot := e.loadImage()
	if postViaSnapshot {
		t.Fatal("the reaped entry still publishes through a snapshot: an emptied " +
			"entry carries its image in the meta word, and retaining the snapshot " +
			"it superseded is the round-three memory regression")
	}
	if !post.set.IsEmpty() {
		t.Fatalf("the reaped entry publishes %v, want an empty image",
			post.set.AppendTo(nil))
	}
	if got := stale.set.AppendTo(nil); len(got) != 1 || got[0] != 7 {
		t.Fatalf("the reap disturbed the image a reader still holds: got %v, want [7]", got)
	}
	if got := stale.set.Cardinality(); got != 1 {
		t.Fatalf("stale image cardinality = %d, want 1", got)
	}
	// And the index answers exactly as it does for an absent value.
	if got := idx.Cardinality(value); got != 0 {
		t.Fatalf("Cardinality of the reaped value = %d, want 0", got)
	}
	if got := idx.LookupAppend(value, nil); len(got) != 0 {
		t.Fatalf("LookupAppend of the reaped value = %v, want empty", got)
	}
	if idx.Contains(value, graph.NodeID(7)) {
		t.Fatal("Contains still reports the reaped value's node")
	}
}

// TestEveryPublishedEntryResolvesToAnImage pins the read path's resolution
// precondition across every construction route.
//
// [Index.Lookup], [Index.LookupAppend], [Index.Cardinality] and
// [Index.Contains] resolve an entry from [entry.meta] and load [entry.snap]
// only when the tag says there is one. Two states break that: an INLINE tag
// with a snapshot still attached (correct answers, and the whole round-three
// memory win handed back silently), and a POINTER tag with no snapshot at rest
// (a reader spins for ever). Neither is observable through any exported
// operation, so every route that can put an entry into a shard map is exercised
// and the whole index swept directly. See [Index.imageTagMismatches].
func TestEveryPublishedEntryResolvesToAnImage(t *testing.T) {
	t.Parallel()

	idx := New[int64]()
	// Route 1: Insert creating a brand-new key, at several tiers.
	for v := int64(0); v < 64; v++ {
		for n := range int(v%12) + 1 {
			idx.Insert(v, graph.NodeID(uint64(n)+1))
		}
	}
	// Route 2: an entry emptied but not reaped (the reap declines on a stale
	// pointer), which is the empty-but-present third state.
	idx.Insert(1000, graph.NodeID(1))
	e, ok := idx.entryFor(1000)
	if !ok {
		t.Fatal("entryFor(1000) found no published entry")
	}
	idx.Delete(1000, graph.NodeID(1))
	idx.Insert(1000, graph.NodeID(2))
	fresh, ok := idx.entryFor(1000)
	if !ok {
		t.Fatal("entryFor(1000) found no re-created entry")
	}
	fresh.mu.Lock()
	fresh.deleteLocked(2)
	fresh.mu.Unlock()
	idx.shard(int64(1000)).nonEmpty.Add(-1)
	idx.shard(int64(1000)).reap(1000, e) // stale pointer: declines
	if !idx.keyPresent(1000) {
		t.Fatal("could not construct an empty-but-present entry")
	}
	mism, inline, viaSnap := idx.imageTagMismatches()
	if mism != 0 {
		t.Fatalf("%d live entries publish a meta tag that disagrees with their "+
			"snapshot after Insert/Delete (%d inline, %d via a snapshot)",
			mism, inline, viaSnap)
	}
	// Non-vacuity in BOTH directions: the workload builds keys at 12 different
	// cardinalities, so it must have produced entries of each kind, or the sweep
	// checked only one of the two states it exists to catch.
	if inline == 0 || viaSnap == 0 {
		t.Fatalf("the sweep saw %d inline entries and %d with a snapshot: it needs "+
			"both to have checked both states", inline, viaSnap)
	}

	// Route 3: Deserialize, which builds a whole new set of entries.
	var buf bytes.Buffer
	if err := idx.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	dst := New[int64]()
	if err := dst.Deserialize(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if dst.DistinctValues() == 0 {
		t.Fatal("the deserialised index is empty: the sweep below is vacuous")
	}
	mism, inline, viaSnap = dst.imageTagMismatches()
	if mism != 0 {
		t.Fatalf("%d live entries publish a meta tag that disagrees with their "+
			"snapshot after Deserialize (%d inline, %d via a snapshot)",
			mism, inline, viaSnap)
	}
	if inline == 0 || viaSnap == 0 {
		t.Fatalf("the deserialised sweep saw %d inline entries and %d with a "+
			"snapshot: it needs both", inline, viaSnap)
	}
}

// TestSharedSnapshotReadsStayConsistentAcrossTierChurn drives all four read
// paths against a writer that crosses the promotion boundary in BOTH
// directions, which is the window tier-transition 3 on [entry] describes: a
// reader loads a shared snapshot and the writer replaces it before the reader
// takes RLock.
//
// Every read must be internally consistent — ascending, within the legal id
// space, and containing every id that is never deleted — whichever side of the
// boundary it lands on. Under -race it additionally proves that no lock-free
// read ever reaches a bitmap a writer is mutating, which is the failure a
// mis-set [snapshot.shared] produces.
//
// # Each branch is CONSTRUCTED, never sampled
//
// The first version of this test counted how many reads happened to land while
// the churning key's snapshot was shared, and required that count to be
// non-zero. That clause failed a clean build under -race: the race detector
// slows the writer far more than the readers, so the promoted window shrank
// until no reader sampled it, and a coverage gate failed a run in which nothing
// was wrong. A non-vacuity clause that depends on the scheduler is not a
// verdict.
//
// So the three things this test needs are each guaranteed by construction
// instead:
//
//   - sharedValue holds enough ids to sit permanently on the BITMAP tier and is
//     only ever mutated in place, so every read of it takes the LOCKED branch.
//     Asserted at setup and re-asserted inside the reader loop.
//   - inlineValue holds two ids and is never written, so every read of it takes
//     the LOCK-FREE branch, and its contents are a constant the readers check
//     exactly.
//   - churnValue crosses the bound in both directions, and the non-vacuity
//     evidence for that is counted BY THE WRITER, which cannot miss its own
//     transitions.
//
// Every reader also runs its body at least once before testing the stop flag,
// so a reader cannot contribute zero observations by starting late.
func TestSharedSnapshotReadsStayConsistentAcrossTierChurn(t *testing.T) {
	t.Parallel()

	const (
		churnValue  = int64(4)
		sharedValue = int64(5)
		inlineValue = int64(6)
		base        = inlineTierMax - 1 // churnValue ids 1..7, never deleted
		churn       = 6                 // churnValue ids 8..13, added and removed
		wide        = inlineTierMax + 8 // sharedValue: always the bitmap tier
		inlineIDs   = 2                 // inlineValue: always an inline tier
		rounds      = 100
		readers     = 4
	)

	idx := New[int64]()
	for n := range base {
		idx.Insert(churnValue, graph.NodeID(uint64(n)+1))
	}
	for n := range wide {
		idx.Insert(sharedValue, graph.NodeID(uint64(n)+1))
	}
	for n := range inlineIDs {
		idx.Insert(inlineValue, graph.NodeID(uint64(n)+1))
	}

	churnEntry, ok := idx.entryFor(churnValue)
	if !ok {
		t.Fatal("entryFor(churnValue) found no published entry")
	}
	sharedEntry, ok := idx.entryFor(sharedValue)
	if !ok {
		t.Fatal("entryFor(sharedValue) found no published entry")
	}
	inlineEntry, ok := idx.entryFor(inlineValue)
	if !ok {
		t.Fatal("entryFor(inlineValue) found no published entry")
	}
	if !sharedEntry.isSharedTier() {
		t.Fatalf("setup: the %d-id control key is not on the bitmap tier, so no read "+
			"in this test would take the locked branch", wide)
	}
	if inlineEntry.isSharedTier() {
		t.Fatalf("setup: the %d-id control key is on the bitmap tier, so no read "+
			"in this test would take the lock-free branch", inlineIDs)
	}

	var (
		stop        atomic.Bool
		promotions  atomic.Int64
		demotions   atomic.Int64
		sharedReads atomic.Int64
		inlineReads atomic.Int64
		wg          sync.WaitGroup
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer stop.Store(true)
		for r := range rounds {
			// Cross the bound upwards, then back down.
			for n := range churn {
				idx.Insert(churnValue, graph.NodeID(uint64(base+n)+1))
			}
			if churnEntry.isSharedTier() {
				promotions.Add(1)
			}
			for n := range churn {
				idx.Delete(churnValue, graph.NodeID(uint64(base+n)+1))
			}
			if !churnEntry.isSharedTier() {
				demotions.Add(1)
			}
			// Mutate the permanently-shared key in place, well above the bound,
			// so concurrent readers of it are exercising the locked branch
			// against a bitmap that really is moving under them.
			extra := graph.NodeID(uint64(1000 + r))
			idx.Insert(sharedValue, extra)
			idx.Delete(sharedValue, extra)
		}
	}()

	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]uint64, 0, wide+1)
			for {
				// ---- the LOCKED branch, by construction ----
				if !sharedEntry.isSharedTier() {
					t.Errorf("the control key left the bitmap tier: reads of it are no " +
						"longer exercising the locked branch")
					return
				}
				sharedReads.Add(1)
				ids := idx.LookupAppend(sharedValue, buf[:0])
				if len(ids) < wide || len(ids) > wide+1 {
					t.Errorf("shared key: LookupAppend returned %d ids, legal range is "+
						"[%d, %d]", len(ids), wide, wide+1)
					return
				}
				for j := 1; j < len(ids); j++ {
					if ids[j] <= ids[j-1] {
						t.Errorf("shared key: non-ascending image %v: a read observed a "+
							"mixture of two states", ids)
						return
					}
				}
				if c := idx.Cardinality(sharedValue); c < wide || c > wide+1 {
					t.Errorf("shared key: Cardinality = %d, legal range is [%d, %d]",
						c, wide, wide+1)
					return
				}
				if !idx.Contains(sharedValue, graph.NodeID(1)) {
					t.Error("shared key: Contains lost never-deleted id 1")
					return
				}

				// ---- the LOCK-FREE branch, by construction ----
				inlineReads.Add(1)
				got := idx.LookupAppend(inlineValue, buf[:0])
				if len(got) != inlineIDs || got[0] != 1 || got[1] != 2 {
					t.Errorf("untouched inline key: LookupAppend = %v, want [1 2]: a "+
						"lock-free read of a key no writer touches must be exact", got)
					return
				}
				if c := idx.Cardinality(inlineValue); c != inlineIDs {
					t.Errorf("untouched inline key: Cardinality = %d, want %d", c, inlineIDs)
					return
				}

				// ---- the key crossing the bound, whichever branch it is on ----
				ids = idx.LookupAppend(churnValue, buf[:0])
				if len(ids) < base || len(ids) > base+churn {
					t.Errorf("churn key: LookupAppend returned %d ids, legal range is "+
						"[%d, %d]", len(ids), base, base+churn)
					return
				}
				for j := 1; j < len(ids); j++ {
					if ids[j] <= ids[j-1] {
						t.Errorf("churn key: non-ascending image %v: a read observed a "+
							"mixture of two published images", ids)
						return
					}
				}
				for _, id := range ids {
					if id < 1 || id > uint64(base+churn) {
						t.Errorf("churn key: id %d is outside the legal space [1, %d]: the "+
							"read tore across a tier transition", id, base+churn)
						return
					}
				}
				for n := range base {
					if !slices.Contains(ids, uint64(n)+1) {
						t.Errorf("churn key: image %v is missing never-deleted id %d",
							ids, n+1)
						return
					}
				}
				if c := idx.Cardinality(churnValue); c < base || c > base+churn {
					t.Errorf("churn key: Cardinality = %d, legal range is [%d, %d]",
						c, base, base+churn)
					return
				}
				if c := idx.Lookup(churnValue).GetCardinality(); c < base || c > base+churn {
					t.Errorf("churn key: Lookup cardinality = %d, legal range is [%d, %d]",
						c, base, base+churn)
					return
				}
				// Tested LAST, so every reader completes a full body at least once
				// however early the writer finishes.
				if stop.Load() {
					return
				}
			}
		}()
	}
	wg.Wait()

	// Non-vacuity, all of it constructed rather than sampled.
	if promotions.Load() == 0 {
		t.Fatalf("the writer never promoted past %d ids: the shared read branch was "+
			"never published for the churning key", inlineTierMax)
	}
	if demotions.Load() == 0 {
		t.Fatal("the writer never demoted back to the inline tier: the demotion " +
			"transition was never exercised")
	}
	if got := sharedReads.Load(); got < readers {
		t.Fatalf("only %d reads took the locked branch, want at least one per reader "+
			"(%d): every reader runs its body once before testing the stop flag, so "+
			"this cannot happen without a defect in the test itself", got, readers)
	}
	if got := inlineReads.Load(); got < readers {
		t.Fatalf("only %d reads took the lock-free branch, want at least one per "+
			"reader (%d)", got, readers)
	}
	t.Logf("promotions=%d demotions=%d sharedReads=%d inlineReads=%d",
		promotions.Load(), demotions.Load(), sharedReads.Load(), inlineReads.Load())
}

// TestPublicationAllocationsArePinned is the efficiency tripwire for the price
// round two pays on the writer.
//
// Publishing an immutable image costs allocations that mutating in place did
// not, and the count must stay exactly what the design says it is — an
// accidental escape, a boxed conversion, or a second helper allocation on this
// path would be invisible to every other test here and would be paid once per
// indexed write.
//
// The workload alternates one Insert and one Delete on a two-id key, so it
// returns to its starting state every iteration and testing.AllocsPerRun can
// repeat it. The expected count is spelled out rather than merely asserted:
//
//   - Insert takes the key from 1 id to 2, which copies onto index.NodeSet's
//     small tier: one fresh 8-uint64 backing array, plus one snapshot. 2.
//   - Delete takes it from 2 ids back to 1, which collapses to the singleton
//     tier — and on round three a singleton is published in the meta word with
//     the snapshot RELEASED, so it allocates NOTHING AT ALL. 0.
//
// That zero is the round-three measurement, and it is asserted here rather than
// only in the footprint benchmark: round two published the same singleton
// through a fresh 24-byte snapshot, and the total for this workload was 3.
//
// It is deliberately NOT a t.Parallel() test, for the reason given on
// TestHotPathsAreAllocationFree: testing.AllocsPerRun reads the process-global
// malloc counter.
func TestPublicationAllocationsArePinned(t *testing.T) {
	const (
		value      = int64(5)
		keeper     = graph.NodeID(1)
		churned    = graph.NodeID(2)
		wantAllocs = 2.0
	)
	idx := New[int64]()
	idx.Insert(value, keeper)

	got := testing.AllocsPerRun(200, func() {
		idx.Insert(value, churned)
		idx.Delete(value, churned)
	})
	if got != wantAllocs {
		t.Errorf("one publish-insert plus one publish-delete = %v allocs, want %v: "+
			"the copy-on-write publication path costs one *snapshot per published "+
			"change ABOVE the inline tags and nothing at all on them, plus whatever "+
			"tier index.NodeSet itself needs", got, wantAllocs)
	}
	// The state must actually have churned, or the count above is the count of
	// doing nothing.
	if c := idx.Cardinality(value); c != 1 {
		t.Fatalf("Cardinality = %d after the loop, want 1: the workload did not "+
			"return to its starting state", c)
	}
	e, ok := idx.entryFor(value)
	if !ok {
		t.Fatal("entryFor found no published entry")
	}
	if e.isSharedTier() {
		t.Fatal("a 1-id list is marked shared: the churn left the wrong tier")
	}
	if sn := e.snap.Load(); sn != nil {
		t.Fatal("the 1-id list this workload settles on still retains a snapshot: " +
			"the allocation count above is then not the one round three claims, " +
			"and every singleton key in a real index keeps paying 24 bytes for an " +
			"image its meta word already carries")
	}
}

// TestNoPublishedEntryIsDead pins the TERMINATION precondition of the retry
// loops in [Index.Insert] and [Index.Delete].
//
// Both loops make progress only because a dead entry has already left its
// shard's map: a mutator that observes the flag drops the entry lock and looks
// the value up again, and the next lookup misses. An entry that were dead AND
// still published would spin those loops for ever. Nothing catches that — the
// race detector does not detect livelock, and a test would simply hang until
// the package timeout with no diagnosis — so the invariant is asserted directly
// after a workload that drives every path capable of setting the flag:
// [hashShard.reap] via an emptying Delete, and [Index.Deserialize] via a
// wholesale displacement, both concurrently with live mutators.
func TestNoPublishedEntryIsDead(t *testing.T) {
	t.Parallel()

	const (
		keys     = 64
		mutators = 4
		rounds   = 400
	)

	src := New[int64]()
	for v := range int64(keys) {
		src.Insert(v, graph.NodeID(uint64(v)+1000))
	}
	var img bytes.Buffer
	if err := src.Serialize(&img); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	payload := img.Bytes()

	idx := New[int64]()
	var (
		stop atomic.Bool
		wg   sync.WaitGroup
	)
	for w := range mutators {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; !stop.Load(); r++ {
				v := int64((w + r) % keys)
				idx.Insert(v, graph.NodeID(uint64(w)))
				// Empties the value whenever this is its last id, which is what
				// drives reap.
				idx.Delete(v, graph.NodeID(uint64(w)))
			}
		}(w)
	}
	for range rounds {
		if err := idx.Deserialize(bytes.NewReader(payload)); err != nil {
			t.Errorf("Deserialize: %v", err)
			break
		}
	}
	stop.Store(true)
	wg.Wait()

	if n := idx.deadPublishedCount(); n != 0 {
		t.Fatalf("%d entries are dead AND still published: the retry loops in Insert "+
			"and Delete would spin for ever on any of them", n)
	}
	// Non-vacuity: the workload has to have left entries behind for the sweep to
	// have looked at anything at all.
	if n := idx.DistinctValues(); n == 0 {
		t.Fatal("the index is empty after the storm: the sweep above examined nothing")
	}
}

// TestBitmapCardIsExact pins the maintained demotion counter against the truth
// it stands in for.
//
// The cardinality in metaBitmap's payload replaces roaring's O(containers)
// GetCardinality on the delete path with an O(1) test, which is what keeps a
// single-id
// [Index.Delete] against a wide posting list off a 690x latency cliff. It is
// deliberately a hint — neither drift direction can mislabel a tier — so a
// mismatch fails no other test in this package, and a posting list would simply
// stop demoting, silently, for ever. So it is asserted directly, after a
// workload that drives every path that moves it: promotion, in-place
// bitmap-tier insert and delete, demotion, reap, and wholesale replacement by
// [Index.Deserialize].
//
// The ids are spaced 1<<16 apart for part of the workload so the bitmaps hold
// one container per id, which is the shape that made the cliff visible.
func TestBitmapCardIsExact(t *testing.T) {
	t.Parallel()

	idx := New[int64]()
	// Keys spanning both sides of the bound, some dense and some one-id-per-
	// container, plus keys that churn across the bound repeatedly.
	for v := int64(0); v < 40; v++ {
		width := int(v)%20 + 1
		for n := range width {
			id := uint64(n)
			if v%2 == 0 {
				id = uint64(n) << 16
			}
			idx.Insert(v, graph.NodeID(id))
		}
	}
	// Churn across the bound in both directions.
	for round := range 5 {
		for v := int64(0); v < 40; v++ {
			id := graph.NodeID(uint64(100+round) << 16)
			idx.Insert(v, id)
			idx.Delete(v, id)
		}
	}
	// Empty a few keys outright, so reap runs.
	for v := int64(0); v < 40; v += 7 {
		for n := range 64 {
			idx.Delete(v, graph.NodeID(uint64(n)))
			idx.Delete(v, graph.NodeID(uint64(n)<<16))
		}
	}
	mism, shared := idx.bitmapCardMismatches()
	if shared == 0 {
		t.Fatal("no entry ended on the bitmap tier: the counter this test exists " +
			"for was never in use, so the assertion below is vacuous")
	}
	if mism != 0 {
		t.Fatalf("%d of the published entries carry a cardinality that disagrees with "+
			"the cardinality of the image they publish (%d shared entries examined): "+
			"the demotion trigger has drifted, so posting lists will stop returning "+
			"to the lock-free read path", mism, shared)
	}
	t.Logf("the maintained cardinality is exact across %d shared entries", shared)

	// Deserialize restates every counter wholesale; check that path too.
	var buf bytes.Buffer
	if err := idx.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	dst := New[int64]()
	if err := dst.Deserialize(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	mism, shared = dst.bitmapCardMismatches()
	if shared == 0 {
		t.Fatal("the deserialised index has no bitmap-tier entry: vacuous")
	}
	if mism != 0 {
		t.Fatalf("%d hydrated entries carry a wrong cardinality (%d shared examined)",
			mism, shared)
	}
}

// TestBitmapCardExactUnderConcurrentChurn is the concurrent half of
// TestBitmapCardIsExact: the counter is maintained under the entry write lock
// from CheckedAdd/CheckedRemove deltas, so a lost update would show up only
// when many writers cross the promotion bound on the same key at once.
func TestBitmapCardExactUnderConcurrentChurn(t *testing.T) {
	t.Parallel()

	const (
		keys    = 16
		writers = 8
		rounds  = 200
	)
	idx := New[int64]()
	for v := range int64(keys) {
		for n := range inlineTierMax {
			idx.Insert(v, graph.NodeID(uint64(n)<<16))
		}
	}
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			id := graph.NodeID(uint64(1000+w) << 16)
			for r := range rounds {
				v := int64((w + r) % keys)
				idx.Insert(v, id)
				idx.Delete(v, id)
			}
		}(w)
	}
	wg.Wait()

	mism, shared := idx.bitmapCardMismatches()
	if mism != 0 {
		t.Fatalf("%d entries carry a drifted cardinality after the concurrent churn "+
			"(%d shared examined)", mism, shared)
	}
	// Every key must have settled back to the inline tier, which is itself the
	// proof that the demotion trigger still fires after the churn.
	for v := range int64(keys) {
		if got := idx.Cardinality(v); got != uint64(inlineTierMax) {
			t.Fatalf("value %d holds %d ids, want %d", v, got, inlineTierMax)
		}
		e, ok := idx.entryFor(v)
		if !ok {
			t.Fatalf("value %d has no published entry", v)
		}
		if e.isSharedTier() {
			t.Fatalf("value %d settled at %d ids but is still on the bitmap tier: the "+
				"demotion trigger stopped firing", v, inlineTierMax)
		}
	}
}

// TestDeleteCostDoesNotScaleWithContainerCount is the regression gate for the
// latency cliff the maintained cardinality exists to prevent.
//
// A demotion trigger that asks roaring for the cardinality walks every
// container, so a single-id [Index.Delete] becomes O(containers) instead of
// O(1). Measured, that took one Delete against a 65 536-id posting list from
// 181.5 ns to 125.277 us — 690x — and no assertion in this package noticed,
// because the RESULT of every operation stays correct. Only the cost moves.
//
// So the oracle is a RATIO between two widths measured in the same test on the
// same host, not a wall-clock deadline: a deadline would have to be calibrated
// to the machine and would drift, whereas the ratio between an O(1) and an
// O(containers) delete is a property of the algorithm. The ids are spaced 1<<16
// apart so each occupies its own container, making the container count equal
// the cardinality — without that spacing both widths fit a handful of
// containers and the defect is invisible.
//
// Robustness, because a timing oracle inside a parallel suite is fragile:
//
//   - The two widths are measured INTERLEAVED over several rounds, so a load
//     spike lands on both arms rather than on one.
//   - Each arm keeps its MINIMUM per-op time across rounds. A minimum cannot be
//     inflated by a competing process, only by a real cost.
//   - The gate is 10x against a measured defect of 51x at these widths and a
//     measured correct value near 1x, so there is roughly 6x of margin on both
//     sides. It is not a tight bound and is not meant to be one.
//   - It is not t.Parallel(), so it does not compete with the rest of the suite.
func TestDeleteCostDoesNotScaleWithContainerCount(t *testing.T) {
	const (
		narrow   = 1024
		wide     = 65536
		ops      = 200
		rounds   = 5
		maxRatio = 10.0
		spacing  = 16 // ids spaced 1<<16 apart: one container each
		theValue = int64(0)
	)

	build := func(width int) *Index[int64] {
		idx := New[int64]()
		for i := range width {
			idx.Insert(theValue, graph.NodeID(uint64(i)<<spacing))
		}
		return idx
	}
	// One Delete plus one Insert per op; the Insert restores the width so every
	// timed Delete faces the same container count. Both arms pay the same
	// Insert, so it cancels in the ratio.
	measure := func(idx *Index[int64], width int) time.Duration {
		victim := graph.NodeID(uint64(width-1) << spacing)
		start := time.Now()
		for range ops {
			idx.Delete(theValue, victim)
			idx.Insert(theValue, victim)
		}
		return time.Since(start) / ops
	}

	narrowIdx, wideIdx := build(narrow), build(wide)
	// Both keys must actually be on the bitmap tier, or neither arm exercises
	// the path under test.
	for name, idx := range map[string]*Index[int64]{"narrow": narrowIdx, "wide": wideIdx} {
		e, ok := idx.entryFor(theValue)
		if !ok {
			t.Fatalf("%s: no published entry", name)
		}
		if !e.isSharedTier() {
			t.Fatalf("%s: the key is not on the bitmap tier, so this test measures "+
				"the wrong path entirely", name)
		}
	}

	best := map[string]time.Duration{"narrow": time.Hour, "wide": time.Hour}
	for range rounds {
		if d := measure(narrowIdx, narrow); d < best["narrow"] {
			best["narrow"] = d
		}
		if d := measure(wideIdx, wide); d < best["wide"] {
			best["wide"] = d
		}
	}
	if best["narrow"] <= 0 {
		t.Fatalf("the narrow arm measured %v per op: the clock resolution cannot "+
			"support this comparison, so the ratio below is meaningless", best["narrow"])
	}
	ratio := float64(best["wide"]) / float64(best["narrow"])
	t.Logf("delete per op: %d containers %v, %d containers %v, ratio %.2fx (gate %.0fx)",
		narrow, best["narrow"], wide, best["wide"], ratio, maxRatio)
	if ratio > maxRatio {
		t.Fatalf("a single-id Delete costs %.2fx more against %d containers than "+
			"against %d (%v vs %v), gate %.0fx: the delete path is walking every "+
			"container, which is the O(containers) demotion trigger the maintained "+
			"cardinality exists to replace",
			ratio, wide, narrow, best["wide"], best["narrow"], maxRatio)
	}
}
