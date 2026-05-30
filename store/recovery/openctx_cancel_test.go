package recovery

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/store/snapshot"
	"gograph/store/txn"
	"gograph/store/wal"
)

// TestRecovery_OpenCtxPreCancelled tests [OpenCtx] with a
// pre-cancelled context. The test builds a non-trivial snapshot
// (100 nodes, 200 edges) so the load path has real work to do, then
// cancels the context before calling [OpenCtx]. The call must return
// context.Canceled (or a wrapping error) within 100 ms.
//
// This test anchors the canonical [OpenCtx] entry point.
func TestRecovery_OpenCtxPreCancelled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Build a non-trivial graph so there is actual snapshot data to skip.
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	s := txn.NewStoreWithOptions[string, int64](g, w, opts)

	// 100 nodes, ring topology: each node connects to the next.
	nodes := make([]string, 100)
	for i := range nodes {
		nodes[i] = string(rune('a'+i%26)) + itoa(i)
	}
	for i := 0; i < len(nodes); i++ {
		tx := s.Begin()
		src := nodes[i]
		dst := nodes[(i+1)%len(nodes)]
		if err := tx.AddEdge(src, dst, int64(i)); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", src, dst, err)
		}
		if i%50 == 0 {
			if err := tx.SetNodeLabel(src, "Node"); err != nil {
				t.Fatalf("SetNodeLabel: %v", err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit %d: %v", i, err)
		}
	}

	c := csr.BuildFromAdjList(g.AdjList())
	if err := snapshot.WriteSnapshotFull(filepath.Join(dir, "snapshot"), c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	// Pre-cancel the context before recovery starts.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err = OpenCtx[string, int64](ctx, dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenCtx pre-cancelled = %v, want context.Canceled", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("OpenCtx with pre-cancelled ctx took %v, want < 100ms", elapsed)
	}
}

// itoa converts a non-negative int to its decimal string without
// importing strconv so this helper stays local to the file.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
