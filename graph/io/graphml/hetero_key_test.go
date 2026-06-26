package graphml_test

// hetero_key_test.go — regression gate for #1791 (sprint 250): GraphML used to
// declare one attr.type per property-key NAME (first-seen kind wins), so two
// nodes sharing a key with different value kinds produced a file that failed to
// re-import (or silently degraded the type). The writer now emits one <key> per
// (name, kind) so each value round-trips with its own type.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/graphml"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestGraphML_HeterogeneousKeyRoundTrips_1791(t *testing.T) {
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	for _, k := range []string{"a", "b", "c"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode %s: %v", k, err)
		}
	}
	// Same key "v" with three different kinds across three nodes.
	if err := g.SetNodeProperty("a", "v", lpg.Int64Value(42)); err != nil {
		t.Fatalf("set a.v: %v", err)
	}
	if err := g.SetNodeProperty("b", "v", lpg.StringValue("hello")); err != nil {
		t.Fatalf("set b.v: %v", err)
	}
	if err := g.SetNodeProperty("c", "v", lpg.Float64Value(3.14)); err != nil {
		t.Fatalf("set c.v: %v", err)
	}

	var buf bytes.Buffer
	if err := graphml.WriteWithProps(&buf, g); err != nil {
		t.Fatalf("WriteWithProps: %v", err)
	}

	g2, _, err := graphml.ReadWithProps(&buf)
	if err != nil {
		t.Fatalf("ReadWithProps: %v (output:\n%s)", err, buf.String())
	}

	want := map[string]struct {
		kind lpg.PropertyKind
		val  string
	}{
		"a": {lpg.PropInt64, "42"},
		"b": {lpg.PropString, "hello"},
		"c": {lpg.PropFloat64, "3.14"},
	}
	for node, exp := range want {
		pv, ok := g2.NodeProperties(node)["v"]
		if !ok {
			t.Errorf("node %q: property v missing after round-trip", node)
			continue
		}
		if pv.Kind() != exp.kind {
			t.Errorf("node %q: v kind = %v, want %v", node, pv.Kind(), exp.kind)
		}
	}
	// Concrete values, faithfully typed.
	if v, _ := g2.NodeProperties("a")["v"].Int64(); v != 42 {
		t.Errorf("a.v int64 = %d, want 42", v)
	}
	if v, _ := g2.NodeProperties("b")["v"].String(); v != "hello" {
		t.Errorf("b.v string = %q, want hello", v)
	}
	if v, _ := g2.NodeProperties("c")["v"].Float64(); v != 3.14 {
		t.Errorf("c.v float64 = %v, want 3.14", v)
	}
}

func TestGraphML_HomogeneousKeyUnchangedID_1791(t *testing.T) {
	// A name with a single kind keeps the legacy "p_<name>" id (byte-stable
	// output for the common case) — no per-kind suffix.
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	_ = g.AddNode("a")
	_ = g.AddNode("b")
	_ = g.SetNodeProperty("a", "name", lpg.StringValue("x"))
	_ = g.SetNodeProperty("b", "name", lpg.StringValue("y"))

	var buf bytes.Buffer
	if err := graphml.WriteWithProps(&buf, g); err != nil {
		t.Fatalf("WriteWithProps: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `id="p_name"`) {
		t.Errorf("homogeneous key should keep legacy id p_name; output:\n%s", out)
	}
	if strings.Contains(out, `id="p_name_string"`) {
		t.Errorf("homogeneous key must NOT get a per-kind suffix; output:\n%s", out)
	}
}
