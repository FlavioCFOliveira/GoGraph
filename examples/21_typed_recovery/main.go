// Example 21_typed_recovery — demonstrates the canonical typed
// recovery API `recovery.Open[N, W]` against a non-string graph.
//
// The example builds an `(int64, float64)` weighted directed graph
// (numeric node IDs, real-valued edge weights), commits several
// weighted edges plus typed properties through a typed Store, takes
// a v2 snapshot, then drops every in-memory reference and recovers
// the graph from disk via `recovery.Open` instantiated with the
// matching codecs. The recovered graph is then compared against the
// pre-snapshot state to confirm that:
//
//  1. every committed edge survives, with its weight preserved
//     bit-for-bit through `txn.NewFloat64WeightCodec`;
//  2. typed properties attached before the snapshot survive;
//  3. `Result.SnapshotSchemaVersion` reports the v2 manifest version
//     so callers can branch on the on-disk schema without re-opening
//     the manifest themselves.
//
// This is the same flow the production restart path uses; the only
// thing that changes from one (N, W) instantiation to another is the
// codec pair passed to `recovery.Options`.
package main

import (
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/store/recovery"
	"gograph/store/snapshot"
	"gograph/store/txn"
	"gograph/store/wal"
)

//nolint:gocyclo // example walk-through: setup + commits + property writes + snapshot + recovery + per-edge assertions
func main() {
	dir, err := os.MkdirTemp("", "gograph-ex21-")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	walPath := filepath.Join(dir, "wal")

	// === Phase 1: open WAL, build a typed store with int64 + float64 ===
	w, err := wal.Open(walPath)
	if err != nil {
		panic(err)
	}
	g := lpg.New[int64, float64](adjlist.Config{Directed: true})
	opts := txn.Options[int64, float64]{
		Codec:       txn.NewInt64Codec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
	store := txn.NewStoreWithOptions[int64, float64](g, w, opts)

	// Three weighted edges between numeric IDs. The weights are
	// chosen to exercise the IEEE-754 round-trip: an exact integer,
	// a transcendental, and a very small denormal-adjacent value.
	commits := []struct {
		src, dst int64
		weight   float64
		label    string
	}{
		{1001, 1002, 1.0, "PRIMARY"},
		{1002, 1003, 3.141592653589793, "ALTERNATE"},
		{1003, 1004, 1e-300, "DEGRADED"},
	}
	for _, c := range commits {
		tx := store.Begin()
		_ = tx.AddEdge(c.src, c.dst, c.weight)
		_ = tx.SetNodeLabel(c.src, "Hop")
		_ = tx.SetEdgeLabel(c.src, c.dst, c.label)
		if err := tx.Commit(); err != nil {
			panic(err)
		}
	}
	_ = g.AdjList() // touch the adjacency list to materialise the mapper

	// Attach typed properties directly on the in-memory graph; these
	// are flushed exclusively through the snapshot (properties are
	// not WAL-logged today).
	if err := g.SetNodeProperty(int64(1001), "name", lpg.StringValue("origin")); err != nil {
		log.Fatalf("SetNodeProperty: %v", err)
	}
	g.SetEdgeProperty(int64(1001), int64(1002), "latency_ms", lpg.Float64Value(0.5))
	g.SetEdgeProperty(int64(1001), int64(1002), "loss", lpg.Float64Value(0.0001))

	// === Phase 2: persist a v2 snapshot ===
	cs := csr.BuildFromAdjList(g.AdjList())
	snapDir := filepath.Join(dir, "snapshot")
	if err := snapshot.WriteSnapshotFull(snapDir, cs, g); err != nil {
		panic(err)
	}
	_ = w.Close()
	fmt.Printf("Committed %d weighted edges; snapshot persisted at %s.\n",
		len(commits), snapDir)

	// === Phase 3: drop every in-memory reference and recover ===
	_ = store
	_ = g

	res, err := recovery.Open[int64, float64](dir, recovery.Options[int64, float64]{
		Codec:       txn.NewInt64Codec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		fmt.Println("recovery.Open failed:", err)
		return
	}
	fmt.Printf("Recovered: WAL ops=%d, snapshot hit=%v, schema version=v%d, "+
		"label records=%d, property records=%d.\n",
		res.WALOps, res.SnapshotHit, res.SnapshotSchemaVersion,
		res.SnapshotLabels, res.SnapshotProperties)

	// === Phase 4: verify the recovered graph matches what was committed ===
	for _, c := range commits {
		if !res.Graph.AdjList().HasEdge(c.src, c.dst) {
			fmt.Printf("  MISSING edge %d -> %d\n", c.src, c.dst)
			continue
		}
		var gotWeight float64
		for n, wgt := range res.Graph.AdjList().Neighbours(c.src) {
			if n == c.dst {
				gotWeight = wgt
			}
		}
		labelOK := res.Graph.HasEdgeLabel(c.src, c.dst, c.label)
		fmt.Printf("  recovered %d -[%s]-> %d  weight=%v  (label OK: %v, "+
			"weight bit-exact: %v)\n",
			c.src, c.label, c.dst, gotWeight, labelOK,
			bitsEqual(gotWeight, c.weight))
	}

	// Properties.
	if v, ok := res.Graph.GetNodeProperty(int64(1001), "name"); ok {
		s, _ := v.String()
		fmt.Printf("  node 1001.name = %q\n", s)
	} else {
		fmt.Println("  MISSING node 1001.name")
	}
	if v, ok := res.Graph.GetEdgeProperty(int64(1001), int64(1002), "latency_ms"); ok {
		f, _ := v.Float64()
		fmt.Printf("  edge (1001,1002).latency_ms = %v\n", f)
	} else {
		fmt.Println("  MISSING edge (1001,1002).latency_ms")
	}

	// Sanity: the schema version surfaced via the new Result field
	// is the same one the manifest carries on disk.
	if res.SnapshotSchemaVersion != snapshot.ManifestVersion {
		fmt.Printf("WARNING: SnapshotSchemaVersion = %d, expected %d\n",
			res.SnapshotSchemaVersion, snapshot.ManifestVersion)
	}
}

// bitsEqual returns true iff a and b have identical IEEE-754
// representations. Used to confirm the float weight survived the
// snapshot+recovery round-trip without rounding.
func bitsEqual(a, b float64) bool {
	return math.Float64bits(a) == math.Float64bits(b)
}
