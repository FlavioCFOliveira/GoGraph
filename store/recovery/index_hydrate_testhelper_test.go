package recovery

// index_hydrate_testhelper_test.go — the test-only stand-in for the production
// loader rmp #2490 deleted.
//
// Recovery used to carry an applySnapshotIndexes helper that fed every snapshot
// index payload into the recovered graph's index.Manager. It was DEAD: recovery
// builds its own graph and never attaches a manager, so the helper returned 0 at
// its first guard on every execution, and Result.SnapshotIndexes was provably
// always 0. It could not be repaired either — WAL replay cannot maintain a
// registered index, so an index registered by recovery would be frozen at the
// snapshot instant while still seekable by the planner.
//
// The helper is gone from production, but the property several tests in this
// package used it to establish is still worth holding: that a real snapshot's
// indexes/<name>.bin payload round-trips back into a fresh index of the same
// kind. That is a serialise/deserialise contract of graph/index and
// store/snapshot, independent of who calls it, so the tests keep asserting it
// through this local helper instead of losing the coverage.
//
// Hydration as the engine actually performs it — per index, by name, only when
// recovery certified the payload usable — is covered behaviourally in package
// cypher (index_hydration_test.go), and the classification recovery reports is
// covered directly in index_payloads_test.go.

import (
	"bytes"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
)

// hydrateReadbacksForTest deserialises every readback in rb into the index
// registered under the same name in mgr and returns how many succeeded. It
// mirrors what the cypher engine does with a certified payload, reduced to the
// one step these round-trip tests care about; unlike the engine it does not
// consult a staleness reason, because the point here is the byte-level
// round-trip and nothing else.
//
// A readback naming an index the manager does not hold, a subscriber that is not
// an index.Serializer, nil bytes, or a failing Deserialize all count as zero and
// are reported through t.Logf rather than failing, so a caller can assert the
// exact successful count (including a deliberate partial one).
func hydrateReadbacksForTest(t *testing.T, mgr *index.Manager, rb []snapshot.IndexReadback) int {
	t.Helper()
	loaded := 0
	for i := range rb {
		r := rb[i]
		sub, err := mgr.GetIndex(r.Name)
		if err != nil {
			t.Logf("hydrateReadbacksForTest: index %q not registered: %v", r.Name, err)
			continue
		}
		ser, ok := sub.(index.Serializer)
		if !ok {
			t.Logf("hydrateReadbacksForTest: index %q does not implement index.Serializer", r.Name)
			continue
		}
		if r.Bytes == nil {
			t.Logf("hydrateReadbacksForTest: index %q has nil bytes (missing file or CRC mismatch)", r.Name)
			continue
		}
		if derr := ser.Deserialize(bytes.NewReader(r.Bytes)); derr != nil {
			t.Logf("hydrateReadbacksForTest: index %q Deserialize failed: %v", r.Name, derr)
			continue
		}
		loaded++
	}
	return loaded
}
