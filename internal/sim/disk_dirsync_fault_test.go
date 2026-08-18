package sim

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// This file pins the FAULT half of the SimDisk directory-fsync model (rmp
// #2537). Until it was written, [SimDisk.DirSync] ended in an unconditional
// return nil while its sibling [SimDisk.ParentDirSync] already consulted an arm:
// the two primitives had silently diverged, and the four snapshot
// directory-fsync error branches audit finding F3 names were therefore
// unreachable.
//
// Every oracle here follows the project's three-gate pattern: an UNCONDITIONAL
// verdict gate, a SEPARATE shape-only non-vacuity gate proving the fault really
// fired (read from [SimDisk.DirSyncFaultCount], never asserted), and witnesses
// through t.Logf only. Several gates additionally carry a CONTROL arm on a
// second disk, because "the file is gone after the crash" is also what a harness
// that drops everything would report.

// -----------------------------------------------------------------------------
// The primitive
// -----------------------------------------------------------------------------

// TestSimDisk_ArmDirSyncFaultForPath_OneShotSelectiveDisarmable pins the arm's
// contract, mirroring TestSimDisk_ArmParentDirSyncFaultForPath: path-selective,
// one-shot, and disarmed by an empty path.
func TestSimDisk_ArmDirSyncFaultForPath_OneShotSelectiveDisarmable(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	writeFile(t, d, "db/f", []byte("x"))
	writeFile(t, d, "other/f", []byte("x"))

	d.ArmDirSyncFaultForPath("db")

	// Verdict: a non-matching directory is untouched by the arm.
	if err := d.DirSync("other"); err != nil {
		t.Fatalf("non-matching DirSync faulted: %v", err)
	}
	// Verdict: the armed directory faults with ErrSimFault.
	if err := d.DirSync("db"); !errors.Is(err, ErrSimFault) {
		t.Fatalf("matching DirSync err=%v, want ErrSimFault", err)
	}
	// Verdict: ONE-SHOT — the arm cleared itself, so the retry succeeds.
	if err := d.DirSync("db"); err != nil {
		t.Fatalf("DirSync still faulting after the one-shot fire: %v", err)
	}
	// Verdict: an empty path disarms.
	d.ArmDirSyncFaultForPath("db")
	d.ArmDirSyncFaultForPath("")
	if err := d.DirSync("db"); err != nil {
		t.Fatalf("DirSync faulted after ArmDirSyncFaultForPath(\"\") disarmed it: %v", err)
	}

	// Non-vacuity (shape only): exactly ONE fault fired across the five calls
	// above. A DirSync that can never fail satisfies every "did not fault" gate
	// here, so without this count the test would report success for the exact
	// defect it exists to catch.
	if got := d.DirSyncFaultCount(); got != 1 {
		t.Fatalf("DirSyncFaultCount=%d, want exactly 1: the arm never bit (0) or bit more than once (>1)", got)
	}
	t.Logf("witness: 5 directory fsyncs issued, 1 faulted, arm disarmed by both the one-shot fire and the empty path")
}

// TestSimDisk_DirSyncFault_MakesNoDirentDurable is the durability verdict: a
// directory fsync that FAILED must leave every dirent in that directory
// non-durable, so a host crash drops the file. POSIX states that on fsync
// failure "outstanding I/O operations are not guaranteed to have been
// completed"; the harness must therefore assume nothing reached the platter.
func TestSimDisk_DirSyncFault_MakesNoDirentDurable(t *testing.T) {
	d := NewSimDisk(NewSeed(2), 0)
	writeFile(t, d, "db/f", []byte("hello"))
	// One non-matching directory fsync first, so the fire count below can tell
	// "one fsync faulted" from "one fsync was issued".
	if err := d.DirSync("other"); err != nil {
		t.Fatalf("unarmed DirSync of another directory: %v", err)
	}
	d.ArmDirSyncFaultForPath("db")

	if err := d.DirSync("db"); !errors.Is(err, ErrSimFault) {
		t.Fatalf("armed DirSync err=%v, want ErrSimFault", err)
	}
	d.CrashHost()

	// Verdict: the name never became durable, so the crash took it.
	if d.Exists("db/f") {
		t.Fatal("a file whose only directory fsync FAILED survived a host crash: the failed fsync durabilised the dirent anyway")
	}
	// Non-vacuity (shape only): the fault fired.
	if got := d.DirSyncFaultCount(); got != 1 {
		t.Fatalf("DirSyncFaultCount=%d, want 1: the verdict above passed without the fault ever firing", got)
	}

	// Control, on a second disk: the SAME sequence with a SUCCESSFUL DirSync
	// keeps the file. Without it, a harness that dropped every file on every
	// crash would pass the verdict gate.
	c := NewSimDisk(NewSeed(2), 0)
	writeFile(t, c, "db/f", []byte("hello"))
	if err := c.DirSync("db"); err != nil {
		t.Fatalf("control DirSync: %v", err)
	}
	c.CrashHost()
	if !c.Exists("db/f") {
		t.Fatal("control: a SUCCESSFUL directory fsync did not make the dirent durable, so the verdict gate above proves nothing")
	}
	t.Logf("witness: failed fsync -> file dropped by the crash; successful fsync -> file survived")
}

// TestSimDisk_DirSyncFault_LeavesRenameUndoPending pins the composition with the
// rename undo log (rmp #2514): pruning the log is the act of declaring a rename
// durable, so a directory fsync that FAILED must not prune it. Were it pruned, a
// later crash would be obliged to KEEP a publish rename whose staging tree was
// never made durable — the precise state the snapshot writer's staging-fsync
// branch exists to prevent.
func TestSimDisk_DirSyncFault_LeavesRenameUndoPending(t *testing.T) {
	d := NewSimDisk(NewSeed(3), 0)
	writeFile(t, d, "db/snapshot.tmp/manifest.json", []byte("m"))
	if err := d.DirSync("db/snapshot.tmp"); err != nil {
		t.Fatalf("staging DirSync: %v", err)
	}
	if err := d.Rename("db/snapshot.tmp", "db/snapshot"); err != nil {
		t.Fatalf("publish rename: %v", err)
	}
	if got := d.PendingRenameCount(); got != 1 {
		t.Fatalf("PendingRenameCount=%d after the publish rename, want 1 (precondition)", got)
	}

	d.ArmDirSyncFaultForPath("db")
	if err := d.ParentDirSync("db/snapshot"); !errors.Is(err, ErrSimFault) {
		t.Fatalf("armed parent fsync err=%v, want ErrSimFault", err)
	}

	// Verdict: the undo record survives a FAILED fsync.
	if got := d.PendingRenameCount(); got != 1 {
		t.Fatalf("PendingRenameCount=%d after a FAILED parent fsync, want 1: the failed fsync pruned the undo log, so a crash can no longer roll the publish back", got)
	}
	// Non-vacuity (shape only): the fault fired.
	if got := d.DirSyncFaultCount(); got != 1 {
		t.Fatalf("DirSyncFaultCount=%d, want 1: the verdict above passed without the fault ever firing", got)
	}
	// Control: the SUCCESS path really does prune, so "still 1" is a decision the
	// failed fsync took and not simply what the log always holds.
	if err := d.ParentDirSync("db/snapshot"); err != nil {
		t.Fatalf("control parent fsync: %v", err)
	}
	if got := d.PendingRenameCount(); got != 0 {
		t.Fatalf("control: a SUCCESSFUL parent fsync left PendingRenameCount=%d, want 0 — the pruning this gate measures never happens, so the verdict proves nothing", got)
	}
	t.Logf("witness: failed parent fsync kept 1 pending rename; the successful retry pruned it to 0")
}

// TestSimDisk_DirSyncFault_DurableDataUnreachableWithoutName pins the
// composition with the per-file durable shadow (rmp #2535). A directory fsync
// never advances a file's durable watermark — on either path, because fsync(2)
// on a directory hardens metadata and not contents — so a failed one leaves a
// component holding durable DATA under a name that is not durable. The host
// crash then deletes it outright, which is what makes the staging-fsync gate
// meaningful: blocks on the platter no directory entry points at are lost.
func TestSimDisk_DirSyncFault_DurableDataUnreachableWithoutName(t *testing.T) {
	d := NewSimDisk(NewSeed(4), 0)
	writeFile(t, d, "db/f", []byte("payload")) // Write + Sync: the DATA is durable.

	before, ok := d.DurableSize("db/f")
	if !ok || before != int64(len("payload")) {
		t.Fatalf("DurableSize before = (%d, %v), want (%d, true) (precondition)", before, ok, len("payload"))
	}

	// One non-matching directory fsync first, so the fire count below can tell
	// "one fsync faulted" from "one fsync was issued".
	if err := d.DirSync("other"); err != nil {
		t.Fatalf("unarmed DirSync of another directory: %v", err)
	}
	d.ArmDirSyncFaultForPath("db")
	if err := d.DirSync("db"); !errors.Is(err, ErrSimFault) {
		t.Fatalf("armed DirSync err=%v, want ErrSimFault", err)
	}

	// Verdict 1: the failed directory fsync moved no file's durable watermark.
	after, ok := d.DurableSize("db/f")
	if !ok || after != before {
		t.Fatalf("DurableSize after the failed dir fsync = (%d, %v), want (%d, true): a directory fsync must not touch file data durability", after, ok, before)
	}
	// Verdict 2: durable data under a non-durable name is unreachable after a
	// host crash.
	d.CrashHost()
	if d.Exists("db/f") {
		t.Fatal("a file with durable data but a never-durable name survived a host crash")
	}
	// Non-vacuity (shape only): the fault fired.
	if got := d.DirSyncFaultCount(); got != 1 {
		t.Fatalf("DirSyncFaultCount=%d, want 1: the verdicts above passed without the fault ever firing", got)
	}
	t.Logf("witness: durable bytes before=%d after=%d, name never durable, file dropped by the crash", before, after)
}

// TestSimDisk_DirSyncFault_SharedByBothEntryPoints is the anti-drift gate. The
// two entry points are ONE primitive behind one body, so a directory-keyed arm
// also fires for a ParentDirSync of a child of that directory (that call IS an
// fsync of the directory), both feed the SAME fire count, and the child-keyed
// arm keeps its exact key so a scenario can still target one specific fsync
// among several of the same parent directory.
func TestSimDisk_DirSyncFault_SharedByBothEntryPoints(t *testing.T) {
	// (a) The directory-keyed arm reaches the ParentDirSync entry point.
	d := NewSimDisk(NewSeed(5), 0)
	writeFile(t, d, "db/wal", []byte("w"))
	d.ArmDirSyncFaultForPath("db")
	if err := d.ParentDirSync("db/wal"); !errors.Is(err, ErrSimFault) {
		t.Fatalf("ParentDirSync under a DIRECTORY-keyed arm err=%v, want ErrSimFault: the two entry points no longer share one body", err)
	}
	if got := d.DirSyncFaultCount(); got != 1 {
		t.Fatalf("DirSyncFaultCount=%d after the directory-keyed arm fired through ParentDirSync, want 1", got)
	}

	// (b) The child-keyed arm keeps its exact key: neither a plain DirSync of the
	// parent nor a ParentDirSync of a DIFFERENT child consumes it.
	e := NewSimDisk(NewSeed(5), 0)
	writeFile(t, e, "db/wal", []byte("w"))
	writeFile(t, e, "db/snapshot", []byte("s"))
	e.ArmParentDirSyncFaultForPath("db/wal")
	if err := e.DirSync("db"); err != nil {
		t.Fatalf("a plain DirSync consumed the CHILD-keyed arm: %v", err)
	}
	if err := e.ParentDirSync("db/snapshot"); err != nil {
		t.Fatalf("a ParentDirSync of a different child consumed the CHILD-keyed arm: %v", err)
	}
	if err := e.ParentDirSync("db/wal"); !errors.Is(err, ErrSimFault) {
		t.Fatalf("the child-keyed arm did not fire on its own path: err=%v, want ErrSimFault", err)
	}
	// Non-vacuity (shape only): both entry points feed ONE counter.
	if got := e.DirSyncFaultCount(); got != 1 {
		t.Fatalf("DirSyncFaultCount=%d on the child-keyed disk, want exactly 1", got)
	}
	t.Logf("witness: directory-keyed arm fired through ParentDirSync; child-keyed arm survived 2 non-matching directory fsyncs and fired on its own")
}

// -----------------------------------------------------------------------------
// Reachability of the snapshot staging-fsync branches (the point of the task)
// -----------------------------------------------------------------------------

// dirSyncFaultTxns is how many single-node transactions the two full-stack
// reachability gates commit before the faulted checkpoint. Modest, so the short
// layer stays well inside its budget.
const dirSyncFaultTxns = 8

// dirSyncFaultIndexDDL registers a serialisable secondary index, so the
// checkpoint's capture carries an indexes/ component and the publish reaches
// writeCapturedIndexes — the store/snapshot/full.go:913 branch.
const dirSyncFaultIndexDDL = "CREATE INDEX dirsync_person_age FOR (n:Person) ON (n.age) OPTIONS {indexType:'btree'}"

// seedDirSyncFaultStore opens a full-stack SimStore over disk, optionally
// creates the index, and commits dirSyncFaultTxns Person transactions. It
// returns the store and the committed name set.
func seedDirSyncFaultStore(t *testing.T, disk *SimDisk, withIndex bool) (*SimStore, map[string]struct{}) {
	t.Helper()
	ctx := context.Background()
	st, err := OpenSimStore(disk, fullStackStoreConfig())
	if err != nil {
		t.Fatalf("OpenSimStore: %v", err)
	}
	if withIndex {
		adapter := NewEngineAdapter(st.Engine())
		res, derr := adapter.RunWrite(ctx, dirSyncFaultIndexDDL, nil)
		if derr != nil {
			t.Fatalf("CREATE INDEX: %v", derr)
		}
		_ = res.Close()
	}
	committed := make(map[string]struct{}, dirSyncFaultTxns)
	for i := 0; i < dirSyncFaultTxns; i++ {
		name := fmt.Sprintf("cp%d", i)
		if err := commitCreatePerson(ctx, st.Engine(), name, i); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		committed[name] = struct{}{}
	}
	return st, committed
}

// assertNothingPublished is the shared post-state verdict of both reachability
// gates: the abort left NO snapshot at the live name and NO staging tree behind.
func assertNothingPublished(t *testing.T, disk *SimDisk) {
	t.Helper()
	for _, p := range []string{
		"db/snapshot/manifest.json",
		"db/snapshot/csr.bin",
		"db/snapshot.tmp/manifest.json",
		"db/snapshot.tmp/csr.bin",
		"db/snapshot.tmp/indexes/dirsync_person_age.bin",
	} {
		if disk.Exists(p) {
			t.Fatalf("%q exists after a staging-fsync abort: the publish went ahead over an unsynced staging tree, or the staging tree was not cleaned up", p)
		}
	}
}

// assertRecoversAll reopens the store over the same durable image after a HOST
// crash and asserts recovery restores exactly the committed set — the proof that
// the aborted checkpoint truncated no WAL prefix and damaged no durable state.
func assertRecoversAll(t *testing.T, disk *SimDisk, committed map[string]struct{}) {
	t.Helper()
	ctx := context.Background()
	st, err := OpenSimStore(disk, fullStackStoreConfig())
	if err != nil {
		t.Fatalf("reopen after the aborted checkpoint: %v", err)
	}
	defer func() { _ = st.Close() }()
	recovered, partial, err := recoveredPersonNames(ctx, st.Engine())
	if err != nil {
		t.Fatalf("read recovered graph: %v", err)
	}
	if missing := setMinus(committed, recovered); len(missing) > 0 {
		t.Fatalf("committed nodes lost after the aborted checkpoint: %v (recovered=%d committed=%d)", missing, len(recovered), len(committed))
	}
	if phantom := setMinus(recovered, committed); len(phantom) > 0 {
		t.Fatalf("nodes recovered that were never committed: %v", phantom)
	}
	if len(partial) > 0 {
		t.Fatalf("torn transactions resurrected: %v", partial)
	}
}

// TestSimStore_StagingDirFsyncFault_AbortsBeforePublish drives
// store/snapshot/full.go writeCaptureCore's staging-directory fsync branch — the
// LAST durability gate before the publish renames, and the branch F3 identified
// as never having executed under simulation. The staging fsync fails; the writer
// must RemoveAll the staging tree and abort BEFORE the archive and publish
// renames, leaving nothing published and every committed transaction recoverable.
func TestSimStore_StagingDirFsyncFault_AbortsBeforePublish(t *testing.T) {
	disk := NewSimDisk(NewSeed(0xD125F00D), 0) // faultRate 0: only the armed fault fires
	st, committed := seedDirSyncFaultStore(t, disk, false)

	disk.ArmDirSyncFaultForPath("db/snapshot.tmp")
	cpErr := st.Checkpoint()

	// Verdict 1: the checkpoint fails rather than publishing an unsynced tree.
	if cpErr == nil {
		st.Crash()
		t.Fatal("the checkpoint SUCCEEDED with the staging-directory fsync faulted: an unsynced staging tree was published")
	}
	// Verdict 2: nothing published, nothing stranded.
	assertNothingPublished(t, disk)
	// Non-vacuity (shape only): the fault fired exactly once, and it fired on the
	// staging fsync rather than on some other directory fsync of the publish.
	if got := disk.DirSyncFaultCount(); got != 1 {
		t.Fatalf("DirSyncFaultCount=%d, want 1: the checkpoint failed for a reason other than the armed staging fsync", got)
	}
	if !strings.Contains(cpErr.Error(), "staging dir fsync") {
		t.Fatalf("checkpoint error = %v; want it to name the staging dir fsync — the branch under test", cpErr)
	}
	// Verdict 3: the durable state is intact — a host crash and a reopen through
	// real recovery restore exactly what was committed.
	st.Crash()
	assertRecoversAll(t, disk, committed)
	t.Logf("witness: checkpoint aborted with %q; 0 snapshots published; %d committed transactions recovered after a host crash", cpErr, len(committed))
}

// TestSimStore_IndexesDirFsyncFault_AbortsBeforePublish drives
// store/snapshot/full.go writeCapturedIndexes' indexes/ fsync branch. The
// checkpoint's capture carries a serialisable index, so the publish fsyncs
// <staging>/indexes; that fsync fails, and the writer must remove the staging
// tree and abort with nothing published.
func TestSimStore_IndexesDirFsyncFault_AbortsBeforePublish(t *testing.T) {
	disk := NewSimDisk(NewSeed(0xD125F00E), 0)
	st, committed := seedDirSyncFaultStore(t, disk, true)

	disk.ArmDirSyncFaultForPath("db/snapshot.tmp/indexes")
	cpErr := st.Checkpoint()

	// Verdict 1: the checkpoint fails.
	if cpErr == nil {
		st.Crash()
		t.Fatal("the checkpoint SUCCEEDED with the indexes/ directory fsync faulted: index dirents were published without ever being made durable")
	}
	// Verdict 2: nothing published, nothing stranded.
	assertNothingPublished(t, disk)
	// Non-vacuity (shape only): the fault fired — which is also the proof that the
	// capture really carried an index, since the writer skips the whole indexes/
	// step (and never fsyncs that directory) when it carries none.
	if got := disk.DirSyncFaultCount(); got != 1 {
		t.Fatalf("DirSyncFaultCount=%d, want 1: the indexes/ fsync was never issued, so the branch under test did not execute", got)
	}
	if !strings.Contains(cpErr.Error(), "fsync indexes dir") {
		t.Fatalf("checkpoint error = %v; want it to name the indexes dir fsync — the branch under test", cpErr)
	}
	// Verdict 3: the durable state is intact.
	st.Crash()
	assertRecoversAll(t, disk, committed)
	t.Logf("witness: checkpoint aborted with %q; 0 snapshots published; %d committed transactions recovered after a host crash", cpErr, len(committed))
}
