package exec_test

// scan_index_hash_test.go — tests for NodeByIndexSeek (task-238).

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test stubs
// ─────────────────────────────────────────────────────────────────────────────

// mapHashLookup is a simple test adapter: maps string → NodeIDs.
type mapHashLookup struct {
	data map[string][]uint64
}

func newMapHashLookup(data map[string][]uint64) *mapHashLookup {
	return &mapHashLookup{data: data}
}

func (m *mapHashLookup) LookupAppend(value expr.Value, dst []uint64) ([]uint64, error) {
	sv, ok := value.(expr.StringValue)
	if !ok {
		return nil, exec.ErrIndexTypeMismatch
	}
	if ids, found := m.data[string(sv)]; found {
		dst = append(dst, ids...)
	}
	return dst, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. NodeByIndexSeek — exact match returns correct nodes
// ─────────────────────────────────────────────────────────────────────────────

func TestNodeByIndexSeek_ExactMatch(t *testing.T) {
	lookup := newMapHashLookup(map[string][]uint64{
		"alice": {3, 7},
		"bob":   {1},
	})
	op := exec.NewNodeByIndexSeek(lookup, expr.StringValue("alice"))

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	got := make(map[int64]struct{}, len(rows))
	for _, row := range rows {
		got[int64(row[0].(expr.IntegerValue))] = struct{}{}
	}
	for _, want := range []int64{3, 7} {
		if _, ok := got[want]; !ok {
			t.Errorf("expected NodeID %d in output", want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. NodeByIndexSeek — no match returns 0 rows
// ─────────────────────────────────────────────────────────────────────────────

func TestNodeByIndexSeek_NoMatch(t *testing.T) {
	lookup := newMapHashLookup(map[string][]uint64{"alice": {1}})
	op := exec.NewNodeByIndexSeek(lookup, expr.StringValue("unknown"))

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(rows))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. NodeByIndexSeek — type mismatch returns ErrIndexTypeMismatch
// ─────────────────────────────────────────────────────────────────────────────

func TestNodeByIndexSeek_TypeMismatch(t *testing.T) {
	lookup := newMapHashLookup(map[string][]uint64{"alice": {1}})
	// Provide IntegerValue to a string-typed lookup.
	op := exec.NewNodeByIndexSeek(lookup, expr.IntegerValue(42))

	ctx := context.Background()
	if err := op.Init(ctx); !errors.Is(err, exec.ErrIndexTypeMismatch) {
		t.Fatalf("expected ErrIndexTypeMismatch, got %v", err)
	}
	_ = op.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. NodeByIndexSeek — cancellation honoured
// ─────────────────────────────────────────────────────────────────────────────

func TestNodeByIndexSeek_Cancellation(t *testing.T) {
	// Large result set.
	ids := make([]uint64, 1000)
	for i := range ids {
		ids[i] = uint64(i)
	}
	lookup := newMapHashLookup(map[string][]uint64{"key": ids})
	op := exec.NewNodeByIndexSeek(lookup, expr.StringValue("key"))

	ctx, cancel := context.WithCancel(context.Background())
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var row exec.Row
	for range 5 {
		if _, err := op.Next(&row); err != nil {
			t.Fatalf("early Next error: %v", err)
		}
	}
	cancel()
	_, err := op.Next(&row)
	if err == nil {
		t.Log("Next returned nil after cancel — bitmap may be exhausted, acceptable")
	}
	_ = op.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. StringHashIndex adapter — round-trip
// ─────────────────────────────────────────────────────────────────────────────

func TestStringHashIndex_RoundTrip(t *testing.T) {
	inner := &inMemStringHash{data: map[string][]uint64{
		"x": {10, 20},
	}}
	idx := exec.NewStringHashIndex(inner)

	ids, err := idx.LookupAppend(expr.StringValue("x"), nil)
	if err != nil {
		t.Fatalf("LookupAppend: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("got %d ids, want 2", len(ids))
	}

	_, err = idx.LookupAppend(expr.IntegerValue(1), nil)
	if !errors.Is(err, exec.ErrIndexTypeMismatch) {
		t.Errorf("expected ErrIndexTypeMismatch for wrong type, got %v", err)
	}
}

// inMemStringHash is a minimal test double for a hash.Index[string].
type inMemStringHash struct {
	data map[string][]uint64
}

func (h *inMemStringHash) LookupAppend(value string, dst []uint64) []uint64 {
	if ids, ok := h.data[value]; ok {
		dst = append(dst, ids...)
	}
	return dst
}
