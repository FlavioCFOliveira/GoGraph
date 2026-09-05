package hash

import (
	"runtime"
	"slices"
	"testing"
	"time"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// TestEntrySizeClass pins the layout arithmetic the whole of round three rests
// on, and MEASURES the size classes it appeals to rather than trusting them.
//
// [entry] is 41 bytes of fields — meta 8, snap 8, mu 24, dead 1 — which Go
// rounds to 48 for alignment and the allocator then serves out of its 48-byte
// size class. There is no class between 48 and 64, so one more word of fields
// costs SIXTEEN bytes on every key in the index, not eight. That is why the
// demotion trigger lives in metaBitmap's payload instead of in a uint64 field
// of its own: at 10 000 000 distinct values the extra class would cost 160 MB
// to save the 24 B per singleton the inline tags just stopped paying, which is
// a net loss on the shape this index exists to serve.
//
// Nothing in the compiler enforces any of that, and the whole memory result of
// round three is void without it, so both halves are asserted here: the size of
// the struct, and the classes themselves.
//
// The classes are read out of runtime.MemStats.BySize — the runtime's own
// account of the classes it serves — rather than inferred from allocation
// measurements. An earlier version of this test did measure them, by allocating
// 20 000 objects and dividing a TotalAlloc delta, and it read 65 bytes for a
// 56-byte object: TotalAlloc is process-global and picks up whatever else the
// runtime allocated inside the window, so it cannot settle an exact boundary.
func TestEntrySizeClass(t *testing.T) {
	t.Parallel()

	const class = 48
	got := unsafe.Sizeof(entry{})
	t.Logf("unsafe.Sizeof(entry{}) = %d bytes", got)
	if got > class {
		t.Errorf("entry is %d bytes, want at most %d: it has left the %d-byte size "+
			"class, so every key in every hash index in the process now costs the "+
			"next class up. Move a field into meta's payload rather than accepting "+
			"this", got, class, class)
	}

	// The classes themselves. 40 bytes must be served by the 48 class (there is
	// none at 40), 48 by the 48 class, and 56 by the 64 class (there is none at
	// 56) — which is the arithmetic the paragraph above depends on.
	classes := sizeClasses()
	if len(classes) == 0 {
		t.Fatal("runtime.MemStats.BySize reported no size classes: this test cannot " +
			"settle anything and the rest of it is vacuous")
	}
	t.Logf("size classes bracketing entry: %v", bracket(classes, class))
	for _, c := range []struct {
		size uintptr
		want uintptr
	}{
		{size: 40, want: 48}, // one word fewer than entry
		{size: class, want: class},
		{size: 56, want: 64}, // one word more than entry
	} {
		if n := classFor(classes, c.size); n != c.want {
			t.Errorf("a %d-byte object is served by the %d-byte class, want %d: the "+
				"size classes are not what entry's layout was designed against, so "+
				"the field budget in entry's own documentation is wrong",
				c.size, n, c.want)
		}
	}
	if got := classFor(classes, got); got != class {
		t.Errorf("entry (%d bytes) is served by the %d-byte class, want %d",
			unsafe.Sizeof(entry{}), got, class)
	}
}

// sizeClasses returns the allocator's size classes in ascending order, as the
// runtime itself reports them.
func sizeClasses() []uintptr {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	out := make([]uintptr, 0, len(ms.BySize))
	// &ms.BySize: ranging the array by value would copy 1 464 bytes.
	for _, b := range &ms.BySize {
		if b.Size > 0 {
			out = append(out, uintptr(b.Size))
		}
	}
	slices.Sort(out)
	return out
}

// classFor returns the size class that serves an object of exactly n bytes, or
// 0 when n is larger than the largest class (such objects are served by whole
// pages instead, which no struct in this package approaches).
func classFor(classes []uintptr, n uintptr) uintptr {
	for _, c := range classes {
		if c >= n {
			return c
		}
	}
	return 0
}

// bracket returns the classes immediately below and above n, for the log line.
func bracket(classes []uintptr, n uintptr) []uintptr {
	for i, c := range classes {
		if c >= n {
			lo := max(i-1, 0)
			hi := min(i+2, len(classes))
			return classes[lo:hi]
		}
	}
	return nil
}

// TestDemotionToSingletonReleasesSnapshot is the whole memory result of round
// three, asserted on the state rather than inferred from a benchmark.
//
// A key that shrinks back to one id — or to none — publishes its image in
// [entry.meta] and must RELEASE the snapshot the tier it left had allocated.
// Retaining it instead would be invisible: every read returns the right answer,
// every tier flag is right, and the only symptom is that a 24-byte snapshot —
// plus, coming off the small tier, its 64-byte backing array — stays reachable
// for the life of the key. On a churning index that is most keys, which is the
// entire cost round three exists to remove.
//
// Both inbound routes are covered, because they publish through different code:
// a bitmap-tier list reaches the singleton tag through the demotion in
// [entry.deleteLocked], while a small-tier one reaches it through the
// copy-on-write branch.
func TestDemotionToSingletonReleasesSnapshot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		preload int
		// remaining is how many ids are left behind: 1 for the singleton tag,
		// 0 for the empty one.
		remaining int
	}{
		{name: "fromBitmap", preload: inlineTierMax + 4, remaining: 1},
		{name: "fromSmall", preload: 3, remaining: 1},
		{name: "toEmpty", preload: 3, remaining: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			const value = int64(13)
			idx := New[int64]()
			for n := range c.preload {
				idx.Insert(value, graph.NodeID(uint64(n)+1))
			}
			e, ok := idx.entryFor(value)
			if !ok {
				t.Fatal("entryFor found no published entry")
			}
			if e.snap.Load() == nil {
				t.Fatalf("setup: a %d-id list published no snapshot, so there is "+
					"nothing for the demotion to release and this test is vacuous",
					c.preload)
			}

			for n := c.remaining; n < c.preload; n++ {
				idx.Delete(value, graph.NodeID(uint64(n)+1))
			}

			if got := idx.Cardinality(value); got != uint64(c.remaining) {
				t.Fatalf("Cardinality = %d after the deletes, want %d", got, c.remaining)
			}
			if sn := e.snap.Load(); sn != nil {
				t.Fatalf("the entry settled at %d id(s) but still retains a %d-id "+
					"snapshot: the image is published in the meta word and the "+
					"snapshot it superseded is never released, so every key that was "+
					"ever wider than a singleton goes on paying for a tier it left",
					c.remaining, sn.set.Cardinality())
			}
			wantTag := metaEmpty
			if c.remaining == 1 {
				wantTag = metaSingleton
			}
			if tag := e.meta.Load() & metaTagMask; tag != wantTag {
				t.Fatalf("meta tag = %#b after the deletes, want %#b", tag, wantTag)
			}
			// And the contents survived the release, on every read path.
			if c.remaining == 1 {
				if got := idx.LookupAppend(value, nil); len(got) != 1 || got[0] != 1 {
					t.Fatalf("LookupAppend = %v, want [1]", got)
				}
				if !idx.Contains(value, graph.NodeID(1)) {
					t.Fatal("Contains lost the surviving id")
				}
				if got := idx.Lookup(value).ToArray(); len(got) != 1 || got[0] != 1 {
					t.Fatalf("Lookup = %v, want [1]", got)
				}
			}
		})
	}
}

// TestDemotionWindowResolvesToInlineImage is state 5 on [entry], CONSTRUCTED
// rather than raced for.
//
// Releasing the snapshot on demotion opens one window a lock-free reader can
// observe: it reads a POINTER tag, a writer then publishes an inline image and
// releases the snapshot, and the reader's own load of that pointer finds nil.
// [entry.publish] writes the inline tag BEFORE releasing the pointer, so a
// re-read of meta resolves it — and because Go's sync/atomic operations are
// sequentially consistent, a reader that observed the nil must observe that
// earlier store.
//
// The state is written directly instead of being raced for, so the assertions
// are arithmetic rather than scheduler-dependent, and the test is non-vacuous
// by construction: while the window is open the read CANNOT legally complete,
// so a reader that returns anyway has either dereferenced the nil or answered
// from no image at all. That is the assertion, and it is what a build with the
// re-read deleted fails on.
func TestDemotionWindowResolvesToInlineImage(t *testing.T) {
	t.Parallel()

	const (
		value    = int64(14)
		preload  = 4 // the small tier: a pointer tag over a real snapshot
		survivor = uint64(7)
	)
	idx := New[int64]()
	for n := range preload {
		idx.Insert(value, graph.NodeID(uint64(n)+1))
	}
	e, ok := idx.entryFor(value)
	if !ok {
		t.Fatal("entryFor found no published entry")
	}
	if e.meta.Load()&metaPtrBit == 0 {
		t.Fatalf("setup: a %d-id list is not on a pointer tag, so the window this "+
			"test constructs cannot occur for it", preload)
	}
	if e.snap.Load() == nil {
		t.Fatal("setup: the entry has no snapshot to release")
	}

	// CONSTRUCT the window: the pointer tag the reader is acting on, with the
	// snapshot already released.
	e.snap.Store(nil)

	var (
		done     = make(chan struct{})
		got      uint64
		panicked any
	)
	go func() {
		defer close(done)
		defer func() { panicked = recover() }()
		got = idx.Cardinality(value)
	}()

	// The window is UNRESOLVED, so no correct read can finish: the entry claims
	// a snapshot it does not have and no inline tag has been published yet.
	select {
	case <-done:
		if panicked != nil {
			t.Fatalf("Cardinality panicked inside the demotion window (%v): it "+
				"dereferenced the released snapshot, so the bounded re-read of meta "+
				"is missing", panicked)
		}
		t.Fatalf("Cardinality returned %d inside the unresolved demotion window: "+
			"it answered without an image at all", got)
	case <-time.After(20 * time.Millisecond):
	}

	// Complete the demotion exactly as [entry.publishInline] does: the inline
	// tag goes in, and the reader's re-read must find it.
	e.meta.Store(metaSingleton | survivor<<metaShift)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Cardinality never returned after the inline tag was published: the " +
			"resolution loop is not re-loading meta, so it cannot make progress and " +
			"every reader that lands in this window parks for ever")
	}
	if panicked != nil {
		t.Fatalf("Cardinality panicked resolving the window: %v", panicked)
	}
	if got != 1 {
		t.Fatalf("Cardinality = %d after the window resolved, want 1: the reader "+
			"did not answer from the inline image the writer published", got)
	}
}

// TestReaderTrustsSnapshotFlagNotMetaTag is why [snapshot.shared] was NOT
// folded into the meta word when that word was introduced.
//
// A reader resolves meta and then loads snap: two separate loads, so a
// promotion can land between them and the snapshot it gets can be one tier
// above the tag that sent it there. If the reader took its lock-or-not decision
// from the TAG it would then read a live roaring bitmap with no lock at all,
// concurrently with the writer mutating it in place. Taking the decision from
// the snapshot's own flag makes that interleaving harmless.
//
// The interleaving is constructed directly — a metaSmall tag over a shared
// snapshot, which is precisely what a reader observes in that window — and the
// oracle is the LOCK rather than the race detector, so the test has teeth in a
// plain `go test` run too: with the entry held for writing, a reader that
// correctly consults the snapshot flag must PARK, and one that trusted the tag
// would sail past and return.
func TestReaderTrustsSnapshotFlagNotMetaTag(t *testing.T) {
	t.Parallel()

	const value = int64(15)
	wide := inlineTierMax + 8
	idx := New[int64]()
	for n := range wide {
		idx.Insert(value, graph.NodeID(uint64(n)+1))
	}
	e, ok := idx.entryFor(value)
	if !ok {
		t.Fatal("entryFor found no published entry")
	}
	sn := e.snap.Load()
	if sn == nil || !sn.shared {
		t.Fatalf("setup: a %d-id list is not on the bitmap tier, so there is no "+
			"in-place writer for a mis-decided read to collide with", wide)
	}

	// CONSTRUCT the mid-promotion state: the tag says the small tier while the
	// snapshot beneath it is the shared bitmap.
	e.meta.Store(metaSmall)

	// Hold the entry for writing. A read that consults snapshot.shared takes the
	// entry read lock and parks here; one that trusted metaSmall does not.
	e.mu.Lock()

	var (
		done = make(chan struct{})
		got  uint64
	)
	go func() {
		defer close(done)
		got = idx.Cardinality(value)
	}()

	select {
	case <-done:
		e.mu.Unlock()
		t.Fatalf("Cardinality returned %d while the entry was held for WRITING: it "+
			"took the lock-free branch for a snapshot whose set aliases a live "+
			"roaring bitmap, so it decided the tier from the meta tag instead of "+
			"from snapshot.shared", got)
	case <-time.After(20 * time.Millisecond):
	}

	e.mu.Unlock()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Cardinality never returned after the entry lock was released")
	}
	if got != uint64(wide) {
		t.Fatalf("Cardinality = %d, want %d: the read answered from something other "+
			"than the snapshot it loaded", got, wide)
	}
}

// TestDriftedCardinalityCannotMoveTheNonEmptyCount holds the one claim about
// the maintained demotion trigger that no other test can reach.
//
// [entry.deleteLocked] documents the counter in metaBitmap's payload as a HINT:
// a drift in either direction can cost a demotion and nothing else. The
// dangerous direction is DOWN. A counter that reads 1 while the list really
// holds many ids makes the removal look like the one that emptied the list, and
// a Delete that reports nowEmpty on a list that is not empty decrements its
// shard's non-empty count — which is exactly the false zero out of
// [Index.DistinctValues] that rmp #1983 was, the failure direction that matters
// in production.
//
// Round three reads that emptiness off the image [entry.publish] just published
// rather than off the counter, so the drift cannot reach the accounting.
// Production cannot construct the drift — the counter is restated on every
// publication — so the claim is only falsifiable through an injection seam, and
// without this test a build that trusted the counter passes everything.
func TestDriftedCardinalityCannotMoveTheNonEmptyCount(t *testing.T) {
	t.Parallel()

	const value = int64(16)
	const wide = inlineTierMax + 12
	idx := New[int64]()
	for n := range wide {
		idx.Insert(value, graph.NodeID(uint64(n)+1))
	}
	if got := idx.DistinctValues(); got != 1 {
		t.Fatalf("setup: DistinctValues = %d, want 1", got)
	}

	// Drive the counter to 1 while the posting list really holds wide ids.
	if !idx.forceBitmapCard(value, 1) {
		t.Fatalf("setup: a %d-id list is not on the bitmap tier, so the payload "+
			"this test drifts does not exist and nothing is being injected", wide)
	}

	// One removal. Against the drifted counter it looks like the last one.
	idx.Delete(value, graph.NodeID(1))

	if got := idx.Cardinality(value); got != wide-1 {
		t.Fatalf("Cardinality = %d after the delete, want %d: the image itself is "+
			"wrong, which is more than a drifted hint can explain", got, wide-1)
	}
	if !idx.keyPresent(value) {
		t.Fatalf("the key holding %d ids was reaped: the drifted counter reached "+
			"the reap", wide-1)
	}
	if got := idx.DistinctValues(); got != 1 {
		t.Fatalf("DistinctValues = %d after a delete against a drifted counter, "+
			"want 1: the delete reported an empty list for one that holds %d ids, "+
			"so the shard's non-empty count was decremented — the false zero of "+
			"rmp #1983, reached from a hint that is only supposed to cost a "+
			"demotion", got, wide-1)
	}
	if n := idx.nonEmptySum(); n != 1 {
		t.Fatalf("the raw non-empty sum is %d, want 1", n)
	}
}
