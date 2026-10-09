package lpg_test

// token_limit_2748_test.go — regression gate for rmp #2748.
//
// graph/lpg bounded no token, so a graph was durable or not by how it was
// built: an in-memory graph accepted a 70000-byte label that the WAL-backed
// store refused at commit, and a bulk import published one into a snapshot
// that its own WAL could never log. Every mutator that takes a label, a
// relationship type or a property key now refuses a name longer than
// lpg.MaxTokenLen (65535) bytes with lpg.ErrTokenTooLong, BEFORE any state
// change, and accepts a name of exactly MaxTokenLen bytes.

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg/schema"
)

var (
	overLong2748 = strings.Repeat("x", lpg.MaxTokenLen+1)
	atLimit2748  = strings.Repeat("y", lpg.MaxTokenLen)
)

type fixture2748 struct {
	g      *lpg.Graph[string, float64]
	handle uint64
	srcID  graph.NodeID
	dstID  graph.NodeID
}

func newFixture2748(t *testing.T) fixture2748 {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, n := range []string{"a", "b"} {
		if err := g.AddNode(n); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.SetNodeLabel("a", "L"); err != nil {
		t.Fatal(err)
	}
	if err := g.SetNodeProperty("a", "p", lpg.Int64Value(1)); err != nil {
		t.Fatal(err)
	}
	h, err := g.AddEdgeH("a", "b", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.SetEdgeLabel("a", "b", "T"); err != nil {
		t.Fatal(err)
	}
	if err := g.SetEdgeLabelByHandle("a", "b", h, "T"); err != nil {
		t.Fatal(err)
	}
	if err := g.SetEdgeProperty("a", "b", "q", lpg.Int64Value(2)); err != nil {
		t.Fatal(err)
	}
	if err := g.SetEdgePropertyByHandle("a", "b", h, "q", lpg.Int64Value(2)); err != nil {
		t.Fatal(err)
	}
	src, _ := g.AdjList().Mapper().Lookup("a")
	dst, _ := g.AdjList().Mapper().Lookup("b")
	return fixture2748{g: g, handle: h, srcID: src, dstID: dst}
}

// state renders everything a refused call could have changed.
func (f fixture2748) state() string {
	g := f.g
	sorted := func(s []string) []string {
		c := append([]string(nil), s...)
		sort.Strings(c)
		return c
	}
	props := func(m map[string]lpg.PropertyValue) string {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&b, "%s=%v;", k, m[k])
		}
		return b.String()
	}
	return fmt.Sprintf("order=%d mapper=%d topo=%d nl=%v np=%s el=%v ep=%s eh=%v ehp=%s eat=%v",
		g.LiveOrder(), g.AdjList().Mapper().Len(), g.TopoGeneration(),
		sorted(g.NodeLabels("a")), props(g.NodeProperties("a")),
		sorted(g.EdgeLabels("a", "b")), props(g.EdgeProperties("a", "b")),
		sorted(g.EdgeLabelsByHandle("a", "b", f.handle)), props(g.EdgePropertiesByHandle("a", "b", f.handle)),
		sorted(g.EdgeLabelsAt("a", "b", 1)))
}

func (f fixture2748) assertNotInterned(t *testing.T, name string) {
	t.Helper()
	if _, ok := f.g.Registry().Lookup(name); ok {
		t.Errorf("the refused name was interned as a label")
	}
	if _, ok := f.g.PropertyKeys().Lookup(name); ok {
		t.Errorf("the refused name was interned as a property key")
	}
}

type mutator2748 struct {
	name string
	call func(f fixture2748, tok string) error
}

func mutators2748() []mutator2748 {
	v := lpg.Int64Value(7)
	wv := func(f fixture2748) lpg.WriteView[string, float64] { return f.g.Writer(lpg.WriteTx{}) }
	return []mutator2748{
		{"SetNodeLabel", func(f fixture2748, s string) error { return f.g.SetNodeLabel("a", s) }},
		{"SetNodeLabel/new node", func(f fixture2748, s string) error { return f.g.SetNodeLabel("fresh", s) }},
		{"RemoveNodeLabel", func(f fixture2748, s string) error { return f.g.RemoveNodeLabel("a", s) }},
		{"SetNodeProperty", func(f fixture2748, s string) error { return f.g.SetNodeProperty("a", s, v) }},
		{"DelNodeProperty", func(f fixture2748, s string) error { return f.g.DelNodeProperty("a", s) }},
		{"SetEdgeLabel", func(f fixture2748, s string) error { return f.g.SetEdgeLabel("a", "b", s) }},
		{"RemoveEdgeLabel", func(f fixture2748, s string) error { return f.g.RemoveEdgeLabel("a", "b", s) }},
		{"SetEdgeProperty", func(f fixture2748, s string) error { return f.g.SetEdgeProperty("a", "b", s, v) }},
		{"DelEdgeProperty", func(f fixture2748, s string) error { return f.g.DelEdgeProperty("a", "b", s) }},
		{"SetEdgeLabelAt", func(f fixture2748, s string) error { return f.g.SetEdgeLabelAt("a", "b", 1, s) }},
		{"SetEdgePropertyAt", func(f fixture2748, s string) error { return f.g.SetEdgePropertyAt("a", "b", 1, s, v) }},
		{"SetEdgeLabelByHandle", func(f fixture2748, s string) error { return f.g.SetEdgeLabelByHandle("a", "b", f.handle, s) }},
		{"SetEdgePropertyByHandle", func(f fixture2748, s string) error {
			return f.g.SetEdgePropertyByHandle("a", "b", f.handle, s, v)
		}},
		{"DelEdgePropertyByHandle", func(f fixture2748, s string) error { return f.g.DelEdgePropertyByHandle("a", "b", f.handle, s) }},
		{"SetEdgeLabelByHandleID", func(f fixture2748, s string) error {
			return f.g.SetEdgeLabelByHandleID(f.srcID, f.dstID, f.handle, s)
		}},
		{"SetEdgePropertyByHandleID", func(f fixture2748, s string) error {
			return f.g.SetEdgePropertyByHandleID(f.srcID, f.dstID, f.handle, s, v)
		}},
		{"DelEdgePropertyByHandleID", func(f fixture2748, s string) error {
			return f.g.DelEdgePropertyByHandleID(f.srcID, f.dstID, f.handle, s)
		}},
		{"SetEdgeRelTypeAtSlotByID", func(f fixture2748, s string) error {
			_, err := f.g.SetEdgeRelTypeAtSlotByID(f.srcID, f.dstID, 0, s)
			return err
		}},
		{"AddEdgeRelTypeOverflowByID", func(f fixture2748, s string) error {
			_, err := f.g.AddEdgeRelTypeOverflowByID(f.srcID, f.dstID, s)
			return err
		}},
		{"AddEdgeLabeled", func(f fixture2748, s string) error { return f.g.AddEdgeLabeled("b", "a", 1, s) }},
		{"AddEdgeLabeledWithProperty/type", func(f fixture2748, s string) error {
			return f.g.AddEdgeLabeledWithProperty("b", "a", 1, s, "k", v)
		}},
		{"AddEdgeLabeledWithProperty/key", func(f fixture2748, s string) error {
			return f.g.AddEdgeLabeledWithProperty("b", "a", 1, "T", s, v)
		}},
		{"LabelRegistry.Intern", func(f fixture2748, s string) error { _, err := f.g.Registry().Intern(s); return err }},
		{"PropertyKeyRegistry.Intern", func(f fixture2748, s string) error {
			_, err := f.g.PropertyKeys().Intern(s)
			return err
		}},
		{"schema.RegisterLabel", func(f fixture2748, s string) error {
			_, err := schema.New(f.g.Registry(), f.g.PropertyKeys()).RegisterLabel(s)
			return err
		}},
		{"schema.RegisterProperty", func(f fixture2748, s string) error {
			_, err := schema.New(f.g.Registry(), f.g.PropertyKeys()).RegisterProperty(s, lpg.PropInt64)
			return err
		}},
		{"WriteView.SetNodeLabel", func(f fixture2748, s string) error { return wv(f).SetNodeLabel("a", s) }},
		{"WriteView.RemoveNodeLabel", func(f fixture2748, s string) error { return wv(f).RemoveNodeLabel("a", s) }},
		{"WriteView.SetNodeProperty", func(f fixture2748, s string) error { return wv(f).SetNodeProperty("a", s, v) }},
		{"WriteView.DelNodeProperty", func(f fixture2748, s string) error { return wv(f).DelNodeProperty("a", s) }},
		{"WriteView.SetEdgeLabel", func(f fixture2748, s string) error { return wv(f).SetEdgeLabel("a", "b", s) }},
		{"WriteView.RemoveEdgeLabel", func(f fixture2748, s string) error { return wv(f).RemoveEdgeLabel("a", "b", s) }},
		{"WriteView.SetEdgeProperty", func(f fixture2748, s string) error { return wv(f).SetEdgeProperty("a", "b", s, v) }},
		{"WriteView.DelEdgeProperty", func(f fixture2748, s string) error { return wv(f).DelEdgeProperty("a", "b", s) }},
		{"WriteView.SetEdgeLabelAt", func(f fixture2748, s string) error { return wv(f).SetEdgeLabelAt("a", "b", 1, s) }},
		{"WriteView.SetEdgePropertyAt", func(f fixture2748, s string) error {
			return wv(f).SetEdgePropertyAt("a", "b", 1, s, v)
		}},
		{"WriteView.SetEdgeLabelByHandle", func(f fixture2748, s string) error {
			return wv(f).SetEdgeLabelByHandle("a", "b", f.handle, s)
		}},
		{"WriteView.SetEdgePropertyByHandle", func(f fixture2748, s string) error {
			return wv(f).SetEdgePropertyByHandle("a", "b", f.handle, s, v)
		}},
		{"WriteView.DelEdgePropertyByHandle", func(f fixture2748, s string) error {
			return wv(f).DelEdgePropertyByHandle("a", "b", f.handle, s)
		}},
	}
}

// TestTokenLimit_EveryMutatorRefusesBeforeAnyChange_2748 is the per-mutator
// gate: a name one byte over the limit is refused with ErrTokenTooLong, the
// graph is byte-for-byte as it was, and neither registry interned the name.
func TestTokenLimit_EveryMutatorRefusesBeforeAnyChange_2748(t *testing.T) {
	t.Parallel()
	for _, m := range mutators2748() {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture2748(t)
			before := f.state()
			err := m.call(f, overLong2748)
			if !errors.Is(err, lpg.ErrTokenTooLong) {
				t.Fatalf("%s(65536-byte name) = %v, want lpg.ErrTokenTooLong", m.name, err)
			}
			if !strings.Contains(err.Error(), "is 65536 bytes, maximum 65535") {
				t.Errorf("error %q does not name the length and the limit", err)
			}
			if after := f.state(); after != before {
				t.Errorf("%s changed state although it refused\n  before: %s\n  after:  %s", m.name, before, after)
			}
			f.assertNotInterned(t, overLong2748)
			if _, ok := f.g.AdjList().Mapper().Lookup("fresh"); ok {
				t.Errorf("%s created a node although it refused", m.name)
			}
		})
	}
}

// TestTokenLimit_ExactlyAtTheLimitIsAccepted_2748 pins the boundary from the
// other side: a name of exactly MaxTokenLen bytes is accepted by every
// mutator, so the bound is no tighter than the WAL's uint16 prefix.
func TestTokenLimit_ExactlyAtTheLimitIsAccepted_2748(t *testing.T) {
	t.Parallel()
	for _, m := range mutators2748() {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture2748(t)
			if err := m.call(f, atLimit2748); err != nil {
				t.Fatalf("%s(65535-byte name) = %v, want nil", m.name, err)
			}
		})
	}
}

// TestTokenLimit_CheckTokenAndConstant_2748 pins the exported surface a caller
// uses to validate a name before any write.
func TestTokenLimit_CheckTokenAndConstant_2748(t *testing.T) {
	t.Parallel()
	if lpg.MaxTokenLen != 65535 {
		t.Fatalf("MaxTokenLen = %d, want 65535 (the WAL's uint16 length prefix)", lpg.MaxTokenLen)
	}
	if err := lpg.CheckToken("node label", atLimit2748); err != nil {
		t.Errorf("CheckToken(65535 bytes) = %v, want nil", err)
	}
	err := lpg.CheckToken("node label", overLong2748)
	if !errors.Is(err, lpg.ErrTokenTooLong) {
		t.Fatalf("CheckToken(65536 bytes) = %v, want ErrTokenTooLong", err)
	}
	if want := "token too long: node label is 65536 bytes, maximum 65535"; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
	// The limit is a byte length: a 3-byte rune counts three times.
	multi := strings.Repeat("€", lpg.MaxTokenLen/3+1)
	if got, want := errors.Is(lpg.CheckToken("k", multi), lpg.ErrTokenTooLong), len(multi) > lpg.MaxTokenLen; got != want {
		t.Errorf("CheckToken disagrees with len() on a multi-byte name of %d bytes", len(multi))
	}
}
