//go:build soak || nightly

package sim

// checkpoint_crash_storm_soak_test.go — the full-scale rmp #2465 crash-during-
// publish storm: production-like connection counts, so the interrupted publish
// is raced by a fleet of committers rather than a handful. Runs under the soak
// layer only (docs/test-layers.md); the short layer runs the smaller
// configuration in checkpoint_crash_storm_test.go.

import (
	"context"
	"testing"

	"go.uber.org/goleak"
)

// soakCheckpointStorm is the full-scale configuration this layer runs.
func soakCheckpointStorm() checkpointStormConfig {
	return checkpointStormConfig{connections: 256, opsPerConn: 16}
}

// TestCheckpointCrashStorm_SoakFullScale drives the three publish windows at
// production-like concurrency and requires the same durability contract: no
// acknowledged commit lost, no half-published snapshot loaded.
func TestCheckpointCrashStorm_SoakFullScale(t *testing.T) {
	defer goleak.VerifyNone(t)
	report, err := runCheckpointCrashStorm(context.Background(), 0x2465_50A4, soakCheckpointStorm())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report != nil {
		t.Fatalf("full-scale checkpoint crash storm failed:\n%s", report.String())
	}
}
