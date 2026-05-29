package wal_test

import (
	"fmt"
	"os"
	"path/filepath"

	"gograph/store/wal"
)

// Example shows the core write-ahead-log loop: open a writer, append a
// few opaque payload frames, Sync them durably, then reopen the file
// with a Reader and replay every frame back in append order.
func Example() {
	dir, err := os.MkdirTemp("", "wal-example")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	path := filepath.Join(dir, "wal")

	// Append three records. A WAL payload is opaque bytes as far as the
	// log is concerned; the durability stack above it (store/txn) gives
	// them meaning. Group-commit: several Appends, then a single Sync.
	w, err := wal.Open(path)
	if err != nil {
		panic(err)
	}
	for _, rec := range [][]byte{[]byte("alpha"), []byte("bravo"), []byte("charlie")} {
		if err := w.Append(rec); err != nil {
			panic(err)
		}
	}
	if err := w.Sync(); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}

	// Replay: a fresh Reader iterates the frames in the order they were
	// appended, stopping cleanly at the first torn frame (none here).
	r, err := wal.OpenReader(path)
	if err != nil {
		panic(err)
	}
	defer func() { _ = r.Close() }()

	count := 0
	err = r.Replay(func(f wal.Frame) error {
		count++
		fmt.Printf("frame %d: %s\n", count, f.Payload)
		return nil
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("replayed %d frames\n", count)

	// Output:
	// frame 1: alpha
	// frame 2: bravo
	// frame 3: charlie
	// replayed 3 frames
}
