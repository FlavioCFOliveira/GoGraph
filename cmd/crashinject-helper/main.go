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
//	constraint.drop.post-wal-sync
//	               — commits a durable CREATE CONSTRAINT (UNIQUE) frame plus a
//	                 node, then commits a durable DROP CONSTRAINT frame, fsyncs
//	                 the WAL, and crashes AFTER the drop frame is durable.
//	                 Recovery over the resulting WAL must yield an EMPTY
//	                 constraint set — the constraint and its backing index gone
//	                 together (both-absent), since recovery reconstructs the
//	                 backing index from the constraint set in one frame, never
//	                 leaving a torn "constraint gone, index lingering" state.
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
//
//	wal.appendrun.frame-emitted
//	wal.sync.pre-datasync
//	               — drives MANY CONCURRENT WRITERS through the durable
//	                 store API and crashes inside the WAL commit path while
//	                 several transactions are genuinely in flight: mid-append
//	                 of one transaction's frame run, or mid-fsync of a group
//	                 commit. Every acknowledged commit is announced on stdout
//	                 before the crash, so the parent has an oracle for what
//	                 MUST have survived. See [runConcurrentWriters].
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
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

	// A WORKLOAD OVERRIDE, checked before the breakpoint switch (rmp #2310).
	//
	// Every other scenario here is selected by its breakpoint name, which works
	// because each breakpoint had exactly one workload worth driving it through.
	// That stopped being true when the checkpoint capture became concurrent: the
	// interesting new question is what the SAME crash point does when transactions
	// are committing throughout the checkpoint, and there is no second breakpoint to
	// name it with. Selecting the workload separately from the crash point is the
	// honest way to express that, and it keeps the breakpoint name meaning one place
	// in the code rather than one place-and-a-workload.
	if w := os.Getenv(envWorkload); w != "" {
		switch w {
		case workloadCheckpointConcurrent:
			runConcurrentCheckpointCrash(dir, scenario)
			return 0
		default:
			fmt.Fprintf(os.Stderr, "crashinject-helper: unknown workload %q\n", w)
			return 1
		}
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
	case "constraint.drop.post-wal-sync":
		runConstraintDropCrash(dir)
	case "edgehandle.setprop.post-wal-sync",
		"edgehandle.delprop.post-wal-sync":
		runEdgeHandlePropCrash(dir, scenario)
	case "edgehandle.delete.post-wal-sync":
		runEdgeHandleDeleteCrash(dir)
	case "wal.appendrun.frame-emitted",
		"wal.sync.pre-datasync":
		runConcurrentWriters(dir, scenario)
	case "mvcc.commit.post-fsync-pre-publish":
		runMVCCCommitCrash(dir, scenario)
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

// constraintDropLabel/Property/Name identify the UNIQUE constraint the
// drop-crash scenario creates and then drops. The parent test reconstructs
// these to assert the recovered constraint set is EMPTY (both the constraint
// and its backing index gone).
const (
	constraintDropLabel    = "Acct"
	constraintDropProperty = "email"
	constraintDropName     = "acct_email"
)

// runConstraintDropCrash exercises the crash-atomicity of DROP CONSTRAINT
// by-name (#1556). It commits a durable CREATE CONSTRAINT (UNIQUE) frame plus a
// node, then commits a durable DROP CONSTRAINT frame, fsyncs the WAL, and
// crashes at constraint.drop.post-wal-sync — AFTER the drop frame is durable.
//
// The constraint removal and its UNIQUE backing-index removal are a single WAL
// frame: recovery reconstructs the backing index FROM the recovered constraint
// set (never from a separate index record), so dropping the constraint record
// drops the index with it. There is therefore no torn intermediate where the
// constraint is gone but the index lingers (or vice-versa). With the drop frame
// durable the recovered constraint set must be EMPTY — the "both-absent" arm of
// the both-or-neither guarantee. The complementary "both-present" arm (a crash
// BEFORE the drop frame) is proven by the parent test recovering the same WAL
// truncated to the pre-drop length.
//
// The artefacts (the WAL carrying the CREATE+node+DROP frames) are left in
// GOGRAPH_CRASH_DIR for the parent to recover from.
func runConstraintDropCrash(dir string) {
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		log.Fatalf("wal.Open: %v", err)
	}

	g := lpg.New[string, float64](adjlist.Config{})
	store := txn.NewStoreWithCodec[string, float64](g, w, txn.NewStringCodec())

	// CREATE CONSTRAINT (UNIQUE) + a node carrying the constrained value.
	txCreate := store.Begin()
	if cerr := txCreate.CreateConstraint(txn.ConstraintUnique, constraintDropLabel, constraintDropProperty, constraintDropName); cerr != nil {
		log.Fatalf("CreateConstraint: %v", cerr)
	}
	if cerr := txCreate.AddNode("n1"); cerr != nil {
		log.Fatalf("AddNode: %v", cerr)
	}
	if cerr := txCreate.SetNodeLabel("n1", constraintDropLabel); cerr != nil {
		log.Fatalf("SetNodeLabel: %v", cerr)
	}
	if cerr := txCreate.SetNodeProperty("n1", constraintDropProperty, lpg.StringValue("a@x")); cerr != nil {
		log.Fatalf("SetNodeProperty: %v", cerr)
	}
	if cerr := txCreate.Commit(); cerr != nil {
		log.Fatalf("Commit(create): %v", cerr)
	}

	// DROP CONSTRAINT — one durable WAL frame, fsynced.
	txDrop := store.Begin()
	if derr := txDrop.DropConstraint(txn.ConstraintUnique, constraintDropLabel, constraintDropProperty, constraintDropName); derr != nil {
		log.Fatalf("DropConstraint: %v", derr)
	}
	if derr := txDrop.Commit(); derr != nil {
		log.Fatalf("Commit(drop): %v", derr)
	}
	if serr := w.Sync(); serr != nil {
		log.Fatalf("Sync: %v", serr)
	}

	// Crash here — the drop frame is durable. SIGKILL delivered immediately
	// under the crash harness.
	crashinject.Breakpoint("constraint.drop.post-wal-sync")

	// Reached only on the non-crash self-test path.
	if cerr := w.Close(); cerr != nil {
		log.Fatalf("wal.Close: %v", cerr)
	}
	fmt.Println("runConstraintDropCrash: completed without crash (GOGRAPH_CRASH_AT != constraint.drop.post-wal-sync)")
}

// Edge-handle property crash scenario constants. The parent test reconstructs
// these to assert the recovered per-instance (by-handle) property state.
const (
	edgeHandleSrcKey = "src"
	edgeHandleDstKey = "dst"
	edgeHandleH1     = uint64(1) // first parallel edge's stable handle
	edgeHandleH2     = uint64(2) // sibling parallel edge's stable handle
)

// runEdgeHandlePropCrash exercises the crash-durability of the per-instance
// (by-handle) edge-property store maintained on a relationship SET/REMOVE
// (#1686). It commits, through the typed Store/Tx API, two parallel edges
// between the same ordered (src, dst) pair — each carrying its own stable
// handle and a distinct CREATE-time `w` property — then:
//
//   - edgehandle.setprop.post-wal-sync: commits a durable
//     OpSetEdgePropertyByHandle that sets tag='set' on the FIRST handle only,
//     fsyncs, and crashes. Recovery must show tag on handle 1 only, the sibling
//     untouched, exactly two parallel edges (no doubling), and the handle
//     high-water re-seeded so no post-recovery AddEdgeH re-mints a live handle.
//
//   - edgehandle.delprop.post-wal-sync: seeds tag='seed' on the FIRST handle at
//     CREATE, then commits a durable OpDelEdgePropertyByHandle removing tag from
//     that handle, fsyncs, and crashes. Recovery must show tag absent on handle
//     1 and the sibling's own state intact.
//
// The artefacts (the WAL carrying the durable frames) are left in
// GOGRAPH_CRASH_DIR for the parent to recover from.
func runEdgeHandlePropCrash(dir, scenario string) {
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		log.Fatalf("wal.Open: %v", err)
	}

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	delScenario := scenario == "edgehandle.delprop.post-wal-sync"

	// Tx 1 — build the two parallel edges, each with its own handle and a
	// distinct per-instance `w`. For the DEL scenario, also seed tag on h1 so
	// the later durable DEL has something to remove.
	tx := store.Begin()
	if e := tx.AddEdgeWithHandle(edgeHandleSrcKey, edgeHandleDstKey, 1, edgeHandleH1); e != nil {
		log.Fatalf("AddEdgeWithHandle h1: %v", e)
	}
	if e := tx.SetEdgePropertyByHandle(edgeHandleSrcKey, edgeHandleDstKey, edgeHandleH1, "w", lpg.Int64Value(1)); e != nil {
		log.Fatalf("SetEdgePropertyByHandle h1 w: %v", e)
	}
	if e := tx.AddEdgeWithHandle(edgeHandleSrcKey, edgeHandleDstKey, 1, edgeHandleH2); e != nil {
		log.Fatalf("AddEdgeWithHandle h2: %v", e)
	}
	if e := tx.SetEdgePropertyByHandle(edgeHandleSrcKey, edgeHandleDstKey, edgeHandleH2, "w", lpg.Int64Value(2)); e != nil {
		log.Fatalf("SetEdgePropertyByHandle h2 w: %v", e)
	}
	if delScenario {
		if e := tx.SetEdgePropertyByHandle(edgeHandleSrcKey, edgeHandleDstKey, edgeHandleH1, "tag", lpg.StringValue("seed")); e != nil {
			log.Fatalf("SetEdgePropertyByHandle h1 tag(seed): %v", e)
		}
	}
	if e := tx.Commit(); e != nil {
		log.Fatalf("Commit(build): %v", e)
	}

	// Tx 2 — the durable per-instance mutation under test on h1 only.
	tx2 := store.Begin()
	if delScenario {
		if e := tx2.DelEdgePropertyByHandle(edgeHandleSrcKey, edgeHandleDstKey, edgeHandleH1, "tag"); e != nil {
			log.Fatalf("DelEdgePropertyByHandle h1 tag: %v", e)
		}
	} else {
		if e := tx2.SetEdgePropertyByHandle(edgeHandleSrcKey, edgeHandleDstKey, edgeHandleH1, "tag", lpg.StringValue("set")); e != nil {
			log.Fatalf("SetEdgePropertyByHandle h1 tag(set): %v", e)
		}
	}
	if e := tx2.Commit(); e != nil {
		log.Fatalf("Commit(mutate): %v", e)
	}
	if e := w.Sync(); e != nil {
		log.Fatalf("Sync: %v", e)
	}

	// Crash here — the mutation frame is durable. SIGKILL delivered immediately
	// under the crash harness.
	crashinject.Breakpoint(scenario)

	// Reached only on the non-crash self-test path.
	if cerr := w.Close(); cerr != nil {
		log.Fatalf("wal.Close: %v", cerr)
	}
	fmt.Printf("runEdgeHandlePropCrash: completed without crash (GOGRAPH_CRASH_AT != %s)\n", scenario)
}

// runEdgeHandleDeleteCrash exercises the crash-durability of an instance-precise
// parallel-edge DELETE (rmp #2018). It commits, through the typed Store/Tx API,
// two parallel edges between the same ordered (src, dst) pair — each carrying
// its own stable handle and a distinct per-instance `w` property — then commits
// a durable OpRemoveEdgeByHandle that retires the SECOND handle (h2) only,
// fsyncs, and crashes.
//
// Recovery over the resulting WAL must land on exactly ONE parallel edge — the
// FIRST handle (h1) with its own w=1 intact — and the removed instance (h2)
// gone, proving the durable removal targeted the EXACT bound instance (not the
// first-match slot) and survives kill -9. The artefacts are left in
// GOGRAPH_CRASH_DIR for the parent to recover from.
func runEdgeHandleDeleteCrash(dir string) {
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		log.Fatalf("wal.Open: %v", err)
	}

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})

	// Tx 1 — build the two parallel edges, each with its own handle and a
	// distinct per-instance `w`.
	tx := store.Begin()
	if e := tx.AddEdgeWithHandle(edgeHandleSrcKey, edgeHandleDstKey, 1, edgeHandleH1); e != nil {
		log.Fatalf("AddEdgeWithHandle h1: %v", e)
	}
	if e := tx.SetEdgePropertyByHandle(edgeHandleSrcKey, edgeHandleDstKey, edgeHandleH1, "w", lpg.Int64Value(1)); e != nil {
		log.Fatalf("SetEdgePropertyByHandle h1 w: %v", e)
	}
	if e := tx.AddEdgeWithHandle(edgeHandleSrcKey, edgeHandleDstKey, 1, edgeHandleH2); e != nil {
		log.Fatalf("AddEdgeWithHandle h2: %v", e)
	}
	if e := tx.SetEdgePropertyByHandle(edgeHandleSrcKey, edgeHandleDstKey, edgeHandleH2, "w", lpg.Int64Value(2)); e != nil {
		log.Fatalf("SetEdgePropertyByHandle h2 w: %v", e)
	}
	if e := tx.Commit(); e != nil {
		log.Fatalf("Commit(build): %v", e)
	}

	// Tx 2 — the durable instance-precise removal under test: retire h2 only.
	tx2 := store.Begin()
	if e := tx2.RemoveEdgeByHandle(edgeHandleSrcKey, edgeHandleDstKey, edgeHandleH2); e != nil {
		log.Fatalf("RemoveEdgeByHandle h2: %v", e)
	}
	if e := tx2.Commit(); e != nil {
		log.Fatalf("Commit(delete): %v", e)
	}
	if e := w.Sync(); e != nil {
		log.Fatalf("Sync: %v", e)
	}

	// Crash here — the removal frame is durable. SIGKILL delivered immediately
	// under the crash harness.
	crashinject.Breakpoint("edgehandle.delete.post-wal-sync")

	// Reached only on the non-crash self-test path.
	if cerr := w.Close(); cerr != nil {
		log.Fatalf("wal.Close: %v", cerr)
	}
	fmt.Println("runEdgeHandleDeleteCrash: completed without crash (GOGRAPH_CRASH_AT != edgehandle.delete.post-wal-sync)")
}

// Concurrent-writer crash scenario (rmp #2302, acceptance criterion 5).
//
// The environment variables the parent uses to size the workload. All three are
// read from the environment rather than hard-coded so the PARENT is the single
// authority on the universe of transaction ids: it computes the expectation from
// the numbers it passed in, and the two sides cannot drift.
const (
	envConcWriters   = "GOGRAPH_CRASH_WRITERS"
	envConcWarmup    = "GOGRAPH_CRASH_WARMUP"
	envConcPerWriter = "GOGRAPH_CRASH_PERWRITER"
)

// ackPrefix is the token the parent scans for in the child's stdout. One line
// per ACKNOWLEDGED commit — printed only after Commit has returned nil, which
// means the transaction's frames and its OpCommit marker were fsynced.
//
// It is the DURABILITY ORACLE, and it is the reason this scenario can be
// asserted at all. With concurrent writers there is no single hand-computable
// post-crash shape: which transactions had committed when the kill landed is up
// to the scheduler. What is not up to the scheduler is the contract — anything
// acknowledged is durable — so the child states what it was promised and the
// parent holds recovery to it.
//
// One fmt.Printf is one write(2) of about a dozen bytes to the pipe the harness
// installs. POSIX guarantees writes of at most PIPE_BUF bytes are atomic, so
// concurrent writers cannot interleave halves of a line, and no mutex is needed
// here (one would perturb the very timing the scenario exists to exercise).
const ackPrefix = "ACK "

// concBase maps a transaction id to the first of the three node keys it owns.
// The stride of 10 keeps every transaction's node keys disjoint from every
// other's, which is what makes per-transaction completeness checkable
// independently after a crash: a transaction is present iff its own three nodes
// and three arcs are, and no other transaction can supply or hide them.
func concBase(id int64) int64 { return id * 10 }

// runConcurrentWriters drives `writers` goroutines committing durable
// multi-op transactions through the typed Store/Tx API — the path that releases
// the writer admission (retired in rmp #2306) after the append and fsyncs outside it, so many
// committers are inside wal.Writer.SyncGroup at once — and crashes at the named
// breakpoint inside the WAL commit path while several transactions are in
// flight.
//
// Each transaction id owns a disjoint 3-node ring and commits exactly six ops —
// three arcs with id-derived weights, one label, one property — so its WAL run is
// six op frames plus the OpCommit marker, long enough for the mid-append
// breakpoint to land strictly inside it. Every
// acknowledged commit prints an ACK line (see [ackPrefix]) BEFORE the crash, so
// the parent can hold recovery to the durability contract without having to
// predict the interleaving.
//
// A warm-up phase commits the first `warmup` ids sequentially. It is not
// decoration: combined with GOGRAPH_CRASH_AFTER it guarantees the crash lands
// after real acknowledged work, so "every acknowledged transaction survived"
// cannot pass vacuously.
func runConcurrentWriters(dir, scenario string) {
	writers := envInt(envConcWriters, 8)
	warmup := envInt(envConcWarmup, 4)
	perWriter := envInt(envConcPerWriter, 200)

	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		log.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[int64, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[int64, int64](g, w, txn.Options[int64, int64]{
		Codec:       txn.NewInt64Codec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})

	// Warm-up: ids 1..warmup, committed one at a time. These are acknowledged
	// before any concurrency starts, so they are the transactions the crash
	// MUST NOT be able to lose however it interleaves.
	for id := int64(1); id <= int64(warmup); id++ {
		commitConcTxn(store, id)
	}

	// Steady state: `writers` goroutines on disjoint id ranges. The breakpoint
	// fires somewhere in here (see GOGRAPH_CRASH_AFTER) and SIGKILLs the whole
	// process, so under the crash harness this loop never completes.
	var wg sync.WaitGroup
	for wi := 0; wi < writers; wi++ {
		wg.Add(1)
		go func(wi int) {
			defer wg.Done()
			first := int64(warmup) + 1 + int64(wi)*int64(perWriter)
			for i := int64(0); i < int64(perWriter); i++ {
				commitConcTxn(store, first+i)
			}
		}(wi)
	}
	wg.Wait()

	// Reached only on the non-crash self-test path (the countdown outlived the
	// workload, or GOGRAPH_CRASH_AT names neither breakpoint). Closing the WAL
	// here proves the workload itself is clean.
	if cerr := w.Close(); cerr != nil {
		log.Fatalf("wal.Close: %v", cerr)
	}
	fmt.Printf("runConcurrentWriters: completed without crash (%s), %d transactions\n",
		scenario, int64(warmup)+int64(writers)*int64(perWriter))
}

// commitConcTxn commits transaction `id`'s six ops and, on success, announces
// the acknowledgement on stdout. A commit error is fatal: the scenario's whole
// premise is that these commits succeed until the process is killed, so a
// rejected one is a helper bug the parent must see rather than a crash it
// should interpret.
func commitConcTxn(store *txn.Store[int64, int64], id int64) {
	base := concBase(id)
	tx := store.Begin()
	for k := int64(0); k < 3; k++ {
		src := base + k
		dst := base + (k+1)%3
		if err := tx.AddEdge(src, dst, id*100+k+1); err != nil {
			log.Fatalf("txn %d AddEdge(%d->%d): %v", id, src, dst, err)
		}
	}
	if err := tx.SetNodeLabel(base, "T"); err != nil {
		log.Fatalf("txn %d SetNodeLabel: %v", id, err)
	}
	if err := tx.SetNodeProperty(base+1, "txn", lpg.Int64Value(id)); err != nil {
		log.Fatalf("txn %d SetNodeProperty: %v", id, err)
	}
	if err := tx.Commit(); err != nil {
		log.Fatalf("txn %d Commit: %v", id, err)
	}
	// Acknowledged: frames + marker are fsynced. Announce it, unbuffered, so the
	// line reaches the parent's pipe before any later SIGKILL.
	fmt.Printf("%s%d\n", ackPrefix, id)
}

// Concurrent-CHECKPOINT crash scenario (rmp #2310, acceptance criterion 4).
const (
	// envWorkload selects the workload independently of the breakpoint name; see
	// the override in run().
	envWorkload = "GOGRAPH_CRASH_WORKLOAD"
	// workloadCheckpointConcurrent is the only value envWorkload currently takes.
	workloadCheckpointConcurrent = "checkpoint-concurrent"
)

// pairBase maps a transaction id to the first of the TWO node keys it owns. The
// stride of 2 with no gap is deliberate: every id in the space belongs to some
// transaction, so a recovered graph holding a node no transaction owns is
// detectable rather than absorbed into a gap.
func pairBase(id int64) int64 { return id * 2 }

// runConcurrentCheckpointCrash drives checkpoints and writers CONCURRENTLY and
// crashes inside a checkpoint, after its self-sufficient snapshot has been
// published and made durable but before the WAL prefix is truncated.
//
// # What it exercises that runCheckpointCrash does not
//
// runCheckpointCrash commits its whole workload, THEN checkpoints: the capture has
// no concurrent writer, so it cannot exercise the property rmp #2310 introduced.
// Since that task, phase 1 holds the commit lock only long enough to read the
// durable WAL offset and open an MVCC snapshot, and the entire image is serialised
// outside the lock while transactions commit. A crash landing in that window is the
// one that can expose a TORN image — components read at different instants — because
// the published snapshot is durable and the WAL prefix that could repair it is still
// present but about to be discarded.
//
// # The workload shape, and why it is this shape
//
// Each transaction commits exactly TWO FRESH NODES AND THE ONE EDGE BETWEEN THEM.
// That makes the structural oracle absolute and independent of the interleaving: for
// this workload a consistent graph has Order == 2*Size exactly, and any image that
// folded a transaction's endpoints without its edge — or its edge without its
// endpoints — violates it. With concurrent writers there is no hand-computable
// transaction COUNT, but there is a hand-computable SHAPE, and the shape is what a
// torn capture breaks.
//
// Every acknowledged commit prints an ACK line (see [ackPrefix]) before the crash,
// so the parent additionally holds recovery to the durability contract: every
// transaction the child was promised must be in the recovered graph, with its own
// two nodes and its own arc.
func runConcurrentCheckpointCrash(dir, scenario string) {
	writers := envInt(envConcWriters, 4)
	warmup := envInt(envConcWarmup, 8)
	perWriter := envInt(envConcPerWriter, 400)

	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		log.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[int64, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[int64, int64](g, w, txn.Options[int64, int64]{
		Codec:       txn.NewInt64Codec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})

	// Warm-up, committed one at a time and acknowledged before any concurrency or
	// any checkpoint starts. These are the transactions the crash MUST NOT lose
	// however it interleaves, and they keep "every acknowledged transaction
	// survived" from passing vacuously on a run where the crash lands early.
	for id := int64(1); id <= int64(warmup); id++ {
		commitPairTxn(store, id)
	}

	// The checkpointer is wired the production way: a codec-aware mapper (so the
	// snapshot is self-sufficient and the WAL is genuinely truncated) and the store's
	// real commit serialiser (so phase 1 takes the watermark and the MVCC instant
	// under the same drain production uses).
	var unusedMu sync.Mutex
	cp := checkpoint.New[int64, int64](
		checkpoint.Config{Dir: dir},
		g, w, &unusedMu,
		checkpoint.WithCommitSerialiser[int64, int64](store.RunUnderCommitLock),
		checkpoint.WithMapperCodec[int64, int64](store.Codec()),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cp.Start(ctx)

	// Checkpoints fire continuously alongside the writers. The breakpoint lands in
	// one of them and SIGKILLs the whole process, so under the crash harness neither
	// this goroutine nor the writers below ever finish.
	var stop atomic.Bool
	var cpWG sync.WaitGroup
	cpWG.Add(1)
	go func() {
		defer cpWG.Done()
		for !stop.Load() {
			if terr := cp.Trigger(); terr != nil {
				log.Fatalf("checkpoint Trigger: %v", terr)
			}
		}
	}()

	var wg sync.WaitGroup
	for wi := 0; wi < writers; wi++ {
		wg.Add(1)
		go func(wi int) {
			defer wg.Done()
			first := int64(warmup) + 1 + int64(wi)*int64(perWriter)
			for i := int64(0); i < int64(perWriter); i++ {
				commitPairTxn(store, first+i)
			}
		}(wi)
	}
	wg.Wait()
	stop.Store(true)
	cpWG.Wait()

	// Reached only on the non-crash self-test path (GOGRAPH_CRASH_AT names a
	// breakpoint this run never hit, or the countdown outlived the workload).
	cp.Stop()
	cancel()
	if cerr := w.Close(); cerr != nil {
		log.Fatalf("wal.Close: %v", cerr)
	}
	fmt.Printf("runConcurrentCheckpointCrash: completed without crash (%s), %d transactions\n",
		scenario, int64(warmup)+int64(writers)*int64(perWriter))
}

// commitPairTxn commits transaction `id`'s two fresh nodes and the single arc
// between them and, on success, announces the acknowledgement on stdout. A commit
// error is fatal for the same reason it is in [commitConcTxn]: the scenario's premise
// is that these commits succeed until the process is killed.
func commitPairTxn(store *txn.Store[int64, int64], id int64) {
	base := pairBase(id)
	tx := store.Begin()
	if err := tx.AddEdge(base, base+1, id); err != nil {
		log.Fatalf("txn %d AddEdge(%d->%d): %v", id, base, base+1, err)
	}
	if err := tx.Commit(); err != nil {
		log.Fatalf("txn %d Commit: %v", id, err)
	}
	// Acknowledged: frames + marker are fsynced. Announce it, unbuffered, so the
	// line reaches the parent's pipe before any later SIGKILL.
	fmt.Printf("%s%d\n", ackPrefix, id)
}

// envInt reads a positive integer from the environment, falling back to def for
// an unset, malformed, or non-positive value.
func envInt(name string, def int) int {
	v, err := strconv.Atoi(os.Getenv(name))
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// mvccCommitSeed is the workload runMVCCCommitCrash commits BEFORE the crashing
// transaction. Each entry is one autocommit Cypher transaction, so each takes its
// own MVCC commit instant and each writes its instant into its own OpCommit marker.
var mvccCommitSeed = []int64{10, 20, 30}

// mvccCommitCrashKey is the id of the node the CRASHING transaction creates. It is
// durable when the crash lands — the fsync returned before the breakpoint — but its
// commit instant was never published, so recovery must apply it anyway.
const mvccCommitCrashKey int64 = 40

// runMVCCCommitCrash commits through the CYPHER engine and crashes in the window
// between the WAL fsync and the MVCC visibility publish (rmp #2309, MVCC C3c).
//
// # Why the cypher engine and not txn.Store directly
//
// The MVCC commit timestamp only exists on the engine path. The store's own
// Tx.Commit has no clock in scope and writes commitTS 0 by design, so a scenario
// built on it would exercise the durability ordering but not the thing under test.
//
// # What the crash window is, and why it is the interesting one
//
// cypher's commitUnderBarrier fsyncs the WAL and only then lets the write bracket
// unwind, which is what publishes the commit instant. Between the two the
// transaction is DURABLE BUT INVISIBLE: its OpCommit marker carries a timestamp no
// reader ever saw. Recovery must apply the transaction (the fsync returned, so it is
// committed) AND derive a clock floor above that timestamp, so the instant is never
// minted a second time.
//
// GOGRAPH_CRASH_AFTER skips the seed transactions' hits, so the crash lands on the
// last one and the recovered graph has both a published prefix and one
// durable-but-unpublished commit to distinguish.
func runMVCCCommitCrash(dir, scenario string) {
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		log.Fatalf("wal.Open: %v", err)
	}

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	eng := cypher.NewEngineWithStore(store)
	ctx := context.Background()

	for _, id := range mvccCommitSeed {
		if _, cerr := eng.RunInTx(ctx, "CREATE (n:Acct {id: $id})",
			map[string]expr.Value{"id": expr.IntegerValue(id)}); cerr != nil {
			log.Fatalf("seed commit %d: %v", id, cerr)
		}
	}

	// The crashing transaction. Under the harness the breakpoint inside
	// commitUnderBarrier self-kills after its fsync, so this never returns.
	if _, cerr := eng.RunInTx(ctx, "CREATE (n:Acct {id: $id})",
		map[string]expr.Value{"id": expr.IntegerValue(mvccCommitCrashKey)}); cerr != nil {
		log.Fatalf("crashing commit: %v", cerr)
	}

	// Reached only on the non-crash self-test path.
	fmt.Printf("runMVCCCommitCrash: completed without crash (GOGRAPH_CRASH_AT != %s)\n", scenario)
}
