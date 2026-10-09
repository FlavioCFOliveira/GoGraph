package cypher

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestSnapshotSeekResidual_StringKeys pins the snapshot fallback of the equality
// and key-set seeks (rmp #3057): for string keys it returns exactly what the
// generic per-node comparison returns, and its allocations do not grow with the
// label's population.
func TestSnapshotSeekResidual_StringKeys(t *testing.T) {
	const population = 256
	g := lpg.New[string, float64](adjlist.Config{})
	for i := 0; i < population; i++ {
		id := fmt.Sprintf("n%d", i)
		if err := g.AddNode(id); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(id, "L"); err != nil {
			t.Fatal(err)
		}
		var v lpg.PropertyValue
		switch i % 4 {
		case 0, 1:
			v = lpg.StringValue(fmt.Sprintf("v%d", i))
		case 2:
			v = lpg.Int64Value(int64(i))
		case 3:
			v = lpg.DateValue(time.Date(2026, 1, 1+i%28, 0, 0, 0, 0, time.UTC))
		}
		if err := g.SetNodeProperty(id, "s", v); err != nil {
			t.Fatal(err)
		}
	}
	dateRaw, ok := g.ReadAt(nil).GetNodeProperty("n3", "s")
	if !ok {
		t.Fatal("fixture: node 3 has no s")
	}
	rawDate, _ := dateRaw.String()

	r := &snapshotSeekResidual{view: g.ReadAt(nil), label: "L", key: "s"}
	// reference is the per-node comparison the residual used before rmp #3057.
	reference := func(keys []expr.Value) []uint64 {
		var out []uint64
		it := (&lpgLabelResolver{g: r.view}).ResolveLabelBitmap("L").Iterator()
		for it.HasNext() {
			id := it.Next()
			pv, ok := r.view.NodePropertyByID(graph.NodeID(id), "s")
			if !ok {
				continue
			}
			for _, k := range keys {
				if snapshotSeekKeyEquals(pv, k) {
					out = append(out, id)
					break
				}
			}
		}
		return out
	}
	for _, keys := range [][]expr.Value{
		{expr.StringValue("v5")},
		{expr.StringValue("v5"), expr.StringValue("v8"), expr.StringValue("absent")},
		{expr.StringValue(rawDate)},
		{expr.StringValue("")},
		{expr.StringValue("v5"), expr.IntegerValue(6)},
	} {
		got := r.AppendMatching(keys, nil)
		if want := reference(keys); !slices.Equal(got, want) {
			t.Errorf("keys %v: AppendMatching = %v, reference = %v", keys, got, want)
		}
	}
	if got := r.AppendMatching([]expr.Value{expr.StringValue("v5")}, nil); len(got) != 1 {
		t.Fatalf("fixture: want exactly one match for v5, got %v", got)
	}

	buf := make([]uint64, 0, 8)
	key := []expr.Value{expr.StringValue("v5")}
	allocs := testing.AllocsPerRun(20, func() { _ = r.AppendMatching(key, buf[:0]) })
	if allocs >= population/8 {
		t.Errorf("AppendMatching over %d nodes allocated %.0f times, want a constant well below the population", population, allocs)
	}
}
