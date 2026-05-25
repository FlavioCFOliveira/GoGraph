package lpg_test

import (
	"math"
	"reflect"
	"testing"
	"time"
	"unicode"

	"pgregory.net/rapid"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// propOp is a single SetNodeProperty operation to be replayed.
type propOp struct {
	key   string
	value lpg.PropertyValue
}

// drawPropOp generates a random key and a random typed PropertyValue.
func drawPropOp(rt *rapid.T, idx int) propOp {
	key := rapid.StringOf(rapid.RuneFrom(nil, unicode.Letter)).
		Filter(func(s string) bool { return len(s) >= 1 && len(s) <= 20 }).
		Draw(rt, "key")

	kind := lpg.PropertyKind(rapid.IntRange(1, 6).Draw(rt, "kind"))

	var value lpg.PropertyValue
	switch kind {
	case lpg.PropString:
		value = lpg.StringValue(rapid.String().Draw(rt, "sv"))
	case lpg.PropInt64:
		value = lpg.Int64Value(rapid.Int64().Draw(rt, "iv"))
	case lpg.PropFloat64:
		f := rapid.Float64().Filter(func(f float64) bool { return !math.IsNaN(f) }).Draw(rt, "fv")
		value = lpg.Float64Value(f)
	case lpg.PropBool:
		value = lpg.BoolValue(rapid.Bool().Draw(rt, "bv"))
	case lpg.PropTime:
		ts := int64(rapid.Uint32().Draw(rt, "ts"))
		value = lpg.TimeValue(time.Unix(ts, 0).UTC())
	case lpg.PropBytes:
		value = lpg.BytesValue(rapid.SliceOf(rapid.Byte()).Draw(rt, "bytesv"))
	}

	_ = idx // suppresses unused-variable warning when called in a loop
	return propOp{key: key, value: value}
}

// TestLPG_PropertyIdempotence verifies that applying the same
// SetNodeProperty call twice leaves the graph in the same state as
// applying it once, for every PropertyKind.
func TestLPG_PropertyIdempotence(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 20).Draw(rt, "n")
		ops := make([]propOp, n)
		for i := range ops {
			ops[i] = drawPropOp(rt, i)
		}

		const nodeKey = "n0"

		// g1: apply each operation exactly once.
		g1 := lpg.New[string, int64](adjlist.Config{Directed: true})
		if err := g1.AddNode(nodeKey); err != nil {
			rt.Fatalf("g1 AddNode: %v", err)
		}
		for _, op := range ops {
			if err := g1.SetNodeProperty(nodeKey, op.key, op.value); err != nil {
				rt.Fatalf("g1 SetNodeProperty(%q): %v", op.key, err)
			}
		}

		// g2: apply each operation twice.
		g2 := lpg.New[string, int64](adjlist.Config{Directed: true})
		if err := g2.AddNode(nodeKey); err != nil {
			rt.Fatalf("g2 AddNode: %v", err)
		}
		for _, op := range ops {
			for range 2 {
				if err := g2.SetNodeProperty(nodeKey, op.key, op.value); err != nil {
					rt.Fatalf("g2 SetNodeProperty(%q): %v", op.key, err)
				}
			}
		}

		// Compare every property present in either graph.
		props1 := g1.NodeProperties(nodeKey)
		props2 := g2.NodeProperties(nodeKey)

		if len(props1) != len(props2) {
			rt.Fatalf("property count mismatch: g1=%d g2=%d", len(props1), len(props2))
		}

		for k, v1 := range props1 {
			v2, ok := props2[k]
			if !ok {
				rt.Fatalf("key %q present in g1 but missing in g2", k)
			}
			if v1.Kind() != v2.Kind() {
				rt.Fatalf("key %q kind mismatch: g1=%d g2=%d", k, v1.Kind(), v2.Kind())
			}
			if !propValEqual(v1, v2) {
				rt.Fatalf("key %q value mismatch: g1=%v g2=%v", k, v1, v2)
			}
		}
	})
}

// propValEqual compares two PropertyValues for equality across all kinds.
// Bytes uses reflect.DeepEqual for slice equality.
func propValEqual(a, b lpg.PropertyValue) bool {
	if a.Kind() != b.Kind() {
		return false
	}
	switch a.Kind() {
	case lpg.PropString:
		sa, _ := a.String()
		sb, _ := b.String()
		return sa == sb
	case lpg.PropInt64:
		ia, _ := a.Int64()
		ib, _ := b.Int64()
		return ia == ib
	case lpg.PropFloat64:
		fa, _ := a.Float64()
		fb, _ := b.Float64()
		return fa == fb
	case lpg.PropBool:
		ba, _ := a.Bool()
		bb, _ := b.Bool()
		return ba == bb
	case lpg.PropTime:
		ta, _ := a.Time()
		tb, _ := b.Time()
		return ta.Equal(tb)
	case lpg.PropBytes:
		ba, _ := a.Bytes()
		bb, _ := b.Bytes()
		return reflect.DeepEqual(ba, bb)
	default:
		return false
	}
}
