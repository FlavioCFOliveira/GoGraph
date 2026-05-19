package lpg

import (
	"bytes"
	"testing"
	"time"

	"gograph/graph/adjlist"
)

func TestPropertyValue_Kinds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    PropertyValue
		kind PropertyKind
	}{
		{"string", StringValue("abc"), PropString},
		{"int64", Int64Value(42), PropInt64},
		{"float64", Float64Value(3.14), PropFloat64},
		{"bool", BoolValue(true), PropBool},
		{"time", TimeValue(time.Unix(0, 0)), PropTime},
		{"bytes", BytesValue([]byte{1, 2}), PropBytes},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.v.Kind() != c.kind {
				t.Fatalf("kind mismatch: got %d want %d", c.v.Kind(), c.kind)
			}
		})
	}
}

func TestPropertyValue_TypedAccess(t *testing.T) {
	t.Parallel()
	if s, ok := StringValue("hello").String(); !ok || s != "hello" {
		t.Fatalf("String accessor: %q %v", s, ok)
	}
	if _, ok := Int64Value(7).String(); ok {
		t.Fatalf("type mismatch should be reported")
	}
	if i, ok := Int64Value(7).Int64(); !ok || i != 7 {
		t.Fatalf("Int64 accessor: %d %v", i, ok)
	}
	if f, ok := Float64Value(2.5).Float64(); !ok || f != 2.5 {
		t.Fatalf("Float64 accessor: %v %v", f, ok)
	}
	if b, ok := BoolValue(true).Bool(); !ok || !b {
		t.Fatalf("Bool accessor")
	}
	now := time.Now()
	if v, ok := TimeValue(now).Time(); !ok || !v.Equal(now) {
		t.Fatalf("Time accessor")
	}
	if b, ok := BytesValue([]byte("payload")).Bytes(); !ok || !bytes.Equal(b, []byte("payload")) {
		t.Fatalf("Bytes accessor")
	}
}

func TestGraph_NodeProperties(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	g.SetNodeProperty("alice", "age", Int64Value(30))
	g.SetNodeProperty("alice", "name", StringValue("Alice"))
	g.SetNodeProperty("alice", "active", BoolValue(true))

	if v, ok := g.GetNodeProperty("alice", "age"); !ok {
		t.Fatalf("missing age")
	} else if i, _ := v.Int64(); i != 30 {
		t.Fatalf("age = %d", i)
	}

	props := g.NodeProperties("alice")
	if len(props) != 3 {
		t.Fatalf("len = %d, want 3", len(props))
	}

	g.DelNodeProperty("alice", "active")
	if _, ok := g.GetNodeProperty("alice", "active"); ok {
		t.Fatalf("active not deleted")
	}
}
