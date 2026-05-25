package lpg_test

import (
	"math"
	"reflect"
	"testing"
	"time"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/internal/shapegen"
)

// propCase describes a single PropertyKind to exercise in the round-trip.
type propCase struct {
	key   string
	value lpg.PropertyValue
	// verify checks that got matches the original value.
	verify func(t *testing.T, got lpg.PropertyValue)
}

// allPropCases returns the six canonical PropertyKind fixtures.
func allPropCases() []propCase {
	wantStr := "hello ∞"
	wantI64 := int64(-1_234_567_890_123)
	wantF64 := math.Pi
	wantBool := true
	wantTime := time.Date(2026, 1, 1, 0, 0, 0, 1, time.UTC)
	wantBytes := []byte{0x00, 0xFF, 0x42}
	wantNilBytes := []byte(nil)

	return []propCase{
		{
			key:   "p_string",
			value: lpg.StringValue(wantStr),
			verify: func(t *testing.T, got lpg.PropertyValue) {
				t.Helper()
				if got.Kind() != lpg.PropString {
					t.Fatalf("kind: got %v, want PropString", got.Kind())
				}
				v, ok := got.String()
				if !ok {
					t.Fatal("String() ok=false")
				}
				if v != wantStr {
					t.Fatalf("string value: got %q, want %q", v, wantStr)
				}
			},
		},
		{
			key:   "p_int64",
			value: lpg.Int64Value(wantI64),
			verify: func(t *testing.T, got lpg.PropertyValue) {
				t.Helper()
				if got.Kind() != lpg.PropInt64 {
					t.Fatalf("kind: got %v, want PropInt64", got.Kind())
				}
				v, ok := got.Int64()
				if !ok {
					t.Fatal("Int64() ok=false")
				}
				if v != wantI64 {
					t.Fatalf("int64 value: got %d, want %d", v, wantI64)
				}
			},
		},
		{
			key:   "p_float64",
			value: lpg.Float64Value(wantF64),
			verify: func(t *testing.T, got lpg.PropertyValue) {
				t.Helper()
				if got.Kind() != lpg.PropFloat64 {
					t.Fatalf("kind: got %v, want PropFloat64", got.Kind())
				}
				v, ok := got.Float64()
				if !ok {
					t.Fatal("Float64() ok=false")
				}
				if v != wantF64 {
					t.Fatalf("float64 value: got %v, want %v", v, wantF64)
				}
			},
		},
		{
			key:   "p_bool",
			value: lpg.BoolValue(wantBool),
			verify: func(t *testing.T, got lpg.PropertyValue) {
				t.Helper()
				if got.Kind() != lpg.PropBool {
					t.Fatalf("kind: got %v, want PropBool", got.Kind())
				}
				v, ok := got.Bool()
				if !ok {
					t.Fatal("Bool() ok=false")
				}
				if v != wantBool {
					t.Fatalf("bool value: got %v, want %v", v, wantBool)
				}
			},
		},
		{
			key:   "p_time",
			value: lpg.TimeValue(wantTime),
			verify: func(t *testing.T, got lpg.PropertyValue) {
				t.Helper()
				if got.Kind() != lpg.PropTime {
					t.Fatalf("kind: got %v, want PropTime", got.Kind())
				}
				v, ok := got.Time()
				if !ok {
					t.Fatal("Time() ok=false")
				}
				if !v.Equal(wantTime) {
					t.Fatalf("time value: got %v, want %v", v, wantTime)
				}
			},
		},
		{
			key:   "p_bytes",
			value: lpg.BytesValue(wantBytes),
			verify: func(t *testing.T, got lpg.PropertyValue) {
				t.Helper()
				if got.Kind() != lpg.PropBytes {
					t.Fatalf("kind: got %v, want PropBytes", got.Kind())
				}
				v, ok := got.Bytes()
				if !ok {
					t.Fatal("Bytes() ok=false")
				}
				if !reflect.DeepEqual(v, wantBytes) {
					t.Fatalf("bytes value: got %v, want %v", v, wantBytes)
				}
			},
		},
		{
			key:   "p_bytes_nil",
			value: lpg.BytesValue(wantNilBytes),
			verify: func(t *testing.T, got lpg.PropertyValue) {
				t.Helper()
				if got.Kind() != lpg.PropBytes {
					t.Fatalf("kind: got %v, want PropBytes", got.Kind())
				}
				v, ok := got.Bytes()
				if !ok {
					t.Fatal("Bytes() ok=false for nil bytes")
				}
				if !reflect.DeepEqual(v, wantNilBytes) {
					t.Fatalf("nil bytes value: got %v, want nil", v)
				}
			},
		},
	}
}

// TestLPG_PropertyKind_K1 exercises all six PropertyKinds on a K1
// (single node, no edges). Validates node properties only.
func TestLPG_PropertyKind_K1(t *testing.T) {
	t.Parallel()
	cases := allPropCases()

	g, err := shapegen.SingleNode().Build(adjlist.Config{})
	if err != nil {
		t.Fatalf("SingleNode.Build: %v", err)
	}

	// Collect all nodes.
	var nodes []int
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, n int) bool {
		nodes = append(nodes, n)
		return true
	})
	if len(nodes) == 0 {
		t.Fatal("K1 must have at least one node")
	}

	for _, node := range nodes {
		node := node
		for _, tc := range cases {
			tc := tc
			t.Run("node/"+tc.key, func(t *testing.T) {
				t.Parallel()
				if err := g.SetNodeProperty(node, tc.key, tc.value); err != nil {
					t.Fatalf("SetNodeProperty(%v, %q): %v", node, tc.key, err)
				}
				got, ok := g.GetNodeProperty(node, tc.key)
				if !ok {
					t.Fatalf("GetNodeProperty(%v, %q): ok=false", node, tc.key)
				}
				tc.verify(t, got)
			})
		}
	}
}

// TestLPG_PropertyKind_Pn exercises all six PropertyKinds on a P10
// path graph (10 nodes, 9 directed edges). Validates both node and
// edge properties across a connected topology.
func TestLPG_PropertyKind_Pn(t *testing.T) {
	t.Parallel()
	cases := allPropCases()

	g, err := shapegen.Path(10, true).Build(adjlist.Config{})
	if err != nil {
		t.Fatalf("Path(10,true).Build: %v", err)
	}

	// Collect all nodes via mapper walk.
	var nodes []int
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, n int) bool {
		nodes = append(nodes, n)
		return true
	})
	if len(nodes) == 0 {
		t.Fatal("Pn must have at least one node")
	}

	// Node property round-trips.
	for _, node := range nodes {
		node := node
		for _, tc := range cases {
			tc := tc
			t.Run("node/"+tc.key, func(t *testing.T) {
				t.Parallel()
				if err := g.SetNodeProperty(node, tc.key, tc.value); err != nil {
					t.Fatalf("SetNodeProperty(%v, %q): %v", node, tc.key, err)
				}
				got, ok := g.GetNodeProperty(node, tc.key)
				if !ok {
					t.Fatalf("GetNodeProperty(%v, %q): ok=false", node, tc.key)
				}
				tc.verify(t, got)
			})
		}
	}

	// Edge property round-trips — iterate each node's out-neighbours.
	for _, src := range nodes {
		src := src
		for dst := range g.AdjList().Neighbours(src) {
			dst := dst
			for _, tc := range cases {
				tc := tc
				t.Run("edge/"+tc.key, func(t *testing.T) {
					t.Parallel()
					if err := g.SetEdgeProperty(src, dst, tc.key, tc.value); err != nil {
						t.Fatalf("SetEdgeProperty: %v", err)
					}
					got, ok := g.GetEdgeProperty(src, dst, tc.key)
					if !ok {
						t.Fatalf("GetEdgeProperty(%v->%v, %q): ok=false", src, dst, tc.key)
					}
					tc.verify(t, got)
				})
			}
		}
	}
}
