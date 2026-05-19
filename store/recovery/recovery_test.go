package recovery

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/txn"
	"gograph/store/wal"
)

// committedOp is a record of what we know to be durable so we can
// rebuild the expected post-recovery state without consulting the
// in-memory graph.
type committedOp struct {
	kind  txn.OpKind
	src   string
	dst   string
	label string
}

func writeWorkload(t *testing.T, dir string, ops []committedOp, syncEvery int) {
	t.Helper()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStore(g, w)
	for i, op := range ops {
		tx := store.Begin()
		switch op.kind {
		case txn.OpAddEdge:
			_ = tx.AddEdge(op.src, op.dst)
		case txn.OpSetNodeLabel:
			_ = tx.SetNodeLabel(op.src, op.label)
		case txn.OpSetEdgeLabel:
			_ = tx.SetEdgeLabel(op.src, op.dst, op.label)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit %d: %v", i, err)
		}
		_ = syncEvery // Commit already fsyncs
	}
}

func TestRecovery_RestoresCommittedOps(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ops := []committedOp{
		{kind: txn.OpSetNodeLabel, src: "alice", label: "Person"},
		{kind: txn.OpSetNodeLabel, src: "bob", label: "Person"},
		{kind: txn.OpAddEdge, src: "alice", dst: "bob"},
		{kind: txn.OpSetEdgeLabel, src: "alice", dst: "bob", label: "KNOWS"},
	}
	writeWorkload(t, dir, ops, 1)
	res, err := OpenString(dir)
	if err != nil {
		t.Fatalf("OpenString: %v", err)
	}
	if res.WALOps != len(ops) {
		t.Fatalf("WALOps = %d, want %d", res.WALOps, len(ops))
	}
	if !res.Graph.HasNodeLabel("alice", "Person") {
		t.Fatalf("alice should carry Person")
	}
	if !res.Graph.AdjList().HasEdge("alice", "bob") {
		t.Fatalf("alice -> bob should be present")
	}
	if !res.Graph.HasEdgeLabel("alice", "bob", "KNOWS") {
		t.Fatalf("alice -> bob should carry KNOWS")
	}
}

func TestRecovery_TornTailDropsLastOp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ops := []committedOp{
		{kind: txn.OpSetNodeLabel, src: "a", label: "L"},
		{kind: txn.OpSetNodeLabel, src: "b", label: "L"},
		{kind: txn.OpSetNodeLabel, src: "c", label: "L"},
	}
	writeWorkload(t, dir, ops, 1)
	// Truncate the WAL file by one byte to simulate a torn tail.
	path := filepath.Join(dir, "wal")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, info.Size()-1); err != nil {
		t.Fatal(err)
	}
	res, err := OpenString(dir)
	if err != nil {
		t.Fatalf("OpenString: %v", err)
	}
	if res.WALOps > 2 {
		t.Fatalf("torn tail should drop at least the last op; got %d", res.WALOps)
	}
	if !res.Graph.HasNodeLabel("a", "L") || !res.Graph.HasNodeLabel("b", "L") {
		t.Fatalf("earlier ops must survive")
	}
}

func TestRecovery_FuzzedTruncation(t *testing.T) {
	t.Parallel()
	const iterations = 200
	r := rand.New(rand.NewPCG(1, 1)) //nolint:gosec // deterministic test RNG
	for it := 0; it < iterations; it++ {
		dir := t.TempDir()
		opCount := 5 + r.IntN(20)
		ops := make([]committedOp, opCount)
		for k := range ops {
			ops[k] = committedOp{
				kind:  txn.OpSetNodeLabel,
				src:   fmt.Sprintf("n%d", k),
				label: "L",
			}
		}
		writeWorkload(t, dir, ops, 1)
		path := filepath.Join(dir, "wal")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		// Random truncation length somewhere within the file.
		newSize := r.Int64N(info.Size())
		if err := os.Truncate(path, newSize); err != nil {
			t.Fatal(err)
		}
		res, err := OpenString(dir)
		if err != nil {
			t.Fatalf("iter %d: OpenString: %v", it, err)
		}
		// Recovery must not panic and must produce some prefix of
		// the committed sequence.
		if res.WALOps > opCount {
			t.Fatalf("iter %d: recovered %d ops, expected <= %d", it, res.WALOps, opCount)
		}
		for k := 0; k < res.WALOps; k++ {
			expectedNode := fmt.Sprintf("n%d", k)
			if !res.Graph.HasNodeLabel(expectedNode, "L") {
				t.Fatalf("iter %d: prefix violation at %d", it, k)
			}
		}
	}
}
