package testlayers

import "testing"

// TestSampleShort is the canonical "this layer is always on" test.
// It runs under every layer (short, soak, nightly) because no build
// tag and no Require* gate is present. The body is intentionally
// trivial so adding test layers does not regress the default-layer
// budget.
func TestSampleShort(t *testing.T) {
	t.Parallel()
	if IsSoak && !soakEnabled() {
		t.Fatalf("inconsistent layer state: IsSoak=true but soakEnabled()=false")
	}
	if IsNightly && !nightlyEnabled() {
		t.Fatalf("inconsistent layer state: IsNightly=true but nightlyEnabled()=false")
	}
}
