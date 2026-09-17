package label

// bitmap_shared_test.go — [Index.BitmapShared] (rmp #2863).
//
// BitmapShared trades [Index.Intersect]'s per-read clone for a per-label image
// that every reader shares until the label is next written. The whole design
// rests on ONE property — the published image is never written again — and a
// violation of it is invisible to any test that only checks the answer, because
// the answer is right either way. So these tests hold an image ACROSS a write
// and assert the image itself did not move.
//
// Layer: short. Race-clean; the concurrency case is the point of the fix.

import (
	"fmt"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// seedLabel adds n nodes (ids 1..n) to label l and returns the index.
func seedLabel(l uint32, n int) *Index {
	idx := NewIndex()
	for i := 1; i <= n; i++ {
		idx.Add(l, graph.NodeID(i))
	}
	return idx
}

// TestBitmapShared_AgreesWithIntersectOnEveryTier pins the answer first: the
// cheap route may not return a different set from the expensive one.
//
// The tiers are enumerated explicitly because [index.NodeSet] answers Bitmap()
// differently on each — only the bitmap tier hands back a live object needing a
// copy — and a tier that silently stopped being covered is how this would rot.
func TestBitmapShared_AgreesWithIntersectOnEveryTier(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 2, 8, 9, 64, 5000} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()
			const l = 7
			idx := seedLabel(l, n)
			want := idx.Intersect(l)
			got := idx.BitmapShared(l)
			if !got.Equals(want) {
				t.Fatalf("BitmapShared(%d) and Intersect(%d) disagree at n=%d: %d vs %d members",
					l, l, n, got.GetCardinality(), want.GetCardinality())
			}
			if got.GetCardinality() != uint64(n) {
				t.Fatalf("BitmapShared cardinality = %d, want %d", got.GetCardinality(), n)
			}
		})
	}
}

// TestBitmapShared_UnknownLabelIsEmpty pins the boundary Intersect already has:
// a label with no entry answers empty rather than nil, so no caller needs a nil
// check that Intersect never made it write.
func TestBitmapShared_UnknownLabelIsEmpty(t *testing.T) {
	t.Parallel()
	idx := NewIndex()
	if bm := idx.BitmapShared(99); bm == nil || !bm.IsEmpty() {
		t.Fatalf("BitmapShared(unknown) = %v, want a non-nil empty bitmap", bm)
	}
}

// TestBitmapShared_WarmPathReusesTheSameObject is the performance claim stated as
// a correctness-shaped assertion: the second read of an unchanged label must hand
// back the SAME object, because handing back a different one means it copied.
//
// Pointer identity is the only honest way to state this. A byte-equality check
// would pass against the old clone-every-time code and prove nothing.
func TestBitmapShared_WarmPathReusesTheSameObject(t *testing.T) {
	t.Parallel()
	const l = 3
	idx := seedLabel(l, 64) // bitmap tier: the only tier where a copy is possible
	first := idx.BitmapShared(l)
	for i := 0; i < 5; i++ {
		if got := idx.BitmapShared(l); got != first {
			t.Fatalf("read %d returned a different object from read 0: the image is being "+
				"rebuilt on every call and the clone was never removed", i+1)
		}
	}
}

// TestBitmapShared_WarmPathAllocatesNothing measures the thing the finding is
// about. 35_mvcc_mixed_workload spent 52.75% of ALL its allocation on the clone
// this replaces; here that is asserted at the source rather than inferred from
// the profile.
func TestBitmapShared_WarmPathAllocatesNothing(t *testing.T) {
	const l = 3
	idx := seedLabel(l, 5000)
	idx.BitmapShared(l) // warm it, so the cold build is not what is measured
	if n := testing.AllocsPerRun(200, func() { _ = idx.BitmapShared(l) }); n != 0 {
		t.Errorf("BitmapShared allocated %.2f objects per warm read, want 0", n)
	}
	// The control: the route it replaces still costs what the finding says it
	// does, so the zero above is a comparison against a live number.
	if n := testing.AllocsPerRun(200, func() { _ = idx.Intersect(l) }); n == 0 {
		t.Error("Intersect now allocates nothing either, so the zero above demonstrates no " +
			"difference and this pair proves nothing")
	}
}

// TestBitmapShared_APublishedImageSurvivesEveryMutator is THE test. It is the
// copy-on-write contract, and every other assertion in this file is downstream
// of it.
//
// A reader may hold the returned pointer with no lock, for as long as it likes.
// That is sound only if a later write to the label REPLACES the image instead of
// editing it. Each subtest holds an image across one mutator and asserts the held
// image is byte-for-byte what it was — and, separately, that the next read sees
// the write, so the invalidation is not simply missing.
func TestBitmapShared_APublishedImageSurvivesEveryMutator(t *testing.T) {
	t.Parallel()
	const l = 5
	cases := []struct {
		name  string
		apply func(*Index)
		// wantAfter is the cardinality a FRESH read must report once apply has run.
		wantAfter uint64
	}{
		{"Add", func(i *Index) { i.Add(l, graph.NodeID(1000)) }, 65},
		{"Remove", func(i *Index) { i.Remove(l, graph.NodeID(1)) }, 63},
		{"AddRange", func(i *Index) { i.AddRange(l, graph.NodeID(2000), graph.NodeID(2009)) }, 74},
		{"RemoveRange", func(i *Index) { i.RemoveRange(l, graph.NodeID(1), graph.NodeID(10)) }, 54},
		{"Apply/add", func(i *Index) {
			i.Apply(index.Change{Op: index.OpAddNodeLabel, Label: l, Node: graph.NodeID(1000)})
		}, 65},
		{"Apply/remove", func(i *Index) {
			i.Apply(index.Change{Op: index.OpRemoveNodeLabel, Label: l, Node: graph.NodeID(1)})
		}, 63},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			idx := seedLabel(l, 64)
			held := idx.BitmapShared(l)
			frozen := held.Clone() // the oracle: what the reader is entitled to keep seeing

			tc.apply(idx)

			if !held.Equals(frozen) {
				t.Errorf("the image handed out BEFORE %s changed under the reader: %d members "+
					"now, %d when it was published. A reader holding this pointer without a lock "+
					"is reading a bitmap being written.",
					tc.name, held.GetCardinality(), frozen.GetCardinality())
			}
			// The mirror image: the invalidation must actually have happened, or
			// the assertion above would pass merely because nothing ever updates.
			fresh := idx.BitmapShared(l)
			if fresh == held {
				t.Fatalf("%s did not drop the published image: the next reader is served a "+
					"pre-write answer", tc.name)
			}
			if got := fresh.GetCardinality(); got != tc.wantAfter {
				t.Errorf("after %s a fresh read reports %d members, want %d", tc.name, got, tc.wantAfter)
			}
		})
	}
}

// TestBitmapShared_ImageIsNotTheLiveSet proves the copy is made where it must be:
// the image may never alias the set the index goes on to mutate.
//
// It is distinct from the test above, which could also be satisfied by an index
// that never mutates anything. Here the LIVE set is driven forward and the image
// is required not to follow.
func TestBitmapShared_ImageIsNotTheLiveSet(t *testing.T) {
	t.Parallel()
	const l = 11
	idx := seedLabel(l, 64)
	img := idx.BitmapShared(l)
	before := img.GetCardinality()

	for i := 100; i < 200; i++ {
		idx.Add(l, graph.NodeID(i))
	}
	if got := img.GetCardinality(); got != before {
		t.Fatalf("the published image tracked 100 subsequent writes (%d -> %d members): it "+
			"aliases the live bitmap and every reader holding it sees writes tear",
			before, got)
	}
	if got := idx.BitmapShared(l).GetCardinality(); got != before+100 {
		t.Fatalf("a fresh read reports %d members after 100 adds, want %d", got, before+100)
	}
}

// TestBitmapShared_ConcurrentReadersAndWritersKeepTheirImages is the mandate's
// own test: massive concurrent use, with the readers asserting the property that
// makes the lock-free hand-off legal.
//
// Each reader takes an image, records its cardinality, then re-reads that SAME
// object repeatedly while writers churn the label. Under `-race` this also
// exercises the hand-off itself: a writer editing a published image in place
// would be an unsynchronised write against these reads.
func TestBitmapShared_ConcurrentReadersAndWritersKeepTheirImages(t *testing.T) {
	const (
		l       = 13
		readers = 16
		writers = 4
		rounds  = 200
	)
	idx := seedLabel(l, 512)

	stop := make(chan struct{})
	var writersWG, readersWG sync.WaitGroup
	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func(base int) {
			defer writersWG.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				id := graph.NodeID(10_000 + base*100_000 + i%1000)
				idx.Add(l, id)
				idx.Remove(l, id)
			}
		}(w)
	}

	errs := make(chan string, readers)
	for r := 0; r < readers; r++ {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			for i := 0; i < rounds; i++ {
				img := idx.BitmapShared(l)
				card := img.GetCardinality()
				for k := 0; k < 20; k++ {
					if got := img.GetCardinality(); got != card {
						errs <- fmt.Sprintf("a held image changed under its reader: %d -> %d", card, got)
						return
					}
					if !img.Contains(1) {
						errs <- "a held image lost a member that was never removed"
						return
					}
				}
			}
		}()
	}
	// The readers run a bounded number of rounds; the writers run until told to
	// stop. Waiting on the readers FIRST is what keeps the two bounded: closing
	// stop before they finish would leave them measuring a quiet index.
	readersWG.Wait()
	close(stop)
	writersWG.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}
