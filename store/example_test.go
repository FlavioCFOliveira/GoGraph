package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// ExampleDB wires the full WAL-backed durability stack — WAL writer, typed
// store, and a background checkpointer — into a composed store.DB, commits a
// transaction, then closes the whole stack with a single DB.Close. Close runs
// the one crash-safe teardown order (stop the checkpoint goroutine, then close
// the WAL), so no goroutine outlives the call and the WAL is durably closed:
// a subsequent append is rejected with wal.ErrWriterClosed.
func ExampleDB() {
	dir, err := os.MkdirTemp("", "store-db-example")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	wlog, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		panic(err)
	}

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	st := txn.NewStoreWithCodec(g, wlog, txn.NewStringCodec())

	// Background checkpointer, wired the production way: the commit serialiser
	// excludes the store's commit window while it snapshots, and the mapper
	// codec makes the snapshot self-sufficient so the WAL can be truncated.
	var unusedMu sync.Mutex
	cp := checkpoint.New(checkpoint.Config{Dir: dir}, g, wlog, &unusedMu,
		checkpoint.WithCommitSerialiser[string, int64](st.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, int64](txn.NewStringCodec()))
	cp.Start(context.Background())

	// Commit one transaction so the stack is live.
	tx := st.Begin()
	if err := tx.AddEdge("alice", "bob", 0); err != nil {
		panic(err)
	}
	if err := tx.Commit(); err != nil {
		panic(err)
	}

	// One composed Close tears the whole stack down in the crash-safe order.
	db := store.New(wlog, store.WithCheckpointer(cp), store.WithFinalCheckpoint())
	if err := db.Close(); err != nil {
		panic(err)
	}

	// The WAL is durably closed: a further append is rejected.
	appendErr := wlog.Append([]byte("after-close"))
	fmt.Println("append after Close is ErrWriterClosed:", errors.Is(appendErr, wal.ErrWriterClosed))

	// Output:
	// append after Close is ErrWriterClosed: true
}

// ExampleOpen opens a store directory, commits a transaction, closes it, and
// reopens it with store.Open: recovery, the clean gate, the WAL open, and the
// transactional store built from the recovery result happen in one call, so
// the reopened store resumes the transaction sequence the WAL already spent.
func ExampleOpen() {
	dir, err := os.MkdirTemp("", "store-open-example")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	opts := store.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
	for _, key := range []string{"alice", "bob"} {
		o, err := store.Open(dir, opts)
		if err != nil {
			panic(err)
		}
		tx := o.Store().Begin()
		if err := tx.AddNode(key); err != nil {
			panic(err)
		}
		if err := tx.Commit(); err != nil {
			panic(err)
		}
		if err := o.Close(); err != nil {
			panic(err)
		}
	}

	o, err := store.Open(dir, opts)
	if err != nil {
		panic(err)
	}
	defer func() { _ = o.Close() }()
	_, hasAlice := o.Graph().AdjList().Mapper().Lookup("alice")
	_, hasBob := o.Graph().AdjList().Mapper().Lookup("bob")
	fmt.Println(hasAlice, hasBob, o.Recovery().MaxTxnSeq)
	// Output: true true 2
}
