//go:build soak

package lpg

// mvcc_label_index_atomicity_soak_test.go — the load-bearing variant of the
// label-bag/label-index atomicity gate (rmp #2326).
//
// Layer: soak.

import (
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// TestLabelIndexSoak_NeverMissesALabelTheBagHas is the variant with enough budget
// to be believed. Detection is probabilistic in the window's width, and 8 s already
// caught the pre-fix ordering in 4 of 4 runs under -race; 45 s carries a wide margin
// over that on a slower or busier host, which the short-layer variant cannot afford.
//
// See [assertLabelIndexNeverMissesABagLabel] for the invariant and why its direction
// is the unrecoverable one.
func TestLabelIndexSoak_NeverMissesALabelTheBagHas(t *testing.T) {
	testlayers.RequireSoak(t)
	assertLabelIndexNeverMissesABagLabel(t, 45*time.Second)
}
