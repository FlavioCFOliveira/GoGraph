//go:build gograph_crashinject

package crashinject_test

// rollover_test.go — segment rollover under extreme concurrency
// (docs/design-wal-v2.md §2.3, §8, §9 step 2). 256 committers write through
// the durable store API onto a WAL of 1 MiB segments with a seeded 1-5 ms fsync
// latency, and the child is SIGKILL'd inside a rollover: after the old segment
// is flushed and before its fdatasync, or after the switch to the new segment
// and before its first frame. Every acknowledged commit must survive and every
// recovered transaction must be whole — the same oracle the concurrent-writers
// tests use (checkConcurrent).

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/crashinject"
)

const (
	rollWriters   = 256
	rollWarmup    = 4
	rollPerWriter = 40
	rollUniverse  = rollWarmup + rollWriters*rollPerWriter
	// rollSegment is wal.MinSegmentSize: the workload (about 600 bytes per
	// transaction) crosses several segments.
	rollSegment = 1 << 20
	// rollSkip lets the first rollover through and kills at the second, so the
	// crash lands after a whole segment of acknowledged work.
	rollSkip = 1
	// rollLatencySeed seeds the fsync latency the child installs.
	rollLatencySeed = 0x3020
)

func runRolloverCrash(t *testing.T, scenario string) (string, map[int64]bool) {
	t.Helper()
	out, err := crashinject.Run(t, scenario, crashinject.Opts{
		Timeout: 120 * time.Second,
		Env: []string{
			"GOGRAPH_CRASH_WRITERS=" + strconv.Itoa(rollWriters),
			"GOGRAPH_CRASH_WARMUP=" + strconv.Itoa(rollWarmup),
			"GOGRAPH_CRASH_PERWRITER=" + strconv.Itoa(rollPerWriter),
			"GOGRAPH_CRASH_AFTER=" + strconv.Itoa(rollSkip),
			"GOGRAPH_CRASH_SEGMENT=" + strconv.Itoa(rollSegment),
			"GOGRAPH_CRASH_SYNCLAT=" + strconv.Itoa(rollLatencySeed),
		},
	})
	if err != nil {
		t.Fatalf("crashinject.Run(%s): %v", scenario, err)
	}
	if out.TimedOut {
		t.Fatalf("%s: child timed out\nstderr: %s", scenario, out.Stderr)
	}
	if !out.Killed {
		t.Fatalf("%s: child not SIGKILL'd (exit %d): the workload did not reach a second rollover\nstdout tail: %.400s\nstderr: %s",
			scenario, out.ExitCode, out.Stdout[max(0, len(out.Stdout)-400):], out.Stderr)
	}
	acked := parseAcks(t, out.Stdout)
	if len(acked) < rollWarmup {
		t.Fatalf("%s: only %d acknowledged commits before the crash", scenario, len(acked))
	}
	t.Logf("%s: child SIGKILL'd after %d acknowledged commits", scenario, len(acked))
	return out.Dir, acked
}

// TestCrashRecovery_Rollover_256Committers kills the child inside a segment
// rollover at each of its two breakpoints.
func TestCrashRecovery_Rollover_256Committers(t *testing.T) {
	for _, scenario := range []string{
		"wal.rollover.old-flushed-pre-fsync",
		"wal.rollover.switched-pre-first-frame",
	} {
		t.Run(scenario, func(t *testing.T) {
			dir, acked := runRolloverCrash(t, scenario)
			if v := checkConcurrent(recoverGraph(t, dir), rollUniverse, acked); len(v) > 0 {
				t.Errorf("%s: recovery violated the ACID contract:\n%s", scenario, strings.Join(v, "\n"))
			}
		})
	}
}
