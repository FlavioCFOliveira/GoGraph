//go:build soak

package sim

// label_index_scoped_soak_test.go — the soak-layer arms of the scoped/range
// label index scenario (rmp #2496).
//
// Five claims live here rather than in the short layer, each for its own reason:
//
//   - The SEED SWEEP. Every non-vacuity gate is a claim about what a run reaches,
//     and a claim verified on one seed is a claim about one seed. The base band,
//     its width, the sweep order, the interleaved single-id operations and the
//     Union subsets are all drawn, so which labels survive, how big they get and
//     which relationships land on a already-crowded label are seed-dependent.
//   - The DETERMINISM SWEEP, separate from the sweep above because the two claims
//     are different: that one asserts the invariants hold, this one asserts the
//     harness is a pure function of the seed. A digest can be stable for one seed
//     and not for the draw sequence another seed takes.
//   - The LONG SWEEP. The short layer gives each accumulating label 24 epochs of
//     history. This arm gives it far more, because "overlapping range operations
//     never drift from the model" is a claim about a SEQUENCE and 24 is a short
//     one — and because a longer history is the only way a label accumulates the
//     fragmented, many-container shape a single epoch cannot build.
//   - The EXHAUSTIVE CORRUPTION SWEEP. The short layer flips one byte in each of
//     seven NAMED regions. The claim being made is stronger than that — the
//     CRC32C covers every byte of the body, so a flip ANYWHERE must be refused —
//     and here every byte offset of a small image is driven, which is the only
//     form of that claim that is actually exhaustive.
//   - The DENSE-SMALL WIDTH SWEEP, which turns the short layer's single pinned
//     width into a characterisation: every width from 1 to smallSetMax+8 is
//     round-tripped and the exact window in which the form is not idempotent is
//     asserted, so a change at either edge of it is loud.
//
// NOTHING in this file calls t.Parallel(), for the reason the short-layer file
// gives: every test here uses goleak, and goleak cannot pass in a parallel test.

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/label"
)

// The soak budgets. Each run is a few tens of milliseconds of in-memory
// arithmetic, so the sweeps are sized by how much seed space they cover rather
// than by wall clock.
const (
	liSoakSeeds       = 64
	liSoakDetermSeeds = 24
	liSoakLongEpochs  = 400
	liSoakWidthMax    = liSmallSetMaxMirror + 8
)

// liRunContainerMinWidth is the contiguous-run length at and above which roaring
// encodes an `AddRange` as a RUN container rather than an array one. It is
// MEASURED, not documented: at widths 1, 2 and 3 an AddRange-built label
// serializes byte-identically to an Add-built one, and from width 4 upwards it
// collapses to a flat 55 bytes whatever the length.
//
// It WAS the lower edge of the window in which the round trip was not
// idempotent, the upper edge being smallSetMax, past which the down-convert does
// not run. rmp #2609 closed that window — Serialize now normalises the container
// encoding at or below smallSetMax — so
// [TestLabelIndexScopedSoak_DenseSmallWindow] asserts a fixpoint at every width
// instead of a window.
//
// The constant is KEPT because the measurement it records is still true of
// roaring and is what makes the normalisation's BOUND meaningful: below width 4
// the two construction routes already agreed, so the bound only has work to do
// in [4, smallSetMax]. A roaring upgrade that moved the encoding threshold would
// change which widths the normalisation is load-bearing for, and this constant
// is where that fact is written down.
const liRunContainerMinWidth = 4

// _ = liRunContainerMinWidth keeps the measured constant referenced now that the
// window assertion it fed has been replaced by a fixpoint assertion.
var _ = liRunContainerMinWidth

// TestLabelIndexScopedSoak_SeedSweep runs the whole scenario across a band of
// seeds and requires every one to be clean.
//
// A gate firing here is as much a finding as a clause firing: it would mean the
// fixture's coverage depends on the seed, which is exactly what the constructed
// sweep is designed to prevent.
func TestLabelIndexScopedSoak_SeedSweep(t *testing.T) {
	defer goleak.VerifyNone(t)
	for i := 0; i < liSoakSeeds; i++ {
		seed := labelIndexScopedDefaultSeed + uint64(i)*0x9E3779B97F4A7C15
		ev, report, err := RunLabelIndexScoped(context.Background(),
			DefaultLabelIndexScopedConfig(seed))
		if err != nil {
			t.Fatalf("seed %#x: run error: %v", seed, err)
		}
		if report != nil {
			t.Fatalf("seed %#x reported %d violation(s): %v\n\nevidence:\n%s",
				seed, len(report.Violations), liUniqueClauses(report.Violations), ev.String())
		}
	}
}

// TestLabelIndexScopedSoak_DeterminismSweep requires each of many seeds to
// reproduce its own digest and summary exactly, and requires the digests to
// DIFFER across seeds.
//
// The second half is what stops the first from passing on a harness that
// measured nothing: a constant digest is perfectly reproducible.
func TestLabelIndexScopedSoak_DeterminismSweep(t *testing.T) {
	defer goleak.VerifyNone(t)
	seen := map[uint64]uint64{}
	for i := 0; i < liSoakDetermSeeds; i++ {
		seed := labelIndexScopedDefaultSeed ^ (uint64(i+1) * 0xD1B54A32D192ED03)
		run := func() *LabelIndexScopedEvidence {
			ev, _, err := RunLabelIndexScoped(context.Background(),
				DefaultLabelIndexScopedConfig(seed))
			if err != nil {
				t.Fatalf("seed %#x: run error: %v", seed, err)
			}
			return ev
		}
		a, b := run(), run()
		if a.Digest != b.Digest || a.ReproducibleSummary() != b.ReproducibleSummary() {
			t.Fatalf("seed %#x is not reproducible: digests %#016x and %#016x\n%s\n%s",
				seed, a.Digest, b.Digest, a.ReproducibleSummary(), b.ReproducibleSummary())
		}
		if prev, dup := seen[a.Digest]; dup {
			t.Errorf("seeds %#x and %#x produced the same digest %#016x", prev, seed, a.Digest)
		}
		seen[a.Digest] = seed
	}
}

// TestLabelIndexScopedSoak_LongSweep drives one index through a far longer op
// stream than the short layer's, so the accumulating labels build the
// fragmented, many-container shape a handful of epochs cannot reach, and the
// model is required to track them the whole way.
func TestLabelIndexScopedSoak_LongSweep(t *testing.T) {
	defer goleak.VerifyNone(t)
	cfg := DefaultLabelIndexScopedConfig(labelIndexScopedDefaultSeed)
	cfg.Epochs = liSoakLongEpochs
	cfg.UnionDraws = 8
	ev, report, err := RunLabelIndexScoped(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if report != nil {
		t.Fatalf("the long sweep reported %d violation(s): %v\n\nevidence:\n%s",
			len(report.Violations), liUniqueClauses(report.Violations), ev.String())
	}
	// The arm is only worth its runtime if it actually reached further than the
	// short layer does.
	if ev.RangeOps < liSoakLongEpochs*int(liRelCount)*int(liDirCount) {
		t.Errorf("the long sweep drove %d range operations over %d epochs, fewer than one per cell",
			ev.RangeOps, liSoakLongEpochs)
	}
	t.Logf("long sweep: %d range ops, %d compares, peak %d members in %d labels, image %d bytes",
		ev.RangeOps, ev.Compares, ev.PeakMembers, ev.FinalLabels, ev.ImageBytes)
}

// TestLabelIndexScopedSoak_EveryByteIsCovered drives EVERY byte offset of a
// serialized image through a flip and requires a refusal at each one.
//
// This is the exhaustive form of the claim the short layer samples at seven
// offsets. The CRC32C trailer covers every byte of the body, and the trailer
// itself is compared against a checksum recomputed from that body, so there is
// no offset at which a single-byte flip can survive — including inside the
// bitmap payload, where a flipped byte might otherwise decode to a different but
// structurally valid set.
//
// It also asserts the receiver survived every one of them, which is the
// atomicity half of Deserialize's contract and the half a sampled sweep is least
// likely to break.
func TestLabelIndexScopedSoak_EveryByteIsCovered(t *testing.T) {
	defer goleak.VerifyNone(t)
	idx := label.NewIndex()
	idx.Add(1, 100)
	idx.Add(1, 101)
	idx.Add(1, 102)
	idx.AddRange(2, 500, 560)
	idx.Add(7, 9)
	img, err := liSerialize(idx)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}

	target := label.NewIndex()
	for k := 0; k < 5; k++ {
		target.Add(0xD00D, graph.NodeID(9000+k))
	}
	before, err := liSerialize(target)
	if err != nil {
		t.Fatalf("serialize the receiver: %v", err)
	}

	accepted, guards := 0, map[string]int{}
	for off := 0; off < len(img); off++ {
		damaged := append([]byte(nil), img...)
		damaged[off] ^= 0xFF

		fresh := label.NewIndex()
		for k := 0; k < 5; k++ {
			fresh.Add(0xD00D, graph.NodeID(9000+k))
		}
		derr := fresh.Deserialize(bytes.NewReader(damaged))
		guards[liClassifyGuard(derr)]++
		if derr == nil {
			accepted++
			t.Errorf("flipping byte %d of %d was ACCEPTED", off, len(img))
			continue
		}
		after, serr := liSerialize(fresh)
		if serr != nil {
			t.Fatalf("re-serialize the receiver after offset %d: %v", off, serr)
		}
		if !bytes.Equal(before, after) {
			t.Errorf("the receiver CHANGED across the refused Deserialize of offset %d", off)
		}
	}
	t.Logf("swept all %d byte offsets: %d accepted, guards %v", len(img), accepted, guards)
	// Every refusal must come from the checksum. If any other guard answered, the
	// flip reached the structural reader, which means the CRC did not cover that
	// byte — a finding, not a pass.
	if guards[liGuardCRC] != len(img) {
		t.Errorf("%d of %d offsets were answered by the CRC; the rest were %v. The trailer covers "+
			"the whole body, so no flip should reach a structural guard",
			guards[liGuardCRC], len(img), guards)
	}
}

// TestLabelIndexScopedSoak_DenseSmallWindow sweeps every cardinality from 1 to
// smallSetMax+8 and requires the serialized form to be a FIXPOINT FROM THE FIRST
// CYCLE at every one of them, by both construction routes.
//
// # What it used to assert, and why it was inverted
//
// It characterised the round-trip NON-idempotence the short layer pinned at one
// width, asserting the window exactly: unstable on the first cycle if and only
// if the label was built by AddRange, long enough for roaring to encode it as a
// run container, and short enough for NodeSetFromBitmap to down-convert it.
//
// rmp #2609 FIXED that non-idempotence — Serialize now normalises the container
// encoding for sets of at most smallSetMax ids, so the emitted bytes follow the
// contents rather than the construction history — and this test began failing at
// widths 4 through 8 with "the AddRange round trip was stable=true", which is the
// repair reporting itself.
//
// IT WAS A SOAK-LAYER TEST, AND #2609's ACCEPTANCE GATE WAS THE SHORT LAYER
// (`go test -race ./graph/index/... ./internal/sim/`), so it was invisible to
// that task and only surfaced when rmp #2620 ran the soak layer. Its short-layer
// sibling, the `dense-small-pin` clause, WAS inverted at the time; this is the
// same inversion, arriving late.
//
// The Add-built control at every width is KEPT and still required to be stable
// from the first cycle. Before the fix it attributed the instability to the run
// encoding; now it guards the other direction — if the Add route ever became
// unstable, the AddRange assertions above would still pass while the form as a
// whole had stopped being reproducible.
func TestLabelIndexScopedSoak_DenseSmallWindow(t *testing.T) {
	defer goleak.VerifyNone(t)
	const base = uint64(32768)
	cycle := func(build func(*label.Index)) (first, second, third int, stable bool) {
		idx := label.NewIndex()
		build(idx)
		i1, err := liSerialize(idx)
		if err != nil {
			t.Fatalf("serialize: %v", err)
		}
		a := label.NewIndex()
		if derr := a.Deserialize(bytes.NewReader(i1)); derr != nil {
			t.Fatalf("first cycle: %v", derr)
		}
		i2, err := liSerialize(a)
		if err != nil {
			t.Fatalf("re-serialize: %v", err)
		}
		b := label.NewIndex()
		if derr := b.Deserialize(bytes.NewReader(i2)); derr != nil {
			t.Fatalf("second cycle: %v", derr)
		}
		i3, err := liSerialize(b)
		if err != nil {
			t.Fatalf("re-serialize: %v", err)
		}
		return len(i1), len(i2), len(i3), bytes.Equal(i1, i2)
	}

	var rows []string
	for w := 1; w <= liSoakWidthMax; w++ {
		width := uint64(w)
		r1, r2, r3, rStable := cycle(func(i *label.Index) {
			i.AddRange(1, graph.NodeID(base), graph.NodeID(base+width-1))
		})
		a1, a2, a3, aStable := cycle(func(i *label.Index) {
			for k := uint64(0); k < width; k++ {
				i.Add(1, graph.NodeID(base+k))
			}
		})
		rows = append(rows, fmt.Sprintf("w=%-3d AddRange %d/%d/%d stable=%-5v | Add %d/%d/%d stable=%v",
			w, r1, r2, r3, rStable, a1, a2, a3, aStable))

		if !rStable {
			t.Errorf("width %d: the AddRange round trip changed the bytes (%d -> %d); since #2609 "+
				"the serialized form is a fixpoint from the FIRST cycle at every width, because "+
				"Serialize normalises the container encoding for sets of at most %d ids so the "+
				"bytes follow the contents rather than the construction history",
				w, r1, r2, liSmallSetMaxMirror)
		}
		if r2 != r3 {
			t.Errorf("width %d: the AddRange form never converges (%d, %d, %d bytes); whatever the "+
				"first cycle does, a form that keeps changing is worse still", w, r1, r2, r3)
		}
		// Within the normalisation bound the two construction routes must agree
		// byte-for-byte: that is the property #2609 delivers, and asserting only
		// fixpoint-ness would be satisfied by two stable-but-different forms.
		if w <= int(liSmallSetMaxMirror) && r1 != a1 {
			t.Errorf("width %d: AddRange serialized to %d bytes and Add to %d; at or below %d ids "+
				"the image must be a function of the contents, not of how the label was built",
				w, r1, a1, liSmallSetMaxMirror)
		}
		if !aStable || a2 != a3 {
			t.Errorf("width %d: the Add-built CONTROL was not byte-stable (%d, %d, %d); without it "+
				"the instability is not attributable to the run encoding", w, a1, a2, a3)
		}
	}
	for _, r := range rows {
		t.Log(r)
	}
}
