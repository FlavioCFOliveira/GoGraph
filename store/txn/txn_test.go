package txn

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/wal"
)

func openStore(t *testing.T) (store *Store[string, int64], walPath string, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	w, err := wal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store = NewStore(g, w)
	walPath = path
	cleanup = func() {
		_ = w.Close()
		_ = os.RemoveAll(dir)
	}
	return store, walPath, cleanup
}

func TestTx_CommitApplies(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openStore(t)
	defer cleanup()
	tx := s.Begin()
	if err := tx.SetNodeLabel("alice", "Person"); err != nil {
		t.Fatal(err)
	}
	if err := tx.AddEdge("alice", "bob", 0); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !s.Graph().HasNodeLabel("alice", "Person") {
		t.Fatalf("commit did not apply label")
	}
	if !s.Graph().AdjList().HasEdge("alice", "bob") {
		t.Fatalf("commit did not apply edge")
	}
}

func TestTx_Rollback(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openStore(t)
	defer cleanup()
	tx := s.Begin()
	if err := tx.SetNodeLabel("ghost", "Forgotten"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if s.Graph().HasNodeLabel("ghost", "Forgotten") {
		t.Fatalf("Rollback left visible changes")
	}
}

func TestTx_FinishedErrors(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openStore(t)
	defer cleanup()
	tx := s.Begin()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tx.AddEdge("a", "b", 0); !errors.Is(err, ErrTxFinished) {
		t.Fatalf("AddEdge after commit: %v", err)
	}
	if err := tx.Commit(); !errors.Is(err, ErrTxFinished) {
		t.Fatalf("Commit after commit: %v", err)
	}
	if err := tx.Rollback(); !errors.Is(err, ErrTxFinished) {
		t.Fatalf("Rollback after commit: %v", err)
	}
}

func TestTx_SerialisedConcurrent(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openStore(t)
	defer cleanup()
	const txns = 32
	var wg sync.WaitGroup
	wg.Add(txns)
	for i := 0; i < txns; i++ {
		go func(i int) {
			defer wg.Done()
			tx := s.Begin()
			_ = tx.SetNodeLabel("alice", "Person")
			_ = tx.Commit()
			_ = i
		}(i)
	}
	wg.Wait()
	if !s.Graph().HasNodeLabel("alice", "Person") {
		t.Fatalf("final state missing label")
	}
}

func TestTx_DurableViaWAL(t *testing.T) {
	t.Parallel()
	s, walPath, cleanup := openStore(t)
	defer cleanup()
	tx := s.Begin()
	_ = tx.SetNodeLabel("alice", "Person")
	_ = tx.AddEdge("alice", "bob", 0)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Without restarting the process, prove the WAL recorded one
	// commit by counting frames via the Reader.
	r, err := wal.OpenReader(walPath)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	frames := 0
	if err := r.Replay(func(_ wal.Frame) error {
		frames++
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if frames != 2 {
		t.Fatalf("WAL frames = %d, want 2 (one per op)", frames)
	}
}
