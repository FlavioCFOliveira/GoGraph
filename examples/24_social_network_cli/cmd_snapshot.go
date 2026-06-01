package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
)

// cmdSnapshot forces a manual checkpoint of the current in-memory
// graph by building a CSR view and calling snapshot.WriteSnapshotFull
// to persist it (manifest + csr.bin + labels.bin + properties.bin +
// indexes) alongside the existing WAL. After this command runs, a
// fresh recovery.Open can rebuild the same graph state from disk
// without replaying the entire WAL history.
//
// On success cmdSnapshot writes a single JSON object to stdout:
//
//	{"snapshot_dir":"<abs>","status":"ok"}
//
// If the data directory has not been initialised (no manifest), the
// command returns an error from openStore that points users at
// `init -d <dir>`.
func cmdSnapshot(args []string) error {
	dir, _, err := parseDataDir("snapshot", args)
	if err != nil {
		return err
	}
	return runSnapshot(context.Background(), dir, os.Stdout)
}

// runSnapshot is the entry point used by both cmdSnapshot and the
// round-trip test in T9.
func runSnapshot(ctx context.Context, dir string, out io.Writer) (retErr error) {
	o, err := openStore(ctx, dir)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := o.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("snapshot: close store: %w", cerr)
		}
	}()

	cs := csr.BuildFromAdjList(o.graph.AdjList())
	_, snapDir := dataDirPaths(dir)
	if err := snapshot.WriteSnapshotFull(snapDir, cs, o.graph); err != nil {
		return fmt.Errorf("snapshot: write: %w", err)
	}
	return writeJSONObject(out, map[string]any{
		"status":       "ok",
		"snapshot_dir": snapDir,
	})
}
