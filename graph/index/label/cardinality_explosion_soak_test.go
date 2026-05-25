//go:build soak

package label_test

import "testing"

// TestCardinalityExplosion_Soak runs the same fixture as the short layer but
// scaled to 1 000 000 labels × 1 node each. Activated by -tags=soak.
func TestCardinalityExplosion_Soak(t *testing.T) {
	t.Parallel()
	cardinalityExplosion(t, 1_000_000)
}
