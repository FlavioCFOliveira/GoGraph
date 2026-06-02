package cypher_test

// multigraph_recovery_test.go — regression coverage for the consumer-reported
// "parallel/typed edges collapse to one across recovery" bug.
//
// openCypher is a multigraph model: CREATE of a relationship is additive, so
// two CREATEs of relationships between the same ordered pair must yield two
// relationships — even with different types. The in-memory engine and the
// persistence layer (WAL / snapshot / CSR) already support this, but
// recovery.Open used to rebuild the graph in simple-graph mode, so every
// consumer that recovers from disk (e.g. a CLI where each command reopens the
// store) silently lost all but the last parallel edge. recovery.Open now
// builds the graph with Multigraph: true, matching the TCK harness.
//
// Layer: short. goleak-clean (engines/graphs are local).

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func mgRecOpts() recovery.Options[string, float64] {
	return recovery.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
}

func mgStoreOpts() txn.Options[string, float64] {
	return txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
}

// mgWriteCycle reopens dir, runs each query, commits, then persists. When
// snap is true it writes a full snapshot and truncates the WAL; otherwise it
// only fsyncs the WAL (pure WAL-replay recovery on the next open).
func mgWriteCycle(t *testing.T, dir string, snap bool, queries ...string) {
	t.Helper()
	res, err := recovery.Open[string, float64](dir, mgRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	store := txn.NewStoreWithOptions[string, float64](res.Graph, w, mgStoreOpts())
	eng := cypher.NewEngineWithStore(store)
	for _, q := range queries {
		r, err := eng.RunInTxAny(context.Background(), q, nil)
		if err != nil {
			w.Close()
			t.Fatalf("RunInTx(%q): %v", q, err)
		}
		for r.Next() { //nolint:revive // drain to run the write to completion
		}
		if rerr := r.Err(); rerr != nil {
			_ = r.Close()
			w.Close()
			t.Fatalf("result err (%q): %v", q, rerr)
		}
		if err := r.Close(); err != nil {
			w.Close()
			t.Fatalf("Close(%q): %v", q, err)
		}
	}
	if snap {
		cs := csr.BuildFromAdjList(res.Graph.AdjList())
		if err := snapshot.WriteSnapshotFullWithMapperCodec(filepath.Join(dir, "snapshot"), cs, res.Graph, txn.NewStringCodec()); err != nil {
			w.Close()
			t.Fatalf("WriteSnapshotFull: %v", err)
		}
	}
	if err := w.Sync(); err != nil {
		w.Close()
		t.Fatalf("Sync: %v", err)
	}
	if snap {
		if _, err := w.Truncate(); err != nil {
			w.Close()
			t.Fatalf("Truncate: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
}

// mgReadTypes reopens dir and returns the sorted relationship types between
// the (key:'x') -> (key:'y') ordered pair.
func mgReadTypes(t *testing.T, dir string) []string {
	t.Helper()
	res, err := recovery.Open[string, float64](dir, mgRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open(read): %v", err)
	}
	r, err := cypher.NewEngine(res.Graph).RunAny(context.Background(),
		`MATCH (:N {key:'x'})-[r]->(:N {key:'y'}) RETURN type(r) AS t`, nil)
	if err != nil {
		t.Fatalf("read types: %v", err)
	}
	rows := collectRecords(t, r)
	types := make([]string, 0, len(rows))
	for _, row := range rows {
		s, ok := row["t"].(expr.StringValue)
		if !ok {
			t.Fatalf("type(r) is %T, want StringValue", row["t"])
		}
		types = append(types, string(s))
	}
	sort.Strings(types)
	return types
}

func runParallelEdgeReopen(t *testing.T, snap bool) {
	t.Helper()
	dir := t.TempDir()
	// Build the two endpoints and two distinctly-typed parallel edges, each in
	// its own reopen cycle, mirroring a one-command-per-process consumer.
	mgWriteCycle(t, dir, snap, `CREATE (a:N {key:'x'})`)
	mgWriteCycle(t, dir, snap, `CREATE (b:N {key:'y'})`)
	mgWriteCycle(t, dir, snap, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:USES]->(b)`)
	mgWriteCycle(t, dir, snap, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:CALLS]->(b)`)

	got := mgReadTypes(t, dir)
	want := []string{"CALLS", "USES"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("parallel typed edges collapsed across reopen (snap=%v): got types %v, want %v",
			snap, got, want)
	}
}

// TestMultigraph_ParallelTypedEdges_WAL verifies two distinctly-typed parallel
// edges survive pure WAL-replay recovery as two relationships.
func TestMultigraph_ParallelTypedEdges_WAL(t *testing.T) {
	t.Parallel()
	runParallelEdgeReopen(t, false)
}

// TestMultigraph_ParallelTypedEdges_Snapshot verifies the same across the
// self-sufficient snapshot recovery path (snapshot + WAL truncate).
func TestMultigraph_ParallelTypedEdges_Snapshot(t *testing.T) {
	t.Parallel()
	runParallelEdgeReopen(t, true)
}
