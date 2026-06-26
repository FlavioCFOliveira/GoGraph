package jsonl

// encode_bound_test.go — regression gate for #1792 (sprint 250): the JSONL list
// encoding embeds each nested level as a re-escaped JSON string, so serialised
// size grows ~4x per nesting level. A list nested a few dozen deep used to OOM
// or hang the writer from a trivially small in-memory value. The encoder now
// fails fast with a typed error, in bounded time.

import (
	"errors"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestEncodePropertyValue_DeepNestBounded_1792(t *testing.T) {
	// Build a list nested 300 deep around a small string payload. Unbounded,
	// the ~4x/level re-escaping would explode long before this completes.
	v := lpg.StringValue("x")
	for i := 0; i < 300; i++ {
		v = lpg.ListValue([]lpg.PropertyValue{v})
	}

	done := make(chan struct{})
	var kind, val string
	var err error
	go func() {
		kind, val, err = encodePropertyValue(v)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("encodePropertyValue did not return within 10s — blowup not bounded")
	}

	if err == nil {
		t.Fatalf("expected a typed error for deeply nested list, got (%q,%d bytes,nil)", kind, len(val))
	}
	if !errors.Is(err, ErrPropertyNestingTooDeep) && !errors.Is(err, ErrPropertyValueTooLarge) {
		t.Fatalf("expected ErrPropertyNestingTooDeep or ErrPropertyValueTooLarge, got %v", err)
	}
}

func TestEncodePropertyValue_ShallowListStillRoundTrips_1792(t *testing.T) {
	// A realistic shallow nested list must still encode and decode faithfully.
	inner := lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(1), lpg.Int64Value(2)})
	v := lpg.ListValue([]lpg.PropertyValue{lpg.StringValue("a"), inner})

	kind, val, err := encodePropertyValue(v)
	if err != nil {
		t.Fatalf("encode shallow list: %v", err)
	}
	got, err := decodePropertyValue(kind, val)
	if err != nil {
		t.Fatalf("decode shallow list: %v", err)
	}
	if got.Kind() != lpg.PropList {
		t.Fatalf("round-trip kind = %v, want PropList", got.Kind())
	}
	elems, _ := got.List()
	if len(elems) != 2 || elems[0].Kind() != lpg.PropString {
		t.Fatalf("round-trip elems = %v, want [string, list]", elems)
	}
}
