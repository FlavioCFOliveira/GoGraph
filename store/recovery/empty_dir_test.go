package recovery

import (
	"testing"

	"gograph/store/txn"
)

// TestRecovery_EmptyDir exercises [Open] on an empty directory across
// two instantiations: the canonical (string, int64) and the numeric
// (int64, float64). It anchors the canonical [Open] entry point and
// confirms that the zero-state contract holds for non-string key
// types.
//
// Expectation per both sub-tests:
//   - No error (empty dir is a valid cold-start state).
//   - [Result.SnapshotHit] == false.
//   - [Result.WALOps] == 0.
//   - [Result.Graph] is non-nil and has zero nodes and zero edges.
func TestRecovery_EmptyDir(t *testing.T) {
	t.Parallel()

	t.Run("string_int64", func(t *testing.T) {
		t.Parallel()
		res, err := Open[string, int64](t.TempDir(), Options[string, int64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewInt64WeightCodec(),
		})
		if err != nil {
			t.Fatalf("Open empty dir = %v, want nil", err)
		}
		if res.SnapshotHit {
			t.Fatal("SnapshotHit on empty dir should be false")
		}
		if res.WALOps != 0 {
			t.Fatalf("WALOps = %d, want 0", res.WALOps)
		}
		if res.Graph == nil {
			t.Fatal("Graph must be non-nil even on empty dir")
		}
		if res.Graph.AdjList().Order() != 0 {
			t.Fatalf("Order = %d, want 0", res.Graph.AdjList().Order())
		}
		if res.Graph.AdjList().Size() != 0 {
			t.Fatalf("Size = %d, want 0", res.Graph.AdjList().Size())
		}
	})

	t.Run("int64_float64", func(t *testing.T) {
		t.Parallel()
		res, err := Open[int64, float64](t.TempDir(), Options[int64, float64]{
			Codec:       txn.NewInt64Codec(),
			WeightCodec: txn.NewFloat64WeightCodec(),
		})
		if err != nil {
			t.Fatalf("Open[int64,float64] empty dir = %v, want nil", err)
		}
		if res.SnapshotHit {
			t.Fatal("SnapshotHit on empty dir should be false")
		}
		if res.WALOps != 0 {
			t.Fatalf("WALOps = %d, want 0", res.WALOps)
		}
		if res.Graph == nil {
			t.Fatal("Graph must be non-nil even on empty dir")
		}
		if res.Graph.AdjList().Order() != 0 {
			t.Fatalf("Order = %d, want 0", res.Graph.AdjList().Order())
		}
	})
}
