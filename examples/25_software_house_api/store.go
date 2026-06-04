package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// ErrStoreClosed is returned by [dataStore.acquire] once [dataStore.Close]
// has run (or is running). A handler that observes it must reject the
// request — typically as 503 Service Unavailable — rather than touch a
// store whose WAL is being released: a write admitted after Close could
// apply in memory and then lose its WAL append, an in-memory/durable
// divergence at shutdown.
var ErrStoreClosed = errors.New("data store is closed")

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
// Concurrency: the engine's read execution is lock-free over an immutable
// snapshot and write commits are atomic, but its plan- and filter-building
// phase reads the live adjacency offsets and interning tables that a
// concurrent write mutates. A dataStore is therefore NOT safe for
// concurrent read+write without serialisation, so it owns that
// serialisation itself: every handler enters through acquire (a shared
// hold for readers, an exclusive hold for writers) and Close takes the
// exclusive hold before releasing the WAL. mu is the store's outermost
// lock — held across the whole engine call (plan-build, drain, and
// Result.Close) — so it cannot invert with the store's internal
// single-writer mutex, and no write can be mid-commit when Close releases
// the WAL.
//
// Once closed is set (under mu), acquire returns [ErrStoreClosed] without
// granting the hold, so a request that arrives after Close is cleanly
// rejected rather than admitted onto a WAL that is being released.
type dataStore struct {
	dir      string
	wal      *wal.Writer
	txnStore *txn.Store[string, float64]
	engine   *cypher.Engine
	graph    *lpg.Graph[string, float64]
	res      recovery.Result[string, float64]

	// mu serialises every engine access (readers share, writers and Close
	// are exclusive) and guards closed. It is the outermost lock.
	mu     sync.RWMutex
	closed bool

	// beforeEngine, when non-nil, is invoked while the acquire hold is held
	// but before the handler runs its engine call. It is a test-only seam
	// for forcing a write to park mid-statement so a racing Close can be
	// observed to wait for it; production code never sets it.
	beforeEngine func()
}

// acquire takes the serialisation hold for one engine access: an exclusive
// (write) hold when write is true, otherwise a shared (read) hold. It
// returns a release function the caller must defer, or [ErrStoreClosed]
// (with no hold taken) when the store has been closed. The hold is held
// across the caller's entire engine call — plan-build, row drain, and the
// final Result.Close — because a writer keeps the store's single-writer
// mutex until its result is closed and a reader's plan-build must not
// observe a concurrent write.
func (s *dataStore) acquire(write bool) (release func(), err error) {
	if write {
		s.mu.Lock()
	} else {
		s.mu.RLock()
	}
	if s.closed {
		if write {
			s.mu.Unlock()
		} else {
			s.mu.RUnlock()
		}
		return nil, ErrStoreClosed
	}
	if s.beforeEngine != nil {
		s.beforeEngine()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if write {
				s.mu.Unlock()
			} else {
				s.mu.RUnlock()
			}
		})
	}, nil
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
	// Fail-stop on a corrupt WAL: recovery surfaces genuine corruption (a CRC
	// mismatch, bad magic, or unsupported record version inside an
	// already-durable frame) via a non-nil error AND res.IsClean() == false.
	// Opening the WAL for append in that state would permanently embed the
	// corruption and silently drop every committed op past the bad frame, so
	// the API refuses to serve from a corrupt data directory. A benign torn
	// tail (the normal crash case) leaves res.IsClean() == true.
	if !res.IsClean() {
		return nil, fmt.Errorf("open: refusing to append to a corrupt WAL: %w", res.TailErr)
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

// Close quiesces in-flight engine writes, then flushes and releases the WAL
// writer (fsyncing its tail). Idempotent: subsequent calls return nil.
//
// Close takes the store's exclusive serialisation hold before touching the
// WAL, so it cannot run while a handler is mid-statement — in particular it
// cannot close the WAL underneath an in-flight write that is about to fsync
// its commit, which would leave that write applied in memory but lost from
// the WAL (an in-memory/durable divergence at shutdown). Once the hold is
// taken, Close marks the store closed under the same lock, so every later
// acquire returns [ErrStoreClosed] and the corresponding request is cleanly
// rejected rather than admitted onto the closing WAL.
//
// Callers do not need to drain or lock around Close; it enforces the
// quiesce contract itself. The example's Server still drains in-flight HTTP
// requests first (so clients see their responses), but correctness no
// longer depends on that ordering.
func (s *dataStore) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.wal == nil {
		s.closed = true
		return nil
	}
	s.closed = true
	err := s.wal.Close()
	s.wal = nil
	return err
}
