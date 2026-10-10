package lpg

// edge_source_slots_test.go — the ReadView slot-index cache (rmp #3089) returns
// exactly what the scanning per-pair surfaces return: at the present instant,
// through a pinned snapshot after further writes, after the indexed entry
// changes under the view, and for many goroutines sharing one view.

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

const slotsTargets = 6

var slotsTypes = []string{"KA", "KB", "KC"}

// slotsGraph returns a graph with two hubs ("h", "g") and targets t0..t5.
func slotsGraph(t *testing.T) *Graph[string, float64] {
	t.Helper()
	g := New[string, float64](adjlist.Config{})
	for _, k := range []string{"h", "g"} {
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < slotsTargets; i++ {
		if err := g.AddNode(fmt.Sprintf("t%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

// slotsMutate applies n random Go-API writes out of src: untyped slots typed by
// naming the pair, typed slots with a per-slot property, per-pair property
// fan-outs, and removals.
func slotsMutate(t *testing.T, rng *rand.Rand, g *Graph[string, float64], src string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		tk := fmt.Sprintf("t%d", rng.Intn(slotsTargets))
		typ := slotsTypes[rng.Intn(len(slotsTypes))]
		var err error
		switch op := rng.Intn(10); {
		case op < 3:
			if err = g.AddEdge(src, tk, 1); err == nil {
				err = g.SetEdgeLabel(src, tk, typ)
			}
		case op < 6:
			err = g.AddEdgeLabeledWithProperty(src, tk, 1, typ, "q", Int64Value(int64(rng.Intn(100))))
		case op < 8:
			err = g.SetEdgeProperty(src, tk, "p", Int64Value(int64(rng.Intn(100))))
		case op < 9:
			err = g.SetEdgeLabel(src, tk, typ)
		default:
			if g.HasEdgeAsOf(src, tk, nil) {
				err = g.RemoveEdge(src, tk)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// pairAnswer renders every per-pair read of (src, dst) the cache serves.
func pairAnswer(labels []string, props map[string]PropertyValue, p, q PropertyValue, pOK, qOK, pHas, qHas bool) string {
	keys := make([]string, 0, len(props))
	for k, v := range props {
		keys = append(keys, fmt.Sprintf("%s=%v", k, v))
	}
	sort.Strings(keys)
	return fmt.Sprintf("L%v P%v p=%v/%t q=%v/%t has=%t,%t",
		labels, keys, p, pOK, q, qOK, pHas, qHas)
}

func viaView(v *ReadView[string, float64], src, dst string) string {
	srcID, _ := v.g.adj.Mapper().Lookup(src)
	dstID, _ := v.g.adj.Mapper().Lookup(dst)
	p, pOK := v.GetEdgeProperty(src, dst, "p")
	q, qOK := v.GetEdgeProperty(src, dst, "q")
	l1 := strings.Join(v.EdgeLabels(src, dst), ",")
	l2 := strings.Join(v.EdgeLabelsByID(srcID, dstID), ",")
	if l1 != l2 {
		return "EdgeLabels/EdgeLabelsByID disagree: " + l1 + " vs " + l2
	}
	return pairAnswer(v.EdgeLabelsByID(srcID, dstID), v.EdgeProperties(src, dst), p, q, pOK, qOK,
		v.EdgeHasProperty(src, dst, "p"), v.EdgeHasProperty(src, dst, "q"))
}

func viaScan(g *Graph[string, float64], snap *Snapshot, src, dst string) string {
	srcID, _ := g.adj.Mapper().Lookup(src)
	dstID, _ := g.adj.Mapper().Lookup(dst)
	p, pOK := g.GetEdgePropertyAsOf(src, dst, "p", snap)
	q, qOK := g.GetEdgePropertyAsOf(src, dst, "q", snap)
	return pairAnswer(g.EdgeLabelsByIDAsOf(srcID, dstID, snap), g.EdgePropertiesAsOf(src, dst, snap), p, q, pOK, qOK,
		g.EdgeHasPropertyAsOf(src, dst, "p", snap), g.EdgeHasPropertyAsOf(src, dst, "q", snap))
}

func slotsOracle(g *Graph[string, float64], snap *Snapshot) map[string]string {
	out := map[string]string{}
	for _, src := range []string{"h", "g"} {
		for i := 0; i < slotsTargets; i++ {
			dst := fmt.Sprintf("t%d", i)
			out[src+">"+dst] = viaScan(g, snap, src, dst)
		}
	}
	return out
}

func checkView(t *testing.T, stage string, v *ReadView[string, float64], want map[string]string) {
	t.Helper()
	for _, src := range []string{"h", "g"} {
		for i := 0; i < slotsTargets; i++ {
			dst := fmt.Sprintf("t%d", i)
			// The first reads of a source scan, a later one builds the index,
			// and the rest read through it.
			for r := 0; r < slotsBuildAfterReads+2; r++ {
				if got := viaView(v, src, dst); got != want[src+">"+dst] {
					t.Fatalf("%s %s>%s read %d:\n got  %s\n want %s", stage, src, dst, r, got, want[src+">"+dst])
				}
			}
		}
	}
}

func TestReadViewSlotCache_EquivalentToScan(t *testing.T) {
	for seed := int64(1); seed <= 6; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			g := slotsGraph(t)
			slotsMutate(t, rng, g, "h", 120)
			slotsMutate(t, rng, g, "g", 120)
			hID, _ := g.adj.Mapper().Lookup("h")
			if deg := len(g.EntryViewAsOf(hID, nil).Neighbours); deg < minSourceSlotsDegree+2 {
				t.Fatalf("hub degree %d below the floor %d: fixture proves nothing", deg, minSourceSlotsDegree)
			}

			v := g.ReadAt(nil)
			checkView(t, "present", v, slotsOracle(g, nil))
			if s := v.slots.Load(); s == nil {
				t.Fatal("no index was built: the cached path was not exercised")
			}

			// Pinned snapshot: write more, then read through the pinned view.
			snap := g.BeginRead()
			want := slotsOracle(g, snap)
			pinned := g.ReadAt(snap)
			slotsMutate(t, rng, g, "h", 40)
			slotsMutate(t, rng, g, "g", 40)
			checkView(t, "pinned", pinned, want)
			g.EndRead(snap)

			// The present view's cached index is now stale: it must fall back.
			checkView(t, "after writes", v, slotsOracle(g, nil))
		})
	}
}

func TestReadViewSlotCache_StaleIndexDeclines(t *testing.T) {
	g := slotsGraph(t)
	slotsMutate(t, rand.New(rand.NewSource(9)), g, "h", 120)
	v := g.ReadAt(nil)
	hID, _ := g.adj.Mapper().Lookup("h")
	s := v.buildSourceSlots(hID)
	if s == nil {
		t.Fatal("hub not indexed")
	}
	if err := g.AddEdge("h", "t0", 1); err != nil {
		t.Fatal(err)
	}
	t0, _ := g.adj.Mapper().Lookup("t0")
	pk, _ := g.pkeys.Lookup("q")
	if _, ok := v.edgeLabelsBySlots(s, t0); ok {
		t.Fatal("stale index answered labels")
	}
	if _, _, ok := v.edgePropertyBySlots(s, t0, pk); ok {
		t.Fatal("stale index answered a property")
	}
	if _, ok := v.edgeHasPropertyBySlots(s, t0, pk); ok {
		t.Fatal("stale index answered presence")
	}
	if v.forEachEdgePropertyBySlots(s, t0, func(string, PropertyValue) {}) {
		t.Fatal("stale index answered properties")
	}
}

// TestReadViewSlotCache_ConcurrentReadersShareOneView runs readers on one view,
// some grouped by source (which builds and reads the index) and some alternating
// sources (which evicts and races it), and checks every answer.
func TestReadViewSlotCache_ConcurrentReadersShareOneView(t *testing.T) {
	g := slotsGraph(t)
	rng := rand.New(rand.NewSource(3))
	slotsMutate(t, rng, g, "h", 150)
	slotsMutate(t, rng, g, "g", 150)
	want := slotsOracle(g, nil)
	v := g.ReadAt(nil)

	const readers, rounds = 8, 60
	var wg sync.WaitGroup
	errs := make(chan string, readers)
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			rr := rand.New(rand.NewSource(int64(r)))
			for i := 0; i < rounds; i++ {
				src := "h"
				if (r%2 == 0 && i%2 == 1) || (r%2 == 1 && (i/8)%2 == 1) {
					src = "g"
				}
				dst := fmt.Sprintf("t%d", rr.Intn(slotsTargets))
				if got := viaView(v, src, dst); got != want[src+">"+dst] {
					errs <- fmt.Sprintf("reader %d %s>%s:\n got  %s\n want %s", r, src, dst, got, want[src+">"+dst])
					return
				}
			}
		}(r)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	if v.slots.Load() == nil {
		t.Fatal("no index was ever built: the shared cached path was not exercised")
	}
}
