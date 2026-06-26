package graphml

// encode_bound_test.go — regression gate for #1792 (sprint 250): the GraphML
// list encoding re-escapes each nested level as a JSON string (~4x/level), so a
// list nested a few dozen deep used to OOM/hang the writer. The encoder now
// fails fast with a typed error, in bounded time.

import (
	"errors"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestSerialisePropertyValue_DeepNestBounded_1792(t *testing.T) {
	v := lpg.StringValue("x")
	for i := 0; i < 300; i++ {
		v = lpg.ListValue([]lpg.PropertyValue{v})
	}

	done := make(chan struct{})
	var out string
	var err error
	go func() {
		out, err = serialisePropertyValue(v)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("serialisePropertyValue did not return within 10s — blowup not bounded")
	}

	if err == nil {
		t.Fatalf("expected a typed error for deeply nested list, got %d bytes,nil", len(out))
	}
	if !errors.Is(err, ErrPropertyNestingTooDeep) && !errors.Is(err, ErrPropertyValueTooLarge) {
		t.Fatalf("expected ErrPropertyNestingTooDeep or ErrPropertyValueTooLarge, got %v", err)
	}
}

func TestSerialisePropertyValue_ShallowListStillRoundTrips_1792(t *testing.T) {
	inner := lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(1), lpg.Int64Value(2)})
	v := lpg.ListValue([]lpg.PropertyValue{lpg.StringValue("a"), inner})

	s, err := serialisePropertyValue(v)
	if err != nil {
		t.Fatalf("serialise shallow list: %v", err)
	}
	got, err := deserialisePropertyValue("list", s)
	if err != nil {
		t.Fatalf("deserialise shallow list: %v", err)
	}
	if got.Kind() != lpg.PropList {
		t.Fatalf("round-trip kind = %v, want PropList", got.Kind())
	}
}
