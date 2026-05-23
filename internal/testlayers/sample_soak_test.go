package testlayers

import "testing"

// TestSampleSoak is the canonical soak-layer test. It calls
// RequireSoak and so skips with no observable cost when the soak
// layer is inactive. When the soak layer is active (build tag
// `soak`, or env `SOAK_FULL=1`, or any nightly opt-in) the body
// runs and verifies the helper's view of the world matches the
// process state.
func TestSampleSoak(t *testing.T) {
	RequireSoak(t)
	if !soakEnabled() {
		t.Fatalf("RequireSoak admitted the test but soakEnabled() returned false")
	}
}
