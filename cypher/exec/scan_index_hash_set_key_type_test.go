package exec_test

// scan_index_hash_set_key_type_test.go — NodeByIndexSeekSet.Init refuses a key
// whose type the index does not hold with a typed error instead of skipping it
// (rmp #2961).

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// TestNodeByIndexSeekSet_MismatchedKeyTypeIsTypedError: a string index cannot
// answer the integer key 5 — a node whose property is the integer 5 is absent
// from it — so Init must fail with *SeekSetKeyTypeError, matchable as
// ErrIndexTypeMismatch, and emit nothing. Before the fix the key was skipped and
// Init returned the string key's node alone.
func TestNodeByIndexSeekSet_MismatchedKeyTypeIsTypedError(t *testing.T) {
	idx := exec.NewStringHashIndex(&inMemStringHash{data: map[string][]uint64{"a": {10}, "b": {20}}})
	keys := []expr.Value{expr.StringValue("a"), expr.Null, expr.IntegerValue(5), expr.StringValue("b")}
	op := exec.NewNodeByIndexSeekSet(idx, keys, 0)
	err := op.Init(context.Background())
	if !errors.Is(err, exec.ErrIndexTypeMismatch) {
		t.Fatalf("Init with the integer key 5 on a string index returned %v, want ErrIndexTypeMismatch", err)
	}
	var kt *exec.SeekSetKeyTypeError
	if !errors.As(err, &kt) {
		t.Fatalf("Init returned %T (%v), want *exec.SeekSetKeyTypeError", err, err)
	}
	if kt.Index != 2 || kt.Kind != expr.KindInteger {
		t.Fatalf("SeekSetKeyTypeError{Index: %d, Kind: %s}, want {Index: 2, Kind: %s}", kt.Index, kt.Kind, expr.KindInteger)
	}
	var row exec.Row
	if ok, nerr := op.Next(&row); ok || nerr != nil {
		t.Fatalf("Next after a failed Init returned (%v, %v), want (false, nil)", ok, nerr)
	}
	_ = op.Close()
}

// TestNodeByIndexSeekSet_StringAndNullKeysServed: string keys are probed and a
// NULL key is skipped — it matches nothing — so the set is answered without an
// error.
func TestNodeByIndexSeekSet_StringAndNullKeysServed(t *testing.T) {
	idx := exec.NewStringHashIndex(&inMemStringHash{data: map[string][]uint64{"a": {10}, "b": {20}}})
	keys := []expr.Value{expr.StringValue("b"), expr.Null, expr.StringValue("a"), nil}
	op := exec.NewNodeByIndexSeekSet(idx, keys, 0)
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var got []int64
	var row exec.Row
	for {
		ok, err := op.Next(&row)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		got = append(got, int64(row[0].(expr.IntegerValue)))
	}
	_ = op.Close()
	if len(got) != 2 || got[0] != 10 || got[1] != 20 {
		t.Fatalf("ids %v, want [10 20]", got)
	}
}
