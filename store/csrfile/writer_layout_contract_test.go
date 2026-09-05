package csrfile

import (
	"errors"
	"hash/crc32"
	"io"
	"testing"
)

// unrepresentableCounts are (NVertices, NEdges, Weight) triples for which
// [Layout] returns a zero totalBytes — its documented signal that the counts
// have no representation on disk.
//
// The first four mirror the overflow corpus in
// TestHeaderValidate_OverflowSafeLayout; the fifth covers the OTHER way Layout
// returns zero, an out-of-range WeightKind, which that corpus does not reach.
var unrepresentableCounts = []struct {
	name              string
	nVertices, nEdges uint64
	weight            WeightKind
}{
	{"NEdges-wrap-on-8x", 0, (uint64(1) << 61) + 1, WeightAbsent},
	{"NVertices-wrap-on-8x", (uint64(1) << 61) + 1, 0, WeightAbsent},
	{"NEdges-max", 0, ^uint64(0), WeightAbsent},
	{"weights-wrap-on-8x", 0, (uint64(1) << 61) + 1, WeightUint64},
	{"weight-kind-out-of-range", 4, 8, weightKindMax + 1},
}

// TestLayoutContract_WriterAndValidatorAgree is the regression gate for the
// second half of rmp #2744.
//
// [Layout]'s godoc names TWO callers that must treat a zero totalBytes as a
// rejection: "the writer and [Header.validate]". Only one of them did.
// Header.validate rejected (format.go), internal/sim's access matrix rejected,
// and writer.go read the zero totalBytes and carried on into the write path.
//
// The two sides answer with DIFFERENT errors, deliberately, because their
// inputs differ: the read side is judging a file that may be corrupt or
// hostile, so it answers with ErrHeaderInconsistent wrapping ErrFileCorrupted;
// the write side is judging an in-memory CSR its own caller supplied, which is
// not corruption, so it answers with ErrNotRepresentable. What must agree — and
// is what this test pins — is the VERDICT: neither side may accept a triple
// Layout refused to lay out.
func TestLayoutContract_WriterAndValidatorAgree(t *testing.T) {
	t.Parallel()

	agreed := 0
	for _, uc := range unrepresentableCounts {
		t.Run("refuse/"+uc.name, func(t *testing.T) {
			t.Parallel()

			// Precondition: the row must genuinely be one Layout refuses.
			// Without this the test could pass by agreeing about nothing.
			if _, total := Layout(uc.nVertices, uc.nEdges, uc.weight); total != 0 {
				t.Fatalf("Layout(%d,%d,%d) totalBytes = %d; this row is not degenerate and proves nothing",
					uc.nVertices, uc.nEdges, uc.weight, total)
			}

			// Write side.
			gotHeader, gotTotal, werr := layoutForWrite(uc.nVertices, uc.nEdges, uc.weight)
			if werr == nil {
				t.Fatalf("layoutForWrite(%d,%d,%d) = (%+v, %d, nil); want an error wrapping ErrNotRepresentable",
					uc.nVertices, uc.nEdges, uc.weight, gotHeader, gotTotal)
			}
			if !errors.Is(werr, ErrNotRepresentable) {
				t.Fatalf("layoutForWrite(%d,%d,%d) err = %v; want errors.Is(err, ErrNotRepresentable)",
					uc.nVertices, uc.nEdges, uc.weight, werr)
			}

			// Read side, same triple.
			h := Header{NVertices: uc.nVertices, NEdges: uc.nEdges, Weight: uc.weight}
			verr := h.validate(1024)
			if verr == nil {
				t.Fatalf("Header{%d,%d,%d}.validate = nil; the two sides disagree about total==0",
					uc.nVertices, uc.nEdges, uc.weight)
			}
			if !errors.Is(verr, ErrFileCorrupted) {
				t.Fatalf("Header{%d,%d,%d}.validate = %v; want errors.Is(err, ErrFileCorrupted)",
					uc.nVertices, uc.nEdges, uc.weight, verr)
			}
		})
		agreed++
	}

	// The control: a triple that IS representable must be accepted by both
	// sides, so the agreement above is not the trivial one of refusing
	// everything. The file length handed to validate is the very totalBytes
	// the write side computed, which is the only length a written file has.
	t.Run("accept/representable-control", func(t *testing.T) {
		t.Parallel()
		const nv, ne = 1000, 4000
		header, total, werr := layoutForWrite(nv, ne, WeightUint64)
		if werr != nil {
			t.Fatalf("layoutForWrite(%d,%d,WeightUint64) = %v; want success on a representable triple", nv, ne, werr)
		}
		if total == 0 {
			t.Fatalf("layoutForWrite(%d,%d,WeightUint64) totalBytes = 0 with a nil error", nv, ne)
		}
		if err := header.validate(int(total)); err != nil {
			t.Fatalf("validate of the header the writer would emit = %v; want nil", err)
		}
	})

	if agreed == 0 {
		t.Fatal("no unrepresentable triple was exercised; the table proves nothing")
	}
}

// TestWriteSections_ZeroHeaderPanics records WHY layoutForWrite's guard has to
// exist, rather than leaving it to look like defensive decoration.
//
// When Layout refuses a triple it returns the ZERO Header alongside the zero
// totalBytes. Before rmp #2744 the writer took that zero Header and proceeded.
// Its first act is writeSections, whose first act is a padding run of
// header.VerticesOffset - HeaderSize; with VerticesOffset == 0 that subtraction
// underflows uint64 to 18446744073709551552, and make([]byte, n) panics.
//
// So "proceed with bogus offsets", the outcome Layout's godoc tells callers to
// avoid, was never a corrupt-file outcome — it was a panic, raised AFTER the
// temp file had been created and with no deferred cleanup to remove it.
func TestWriteSections_ZeroHeaderPanics(t *testing.T) {
	t.Parallel()

	// The zero Header is exactly what Layout hands back on refusal; take it
	// from Layout rather than writing Header{} by hand, so this test tracks
	// Layout's refusal value if it ever changes.
	zero, total := Layout(0, ^uint64(0), WeightAbsent)
	if total != 0 || zero != (Header{}) {
		t.Fatalf("Layout(0, MaxUint64, WeightAbsent) = (%+v, %d); want the zero header and zero total", zero, total)
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("writeSections accepted the zero header without panicking; " +
				"if that is now genuinely safe, layoutForWrite's guard needs its rationale rewritten, not deleted")
		}
		t.Logf("writeSections(zero header) panicked as expected: %v", r)
	}()

	_ = writeSections[struct{}](io.Discard, crc32.New(castagnoli), zero, nil, nil, nil)
}
