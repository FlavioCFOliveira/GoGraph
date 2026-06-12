//go:build soak || nightly

package hash_test

import "testing"

// TestPropertySingleton_Soak is the soak-layer variant of [TestPropertySingleton],
// scaled to 1 000 000 nodes sharing a single value. Activated by -tags=soak.
func TestPropertySingleton_Soak(t *testing.T) {
	t.Parallel()
	propertySingleton(t, 1_000_000)
}
