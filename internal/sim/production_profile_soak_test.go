//go:build soak || nightly

package sim

// production_profile_soak_test.go — the full-scale rmp #2441 production
// profile: production-like connection counts over more crash cycles. Runs
// under the soak layer only (docs/test-layers.md); the short layer runs the
// smaller configuration in production_profile_test.go.

import (
	"context"
	"testing"

	"go.uber.org/goleak"
)

// soakProductionProfile is the full-scale configuration this layer runs:
// production-like connection counts over more cycles.
func soakProductionProfile() productionProfileConfig {
	return productionProfileConfig{connections: 256, opsPerConn: 12, cycles: 3, counters: 4}
}

// TestProductionProfile_SoakFullScale runs the 256-connection profile.
func TestProductionProfile_SoakFullScale(t *testing.T) {
	defer goleak.VerifyNone(t)
	report, err := runProductionProfile(context.Background(), 0x9600D0C5, soakProductionProfile())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report != nil {
		t.Fatalf("full-scale production profile failed:\n%s", report.String())
	}
}
