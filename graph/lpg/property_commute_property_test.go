// Package lpg_test contains property-based tests for the lpg package.
package lpg_test

import (
	"testing"
	"time"

	"pgregory.net/rapid"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// commuteKey is a canonical property key with a fixed PropertyKind so
// the commutation test can generate values for each key without
// needing to pick kinds at random.
type commuteKey struct {
	name string
	kind lpg.PropertyKind
}

// commuteKeys is the fixed alphabet of (key, kind) pairs for the
// commutativity test.  The bijection between name and kind is stable:
// all generators that see a key must produce a value of the
// corresponding kind.
var commuteKeys = []commuteKey{
	{"age", lpg.PropInt64},
	{"name", lpg.PropString},
	{"score", lpg.PropFloat64},
	{"active", lpg.PropBool},
	{"ts", lpg.PropTime},
	{"data", lpg.PropBytes},
}

// drawValueForKey generates a random PropertyValue of the kind
// dictated by ck.
func drawValueForKey(rt *rapid.T, ck commuteKey) lpg.PropertyValue {
	switch ck.kind {
	case lpg.PropInt64:
		return lpg.Int64Value(rapid.Int64().Draw(rt, ck.name+"_v"))
	case lpg.PropString:
		return lpg.StringValue(rapid.String().Draw(rt, ck.name+"_v"))
	case lpg.PropFloat64:
		// Exclude NaN: NaN != NaN would always fail the equality check.
		f := rapid.Float64().
			Filter(func(x float64) bool { return x == x }).
			Draw(rt, ck.name+"_v")
		return lpg.Float64Value(f)
	case lpg.PropBool:
		return lpg.BoolValue(rapid.Bool().Draw(rt, ck.name+"_v"))
	case lpg.PropTime:
		ts := int64(rapid.Uint32().Draw(rt, ck.name+"_v"))
		return lpg.TimeValue(time.Unix(ts, 0).UTC())
	case lpg.PropBytes:
		return lpg.BytesValue(rapid.SliceOf(rapid.Byte()).Draw(rt, ck.name+"_v"))
	default:
		rt.Fatalf("drawValueForKey: unhandled kind %d for key %q", ck.kind, ck.name)
		return lpg.PropertyValue{} // unreachable
	}
}

// TestLPG_PropertyCommute verifies that setting two properties with
// distinct keys commutes: applying (k1=v1, k2=v2) yields the same
// NodeProperties as applying (k2=v2, k1=v1).
//
// This holds because each SetNodeProperty call is a map-put keyed by
// (NodeID, PropertyKeyID); distinct keys never overwrite each other.
func TestLPG_PropertyCommute(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		// Draw two distinct indices into commuteKeys.
		i1 := rapid.IntRange(0, len(commuteKeys)-1).Draw(rt, "i1")
		i2 := rapid.IntRange(0, len(commuteKeys)-1).
			Filter(func(i int) bool { return i != i1 }).
			Draw(rt, "i2")

		ck1 := commuteKeys[i1]
		ck2 := commuteKeys[i2]

		v1 := drawValueForKey(rt, ck1)
		v2 := drawValueForKey(rt, ck2)

		const nodeKey = 0

		// g1: order A — set k1 then k2.
		g1 := lpg.New[int, int64](adjlist.Config{Directed: true})
		if err := g1.AddNode(nodeKey); err != nil {
			rt.Fatalf("g1 AddNode: %v", err)
		}
		if err := g1.SetNodeProperty(nodeKey, ck1.name, v1); err != nil {
			rt.Fatalf("g1 SetNodeProperty(%q): %v", ck1.name, err)
		}
		if err := g1.SetNodeProperty(nodeKey, ck2.name, v2); err != nil {
			rt.Fatalf("g1 SetNodeProperty(%q): %v", ck2.name, err)
		}

		// g2: order B — set k2 then k1.
		g2 := lpg.New[int, int64](adjlist.Config{Directed: true})
		if err := g2.AddNode(nodeKey); err != nil {
			rt.Fatalf("g2 AddNode: %v", err)
		}
		if err := g2.SetNodeProperty(nodeKey, ck2.name, v2); err != nil {
			rt.Fatalf("g2 SetNodeProperty(%q): %v", ck2.name, err)
		}
		if err := g2.SetNodeProperty(nodeKey, ck1.name, v1); err != nil {
			rt.Fatalf("g2 SetNodeProperty(%q): %v", ck1.name, err)
		}

		props1 := g1.NodeProperties(nodeKey)
		props2 := g2.NodeProperties(nodeKey)

		if len(props1) != len(props2) {
			rt.Fatalf("property count mismatch: order-A=%d order-B=%d", len(props1), len(props2))
		}

		for k, a := range props1 {
			b, ok := props2[k]
			if !ok {
				rt.Fatalf("key %q present in order-A but missing in order-B", k)
			}
			if !propValEqual(a, b) {
				rt.Fatalf("key %q value differs between orderings: A=%v B=%v", k, a, b)
			}
		}
	})
}
