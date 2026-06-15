// Command crashinject-helper is the child process spawned by the
// crashinject harness during crash-injection tests. It should never
// be run directly; it is always invoked by [crashinject.Run] with the
// environment variables GOGRAPH_CRASH_AT and GOGRAPH_CRASH_DIR set.
//
// Each scenario writes a specific artefact (WAL file, snapshot, …)
// to GOGRAPH_CRASH_DIR and then calls [crashinject.Breakpoint] at
// the named execution point. [crashinject.Breakpoint] sends SIGKILL
// to the process when GOGRAPH_CRASH_AT matches the breakpoint name,
// leaving the artefact in a deterministically torn state.
//
// Registered scenarios:
//
//	wal.mid-frame  — writes one complete WAL frame, appends a partial
//	                 second-frame header, then crashes. The resulting
//	                 WAL file has a torn tail that wal.Reader must
//	                 detect as ErrTornFrame.
//
//	checkpoint.p2-snapshot-published-pre-truncate
//	               — commits an int64-keyed workload, then triggers a
//	                 non-blocking codec-aware checkpoint that crashes AFTER
//	                 the self-sufficient snapshot is published and durable
//	                 but BEFORE the WAL prefix is truncated. Recovery must
//	                 reconstruct the full state from the snapshot plus the
//	                 still-intact full WAL (idempotent whole-WAL replay).
//
//	checkpoint.truncprefix.tmp-written-pre-rename
//	checkpoint.truncprefix.post-rename-pre-dirfsync
//	checkpoint.truncprefix.post-rename-pre-bookkeeping
//	               — commits the seed, runs ONE complete checkpoint
//	                 (prefix-truncating to a self-sufficient snapshot),
//	                 commits one more "post" edge so the WAL carries a real
//	                 non-empty suffix, then triggers a SECOND checkpoint
//	                 whose prefix-truncate crashes at the named point in
//	                 wal.Writer.TruncatePrefix's atomic copy-then-rename.
//	                 Recovery must reconstruct the full committed state
//	                 (seed + post edge) from the snapshot plus whichever WAL
//	                 — original full or suffix-only — survives the crash.
//
//	recovery.snapshot-promote-post-rename-pre-fsync
//	               — builds the interrupted-publish on-disk state (a
//	                 stranded snapshot.bak with the live snapshot name
//	                 absent) and then runs recovery, which crashes AFTER
//	                 it renames the backup back onto the live name but
//	                 BEFORE it fsyncs the parent directory. Recovery from
//	                 the resulting artefacts must reconstruct the full
//	                 committed state — the promotion is idempotent and
//	                 crash-safe across a second crash at this point.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/crashinject"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("crashinject-helper: ")
	// run owns the deferred cleanup; main translates its return value
	// into an exit code via os.Exit only after run's defers have all
	// fired. This avoids the exitAfterDefer pitfall where os.Exit inside
	// run would silently skip the temp-dir RemoveAll.
	os.Exit(run())
}

// run executes the requested crash-injection scenario and returns a
// process exit code. Any deferred cleanup registered here runs before
// the caller invokes os.Exit.
func run() int {
	scenario := os.Getenv(crashinject.EnvCrashAt)
	if scenario == "" {
		fmt.Fprintln(os.Stderr, "crashinject-helper: GOGRAPH_CRASH_AT is not set; refusing to run")
		return 1
	}

	dir := os.Getenv(crashinject.EnvCrashDir)
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "crashinject-*")
		if err != nil {
			log.Printf("MkdirTemp: %v", err)
			return 1
		}
		// Clean up when the helper exits normally (non-crash path).
		// dir originates from os.MkdirTemp ("" prefix forces $TMPDIR), so
		// the path is process-local and not user-tainted; gosec G703
		// otherwise flags every os.RemoveAll(variable) call.
		defer func() { _ = os.RemoveAll(dir) }() //nolint:gosec // G703: dir is from MkdirTemp, not user input
	}

	switch scenario {
	case "wal.mid-frame":
		runWALMidFrame(dir)
	case "checkpoint.p2-snapshot-published-pre-truncate":
		runCheckpointCrash(dir, scenario)
	case "checkpoint.truncprefix.tmp-written-pre-rename",
		"checkpoint.truncprefix.post-rename-pre-dirfsync",
		"checkpoint.truncprefix.post-rename-pre-bookkeeping":
		runCheckpointPrefixCrash(dir, scenario)
	case "recovery.snapshot-promote-post-rename-pre-fsync":
		runRecoveryPromoteCrash(dir)
	default:
		fmt.Fprintf(os.Stderr, "crashinject-helper: unknown scenario %q\n", scenario)
		return 1
	}
	return 0
}

// checkpointSeedEdges is the deterministic int64-keyed workload the
// checkpoint crash scenarios commit before the checkpoint fires. The
// parent test reconstructs the same expectations to assert no data
// loss after recovery.
var checkpointSeedEdges = []struct {
	src, dst int64
	weight   int64
}{
	{1, 2, 100},
	{2, 3, 200},
	{3, 1, 300},
}

// runCheckpointCrash commits an int64-keyed workload through a typed
// Store, then drives a non-blocking codec-aware checkpoint that crashes at
// checkpoint.p2-snapshot-published-pre-truncate: after the self-sufficient
// snapshot is published and durable but before the WAL prefix is truncated.
//
// It relies on WithMapperCodec so the snapshot carries mapper.bin for the
// int64 key type and is therefore self-sufficient on load. The artefacts
// (snapshot/ + the still-intact full WAL) are left in GOGRAPH_CRASH_DIR for
// the parent to recover from.
func runCheckpointCrash(dir, scenario string) {
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		log.Fatalf("wal.Open: %v", err)
	}

	g := lpg.New[int64, int64](adjlist.Config{Directed: true})
	opts := txn.Options[int64, int64]{
		Codec:       txn.NewInt64Codec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	store := txn.NewStoreWithOptions[int64, int64](g, w, opts)

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
		log.Fatalf("Commit: %v", err)
	}

	// Codec-aware checkpointer: the int64 mapper is persisted, so the
	// snapshot is self-sufficient and the checkpointer will attempt to
	// truncate the WAL — exactly the path the two breakpoints sit on.
	var mu sync.Mutex
	cp := checkpoint.New[int64, int64](
		checkpoint.Config{Dir: dir},
		g, w, &mu,
		checkpoint.WithMapperCodec[int64, int64](store.Codec()),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cp.Start(ctx)

	// Trigger blocks until the checkpoint completes — but the breakpoint
	// (GOGRAPH_CRASH_AT=scenario) self-kills the process mid-checkpoint,
	// so under the crash harness this call never returns. On the
	// non-crash self-test path we shut the goroutine down cleanly and
	// release the context before reporting; cancel() is invoked
	// explicitly (no defer) so the gocritic exitAfterDefer pitfall the
	// rest of this file guards against cannot arise.
	err = cp.Trigger()
	cp.Stop()
	cancel()
	if err != nil {
		log.Fatalf("checkpoint Trigger: %v", err)
	}

	// Reached only on the non-crash self-test path
	// (GOGRAPH_CRASH_AT != scenario).
	fmt.Printf("runCheckpointCrash: completed without crash (GOGRAPH_CRASH_AT != %s)\n", scenario)
}

// checkpointPostEdge is the extra edge runCheckpointPrefixCrash commits before
// the crashing checkpoint, so the recovered graph must carry it in addition to
// the seed for the durability assertion to pass.
var checkpointPostEdge = struct{ src, dst, weight int64 }{3, 4, 400}

// runCheckpointPrefixCrash exercises a crash inside the WAL prefix-truncate
// (wal.Writer.TruncatePrefix) of a non-blocking checkpoint. It commits the seed
// workload plus one more "post" edge (3->4), then triggers a single checkpoint
// whose prefix-truncate crashes at the named breakpoint inside the atomic
// copy-then-rename (tmp-written-pre-rename, post-rename-pre-dirfsync, or
// post-rename-pre-bookkeeping).
//
// At every one of those crash points recovery must reconstruct the FULL
// committed state — seed plus the post edge — from the self-sufficient snapshot
// plus whichever WAL survives (the original full WAL before the rename, or the
// suffix-only WAL after it). The non-empty-suffix-PRESERVATION property of
// TruncatePrefix itself is proven separately and deterministically by the
// store/wal unit test TestTruncatePrefix_PreservesSuffix; here the focus is the
// crash-atomicity of the rename at every interleaving (no committed transaction
// is ever lost). The artefacts are left in GOGRAPH_CRASH_DIR for the parent.
func runCheckpointPrefixCrash(dir, scenario string) {
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		log.Fatalf("wal.Open: %v", err)
	}

	g := lpg.New[int64, int64](adjlist.Config{Directed: true})
	opts := txn.Options[int64, int64]{
		Codec:       txn.NewInt64Codec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	store := txn.NewStoreWithOptions[int64, int64](g, w, opts)

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

	// Commit the post edge before the checkpoint so it is definitely durable
	// and applied; the single checkpoint folds seed+post into the snapshot and
	// prefix-truncates. (The breakpoint fires on THIS — the only — checkpoint.)
	txPost := store.Begin()
	if err := txPost.AddEdge(checkpointPostEdge.src, checkpointPostEdge.dst, checkpointPostEdge.weight); err != nil {
		log.Fatalf("AddEdge(post %d->%d): %v", checkpointPostEdge.src, checkpointPostEdge.dst, err)
	}
	if err := txPost.Commit(); err != nil {
		log.Fatalf("Commit(post): %v", err)
	}

	var mu sync.Mutex
	cp := checkpoint.New[int64, int64](
		checkpoint.Config{Dir: dir},
		g, w, &mu,
		checkpoint.WithMapperCodec[int64, int64](store.Codec()),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cp.Start(ctx)

	// The checkpoint's prefix-truncate crashes at the named breakpoint inside
	// wal.Writer.TruncatePrefix. Under the crash harness this never returns.
	err = cp.Trigger()
	cp.Stop()
	cancel()
	if err != nil {
		log.Fatalf("checkpoint Trigger: %v", err)
	}

	// Reached only on the non-crash self-test path.
	fmt.Printf("runCheckpointPrefixCrash: completed without crash (GOGRAPH_CRASH_AT != %s)\n", scenario)
}

// runRecoveryPromoteCrash builds the interrupted-publish on-disk state
// and then drives recovery.Open, which crashes at the
// recovery.snapshot-promote-post-rename-pre-fsync breakpoint: AFTER the
// stranded snapshot backup (snapshot.bak) has been renamed back onto the
// live snapshot name but BEFORE recovery fsyncs the parent directory to
// make that rename durable.
//
// The setup mirrors runCheckpointCrash so the snapshot is self-sufficient
// (WithMapperCodec persists the int64 mapper): it commits the seed
// workload, checkpoints it (the WAL prefix is truncated, so the seed data
// then lives ONLY in the snapshot), commits one WAL-only "post" edge,
// closes the WAL, then stages the crash window by archiving the live
// snapshot to snapshot.bak with the live name absent and a stale staging
// directory stranded — exactly the state recovery's interrupted-publish
// repair promotes from.
//
// On recovery the promotion rename runs, the breakpoint SIGKILLs the
// process, and the artefacts are left in GOGRAPH_CRASH_DIR. The parent
// test re-runs recovery over them and asserts every committed transaction
// (checkpointed seed + WAL-only post) survives — recovery is idempotent
// and crash-safe across the promotion point, the second-crash-during-
// recovery property the parent-dir fsync hardens.
func runRecoveryPromoteCrash(dir string) {
	walPath := filepath.Join(dir, "wal")
	snapDir := filepath.Join(dir, "snapshot")

	w, err := wal.Open(walPath)
	if err != nil {
		log.Fatalf("wal.Open: %v", err)
	}

	g := lpg.New[int64, int64](adjlist.Config{Directed: true})
	opts := txn.Options[int64, int64]{
		Codec:       txn.NewInt64Codec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	store := txn.NewStoreWithOptions[int64, int64](g, w, opts)

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

	// Checkpoint: self-sufficient snapshot written, WAL truncated. The seed
	// workload now lives ONLY in snapshot/.
	var mu sync.Mutex
	cp := checkpoint.New[int64, int64](
		checkpoint.Config{Dir: dir},
		g, w, &mu,
		checkpoint.WithMapperCodec[int64, int64](store.Codec()),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cp.Start(ctx)
	if err := cp.Trigger(); err != nil {
		cp.Stop()
		cancel()
		log.Fatalf("checkpoint Trigger: %v", err)
	}
	cp.Stop()
	cancel()

	// One WAL-only "post" edge committed after the checkpoint.
	txPost := store.Begin()
	if err := txPost.AddEdge(3, 4, 400); err != nil {
		log.Fatalf("AddEdge(post 3->4): %v", err)
	}
	if err := txPost.Commit(); err != nil {
		log.Fatalf("Commit(post): %v", err)
	}
	if err := w.Close(); err != nil {
		log.Fatalf("wal.Close: %v", err)
	}

	// Stage the interrupted-publish crash window: live snapshot archived to
	// snapshot.bak, live name absent, stale staging directory stranded.
	//nolint:gosec // G703: snapDir derives from GOGRAPH_CRASH_DIR (the crash harness) or MkdirTemp, not user input; this is a test-only helper binary.
	if err := os.Rename(snapDir, snapDir+".bak"); err != nil {
		log.Fatalf("stage crash: rename live snapshot to backup: %v", err)
	}
	//nolint:gosec // G703: snapDir derives from GOGRAPH_CRASH_DIR (the crash harness) or MkdirTemp, not user input; this is a test-only helper binary.
	if err := os.MkdirAll(snapDir+".tmp", 0o750); err != nil {
		log.Fatalf("stage crash: create stale staging dir: %v", err)
	}

	// Recovery promotes the backup and, at the breakpoint between the
	// promotion rename and the parent-dir fsync, SIGKILLs the process under
	// the crash harness. On the non-crash self-test path it returns
	// normally.
	if _, err := recovery.Open[int64, int64](dir, recovery.OptionsFromTxn(opts)); err != nil {
		log.Fatalf("recovery.Open: %v", err)
	}

	// Reached only on the non-crash self-test path
	// (GOGRAPH_CRASH_AT != recovery.snapshot-promote-post-rename-pre-fsync).
	fmt.Println("runRecoveryPromoteCrash: completed without crash")
}

// runWALMidFrame writes one complete WAL frame to a file in dir,
// then appends a 10-byte partial frame header (magic + version +
// length, without CRC or payload) to leave the WAL in a torn state,
// and finally calls [crashinject.Breakpoint]("wal.mid-frame") to
// self-kill via SIGKILL.
//
// The resulting file path is dir/crash.wal. A wal.Reader opened on
// that file must:
//   - Decode exactly one complete frame.
//   - Return ErrTornFrame (or ErrCRCMismatch) on the partial second frame.
func runWALMidFrame(dir string) {
	walPath := filepath.Join(dir, "crash.wal")

	// Write one complete frame via the WAL writer.
	w, err := wal.Open(walPath)
	if err != nil {
		log.Fatalf("wal.Open: %v", err)
	}
	if err := w.Append(bytes.Repeat([]byte{0xAA}, 100)); err != nil {
		log.Fatalf("Append frame1: %v", err)
	}
	if err := w.Sync(); err != nil {
		log.Fatalf("Sync frame1: %v", err)
	}
	if err := w.Close(); err != nil {
		log.Fatalf("Close writer: %v", err)
	}

	// Append a partial second-frame header:
	//   magic (4B) + version (2B) + length (4B) = 10 bytes
	// The CRC field (4B) and the 100-byte payload are missing, so
	// the WAL reader will surface ErrTornFrame when it tries to read
	// the remaining 104 bytes.
	f, err := os.OpenFile(walPath, os.O_RDWR|os.O_APPEND, 0o644) //nolint:gosec
	if err != nil {
		log.Fatalf("open WAL for partial write: %v", err)
	}
	partial := make([]byte, 10)
	copy(partial[0:4], wal.Magic[:])                  // magic
	binary.LittleEndian.PutUint16(partial[4:6], 1)    // version = 1
	binary.LittleEndian.PutUint32(partial[6:10], 100) // payload length = 100
	if _, err := f.Write(partial); err != nil {
		_ = f.Close()
		log.Fatalf("write partial header: %v", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		log.Fatalf("sync partial header: %v", err)
	}
	_ = f.Close()

	// Crash here — SIGKILL will be delivered immediately.
	crashinject.Breakpoint("wal.mid-frame")

	// Reached only when GOGRAPH_CRASH_AT != "wal.mid-frame"
	// (non-crash self-test path).
	fmt.Println("runWALMidFrame: completed without crash (GOGRAPH_CRASH_AT != wal.mid-frame)")
}
