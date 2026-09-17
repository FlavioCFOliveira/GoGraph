package exec

import (
	"testing"
	"unsafe"
)

// TestExpandLayoutUnchangedByStoredDir guards the placement rule the Expand
// struct documents (rmp #2629): adding a field EARLIER in it shifts every field
// after it, and the reverse-cursor state is cache-line sensitive — placing
// dstAdmit beside multiplicity measured +6.74% median on a benchmark carrying no
// gate at all.
//
// rmp #2864 added two bools, emitStoredDir and cPendInv. Both are appended at
// the END, where they share the word dstGated already occupies, so the struct
// neither grows nor moves anything. These are the numbers measured on HEAD
// (a2c04c10) before the change, and cPendInv was moved here from beside the
// other cPend* fields precisely because sitting there pushed the four bools
// after cPendRemaining one byte along.
//
// A failure is not necessarily a defect — a Go release may repack the struct —
// but it does mean the layout moved, and the rule is that a layout move is
// measured with the two-binary interleaved A/B on
// BenchmarkExpandDir_InVsOut_Baseline before it is accepted.
func TestExpandLayoutUnchangedByStoredDir(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"Sizeof(Expand)", unsafe.Sizeof(Expand{}), 576},
		{"Offsetof(dstAdmit)", unsafe.Offsetof(Expand{}.dstAdmit), 560},
		{"Offsetof(dir)", unsafe.Offsetof(Expand{}.dir), 552},
		{"Offsetof(slotsRejected)", unsafe.Offsetof(Expand{}.slotsRejected), 496},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d (the layout moved: re-run the two-binary "+
				"A/B on BenchmarkExpandDir_InVsOut_Baseline before accepting it)",
				tc.name, tc.got, tc.want)
		}
	}
}
