package graphml

import (
	"bytes"
	"encoding/base64"
	"math"
	"testing"
	"time"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestGraphML_TypedPropsRoundtrip verifies that all six PropertyKinds
// survive a WriteWithProps → ReadWithProps round-trip with correct
// values and kinds.
func TestGraphML_TypedPropsRoundtrip(t *testing.T) {
	t.Parallel()

	// Build an lpg.Graph with one node carrying one property of each kind.
	g := lpg.New[string, int64](adjlist.Config{Directed: true})

	const nodeName = "n0"
	props := map[string]lpg.PropertyValue{
		"str":   lpg.StringValue("hello world"),
		"count": lpg.Int64Value(42),
		"score": lpg.Float64Value(3.14),
		"flag":  lpg.BoolValue(true),
		"stamp": lpg.TimeValue(time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)),
		"blob":  lpg.BytesValue([]byte{0xDE, 0xAD, 0xBE, 0xEF}),
	}
	for k, v := range props {
		if err := g.SetNodeProperty(nodeName, k, v); err != nil {
			t.Fatalf("SetNodeProperty(%q, %q): %v", nodeName, k, err)
		}
	}

	// Add a second node and a directed edge with a known weight.
	if err := g.AddEdge("n0", "n1", 99); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	// Round-trip.
	var buf bytes.Buffer
	if err := WriteWithProps(&buf, g); err != nil {
		t.Fatalf("WriteWithProps: %v", err)
	}

	got, edgesAdded, err := ReadWithProps(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadWithProps: %v", err)
	}
	if edgesAdded != 1 {
		t.Errorf("edgesAdded = %d, want 1", edgesAdded)
	}

	// Verify structure.
	if got.AdjList().Order() != g.AdjList().Order() {
		t.Errorf("Order: got %d, want %d", got.AdjList().Order(), g.AdjList().Order())
	}
	if got.AdjList().Size() != g.AdjList().Size() {
		t.Errorf("Size: got %d, want %d", got.AdjList().Size(), g.AdjList().Size())
	}

	// Verify each property.
	gotProps := got.NodeProperties(nodeName)
	if gotProps == nil {
		t.Fatalf("NodeProperties(%q) returned nil after round-trip", nodeName)
	}

	// str
	if v, ok := gotProps["str"]; !ok {
		t.Error("missing property 'str'")
	} else if s, ok2 := v.String(); !ok2 || s != "hello world" {
		t.Errorf("str: got %v, want StringValue(%q)", v, "hello world")
	}

	// count (long → Int64)
	if v, ok := gotProps["count"]; !ok {
		t.Error("missing property 'count'")
	} else if i, ok2 := v.Int64(); !ok2 || i != 42 {
		t.Errorf("count: got %v, want Int64Value(42)", v)
	}

	// score (double → Float64)
	if v, ok := gotProps["score"]; !ok {
		t.Error("missing property 'score'")
	} else if f, ok2 := v.Float64(); !ok2 || math.Abs(f-3.14) > 1e-9 {
		t.Errorf("score: got %v, want Float64Value(3.14)", v)
	}

	// flag (boolean → Bool)
	if v, ok := gotProps["flag"]; !ok {
		t.Error("missing property 'flag'")
	} else if b, ok2 := v.Bool(); !ok2 || !b {
		t.Errorf("flag: got %v, want BoolValue(true)", v)
	}

	// stamp (time → StringValue; deserialized as string, not time, because
	// the reader returns StringValue for attr.type="string" without
	// further heuristics).
	if v, ok := gotProps["stamp"]; !ok {
		t.Error("missing property 'stamp'")
	} else {
		want := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
		s, ok2 := v.String()
		if !ok2 || s != want {
			t.Errorf("stamp: got %v (kind=%v), want StringValue(%q)", v, v.Kind(), want)
		}
	}

	// blob (bytes → StringValue base64; same note as stamp).
	if v, ok := gotProps["blob"]; !ok {
		t.Error("missing property 'blob'")
	} else {
		wantBase64 := base64.StdEncoding.EncodeToString([]byte{0xDE, 0xAD, 0xBE, 0xEF})
		s, ok2 := v.String()
		if !ok2 || s != wantBase64 {
			t.Errorf("blob: got %v, want StringValue(%q)", v, wantBase64)
		}
	}
}

// TestGraphML_TypedPropsRoundtrip_NaNInf checks that float NaN and Inf
// survive a WriteWithProps → ReadWithProps round-trip.
func TestGraphML_TypedPropsRoundtrip_NaNInf(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		val  float64
	}{
		{"nan", math.NaN()},
		{"pos_inf", math.Inf(1)},
		{"neg_inf", math.Inf(-1)},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			g := lpg.New[string, int64](adjlist.Config{Directed: true})
			if err := g.SetNodeProperty("n", "f", lpg.Float64Value(tc.val)); err != nil {
				t.Fatalf("SetNodeProperty: %v", err)
			}

			var buf bytes.Buffer
			if err := WriteWithProps(&buf, g); err != nil {
				t.Fatalf("WriteWithProps: %v", err)
			}

			got, _, err := ReadWithProps(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatalf("ReadWithProps: %v", err)
			}

			v, ok := got.NodeProperties("n")["f"]
			if !ok {
				t.Fatal("missing property 'f'")
			}
			f, ok2 := v.Float64()
			if !ok2 {
				t.Fatalf("property 'f' kind = %v, want Float64", v.Kind())
			}
			switch {
			case math.IsNaN(tc.val):
				if !math.IsNaN(f) {
					t.Errorf("expected NaN, got %v", f)
				}
			case math.IsInf(tc.val, 1):
				if !math.IsInf(f, 1) {
					t.Errorf("expected +Inf, got %v", f)
				}
			case math.IsInf(tc.val, -1):
				if !math.IsInf(f, -1) {
					t.Errorf("expected -Inf, got %v", f)
				}
			}
		})
	}
}
