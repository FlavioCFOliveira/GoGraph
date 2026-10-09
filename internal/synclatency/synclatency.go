// Package synclatency hands a test a seeded [wal.SyncLatency], so a test that
// runs on a RAM drive still sees the fsync window of a real device (rmp #3022).
//
// It is the ONLY place the latency is configured from the environment, and it is
// imported only by tests and by the test configuration of example 37; the store
// and WAL packages read no environment variable.
//
//   - GOGRAPH_FSYNC_LATENCY=off disables it: [ForTest] returns nil and the store
//     behaves exactly as a production one.
//   - GOGRAPH_FSYNC_LATENCY_SEED=<uint64> fixes the seed, to replay a failure.
//
// Otherwise every call draws a fresh seed, and the seed is logged when the test
// fails.
package synclatency

import (
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// The environment variables [ForTest] reads.
const (
	EnvSwitch = "GOGRAPH_FSYNC_LATENCY"
	EnvSeed   = "GOGRAPH_FSYNC_LATENCY_SEED"
)

// Min and Max bound the injected delay per fsync: uniform in [Min, Max], the
// range of a consumer SSD's fsync under light load.
const (
	Min = 1 * time.Millisecond
	Max = 5 * time.Millisecond
)

// ForTest returns a seeded [wal.SyncLatency] for tb, or nil when
// GOGRAPH_FSYNC_LATENCY=off. When tb fails, a cleanup logs the seed and the
// command-line setting that replays it. It fails tb if GOGRAPH_FSYNC_LATENCY_SEED
// is set but not an unsigned integer.
func ForTest(tb testing.TB) *wal.SyncLatency {
	tb.Helper()
	if os.Getenv(EnvSwitch) == "off" {
		return nil
	}
	seed, err := Seed()
	if err != nil {
		tb.Fatal(err)
	}
	l := wal.NewSyncLatency(seed, Min, Max)
	tb.Cleanup(func() {
		if tb.Failed() {
			tb.Logf("fsync latency %v-%v, seed %d (replay with %s=%d)", Min, Max, seed, EnvSeed, seed)
		}
	})
	return l
}

// Seed returns GOGRAPH_FSYNC_LATENCY_SEED when it is set, or a fresh seed from the
// clock otherwise.
func Seed() (uint64, error) {
	if v := os.Getenv(EnvSeed); v != "" {
		s, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("synclatency: %s=%q: %w", EnvSeed, v, err)
		}
		return s, nil
	}
	return uint64(time.Now().UnixNano()), nil
}

// Enabled reports whether GOGRAPH_FSYNC_LATENCY leaves the latency on.
func Enabled() bool { return os.Getenv(EnvSwitch) != "off" }
