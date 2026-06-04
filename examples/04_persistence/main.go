// Example 04_persistence — opens a WAL, performs a few transactions
// that include both node and edge labels, attaches typed properties
// directly on the in-memory graph, then takes a v2 snapshot (CSR +
// labels.bin + properties.bin) and demonstrates that labels and
// typed properties survive a restart. The flow mirrors what a
// production durability path looks like:
//
//  1. Transactions append framed ops to the WAL and apply them to
//     the in-memory LPG.
//  2. Typed properties (currently not WAL-logged) are set directly
//     on the graph.
//  3. snapshot.WriteSnapshotFull persists the CSR view, labels.bin,
//     and properties.bin atomically alongside the WAL.
//  4. The process "restarts" — every in-memory reference is dropped
//     and recovery.Open rebuilds the graph from disk. The WAL
//     replay re-populates the mapper; labels.bin re-attaches the
//     snapshot-time label set; properties.bin re-attaches the
//     snapshot-time typed property set.
//
// Sample output: run `go run ./examples/04_persistence` and capture the
// stdout — the output is deterministic for the inputs hard-coded
// above and serves as the regression baseline a future change should
// preserve. The example persists to a directory created with
// os.MkdirTemp; that path is intentionally kept out of stdout so the
// output stays stable across runs.
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run drives the full persistence walk-through — WAL transactions,
// typed property writes, a v2 snapshot, and recovery from disk — and
// writes the report to w. All output goes to w so a test can capture
// and assert it; run returns wrapped errors rather than terminating
// the process. The temp directory it persists to is never written to
// w, so the report stays deterministic.
//
//nolint:gocyclo // example walk-through: setup + commits + property writes + snapshot + recovery + per-kind assertions
func run(w io.Writer) error {
	dir, err := os.MkdirTemp("", "gograph-ex04-")
	if err != nil {
		return fmt.Errorf("MkdirTemp: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	walPath := filepath.Join(dir, "wal")

	wl, err := wal.Open(walPath)
	if err != nil {
		return fmt.Errorf("wal.Open: %w", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithCodec(g, wl, txn.NewStringCodec())

	commits := []struct{ src, dst, nodeLabel, edgeLabel string }{
		{"alice", "bob", "Person", "KNOWS"},
		{"bob", "carol", "Person", "KNOWS"},
		{"carol", "dave", "Person", "FOLLOWS"},
	}
	for _, c := range commits {
		tx := store.Begin()
		_ = tx.SetNodeLabel(c.src, c.nodeLabel)
		_ = tx.SetNodeLabel(c.dst, c.nodeLabel)
		_ = tx.AddEdge(c.src, c.dst, 0)
		_ = tx.SetEdgeLabel(c.src, c.dst, c.edgeLabel)
		_ = tx.Commit()
	}
	fmt.Fprintf(w, "Committed %d transactions to the WAL.\n", len(commits))

	// Attach typed properties before snapshotting. These travel
	// through properties.bin only; the WAL records labels and edges
	// today, not property writes.
	joined := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	if err := g.SetNodeProperty("alice", "name", lpg.StringValue("Alice")); err != nil {
		return fmt.Errorf("SetNodeProperty name: %w", err)
	}
	if err := g.SetNodeProperty("alice", "age", lpg.Int64Value(30)); err != nil {
		return fmt.Errorf("SetNodeProperty age: %w", err)
	}
	if err := g.SetNodeProperty("alice", "joined", lpg.TimeValue(joined)); err != nil {
		return fmt.Errorf("SetNodeProperty joined: %w", err)
	}
	if err := g.SetEdgeProperty("alice", "bob", "since", lpg.StringValue("2026")); err != nil {
		return fmt.Errorf("SetEdgeProperty since: %w", err)
	}
	if err := g.SetEdgeProperty("alice", "bob", "weight", lpg.Int64Value(7)); err != nil {
		return fmt.Errorf("SetEdgeProperty weight: %w", err)
	}
	fmt.Fprintln(w, "Typed properties set on alice and edge alice->bob.")

	// Persist a v2 snapshot (CSR + labels.bin + properties.bin)
	// alongside the WAL.
	cs := csr.BuildFromAdjList(g.AdjList())
	snapDir := filepath.Join(dir, "snapshot")
	if err := snapshot.WriteSnapshotFull(snapDir, cs, g); err != nil {
		fmt.Fprintln(w, "snapshot.WriteSnapshotFull:", err)
		return nil
	}
	fmt.Fprintln(w, "v2 snapshot persisted: csr.bin + labels.bin + properties.bin + manifest.json.")
	_ = wl.Close()

	// "Restart": drop all in-memory references and rebuild from disk.
	_ = store
	_ = g
	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		fmt.Fprintln(w, "recovery.Open:", err)
		return nil
	}
	// A corrupt WAL is fail-stop: recovery returns a non-nil error (handled
	// above) and res.IsClean() reports false. Refuse to build on a corrupt
	// log rather than silently proceeding with a damaged prefix.
	if !res.IsClean() {
		fmt.Fprintln(w, "recovery: refusing to use a corrupt WAL:", res.TailErr)
		return nil
	}
	fmt.Fprintf(w, "Recovered: WAL ops=%d, snapshot hit=%v, snapshot label records=%d, snapshot property records=%d.\n",
		res.WALOps, res.SnapshotHit, res.SnapshotLabels, res.SnapshotProperties)
	for _, c := range commits {
		if res.Graph.HasNodeLabel(c.src, c.nodeLabel) &&
			res.Graph.HasEdgeLabel(c.src, c.dst, c.edgeLabel) {
			fmt.Fprintf(w, "  recovered %s -[%s]-> %s (src carries %q)\n",
				c.src, c.edgeLabel, c.dst, c.nodeLabel)
		} else {
			fmt.Fprintf(w, "  MISSING label data for %s -> %s\n", c.src, c.dst)
		}
	}

	// Assert typed-property survival.
	if v, ok := res.Graph.GetNodeProperty("alice", "name"); !ok {
		fmt.Fprintln(w, "  MISSING property alice.name")
	} else if s, _ := v.String(); s != "Alice" {
		fmt.Fprintf(w, "  property alice.name mismatch: %q\n", s)
	} else {
		fmt.Fprintf(w, "  recovered alice.name = %q\n", s)
	}
	if v, ok := res.Graph.GetNodeProperty("alice", "age"); !ok {
		fmt.Fprintln(w, "  MISSING property alice.age")
	} else if i, _ := v.Int64(); i != 30 {
		fmt.Fprintf(w, "  property alice.age mismatch: %d\n", i)
	} else {
		fmt.Fprintf(w, "  recovered alice.age = %d\n", i)
	}
	if v, ok := res.Graph.GetNodeProperty("alice", "joined"); !ok {
		fmt.Fprintln(w, "  MISSING property alice.joined")
	} else if tm, _ := v.Time(); !tm.Equal(joined) {
		fmt.Fprintf(w, "  property alice.joined mismatch: %v\n", tm)
	} else {
		fmt.Fprintf(w, "  recovered alice.joined = %s\n", tm.Format(time.RFC3339))
	}
	if v, ok := res.Graph.GetEdgeProperty("alice", "bob", "since"); !ok {
		fmt.Fprintln(w, "  MISSING edge property since")
	} else if s, _ := v.String(); s != "2026" {
		fmt.Fprintf(w, "  edge(alice,bob).since mismatch: %q\n", s)
	} else {
		fmt.Fprintf(w, "  recovered edge(alice,bob).since = %q\n", s)
	}
	if v, ok := res.Graph.GetEdgeProperty("alice", "bob", "weight"); !ok {
		fmt.Fprintln(w, "  MISSING edge property weight")
	} else if i, _ := v.Int64(); i != 7 {
		fmt.Fprintf(w, "  edge(alice,bob).weight mismatch: %d\n", i)
	} else {
		fmt.Fprintf(w, "  recovered edge(alice,bob).weight = %d\n", i)
	}
	return nil
}
