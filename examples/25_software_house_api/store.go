package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/store/recovery"
	"gograph/store/snapshot"
	"gograph/store/txn"
	"gograph/store/wal"
)

// lpgConfig is the adjacency-list configuration for the example's graph.
// Every relationship type in the model is directional, so the backend is
// directed. The same config writes the initial empty snapshot and is the
// shape recovery reconstructs on open.
func lpgConfig() adjlist.Config {
	return adjlist.Config{Directed: true}
}

// dataDirPaths returns the canonical WAL file and snapshot directory
// inside dir: <dir>/wal and <dir>/snapshot.
func dataDirPaths(dir string) (walPath, snapDir string) {
	return filepath.Join(dir, "wal"), filepath.Join(dir, "snapshot")
}

// dataDirOptions returns the recovery/txn codec pair for the
// [string, float64] graph shape used throughout the example.
func dataDirOptions() recovery.Options[string, float64] {
	return recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

// hasManifest reports whether dir already holds a snapshot manifest.
func hasManifest(dir string) bool {
	_, snapDir := dataDirPaths(dir)
	_, err := os.Stat(filepath.Join(snapDir, "manifest.json"))
	return err == nil
}

// dataStore bundles the durable resources held for the lifetime of the
// server: the WAL writer, the WAL-backed transactional store, the Cypher
// engine, and the in-memory graph. A single dataStore is opened at
// startup and shared by every HTTP handler.
//
// Concurrency: the embedded *cypher.Engine is safe for concurrent use.
// Read queries run under a read lock and materialise before returning;
// write queries serialise on the store's single-writer mutex and become
// visible atomically. Handlers therefore need no additional locking.
type dataStore struct {
	dir      string
	wal      *wal.Writer
	txnStore *txn.Store[string, float64]
	engine   *cypher.Engine
	graph    *lpg.Graph[string, float64]
	res      recovery.Result[string, float64]
}

// openStore opens the data directory dir for read+write traffic and
// returns a live dataStore, creating the directory and an empty snapshot
// if it does not yet exist. On an existing directory it recovers the
// snapshot and replays the WAL tail. The returned dataStore must be
// closed exactly once via Close.
func openStore(ctx context.Context, dir string) (*dataStore, error) {
	if dir == "" {
		return nil, errors.New("open: empty data dir")
	}
	if err := ensureInit(dir); err != nil {
		return nil, err
	}
	res, err := recovery.OpenCtx[string, float64](ctx, dir, dataDirOptions())
	if err != nil {
		return nil, fmt.Errorf("open: recover: %w", err)
	}
	walPath, _ := dataDirPaths(dir)
	w, err := wal.Open(walPath)
	if err != nil {
		return nil, fmt.Errorf("open: wal: %w", err)
	}
	ts := txn.NewStoreWithOptions(res.Graph, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	return &dataStore{
		dir:      dir,
		wal:      w,
		txnStore: ts,
		engine:   cypher.NewEngineWithStore(ts),
		graph:    res.Graph,
		res:      res,
	}, nil
}

// ensureInit creates dir (if missing) and writes a fresh empty snapshot
// plus an empty WAL when dir has no manifest yet. Idempotent: a directory
// that already holds a manifest is left untouched.
func ensureInit(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("init: mkdir %q: %w", dir, err)
	}
	if hasManifest(dir) {
		return nil
	}
	walPath, snapDir := dataDirPaths(dir)
	// Touch the WAL so the file exists before any write. Recovery
	// tolerates an empty or absent WAL; this only makes the layout
	// complete and predictable.
	w, err := wal.Open(walPath)
	if err != nil {
		return fmt.Errorf("init: open wal: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("init: close wal: %w", err)
	}
	g := lpg.New[string, float64](lpgConfig())
	cs := csr.BuildFromAdjList(g.AdjList())
	if err := snapshot.WriteSnapshotFull(snapDir, cs, g); err != nil {
		return fmt.Errorf("init: snapshot: %w", err)
	}
	return nil
}

// snapshotNow builds a CSR view of the current in-memory graph and writes
// a full snapshot alongside the WAL. It shortens WAL replay on the next
// open; it is not required for durability, because every committed write
// is already fsynced to the WAL before the commit is acknowledged.
func (s *dataStore) snapshotNow() error {
	cs := csr.BuildFromAdjList(s.graph.AdjList())
	_, snapDir := dataDirPaths(s.dir)
	if err := snapshot.WriteSnapshotFull(snapDir, cs, s.graph); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	return nil
}

// Close flushes and releases the WAL writer (fsyncing its tail).
// Idempotent: subsequent calls return nil.
func (s *dataStore) Close() error {
	if s == nil || s.wal == nil {
		return nil
	}
	err := s.wal.Close()
	s.wal = nil
	return err
}
