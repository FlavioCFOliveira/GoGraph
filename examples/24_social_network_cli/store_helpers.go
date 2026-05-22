package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gograph/cypher"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/store/recovery"
	"gograph/store/snapshot"
	"gograph/store/txn"
	"gograph/store/wal"
)

// dataDirPaths returns the canonical WAL and snapshot paths inside dir.
// The layout is the one consumed by recovery.Open: <dir>/wal and
// <dir>/snapshot/manifest.json plus its sibling .bin files.
func dataDirPaths(dir string) (walPath, snapDir string) {
	return filepath.Join(dir, "wal"), filepath.Join(dir, "snapshot")
}

// hasManifest reports whether dir already contains a v2 snapshot
// manifest. Used by `init` to short-circuit idempotent re-invocations
// without overwriting an existing graph with an empty one.
func hasManifest(dir string) bool {
	_, snapDir := dataDirPaths(dir)
	_, err := os.Stat(filepath.Join(snapDir, "manifest.json"))
	return err == nil
}

// dataDirOptions returns the canonical recovery.Options for the
// example's [string, float64] graph shape. Pinned at package level so
// every subcommand uses the same string / float64 codec pair.
func dataDirOptions() recovery.Options[string, float64] {
	return recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

// openedStore bundles the live resources held while a subcommand is
// running against the data directory: the WAL-backed transactional
// store, the WAL-backed Cypher engine, and the recovery metadata. The
// close method releases the WAL writer and flushes its tail.
//
// One openedStore is created per subcommand invocation. It is not safe
// for concurrent use across subcommands; the CLI is one-shot by design.
type openedStore struct {
	dir    string
	wal    *wal.Writer
	store  *txn.Store[string, float64]
	engine *cypher.Engine
	graph  *lpg.Graph[string, float64]
	res    recovery.Result[string, float64]
}

// open prepares a data directory for read+write traffic. It requires
// the directory to have been initialised previously by [initEmpty];
// callers that want a create-if-missing semantic should call initEmpty
// first.
//
// The returned openedStore must be closed exactly once via [Close].
func openStore(ctx context.Context, dir string) (*openedStore, error) {
	if dir == "" {
		return nil, errors.New("open: empty data dir")
	}
	if !hasManifest(dir) {
		return nil, fmt.Errorf("open: data dir %q has no manifest; run `init -d %s` first", dir, dir)
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
	store := txn.NewStoreWithOptions(res.Graph, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	eng := cypher.NewEngineWithStore(store)
	return &openedStore{
		dir:    dir,
		wal:    w,
		store:  store,
		engine: eng,
		graph:  res.Graph,
		res:    res,
	}, nil
}

// Close releases the WAL writer (which fsyncs its tail) and returns any
// shutdown error. Calling Close more than once is safe; subsequent
// calls return nil.
func (o *openedStore) Close() error {
	if o == nil || o.wal == nil {
		return nil
	}
	err := o.wal.Close()
	o.wal = nil
	return err
}

// initEmpty creates the data directory (if missing) and writes a fresh
// empty snapshot plus an empty WAL inside it. If dir already contains a
// valid manifest, initEmpty is a no-op: the existing graph is left
// untouched so that running `init` twice is safe.
func initEmpty(dir string) error {
	if dir == "" {
		return errors.New("init: empty data dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("init: mkdir %q: %w", dir, err)
	}
	if hasManifest(dir) {
		return nil // idempotent re-invocation
	}
	walPath, snapDir := dataDirPaths(dir)

	// Open and immediately close the WAL so the file exists with
	// mode 0o644 before any future write. The recovery path tolerates
	// an empty or absent WAL, so the only purpose here is to make the
	// directory structure complete and predictable for callers.
	w, err := wal.Open(walPath)
	if err != nil {
		return fmt.Errorf("init: open wal: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("init: close wal: %w", err)
	}

	// Build an empty CSR from an empty LPG graph and snapshot it.
	g := lpg.New[string, float64](lpgConfig())
	cs := csr.BuildFromAdjList(g.AdjList())
	if err := snapshot.WriteSnapshotFull(snapDir, cs, g); err != nil {
		return fmt.Errorf("init: snapshot: %w", err)
	}
	return nil
}

// writeJSONObject emits obj as a single, alphabetically-keyed JSON
// object terminated by '\n'. Used by init / snapshot / stats for their
// one-line success replies.
func writeJSONObject(w io.Writer, obj map[string]any) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}
