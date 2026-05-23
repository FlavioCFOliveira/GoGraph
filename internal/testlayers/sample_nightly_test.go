package testlayers

import "testing"

// TestSampleNightly is the canonical nightly-layer test. It calls
// RequireNightly and so skips with no observable cost when the
// nightly layer is inactive. When the nightly layer is active
// (build tag `nightly`, or env `GOGRAPH_NIGHTLY=1`) the body runs
// and verifies the helper's view of the world matches the process
// state.
func TestSampleNightly(t *testing.T) {
	RequireNightly(t)
	if !nightlyEnabled() {
		t.Fatalf("RequireNightly admitted the test but nightlyEnabled() returned false")
	}
}
