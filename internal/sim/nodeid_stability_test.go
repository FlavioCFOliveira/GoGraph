package sim

import (
	"context"
	"testing"
)

// TestSimulator_NodeIDStabilityAcrossCrashes runs the full-stack crash and
// checkpoint storm with the NodeID stability oracle armed at every recovery (WAL
// v2 step 3): a node alive on both sides of a recovery keeps its id. The run fails
// on a violation; this test adds the non-vacuity witness that the oracle compared
// surviving nodes.
func TestSimulator_NodeIDStabilityAcrossCrashes(t *testing.T) {
	for _, seed := range []uint64{0xF0FA, 0x3021} {
		s := runFullStackSim(t, fullStackConfig(seed))
		if s.CrashCount() == 0 {
			t.Fatalf("seed %#x: no crash fired; the oracle never ran", seed)
		}
		if s.NodeIDsCompared() == 0 {
			t.Fatalf("seed %#x: the NodeID stability oracle compared no surviving node across %d crashes", seed, s.CrashCount())
		}
		t.Logf("seed %#x: %d crashes, %d node ids compared", seed, s.CrashCount(), s.NodeIDsCompared())
	}
}

// TestMVCCSessionsCrash_NodeIDStability is the same oracle in the multi-session
// mode, whose overlapping explicit transactions roll back and abort: a rolled-back
// creation keeps its mapper slot in the process that ran it, which is what moved
// ids across a replay before WAL v2 step 3.
func TestMVCCSessionsCrash_NodeIDStability(t *testing.T) {
	for _, seed := range []uint64{1, 7, 42} {
		res, err := RunMVCCSessions(context.Background(), mvccCrashConfig(seed))
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if !res.Clean() {
			t.Fatalf("seed %d: violations=%v foldErrors=%v", seed, res.Violations, res.FoldErrors)
		}
		if res.Crashes == 0 || res.NodeIDsCompared == 0 {
			t.Fatalf("seed %d: vacuous: %d crashes, %d node ids compared", seed, res.Crashes, res.NodeIDsCompared)
		}
		t.Logf("seed %d: %d crashes, %d node ids compared", seed, res.Crashes, res.NodeIDsCompared)
	}
}
