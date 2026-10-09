package main

// Crash scenarios of the segmented WAL (docs/design-wal-v2.md §8): the control
// file at store creation, the first segment, the legacy migration, the
// checkpoint's control write and segment unlink, and the legacy stub. The
// rollover breakpoints reuse runConcurrentWriters with a small segment size
// (envSegmentSize).

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

const (
	// envSegmentSize, when set, makes runConcurrentWriters open its WAL with
	// that segment size, so the rollover breakpoints are reached.
	envSegmentSize = "GOGRAPH_CRASH_SEGMENT"
	// envSyncLatency, when set to a positive seed, installs a seeded 1-5 ms
	// fsync latency on runConcurrentWriters' WAL, opening the commit-ordering
	// window a RAM drive closes.
	envSyncLatency = "GOGRAPH_CRASH_SYNCLAT"
)

// segFillerNodes is how many 64 KiB property writes runSegmentedCheckpointCrash
// commits between the seed and the post edge: about 2.6 MiB of log, so with
// 1 MiB segments the redo position lies in the third segment and the checkpoint
// unlinks two segments.
const segFillerNodes = 40

// segFillerValue is the property every filler node carries.
var segFillerValue = strings.Repeat("v", 64<<10)

// int64Opts are the codecs every scenario in this file uses.
func int64Opts() txn.Options[int64, int64] {
	return txn.Options[int64, int64]{Codec: txn.NewInt64Codec(), WeightCodec: txn.NewInt64WeightCodec()}
}

// commitSeed commits the checkpoint seed workload (checkpointSeedEdges, the
// Root label, weight=42 on node 2) in one transaction.
func commitSeed(store *txn.Store[int64, int64]) {
	tx := store.Begin()
	for _, e := range checkpointSeedEdges {
		if err := tx.AddEdge(e.src, e.dst, e.weight); err != nil {
			log.Fatalf("AddEdge(%d->%d): %v", e.src, e.dst, err)
		}
	}
	if err := tx.SetNodeLabel(1, "Root"); err != nil {
		log.Fatalf("SetNodeLabel: %v", err)
	}
	if err := tx.SetNodeProperty(2, "weight", lpg.Int64Value(42)); err != nil {
		log.Fatalf("SetNodeProperty: %v", err)
	}
	if err := tx.Commit(); err != nil {
		log.Fatalf("Commit(seed): %v", err)
	}
}

// commitPost commits checkpointPostEdge.
func commitPost(store *txn.Store[int64, int64]) {
	tx := store.Begin()
	if err := tx.AddEdge(checkpointPostEdge.src, checkpointPostEdge.dst, checkpointPostEdge.weight); err != nil {
		log.Fatalf("AddEdge(post): %v", err)
	}
	if err := tx.Commit(); err != nil {
		log.Fatalf("Commit(post): %v", err)
	}
}

// runCheckpoint runs one codec-aware checkpoint of store's graph over w.
func runCheckpoint(dir string, g *lpg.Graph[int64, int64], w *wal.Writer, store *txn.Store[int64, int64]) error {
	var mu sync.Mutex
	cp := checkpoint.New[int64, int64](
		checkpoint.Config{Dir: dir},
		g, w, &mu,
		checkpoint.WithCommitSerialiser[int64, int64](store.RunUnderCommitLock),
		checkpoint.WithMapperCodec[int64, int64](store.Codec()),
	)
	return cp.RunCheckpoint()
}

// runSegmentedCheckpointCrash commits the seed, segFillerNodes 64 KiB property
// writes (nodes 100..) and the post edge on a WAL of 1 MiB segments, then runs
// one checkpoint, which crashes at the named point of its control-file write or
// segment unlink. Recovery must reconstruct the whole committed state from the
// snapshot plus whichever segments survive.
func runSegmentedCheckpointCrash(dir, scenario string) {
	w, err := wal.OpenWithOptions(filepath.Join(dir, "wal"), wal.Options{SegmentSize: wal.MinSegmentSize})
	if err != nil {
		log.Fatalf("wal.OpenWithOptions: %v", err)
	}
	g := lpg.New[int64, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[int64, int64](g, w, int64Opts())
	commitSeed(store)
	for i := int64(0); i < segFillerNodes; i++ {
		tx := store.Begin()
		if err := tx.SetNodeProperty(100+i, "fill", lpg.StringValue(segFillerValue)); err != nil {
			log.Fatalf("SetNodeProperty(filler %d): %v", i, err)
		}
		if err := tx.Commit(); err != nil {
			log.Fatalf("Commit(filler %d): %v", i, err)
		}
	}
	commitPost(store)
	if _, err := os.Stat(wal.SegmentPath(filepath.Join(dir, "wal"), 3)); err != nil { //nolint:gosec // G703: dir is the crash harness's own directory
		log.Fatalf("the workload did not reach a third segment: %v", err)
	}
	if err := runCheckpoint(dir, g, w, store); err != nil {
		log.Fatalf("checkpoint: %v", err)
	}
	fmt.Printf("runSegmentedCheckpointCrash: completed without crash (GOGRAPH_CRASH_AT != %s)\n", scenario)
}

// buildLegacyStore writes the seed into a legacy single-file log at dir/wal
// (a single-file writer: no control file, no segments) and closes it.
func buildLegacyStore(dir string) {
	f, err := os.OpenFile(filepath.Join(dir, "wal"), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // G304: dir is the crash harness's own directory
	if err != nil {
		log.Fatalf("create legacy log: %v", err)
	}
	w, err := wal.OpenWith(f)
	if err != nil {
		log.Fatalf("wal.OpenWith: %v", err)
	}
	g := lpg.New[int64, int64](adjlist.Config{Directed: true})
	commitSeed(txn.NewStoreWithOptions[int64, int64](g, w, int64Opts()))
	if err := w.Close(); err != nil {
		log.Fatalf("close legacy log: %v", err)
	}
}

// runMigrateCrash builds a legacy store holding the seed, then opens it for
// writing, which migrates it and crashes at the named migration point.
func runMigrateCrash(dir, scenario string) {
	buildLegacyStore(dir)
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		log.Fatalf("wal.Open (migration): %v", err)
	}
	_ = w.Close()
	fmt.Printf("runMigrateCrash: completed without crash (GOGRAPH_CRASH_AT != %s)\n", scenario)
}

// runLegacyStubCrash builds a legacy store holding the seed, recovers and
// migrates it, commits the post edge, and runs a checkpoint, which replaces the
// legacy file with its seal stub and crashes after the rename, before the
// directory fsync.
func runLegacyStubCrash(dir, scenario string) {
	buildLegacyStore(dir)
	res, err := recovery.Open[int64, int64](dir, recovery.OptionsFromTxn(int64Opts()))
	if err != nil || !res.IsClean() {
		log.Fatalf("recover legacy store: err=%v clean=%v", err, res.IsClean()) //nolint:gosec // G706: the values are this helper's own recovery results
	}
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		log.Fatalf("wal.Open (migration): %v", err)
	}
	store := res.NewStore(w, int64Opts())
	commitPost(store)
	if err := runCheckpoint(dir, res.Graph, w, store); err != nil {
		log.Fatalf("checkpoint: %v", err)
	}
	fmt.Printf("runLegacyStubCrash: completed without crash (GOGRAPH_CRASH_AT != %s)\n", scenario)
}

// runCreateCrash opens a WAL in an empty directory, which creates the control
// file, the first segment and the seal stub, and crashes at the named point.
func runCreateCrash(dir, scenario string) {
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		log.Fatalf("wal.Open: %v", err)
	}
	_ = w.Close()
	fmt.Printf("runCreateCrash: completed without crash (GOGRAPH_CRASH_AT != %s)\n", scenario)
}

// openConcurrentWAL opens runConcurrentWriters' WAL, honouring envSegmentSize
// and envSyncLatency.
func openConcurrentWAL(dir string) *wal.Writer {
	opts := wal.Options{SegmentSize: int64(envInt(envSegmentSize, 0))}
	if seed := envInt(envSyncLatency, 0); seed > 0 {
		opts.SyncLatency = wal.NewSyncLatency(uint64(seed), time.Millisecond, 5*time.Millisecond)
	}
	w, err := wal.OpenWithOptions(filepath.Join(dir, "wal"), opts)
	if err != nil {
		log.Fatalf("wal.OpenWithOptions: %v", err)
	}
	return w
}
