package lpg

import (
	"testing"
)

// TestNodePropertyStringIDAsOfAgreesWithBagRebuild pins the single-key chain
// walk of [Graph.NodePropertyStringIDAsOf] to the whole-bag rebuild behind
// [Graph.NodePropertyIDAsOf], at a snapshot taken before every kind of change
// (rmp #3057), and checks that reading an older version allocates nothing.
func TestNodePropertyStringIDAsOfAgreesWithBagRebuild(t *testing.T) {
	g, ids := propGraph(t, "a")
	id := ids["a"]
	steps := []func() error{
		func() error { return g.SetNodeProperty("a", "s", StringValue("v1")) }, // absent to string
		func() error { return g.SetNodeProperty("a", "o", Int64Value(1)) },     // another key
		func() error { return g.SetNodeProperty("a", "s", StringValue("v2")) }, // string to string
		func() error { return g.SetNodeProperty("a", "s", Int64Value(5)) },     // string to int
		func() error { return g.DelNodeProperty("a", "s") },                    // to absent
		func() error { return g.SetNodeProperty("a", "s", StringValue("v3")) }, // absent to string
	}
	snaps := make([]*Snapshot, 0, len(steps)+2)
	for _, step := range steps {
		snap := g.BeginRead()
		if snap == nil {
			t.Fatal("versioning is not armed")
		}
		snaps = append(snaps, snap)
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	snaps = append(snaps, g.BeginRead(), nil)
	defer func() {
		for _, s := range snaps {
			if s != nil {
				g.EndRead(s)
			}
		}
	}()
	pid, ok := g.PropertyKeys().Lookup("s")
	if !ok {
		t.Fatal("fixture: s not interned")
	}
	for i, s := range snaps {
		want, wantOK := g.NodePropertyIDAsOf(id, pid, s)
		str, isString, ok := g.NodePropertyStringIDAsOf(id, pid, s)
		if ok != wantOK || isString != (wantOK && want.Kind() == PropString) {
			t.Fatalf("snapshot %d: got (isString %v, ok %v), rebuild gives (%v, %v)", i, isString, ok, want.Kind(), wantOK)
		}
		if ws, _ := want.String(); isString && str != ws {
			t.Fatalf("snapshot %d: got %q, rebuild gives %q", i, str, ws)
		}
	}
	old := snaps[2] // s = "v1" with three later versions on the chain
	if n := testing.AllocsPerRun(100, func() { _, _, _ = g.NodePropertyStringIDAsOf(id, pid, old) }); n != 0 {
		t.Errorf("reading an older version allocated %.0f times, want 0", n)
	}
}
