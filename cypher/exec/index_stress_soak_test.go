//go:build soak || nightly

package exec_test

// index_stress_soak_test.go — the 1024-goroutine concurrent index stress level
// (rmp #2672).
//
// # Why this file is soak-layer
//
// CLAUDE.md's EXTREME/MASSIVE Concurrent Ready mandate publishes latency and
// throughput at 1, 8, 64, 256 and 1024 goroutines, so 1024 concurrent readers is
// a level this module is committed to exercising. It cannot live in the short
// layer, because under -race it is the single most expensive test in the module
// relative to the work it does.
//
// The reason is measured, not assumed. Every reader calls label.Index.Count,
// which takes i.mu.RLock() on ONE shared sync.RWMutex, and ThreadSanitizer must
// order each acquire against every other goroutine participating in that sync
// object — so cost grows superlinearly in the number of distinct goroutines
// sharing it. On the reference host (Apple M4, 10 cores, darwin/arm64,
// go1.27.0), quiet, under -race:
//
//	  10 readers ....... 0.12 s
//	  25 readers ....... 4.43 s
//	  50 readers ....... 9.98 s   <- the short layer's level
//	 100 readers ...... 34.24 s
//	 200 readers ..... 112.55 s
//	1000 readers, in-package, -count=3: 49.35 s, 76.59 s, 174.32 s (253 % spread)
//
// Two control arms establish that the shared sync object is the cause rather
// than the readers' own work: 1000 readers spinning without touching the index
// cost 0.00 s, and 1000 readers each reading their OWN label.Index cost 0.24 s —
// 343x cheaper than the same work on the shared one.
//
// So the short layer keeps the exercise at the measured 50-reader point (see
// index_stress_test.go) and this file keeps the published 1024 level. No
// coverage is removed; the cost is relocated. HARD_BUDGET stays 240 s and
// cypher/exec gets no per-package override — see docs/test-layers.md.

import (
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// massiveConcurrencyStressReaders is the published EXTREME-concurrency level from
// CLAUDE.md's mandate (1, 8, 64, 256, 1024). It lives in this soak-tagged file,
// not beside its short-layer counterpart, because a constant referenced only
// from a build-tagged file is unused in the default build.
const massiveConcurrencyStressReaders = 1024

// TestIndexBuffer_MassiveConcurrentStress is TestIndexBuffer_ConcurrentStress at
// the published 1024-goroutine concurrency level. It shares the same driver, so
// the assertion and the shape cannot drift from the short-layer variant.
func TestIndexBuffer_MassiveConcurrentStress(t *testing.T) {
	testlayers.RequireSoak(t)
	defer goleak.VerifyNone(t)

	runIndexBufferConcurrentStress(t, massiveConcurrencyStressReaders)
}
