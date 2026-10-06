package bulkimport

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestImportInto_ByteReproducible pins rmp #2519: publishing identical records
// twice yields byte-identical snapshot data components once items carry two or
// more properties. Before the fix the importer applied each Properties map in
// iteration order, which fixed the property-key interning order and each
// item's bag layout, so properties.bin differed between two publishes.
//
// manifest.json is excluded: it carries a wall-clock created_at, documented in
// docs/test-layers.md as the one snapshot field that reflects real time.
func TestImportInto_ByteReproducible(t *testing.T) {
	t.Parallel()
	nodes := make([]Node, 0, 64)
	for i := 0; i < 64; i++ {
		nodes = append(nodes, Node{
			Key:    fmt.Sprintf("n%02d", i),
			Labels: []string{"L"},
			Properties: map[string]lpg.PropertyValue{
				"a": lpg.Int64Value(int64(i)), "b": lpg.StringValue("x"),
				"c": lpg.BoolValue(i%2 == 0), "d": lpg.Float64Value(1.5),
				"e": lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(int64(i))}),
			},
		})
	}
	edges := make([]Edge[int64], 0, 63)
	for i := 1; i < 64; i++ {
		edges = append(edges, Edge[int64]{
			Src: nodes[0].Key, Dst: nodes[i].Key, Type: "T", Weight: int64(i),
			Properties: map[string]lpg.PropertyValue{
				"p": lpg.Int64Value(1), "q": lpg.StringValue("y"), "r": lpg.Int64Value(3),
			},
		})
	}
	publish := func() map[string][]byte {
		dir := filepath.Join(t.TempDir(), "store")
		if _, err := ImportInto[int64](context.Background(), dir,
			Options{Directed: true, Multigraph: true}, nodes, edges); err != nil {
			t.Fatalf("ImportInto: %v", err)
		}
		snap := filepath.Join(dir, snapshotName)
		entries, err := os.ReadDir(snap)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		out := make(map[string][]byte, len(entries))
		for _, e := range entries {
			if e.IsDir() || e.Name() == "manifest.json" {
				continue
			}
			//nolint:gosec // G304: snap is this test's own t.TempDir and the name came from ReadDir of it.
			b, err := os.ReadFile(filepath.Join(snap, e.Name()))
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			out[e.Name()] = b
		}
		return out
	}
	first := publish()
	if _, ok := first["properties.bin"]; !ok || len(first) < 5 {
		t.Fatalf("published data components %d (properties.bin present=%t); the comparison would be vacuous",
			len(first), ok)
	}
	for i := 0; i < 4; i++ {
		again := publish()
		if len(again) != len(first) {
			t.Fatalf("publish %d wrote %d data components, first wrote %d", i+2, len(again), len(first))
		}
		for name, b := range first {
			if !bytes.Equal(b, again[name]) {
				t.Fatalf("publish %d: %s differs byte for byte from the first publish of identical records",
					i+2, name)
			}
		}
	}
}
