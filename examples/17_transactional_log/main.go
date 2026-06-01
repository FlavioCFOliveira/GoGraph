// Example 17_transactional_log — end-to-end durability walk-through with
// an ACID-safe background checkpointer.
//
// The flow: open a WAL-backed transaction store, commit a sequence of
// transactions, run a background checkpointer that folds the WAL tail into
// a self-sufficient on-disk snapshot, then simulate a crash (the process
// abandons every in-memory reference) and recover the graph from disk —
// verifying that every committed edge and label comes back.
//
// Shared-lock ACID coordination. The checkpointer takes its snapshot by
// reading the live in-memory graph; if it read concurrently with a
// transaction's in-memory apply it could capture a partially-applied
// transaction and persist a state that violates Atomicity. To prevent
// that, this example follows the contract documented on
// [checkpoint.New]: the checkpointer is handed the SAME [sync.Mutex] that
// the writer holds across each transaction's Begin→Commit window. The
// checkpointer acquires that mutex before it builds the CSR snapshot, so a
// snapshot can never interleave with a commit — the checkpointer observes
// either all of a transaction's writes or none of them. This is the wiring
// the checkpoint package's own ExampleCheckpointer and tests use; here the
// writer additionally holds the lock so the coordination is real rather
// than nominal.
//
// Output is non-deterministic: the checkpoint cadence depends on timing and
// the printed stats vary per run, and the WAL/snapshot live under an
// os.MkdirTemp directory whose path changes every run. The regression test
// (example_test.go) therefore asserts the deterministic invariant — every
// committed edge and label is recovered — rather than pinning a byte-stable
// // Output: block.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// commits is the fixed transaction workload: each entry is
// {src, dst, label}. Recovery must reproduce every one of these edges and
// its label regardless of when the background checkpointer happens to fire.
var commits = [][3]string{
	{"alice", "bob", "KNOWS"},
	{"bob", "carol", "KNOWS"},
	{"carol", "dave", "KNOWS"},
	{"alice", "carol", "FOLLOWS"},
}

// run drives the whole walk-through and writes its report to w. It returns
// an error instead of terminating so a test can drive it and assert on the
// recovered state.
func run(w io.Writer) error {
	dir, err := os.MkdirTemp("", "gograph-ex17-")
	if err != nil {
		return fmt.Errorf("mkdir temp: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	walPath := filepath.Join(dir, "wal")
	wlog, err := wal.Open(walPath)
	if err != nil {
		return fmt.Errorf("open WAL: %w", err)
	}

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithCodec(g, wlog, txn.NewStringCodec())

	// commitMu is the shared single-writer lock. The writer below holds it
	// across each transaction's Begin→Commit window, and the SAME mutex is
	// handed to the checkpointer (checkpoint.New's storeMu parameter). The
	// checkpointer acquires it before snapshotting, so a snapshot can never
	// capture a half-applied transaction — the Atomicity guarantee the
	// checkpoint package's contract depends on.
	var commitMu sync.Mutex

	// Background checkpointer firing roughly every 50 ms. WithMapperCodec
	// makes the string-keyed snapshot self-sufficient (it persists the
	// NodeID->key mapper.bin), so the checkpointer can safely truncate the
	// WAL after each snapshot.
	cp := checkpoint.New(checkpoint.Config{
		Dir:      dir,
		MaxAge:   50 * time.Millisecond,
		Interval: 25 * time.Millisecond,
	}, g, wlog, &commitMu, checkpoint.WithMapperCodec[string, int64](txn.NewStringCodec()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)

	// Commit the workload. Each transaction holds commitMu for its whole
	// lifetime; the background checkpointer contends for the same lock, so
	// the two never overlap.
	for _, c := range commits {
		if err := commitEdge(store, &commitMu, c[0], c[1], c[2]); err != nil {
			cp.Stop()
			_ = wlog.Close()
			return fmt.Errorf("commit %s->%s: %w", c[0], c[1], err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cp.Stop()
	if err := wlog.Close(); err != nil {
		return fmt.Errorf("close WAL: %w", err)
	}
	fmt.Fprintf(w, "Committed %d transactions.\n", len(commits))
	fmt.Fprintf(w, "Checkpoint stats: %+v\n", cp.Stats())

	// Simulate a crash by abandoning every in-memory reference and
	// recovering state from the snapshot plus any WAL tail. We do not touch
	// g or store after this point.
	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		return fmt.Errorf("recovery open: %w", err)
	}
	fmt.Fprintf(w, "\nRecovered %d ops from WAL (snapshot used: %v).\n", res.WALOps, res.SnapshotHit)
	for _, c := range commits {
		if !res.Graph.AdjList().HasEdge(c[0], c[1]) {
			return fmt.Errorf("recovery lost committed edge %s->%s", c[0], c[1])
		}
		if !res.Graph.HasEdgeLabel(c[0], c[1], c[2]) {
			return fmt.Errorf("recovery lost label %q on edge %s->%s", c[2], c[0], c[1])
		}
		fmt.Fprintf(w, "  recovered %s -> %s with label %q\n", c[0], c[1], c[2])
	}
	return nil
}

// commitEdge holds commitMu for the entire Begin→Commit window of one
// transaction, so the background checkpointer (which acquires the same
// mutex before snapshotting) can never observe this transaction
// partially applied.
func commitEdge(store *txn.Store[string, int64], commitMu *sync.Mutex, src, dst, label string) error {
	commitMu.Lock()
	defer commitMu.Unlock()

	tx := store.Begin()
	if err := tx.AddEdge(src, dst, 0); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("add edge: %w", err)
	}
	if err := tx.SetEdgeLabel(src, dst, label); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("set edge label: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
