package lpg

// present_read_committed_test.go — rmp #2965 round 5, finding R5-F3: every
// direct present-state accessor of Graph and AdjList returns the newest
// COMMITTED state, so a durable commit applied but not yet fsynced is invisible
// to it, and stays invisible when the fsync fails and the commit is withdrawn.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// presentReadings renders the answer of every direct present-state accessor
// about the fixture's nodes n, b and z.
func presentReadings(g *Graph[string, float64]) string {
	var sb strings.Builder
	put := func(name string, v any) { fmt.Fprintf(&sb, "%s=%v\n", name, v) }
	id := func(k string) graph.NodeID { x, _ := g.AdjList().Mapper().Lookup(k); return x }
	sorted := func(xs []string) []string { ys := slices.Clone(xs); sort.Strings(ys); return ys }
	mapStr := func(m map[string]PropertyValue) string {
		ks := make([]string, 0, len(m))
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		out := ""
		for _, k := range ks {
			out += fmt.Sprintf("%s:%v;", k, m[k])
		}
		return out
	}
	n, b := id("n"), id("b")
	const h = uint64(1 << 40)

	// Node properties.
	v, ok := g.GetNodeProperty("n", "v")
	put("GetNodeProperty", fmt.Sprint(v, ok))
	put("NodeProperties", mapStr(g.NodeProperties("n")))
	put("NodePropertiesByID", mapStr(g.NodePropertiesByID(n)))
	v, ok = g.NodePropertyByID(n, "old")
	put("NodePropertyByID", fmt.Sprint(v, ok))
	var fn []string
	g.NodePropertiesByIDFunc(n, func(k string, pv PropertyValue) { fn = append(fn, fmt.Sprint(k, pv)) })
	put("NodePropertiesByIDFunc", sorted(fn))
	// Node labels.
	put("HasNodeLabel", g.HasNodeLabel("n", "M"))
	put("HasNodeLabelByID", g.HasNodeLabelByID(n, "K"))
	put("NodeLabels", sorted(g.NodeLabels("n")))
	put("NodeLabelsByID", sorted(g.NodeLabelsByID(n)))
	var fl []string
	g.ForEachNodeLabelByID(n, func(s string) { fl = append(fl, s) })
	put("ForEachNodeLabelByID", sorted(fl))
	// Edge labels.
	put("EdgeLabels", sorted(g.EdgeLabels("n", "b")))
	put("EdgeLabelsByID", sorted(g.EdgeLabelsByID(n, b)))
	var fe []string
	g.ForEachEdgeLabelByID(n, b, func(s string) { fe = append(fe, s) })
	put("ForEachEdgeLabelByID", sorted(fe))
	put("HasEdgeLabel", g.HasEdgeLabel("n", "b", "R"))
	put("EdgeLabelsAt", sorted(g.EdgeLabelsAt("n", "b", 0)))
	put("EdgeLabelsByHandle", sorted(g.EdgeLabelsByHandle("n", "b", h)))
	put("EdgeLabelsByHandleID", sorted(g.EdgeLabelsByHandleID(n, b, h)))
	put("HasEdgeHandleLabelRecordByID", g.HasEdgeHandleLabelRecordByID(n, b, h))
	var ps []string
	g.ForEachPairSlotRelTypeByID(n, b, func(o int, s string) { ps = append(ps, fmt.Sprint(o, s)) })
	put("ForEachPairSlotRelTypeByID", ps)
	var po []string
	g.ForEachPairOverflowRelTypeByID(n, b, func(s string) { po = append(po, s) })
	put("ForEachPairOverflowRelTypeByID", sorted(po))
	// Edge properties.
	put("EdgeProperties", mapStr(g.EdgeProperties("n", "b")))
	put("EdgePropertiesByID", mapStr(g.EdgePropertiesByID(n, b)))
	v, ok = g.GetEdgeProperty("n", "b", "pp")
	put("GetEdgeProperty", fmt.Sprint(v, ok))
	put("EdgeHasProperty", g.EdgeHasProperty("n", "b", "pp"))
	var fp []string
	g.ForEachEdgeProperty("n", "b", func(k string, pv PropertyValue) { fp = append(fp, fmt.Sprint(k, pv)) })
	put("ForEachEdgeProperty", sorted(fp))
	var fpi []string
	g.ForEachEdgePropertyByID(n, b, func(k string, pv PropertyValue) { fpi = append(fpi, fmt.Sprint(k, pv)) })
	put("ForEachEdgePropertyByID", sorted(fpi))
	put("EdgePropertiesAt", mapStr(g.EdgePropertiesAt("n", "b", 0)))
	put("EdgePropertiesByHandle", mapStr(g.EdgePropertiesByHandle("n", "b", h)))
	put("EdgePropertiesByHandleID", mapStr(g.EdgePropertiesByHandleID(n, b, h)))
	v, ok = g.EdgePropertyByHandle("n", "b", h, "ep")
	put("EdgePropertyByHandle", fmt.Sprint(v, ok))
	// Topology through the graph.
	w, ok := g.EdgeWeight("n", "b")
	put("EdgeWeight", fmt.Sprint(w, ok))
	fh, ok := g.FirstEdgeHandle("n", "b")
	put("FirstEdgeHandle", fmt.Sprint(fh, ok))
	put("HasEdgeHandle", g.HasEdgeHandle("n", "b", h))
	put("AppendEdgeHandles", g.AppendEdgeHandles("n", "b", nil))
	var walked int
	g.WalkEdgeHandles(func(EdgeHandleTriple) bool { walked++; return true })
	put("WalkEdgeHandles", walked)
	d, ok := g.OutDegree("n")
	put("OutDegree", fmt.Sprint(d, ok))
	d, ok = g.OutDegreeByID(n)
	put("OutDegreeByID", fmt.Sprint(d, ok))
	d, ok = g.OutDegreeBoundedByID(n, 10)
	put("OutDegreeBoundedByID", fmt.Sprint(d, ok))
	lid, _ := g.Registry().Lookup("T")
	d, ok = g.OutDegreeByType("n", lid)
	put("OutDegreeByType", fmt.Sprint(d, ok))
	d, ok = g.OutDegreeByTypeBounded("n", lid, 10)
	put("OutDegreeByTypeBounded", fmt.Sprint(d, ok))
	d, ok = g.OutDegreeByTypeBoundedByID(n, lid, 10)
	put("OutDegreeByTypeBoundedByID", fmt.Sprint(d, ok))
	d, ok = g.OutDegreeMatchingBoundedByID(n, lid, false, 10, nil)
	put("OutDegreeMatchingBoundedByID", fmt.Sprint(d, ok))
	// Topology through the adjacency.
	a := g.AdjList()
	put("AdjList.HasEdge", a.HasEdge("n", "b"))
	var nb []string
	for k := range a.Neighbours("n") {
		nb = append(nb, k)
	}
	put("AdjList.Neighbours", sorted(nb))
	d, ok = a.OutDegree("n")
	put("AdjList.OutDegree", fmt.Sprint(d, ok))
	d, ok = a.OutDegreeByID(n)
	put("AdjList.OutDegreeByID", fmt.Sprint(d, ok))
	d, ok = a.OutDegreeFunc("n", func(graph.NodeID, uint32) bool { return true })
	put("AdjList.OutDegreeFunc", fmt.Sprint(d, ok))
	d, ok = a.OutDegreeFuncBounded("n", 10, func(graph.NodeID, uint32) bool { return true })
	put("AdjList.OutDegreeFuncBounded", fmt.Sprint(d, ok))
	put("AdjList.InNeighbourIDs", len(a.InNeighbourIDs(b)))
	put("AdjList.InNeighbours", sorted(a.InNeighbours("b")))
	// Node existence (round 6): the in-flight commit removes rm.
	rm := id("rm")
	put("IsTombstoned", g.IsTombstoned(rm))
	put("TombstonedIDs", len(g.TombstonedIDs()))
	put("TombstoneCount", g.TombstoneCount())
	put("LiveOrder", g.LiveOrder())
	live := g.LiveNodeFilter()
	put("LiveNodeFilter", live == nil || live(rm))
	return sb.String()
}

// presentReadFixture builds the committed state the in-flight commit changes.
func presentReadFixture(t *testing.T) *Graph[string, float64] {
	t.Helper()
	g := newDirectTxGraph(t, true)
	for _, k := range []string{"n", "b", "rm"} {
		requireNoErr(t, g.AddNode(k))
	}
	requireNoErr(t,
		g.SetNodeProperty("n", "v", StringValue("old")),
		g.SetNodeProperty("n", "old", Int64Value(1)),
		g.SetNodeLabel("n", "K"),
	)
	return g
}

// inFlightWrites is one durable commit touching every store the accessors read.
func inFlightWrites(g *Graph[string, float64]) func(WriteTx) error {
	return func(wtx WriteTx) error {
		w := g.Writer(wtx)
		const h = uint64(1 << 40)
		if err := w.SetNodeProperty("n", "v", StringValue("NEW")); err != nil {
			return err
		}
		if err := w.DelNodeProperty("n", "old"); err != nil {
			return err
		}
		if err := w.SetNodeLabel("n", "M"); err != nil {
			return err
		}
		if err := w.RemoveNodeLabel("n", "K"); err != nil {
			return err
		}
		if _, err := w.AddEdgeHIfAbsent("n", "b", 3, h); err != nil {
			return err
		}
		if err := w.SetEdgeLabelByHandle("n", "b", h, "T"); err != nil {
			return err
		}
		if err := w.SetEdgePropertyByHandle("n", "b", h, "ep", Int64Value(9)); err != nil {
			return err
		}
		if err := w.SetEdgeLabel("n", "b", "R"); err != nil {
			return err
		}
		if err := w.SetEdgeProperty("n", "b", "pp", Int64Value(8)); err != nil {
			return err
		}
		if ok, _ := w.RemoveNode("rm"); !ok {
			return errors.New("the removal of rm was refused")
		}
		return nil
	}
}

func TestPresentReads_NeverSeeAnUnpublishedCommit(t *testing.T) {
	boom := errors.New("injected fsync failure")
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fsyncFails=%v", fail), func(t *testing.T) {
			g := presentReadFixture(t)
			before := presentReadings(g)
			var during string
			err := g.ApplyDurable(context.Background(), inFlightWrites(g), func() error {
				done := make(chan struct{})
				go func() { defer close(done); during = presentReadings(g) }()
				<-done
				if fail {
					return boom
				}
				return nil
			})
			if fail != (err != nil) {
				t.Fatalf("ApplyDurable = %v, fsyncFails=%v", err, fail)
			}
			after := presentReadings(g)
			if d := diffReadings(before, during); d != "" {
				t.Errorf("present reads during the fsync saw the unpublished commit:\n%s", d)
			}
			if fail {
				if d := diffReadings(before, after); d != "" {
					t.Errorf("present reads after the failed fsync differ from the committed state:\n%s", d)
				}
				return
			}
			// The control: once published, the same readings DO change, so the
			// comparison above can fail.
			if after == before {
				t.Fatal("the published commit changed no reading: the fixture proves nothing")
			}
		})
	}
}

// diffReadings lists the accessors whose readings differ.
func diffReadings(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	out := ""
	for i := range w {
		if i >= len(g) || w[i] != g[i] {
			gi := ""
			if i < len(g) {
				gi = g[i]
			}
			out += fmt.Sprintf("  want %s\n   got %s\n", w[i], gi)
		}
	}
	return out
}

// TestPresentReads_ReadYourOwnWritesThroughTheTransaction keeps the other half:
// the transaction itself reads its own uncommitted writes through its view.
func TestPresentReads_ReadYourOwnWritesThroughTheTransaction(t *testing.T) {
	g := presentReadFixture(t)
	err := g.ApplyDurable(context.Background(), inFlightWrites(g), func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	err = g.ApplyVersioned(func(wtx WriteTx) error {
		w := g.Writer(wtx)
		if e := w.SetNodeProperty("n", "v", StringValue("MINE")); e != nil {
			return e
		}
		if v, _ := g.WriterViewOf(wtx).GetNodeProperty("n", "v"); v != StringValue("MINE") {
			t.Errorf("the transaction's own view reads %v, want its own write", v)
		}
		if v, _ := g.GetNodeProperty("n", "v"); v != StringValue("NEW") {
			t.Errorf("a present read inside the bracket reads %v, want the committed NEW", v)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// BenchmarkPresentRead_CommittedHead measures the direct accessors' fast path
// — the newest version committed — which must allocate nothing.
func BenchmarkPresentRead_CommittedHead(b *testing.B) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	defer func() { _ = g.Close() }()
	_ = g.AddNode("n")
	_ = g.AddNode("b")
	_ = g.SetNodeProperty("n", "v", Int64Value(1))
	_ = g.SetNodeLabel("n", "L")
	_ = g.AddEdge("n", "b", 1)
	_ = g.SetEdgeLabel("n", "b", "T")
	lid, _ := g.Registry().Lookup("T")
	b.Run("GetNodeProperty", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = g.GetNodeProperty("n", "v")
		}
	})
	b.Run("HasNodeLabel", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = g.HasNodeLabel("n", "L")
		}
	})
	b.Run("AdjList.HasEdge", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = g.AdjList().HasEdge("n", "b")
		}
	})
	b.Run("IsTombstoned", func(b *testing.B) {
		id, _ := g.AdjList().Mapper().Lookup("n")
		b.ReportAllocs()
		for b.Loop() {
			_ = g.IsTombstoned(id)
		}
	})
	b.Run("OutDegreeByType", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = g.OutDegreeByType("n", lid)
		}
	})
	b.Run("OutDegreeByID", func(b *testing.B) {
		id, _ := g.AdjList().Mapper().Lookup("n")
		b.ReportAllocs()
		for b.Loop() {
			_, _ = g.OutDegreeByID(id)
		}
	})
	b.Run("OutDegree", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = g.OutDegree("n")
		}
	})
}
