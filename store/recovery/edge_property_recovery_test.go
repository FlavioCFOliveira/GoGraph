package recovery

// edge_property_recovery_test.go — regression gate for task #1418.
//
// applyOp (txn/txn.go) and applySetEdgePropertyByHandle (recovery.go) both
// ignored the error returned by lpg.Graph.SetEdgePropertyByHandle when the
// installed SchemaValidator rejected a write. The sibling ops OpSetNodeProperty
// and OpSetEdgeProperty propagate the error (ErrCommittedNotApplied on Commit,
// fail-stop on recovery), so OpSetEdgePropertyByHandle must do the same.
//
// Fix:
//   - txn.go OpSetEdgePropertyByHandle: return g.SetEdgePropertyByHandle(...)
//   - recovery.go applySetEdgePropertyByHandle: check error, return false on rejection
//
// Tests:
//  1. Txn path (txn.go): Commit with a rejecting validator returns
//     ErrCommittedNotApplied — consistent with OpSetNodeProperty.
//  2. Recovery path (recovery.go): applySetEdgePropertyByHandle returns false
//     when the validator rejects the value (white-box via internal function).
//  3. Consistency: both node-property and edge-property-by-handle reject paths
//     surface the same error from lpg.Graph, confirming uniform behaviour.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// rejectingValidator is a lpg.SchemaValidator that rejects every write for the
// named property key unconditionally.
type rejectingValidator struct{ key string }

func (v *rejectingValidator) Validate(propertyName string, _ lpg.PropertyValue) error {
	if propertyName == v.key {
		return fmt.Errorf("rejectingValidator: rejected write to property %q", v.key)
	}
	return nil
}

// buildSetEdgePropertyByHandleRest encodes the "rest" body expected by
// applySetEdgePropertyByHandle: uint16 key-len + key + Int64Value bytes
// (tag 0x01 + 8-byte LE int64) + 8-byte trailing handle.
func buildSetEdgePropertyByHandleRest(key string, val int64, handle uint64) []byte {
	keyLen := len(key)
	buf := make([]byte, 2+keyLen+1+8+8)
	binary.LittleEndian.PutUint16(buf[0:], uint16(keyLen))
	copy(buf[2:], key)
	off := 2 + keyLen
	buf[off] = 0x01 // PropertyKindInt64 tag (graph/lpg property encoding)
	off++
	binary.LittleEndian.PutUint64(buf[off:], uint64(val))
	off += 8
	binary.LittleEndian.PutUint64(buf[off:], handle)
	return buf
}

// TestEdgePropertyByHandle_Txn_ValidatorRejection_ReportsErrCommittedNotApplied
// exercises the txn.go applyOp path.
//
// Pre-fix: _ = g.SetEdgePropertyByHandle(...) silences the error → Commit nil.
// Post-fix: return g.SetEdgePropertyByHandle(...) propagates ErrCommittedNotApplied.
func TestEdgePropertyByHandle_Txn_ValidatorRejection_ReportsErrCommittedNotApplied(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	const propKey = "secret"
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	// Install the validator before building the store so it is active during
	// the in-memory apply phase of Commit (applyOp runs after WAL fsync).
	g.SetValidator(&rejectingValidator{key: propKey})
	defer g.SetValidator(nil)

	s := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})

	tx := s.Begin()
	mustTx(t, tx.AddNode("x"))
	mustTx(t, tx.AddNode("y"))
	mustTx(t, tx.AddEdge("x", "y", 1))
	mustTx(t, tx.SetEdgePropertyByHandle("x", "y", 1, propKey, lpg.Int64Value(42)))

	commitErr := tx.Commit()
	if commitErr == nil {
		t.Fatal("Commit returned nil with a rejecting validator; expected ErrCommittedNotApplied")
	}
	if !errors.Is(commitErr, txn.ErrCommittedNotApplied) {
		t.Fatalf("Commit error = %v; want errors.Is(..., txn.ErrCommittedNotApplied)", commitErr)
	}

	_ = w.Close()
}

// TestEdgePropertyByHandle_Recovery_ValidatorRejection_IsFailStop exercises
// applySetEdgePropertyByHandle directly (white-box).
//
// Pre-fix: returns true (error silenced). Post-fix: returns false (fail-stop).
func TestEdgePropertyByHandle_Recovery_ValidatorRejection_IsFailStop(t *testing.T) {
	t.Parallel()

	const propKey = "classified"

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode a: %v", err)
	}
	if err := g.AddNode("b"); err != nil {
		t.Fatalf("AddNode b: %v", err)
	}
	h, err := g.AddEdgeH("a", "b", 0)
	if err != nil {
		t.Fatalf("AddEdgeH: %v", err)
	}

	g.SetValidator(&rejectingValidator{key: propKey})
	defer g.SetValidator(nil)

	rest := buildSetEdgePropertyByHandleRest(propKey, 99, h)
	ok := applySetEdgePropertyByHandle(g, "a", "b", rest)
	if ok {
		t.Fatal("applySetEdgePropertyByHandle returned true with a rejecting validator; expected false (fail-stop)")
	}
}

// TestEdgePropertyByHandle_Recovery_ConsistentWithSetNodeProperty confirms
// that the graph-level lpg.Graph methods used by both apply paths surface errors
// uniformly when the validator rejects a write — OpSetEdgePropertyByHandle and
// OpSetNodeProperty must therefore exhibit the same fail-stop behaviour.
func TestEdgePropertyByHandle_Recovery_ConsistentWithSetNodeProperty(t *testing.T) {
	t.Parallel()

	const propKey = "forbidden"

	// Sub-test A: SetEdgePropertyByHandle returns error on validator rejection.
	t.Run("SetEdgePropertyByHandle_returns_error", func(t *testing.T) {
		t.Parallel()
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		if err := g.AddNode("a"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.AddNode("b"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		h, err := g.AddEdgeH("a", "b", 0)
		if err != nil {
			t.Fatalf("AddEdgeH: %v", err)
		}
		g.SetValidator(&rejectingValidator{key: propKey})
		defer g.SetValidator(nil)

		if err := g.SetEdgePropertyByHandle("a", "b", h, propKey, lpg.Int64Value(1)); err == nil {
			t.Fatal("SetEdgePropertyByHandle: expected error from validator, got nil")
		}
	})

	// Sub-test B: SetNodeProperty returns error on validator rejection (sibling).
	t.Run("SetNodeProperty_returns_error", func(t *testing.T) {
		t.Parallel()
		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		if err := g.AddNode("n"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		g.SetValidator(&rejectingValidator{key: propKey})
		defer g.SetValidator(nil)

		if err := g.SetNodeProperty("n", propKey, lpg.Int64Value(1)); err == nil {
			t.Fatal("SetNodeProperty: expected error from validator, got nil")
		}
	})
}
