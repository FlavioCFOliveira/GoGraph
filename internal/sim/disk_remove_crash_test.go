package sim

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// disk_remove_crash_test.go — harness-fidelity gates for the crash semantics of
// [SimDisk.Remove] and [SimDisk.RemoveAll] (rmp #2536, audit finding F2).
//
// unlink(2) and rmdir(2) mutate a DIRECTORY. That puts a removal in the same
// durability class as the create and the rename this model already treats
// carefully: until the containing directory is fsync'd, a crash may legally leave
// the removed name in place. The simulator applied the unlink immediately and
// unconditionally, so the state "the removal did not stick" was unreachable by
// any route — measured before the fix: a file created, Sync'd and DirSync'd, then
// Removed with no subsequent parent fsync, was absent after Crash().
//
// The direction of that infidelity is the mirror of the rename defect of rmp
// #2514: too MUCH durability rather than too little. It never accused the engine
// of anything; it withheld the harder input. The publish protocol is built out of
// removals — the snapshot writer's RemoveAll of its staging tree and of the stale
// and happy-path backups, the WAL writer's Remove of its suffix temp file, and
// recovery's RemoveAll of a stale staging directory, which exists PRECISELY to
// tolerate a .tmp left behind by a previous crash — so the engine's tolerance of
// a leftover artefact was never exercised from the removal side, which is the
// side a real deployment hits. The engine-level half of that demonstration lives
// in disk_remove_tolerance_test.go.
//
// The gates below are written to the project's three-gate shape: an
// UNCONDITIONAL verdict gate that fails on an illegal outcome; a SEPARATE
// shape-only gate proving the situation actually arose; and witness detail via
// t.Logf only, because an unmet precondition is a fact to report, not a defect.

// removeCrashSeeds is the seed spread the outcome-set gates sample. The boundary
// is drawn uniformly over the log length, so with this many seeds the chance that
// a reachable branch is missed is negligible.
func removeCrashSeeds() []uint64 {
	seeds := make([]uint64, 0, 96)
	for i := uint64(0); i < 96; i++ {
		seeds = append(seeds, 0x2536_0000+i*0x9E37_79B1)
	}
	return seeds
}

// stageDurableFile writes path, fsyncs its bytes and fsyncs its parent directory,
// so that BOTH halves of its durability are on stable storage before the test
// touches it. Without the parent fsync a crash would drop the name for reasons
// that have nothing to do with the removal under test.
func stageDurableFile(t *testing.T, d *SimDisk, path string, data []byte) {
	t.Helper()
	writeFile(t, d, path, data)
	if err := d.ParentDirSync(path); err != nil {
		t.Fatalf("ParentDirSync %s: %v", path, err)
	}
}

// TestSimDiskRemoveCrash_BothOutcomesAreSampled is the VERDICT plus SHAPE gate on
// the model's core claim: a crash after an unlink but before the parent fsync can
// leave the file PRESENT, and both legal outcomes actually occur across seeds.
//
// It is the test that fails on the pre-#2536 code, where "present" was
// unreachable for every seed.
func TestSimDiskRemoveCrash_BothOutcomesAreSampled(t *testing.T) {
	const payload = "durable-bytes"
	present, absent := 0, 0
	for _, seed := range removeCrashSeeds() {
		d := NewSimDisk(NewSeed(seed), 0) // no data faults: isolate the dirent model
		stageDurableFile(t, d, "dir/f", []byte(payload))
		if err := d.Remove("dir/f"); err != nil {
			t.Fatalf("seed %#x: Remove: %v", seed, err)
		}
		// Shape: the crash really is inside the unlink window. Asserted per seed
		// and separately from the outcome, because a crash that landed after the
		// window would make the outcome below meaningless rather than wrong.
		if got := d.PendingRemoveCount(); got != 1 {
			t.Fatalf("seed %#x: PendingRemoveCount = %d, want 1: the removal was not recorded, so the crash adjudicates nothing",
				seed, got)
		}
		d.Crash()
		pending, restored := d.LastCrashRemoveOutcome()
		if pending != 1 {
			t.Fatalf("seed %#x: LastCrashRemoveOutcome pending = %d, want 1", seed, pending)
		}

		switch {
		case d.Exists("dir/f"):
			present++
			// Verdict: a restored name points at the SAME inode, so it must come
			// back with the bytes that reached stable storage — not empty, and not
			// resurrected as some other file.
			got, err := d.ReadFile("dir/f")
			if err != nil {
				t.Fatalf("seed %#x: the name came back but ReadFile failed: %v", seed, err)
			}
			if string(got) != payload {
				t.Fatalf("seed %#x: restored name holds %q, want %q: the undo restored the name but not the inode behind it",
					seed, got, payload)
			}
			if restored != 1 {
				t.Fatalf("seed %#x: the name is present but LastCrashRemoveOutcome restored = %d, want 1: the observable disagrees with the state",
					seed, restored)
			}
		default:
			absent++
			if restored != 0 {
				t.Fatalf("seed %#x: the name is absent but LastCrashRemoveOutcome restored = %d, want 0", seed, restored)
			}
		}
	}

	// Shape: both branches are really sampled. Either count at zero means the
	// model collapsed onto one outcome, which is exactly the defect this fixes
	// (before #2536, present was 0 for every seed).
	if present == 0 {
		t.Fatalf("the removal stuck on all %d seeds: a crash can never leave an unlinked name in place, which is the pre-#2536 model",
			len(removeCrashSeeds()))
	}
	if absent == 0 {
		t.Fatalf("the removal failed to stick on all %d seeds: the model no longer samples the ordinary outcome",
			len(removeCrashSeeds()))
	}
	t.Logf("unlink crash outcomes over %d seeds: restored=%d stuck=%d", len(removeCrashSeeds()), present, absent)
}

// TestSimDiskRemoveCrash_RollbackArmPinsTheRestoredBranch forces the branch the
// engine's leftover-artefact tolerance needs, and proves the arm fired rather
// than being silently ignored.
func TestSimDiskRemoveCrash_RollbackArmPinsTheRestoredBranch(t *testing.T) {
	d := NewSimDisk(NewSeed(0x2536_AA01), 0)
	stageDurableFile(t, d, "dir/f", []byte("payload"))
	d.ArmRemoveRollbackForPath("dir/f")
	if err := d.Remove("dir/f"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := d.RemoveRollbackCount(); got != 1 {
		t.Fatalf("RemoveRollbackCount = %d, want 1: the arm never matched, so this test pins nothing", got)
	}
	d.Crash()
	if !d.Exists("dir/f") {
		t.Fatal("the pinned removal still stuck: the restored branch is unreachable even when armed")
	}
	if pending, restored := d.LastCrashRemoveOutcome(); pending != 1 || restored != 1 {
		t.Fatalf("LastCrashRemoveOutcome = (%d, %d), want (1, 1)", pending, restored)
	}
}

// TestSimDiskRemoveCrash_WritebackArmPinsTheStuckBranch forces the other legal
// branch. A test that must assert "the removal stuck" unconditionally needs it
// pinned: fsyncing the parent directory would work too, but it would harden every
// other name in that directory and the assertion would no longer be about the
// removal.
func TestSimDiskRemoveCrash_WritebackArmPinsTheStuckBranch(t *testing.T) {
	d := NewSimDisk(NewSeed(0x2536_AA02), 0)
	stageDurableFile(t, d, "dir/f", []byte("payload"))
	stageDurableFile(t, d, "dir/keep", []byte("other"))
	d.ArmRemoveWritebackForPath("dir/f")
	if err := d.Remove("dir/f"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := d.RemoveWritebackCount(); got != 1 {
		t.Fatalf("RemoveWritebackCount = %d, want 1: the arm never matched", got)
	}
	// An unlink declared durable records nothing, so there is nothing for the
	// crash to adjudicate.
	if got := d.PendingRemoveCount(); got != 0 {
		t.Fatalf("PendingRemoveCount = %d, want 0: a durable unlink must not be recorded", got)
	}
	d.Crash()
	if d.Exists("dir/f") {
		t.Fatal("the write-back arm did not make the unlink durable: the name came back")
	}
	if !d.Exists("dir/keep") {
		t.Fatal("the write-back arm reached beyond the name it was armed on")
	}
}

// TestSimDiskRemoveCrash_ArmsAreOneShotAndPathKeyed proves the two counters
// separate "the primitive fired" from "the arm was ignored". An arm whose path
// never matches is a silent no-op, and a scenario that mistook one for the other
// would diagnose the resulting durable image as an engine defect.
func TestSimDiskRemoveCrash_ArmsAreOneShotAndPathKeyed(t *testing.T) {
	t.Run("wrong path never fires", func(t *testing.T) {
		d := NewSimDisk(NewSeed(0x2536_AA03), 0)
		stageDurableFile(t, d, "dir/f", []byte("payload"))
		d.ArmRemoveRollbackForPath("dir/other")
		if err := d.Remove("dir/f"); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if got := d.RemoveRollbackCount(); got != 0 {
			t.Fatalf("RemoveRollbackCount = %d, want 0: the arm fired on a path it was not keyed on", got)
		}
	})
	t.Run("one shot", func(t *testing.T) {
		d := NewSimDisk(NewSeed(0x2536_AA04), 0)
		stageDurableFile(t, d, "dir/f", []byte("one"))
		stageDurableFile(t, d, "dir/g", []byte("two"))
		d.ArmRemoveWritebackForPath("dir/f")
		if err := d.Remove("dir/f"); err != nil {
			t.Fatalf("Remove f: %v", err)
		}
		// Re-create and remove the same name again: the arm is spent.
		stageDurableFile(t, d, "dir/f", []byte("again"))
		if err := d.Remove("dir/f"); err != nil {
			t.Fatalf("Remove f again: %v", err)
		}
		if got := d.RemoveWritebackCount(); got != 1 {
			t.Fatalf("RemoveWritebackCount = %d, want 1: the arm is not one-shot", got)
		}
		if got := d.PendingRemoveCount(); got != 1 {
			t.Fatalf("PendingRemoveCount = %d, want 1: the second removal must be recorded", got)
		}
	})
	t.Run("empty path disarms", func(t *testing.T) {
		d := NewSimDisk(NewSeed(0x2536_AA05), 0)
		stageDurableFile(t, d, "dir/f", []byte("payload"))
		d.ArmRemoveRollbackForPath("dir/f")
		d.ArmRemoveRollbackForPath("")
		if err := d.Remove("dir/f"); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if got := d.RemoveRollbackCount(); got != 0 {
			t.Fatalf("RemoveRollbackCount = %d, want 0: the empty path did not disarm", got)
		}
	})
}

// TestSimDiskRemoveCrash_RemoveAllRestoresTheWholeSubtree pins the granularity: a
// RemoveAll is ONE metadata record, so a crash either keeps the whole removal or
// restores the whole subtree. Restoring half of it would model an interleaving
// os.RemoveAll's callers never see.
//
// Each restored name comes back with its OWN dirent durability, which is why the
// non-durable member below still vanishes: its name had never been
// crash-survivable, so its absence is not the removal having stuck.
func TestSimDiskRemoveCrash_RemoveAllRestoresTheWholeSubtree(t *testing.T) {
	build := func(t *testing.T, seed uint64) *SimDisk {
		t.Helper()
		d := NewSimDisk(NewSeed(seed), 0)
		writeFile(t, d, "db/stage/a", []byte("aa"))
		writeFile(t, d, "db/stage/b", []byte("bb"))
		if err := d.DirSync("db/stage"); err != nil {
			t.Fatalf("DirSync: %v", err)
		}
		// A third name created AFTER the staging fsync: durable bytes, but a
		// dirent that never reached stable storage.
		writeFile(t, d, "db/stage/c", []byte("cc"))
		return d
	}

	t.Run("restored", func(t *testing.T) {
		d := build(t, 0x2536_BB01)
		d.ArmRemoveRollbackForPath("db/stage")
		if err := d.RemoveAll("db/stage"); err != nil {
			t.Fatalf("RemoveAll: %v", err)
		}
		if got := d.RemoveRollbackCount(); got != 1 {
			t.Fatalf("RemoveRollbackCount = %d, want 1", got)
		}
		d.Crash()
		for _, name := range []string{"db/stage/a", "db/stage/b"} {
			if !d.Exists(name) {
				t.Errorf("%s did not come back: a restored RemoveAll must restore every durably-linked name it removed", name)
			}
		}
		if d.Exists("db/stage/c") {
			t.Error("db/stage/c came back although its own dirent never reached stable storage: the crash lost LESS than a real one")
		}
	})

	t.Run("stuck", func(t *testing.T) {
		d := build(t, 0x2536_BB02)
		d.ArmRemoveWritebackForPath("db/stage")
		if err := d.RemoveAll("db/stage"); err != nil {
			t.Fatalf("RemoveAll: %v", err)
		}
		d.Crash()
		for _, name := range []string{"db/stage/a", "db/stage/b", "db/stage/c"} {
			if d.Exists(name) {
				t.Errorf("%s came back although the unlink was durable", name)
			}
		}
	})
}

// TestSimDiskRemoveCrash_ParentDirSyncRetiresTheRecord pins the retirement rule.
// An unlink cannot be recognised as durable by inspecting the name it removed —
// the name is gone — so what retires the record is an fsync of the directory the
// name was removed FROM. After that fsync no crash may bring the name back, on
// any seed.
//
// Its sensitivity is the sweep in TestSimDiskRemoveCrash_BothOutcomesAreSampled,
// which is the same sequence WITHOUT the fsync and does bring the name back.
func TestSimDiskRemoveCrash_ParentDirSyncRetiresTheRecord(t *testing.T) {
	for _, seed := range removeCrashSeeds() {
		d := NewSimDisk(NewSeed(seed), 0)
		stageDurableFile(t, d, "dir/f", []byte("payload"))
		if err := d.Remove("dir/f"); err != nil {
			t.Fatalf("seed %#x: Remove: %v", seed, err)
		}
		if err := d.ParentDirSync("dir/f"); err != nil {
			t.Fatalf("seed %#x: ParentDirSync: %v", seed, err)
		}
		if got := d.PendingRemoveCount(); got != 0 {
			t.Fatalf("seed %#x: PendingRemoveCount = %d, want 0: an fsync of the parent must retire the unlink record",
				seed, got)
		}
		d.Crash()
		if d.Exists("dir/f") {
			t.Fatalf("seed %#x: the name came back after the parent directory was fsync'd", seed)
		}
	}
}

// TestSimDiskRemoveCrash_NoOpRemovalRecordsNothing pins the boundary of the model:
// removing an absent path is not a metadata mutation, so it must neither occupy a
// slot in the undo log — which would shift the durable-prefix draw — nor count as
// a cleanup that found something.
func TestSimDiskRemoveCrash_NoOpRemovalRecordsNothing(t *testing.T) {
	d := NewSimDisk(NewSeed(0x2536_CC01), 0)
	if err := d.Remove("dir/absent"); err != nil {
		t.Fatalf("Remove absent: %v", err)
	}
	if err := d.RemoveAll("dir/absent-tree"); err != nil {
		t.Fatalf("RemoveAll absent: %v", err)
	}
	if got := d.PendingRemoveCount(); got != 0 {
		t.Fatalf("PendingRemoveCount = %d, want 0: a no-op removal was recorded", got)
	}
	if got := d.RemoveHitCount(); got != 0 {
		t.Fatalf("RemoveHitCount = %d, want 0: a no-op removal was counted as a cleanup that found something", got)
	}
}

// TestSimDiskRemoveCrash_HitCountSeparatesACleanupFromANoOp proves the observable
// the engine-level tolerance demonstration rests on. A tolerant cleanup that ran
// as a no-op and one that really deleted a leftover are indistinguishable from
// outside; RemoveHitCountForPath is what separates them.
func TestSimDiskRemoveCrash_HitCountSeparatesACleanupFromANoOp(t *testing.T) {
	d := NewSimDisk(NewSeed(0x2536_CC02), 0)
	writeFile(t, d, "db/stage/a", []byte("aa"))

	if err := d.RemoveAll("db/other"); err != nil {
		t.Fatalf("RemoveAll other: %v", err)
	}
	if got := d.RemoveHitCountForPath("db/other"); got != 0 {
		t.Fatalf("RemoveHitCountForPath(db/other) = %d, want 0", got)
	}
	if err := d.RemoveAll("db/stage"); err != nil {
		t.Fatalf("RemoveAll stage: %v", err)
	}
	if got := d.RemoveHitCountForPath("db/stage"); got != 1 {
		t.Fatalf("RemoveHitCountForPath(db/stage) = %d, want 1: a removal that deleted a subtree was not counted", got)
	}
	// The same path removed twice: the second call finds nothing, so the count
	// stays put and a delta remains attributable to the call that mattered.
	if err := d.RemoveAll("db/stage"); err != nil {
		t.Fatalf("RemoveAll stage again: %v", err)
	}
	if got := d.RemoveHitCountForPath("db/stage"); got != 1 {
		t.Fatalf("RemoveHitCountForPath(db/stage) = %d, want 1: a no-op repeat was counted", got)
	}
	if got := d.RemoveHitCount(); got != 1 {
		t.Fatalf("RemoveHitCount = %d, want 1", got)
	}
}

// direntOutcome is the durable image a crash left, reduced to the names that
// identify which prefix of a (rename, removal) pair reached stable storage.
type direntOutcome struct {
	oldName     bool // the rename's source: present only if the rename was rolled back
	newName     bool // the rename's destination
	removedName bool // the removal's target: present only if the unlink was restored
}

func (o direntOutcome) String() string {
	return fmt.Sprintf("old=%t new=%t removed=%t", o.oldName, o.newName, o.removedName)
}

// stageRenameThenRemove issues a rename and then, in a DIFFERENT directory, a
// removal — two metadata mutations in a fixed order, neither made durable — and
// crashes there. Both directories are fsync'd beforehand so the only facts the
// crash has to adjudicate are the two mutations.
func stageRenameThenRemove(t *testing.T, seed uint64, arm func(d *SimDisk)) (*SimDisk, direntOutcome) {
	t.Helper()
	d := NewSimDisk(NewSeed(seed), 0)
	stageDurableFile(t, d, "a/src", []byte("moved"))
	stageDurableFile(t, d, "b/victim", []byte("removed"))
	if arm != nil {
		arm(d)
	}
	if err := d.Rename("a/src", "a/dst"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := d.Remove("b/victim"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	d.Crash()
	return d, direntOutcome{
		oldName:     d.Exists("a/src"),
		newName:     d.Exists("a/dst"),
		removedName: d.Exists("b/victim"),
	}
}

// TestSimDiskDirentUndo_NoUnlinkOutlivesARolledBackRename is the gate on the
// crux of the shared undo log: a crash must never keep an unlink while rolling
// back a rename issued BEFORE it.
//
// Both are metadata mutations of directories on the same filesystem, and a
// journalling filesystem commits metadata in ORDER, so the durable set is always
// a PREFIX of the issued sequence. "The removal reached stable storage but the
// rename that preceded it did not" is a durable prefix with a hole in it — no
// filesystem produces it. One ordered log with ONE draw makes it unreachable by
// construction; two independent logs with two draws would produce it, which is
// what TestSimDiskDirentUndo_SeparateLogsReachTheImpossibleInterleaving
// demonstrates.
func TestSimDiskDirentUndo_NoUnlinkOutlivesARolledBackRename(t *testing.T) {
	legal := map[direntOutcome]string{
		{oldName: true, newName: false, removedName: true}:  "neither mutation reached stable storage",
		{oldName: false, newName: true, removedName: true}:  "the rename reached it, the unlink did not",
		{oldName: false, newName: true, removedName: false}: "both reached it",
	}
	seen := map[direntOutcome]int{}
	for _, seed := range removeCrashSeeds() {
		d, got := stageRenameThenRemove(t, seed, nil)
		if _, ok := legal[got]; !ok {
			pendingRen, rolled := d.LastCrashRenameOutcome()
			pendingRem, restored := d.LastCrashRemoveOutcome()
			t.Fatalf("seed %#x: crash left %s, which is not a durable PREFIX of (rename, unlink) "+
				"[renames pending=%d rolledBack=%d; removes pending=%d restored=%d]",
				seed, got, pendingRen, rolled, pendingRem, restored)
		}
		// The load-bearing half, stated separately so a regression names itself.
		if got.oldName && !got.removedName {
			t.Fatalf("seed %#x: the unlink stuck while the rename before it was rolled back (%s): "+
				"the durable metadata set has a hole in it", seed, got)
		}
		seen[got]++
	}

	// Shape: all three prefixes are really sampled, so the verdict adjudicates a
	// live choice rather than a constant.
	for want, why := range legal {
		if seen[want] == 0 {
			t.Errorf("outcome %s (%s) never occurred across %d seeds: the shared log does not sample that prefix",
				want, why, len(removeCrashSeeds()))
		}
	}
	keys := make([]string, 0, len(seen))
	for o, n := range seen {
		keys = append(keys, fmt.Sprintf("%s x%d", o, n))
	}
	sort.Strings(keys)
	t.Logf("prefix distribution over %d seeds: %s", len(removeCrashSeeds()), strings.Join(keys, " | "))
}

// TestSimDiskDirentUndo_SeparateLogsReachTheImpossibleInterleaving is the gate on
// the gate: it proves the verdict above can fail, by reproducing what the design
// alternative would have done.
//
// Keeping unlinks in their own log with their own draw is the natural way to add
// them, and it is wrong. The state it produces is constructed here surgically —
// the unlink record is dropped from the shared log while the rename before it
// stays pending, which is exactly "the unlink's own draw kept it" — and the
// resulting durable image is one the verdict gate rejects. Without this, a
// verdict gate that passed because the model no longer reaches any interesting
// state would be indistinguishable from one that passed because the model is
// correct.
func TestSimDiskDirentUndo_SeparateLogsReachTheImpossibleInterleaving(t *testing.T) {
	d := NewSimDisk(NewSeed(0x2536_DD01), 0)
	stageDurableFile(t, d, "a/src", []byte("moved"))
	stageDurableFile(t, d, "b/victim", []byte("removed"))

	// Pin the rename to the rolled-back branch, so the only question is what
	// happens to the unlink issued after it.
	d.ArmRenameRollbackForPath("a/dst")
	if err := d.Rename("a/src", "a/dst"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := d.Remove("b/victim"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Simulate the two-log design: adjudicate the unlink independently of the
	// rename by removing its record from the shared log. Nothing in the public
	// API can do this, which is the point — under the shared log the outcome
	// below is unreachable.
	kept := d.direntUndos[:0]
	dropped := 0
	for _, rec := range d.direntUndos {
		if rec.kind == direntUndoUnlink {
			dropped++
			continue
		}
		kept = append(kept, rec)
	}
	d.direntUndos = kept
	if dropped != 1 {
		t.Fatalf("dropped %d unlink records, want 1: the removal was not recorded, so this test reproduces nothing", dropped)
	}

	d.Crash()
	got := direntOutcome{
		oldName:     d.Exists("a/src"),
		newName:     d.Exists("a/dst"),
		removedName: d.Exists("b/victim"),
	}
	if !got.oldName || got.removedName {
		t.Fatalf("the reproduction did not produce the impossible interleaving: got %s, want the rename rolled back (old=true) and the unlink kept (removed=false)",
			got)
	}
	t.Logf("two independent draws reproduce the impossible durable prefix: %s", got)
}

// TestSimDiskDirentUndo_ChainedRenameKeepsOneName is a regression gate on the
// defect rmp #2536 uncovered while generalising the log, and it fails on the code
// as it stood before.
//
// A record used to leave the undo log — pinned by a later operation that consumed
// its destination — WITHOUT its dirent being made durable. The log then said the
// rename was durable while the state said the name was not, and the next crash
// resolved the contradiction by deleting the name: a chained rename (a -> b, then
// b -> c) whose second hop was rolled back restored b and then revoked it, losing
// BOTH names of a rename. That is the outcome rmp #2514 exists to forbid, and it
// needed no arming to reach — measured at 23 of 64 sampled seeds before the fix,
// and 35 of the 96 this test samples.
//
// Only dir/b and dir/c are reachable outcomes: the second rename consumes dir/b,
// which pins the first rename into the durable prefix, so "neither rename reached
// stable storage" is no longer sampled. The gate is on the count of surviving
// names rather than on which one, so it does not depend on that narrowing.
func TestSimDiskDirentUndo_ChainedRenameKeepsOneName(t *testing.T) {
	legal := []string{"dir/a", "dir/b", "dir/c"}
	seen := map[string]int{}
	for _, seed := range removeCrashSeeds() {
		d := NewSimDisk(NewSeed(seed), 0)
		stageDurableFile(t, d, "dir/a", []byte("payload"))
		if err := d.Rename("dir/a", "dir/b"); err != nil {
			t.Fatalf("seed %#x: rename 1: %v", seed, err)
		}
		if err := d.Rename("dir/b", "dir/c"); err != nil {
			t.Fatalf("seed %#x: rename 2: %v", seed, err)
		}
		d.Crash()
		held := make([]string, 0, len(legal))
		for _, name := range legal {
			if d.Exists(name) {
				held = append(held, name)
			}
		}
		if len(held) != 1 {
			t.Fatalf("seed %#x: after a chained rename the file is under %v; exactly one of %v must hold it — "+
				"losing every name is not an outcome rename(2) can produce", seed, held, legal)
		}
		seen[held[0]]++
	}
	// Shape: the intermediate name is really reachable, so the gate is watching a
	// live choice. dir/a and dir/c are the two extremes of the prefix draw.
	if seen["dir/b"] == 0 {
		t.Errorf("the intermediate name dir/b never survived across %d seeds: the chained-rename prefix is not sampled",
			len(removeCrashSeeds()))
	}
	t.Logf("chained-rename surviving name: %v", seen)
}

// TestSimDiskDirentUndo_PrunedPrefixHardensEveryRecordItDrops is the other route
// into the same defect, and it also fails on the pre-#2536 code.
//
// A directory fsync declares its own directory's mutations durable, and by the
// prefix rule every mutation issued before them — including renames in OTHER
// directories, which that fsync does not itself harden. Those records left the log
// non-durable, so a later crash revoked the name each had created while its source
// name was already gone. Here the fsync of b/ must make the earlier rename in a/
// crash-survivable; before the fix a/y vanished with no a/x to fall back on.
func TestSimDiskDirentUndo_PrunedPrefixHardensEveryRecordItDrops(t *testing.T) {
	d := NewSimDisk(NewSeed(0x2536_EE01), 0)
	stageDurableFile(t, d, "a/x", []byte("payload-a"))
	stageDurableFile(t, d, "b/p", []byte("payload-b"))
	if err := d.Rename("a/x", "a/y"); err != nil {
		t.Fatalf("rename in a: %v", err)
	}
	if err := d.Rename("b/p", "b/q"); err != nil {
		t.Fatalf("rename in b: %v", err)
	}
	// An fsync of b only. It durabilises b/q directly and, by the prefix rule,
	// declares the earlier rename in a durable too.
	if err := d.DirSync("b"); err != nil {
		t.Fatalf("DirSync b: %v", err)
	}
	if got := d.PendingRenameCount(); got != 0 {
		t.Fatalf("PendingRenameCount = %d, want 0: the fsync must retire both records by the prefix rule", got)
	}
	d.Crash()
	for _, name := range []string{"a/y", "b/q"} {
		if !d.Exists(name) {
			t.Errorf("%s did not survive: a record declared durable by the prefix rule was left non-durable, so the crash lost both names of that rename", name)
		}
	}
	if d.Exists("a/x") || d.Exists("b/p") {
		t.Errorf("a source name came back although its rename was declared durable: a/x=%t b/p=%t",
			d.Exists("a/x"), d.Exists("b/p"))
	}
}

// TestSimDiskDirentUndo_WritebackArmPinsThePrefix is the third route, on the arms
// themselves. A write-back declares ONE mutation durable; by journal order every
// mutation pending at that instant is durable too. Without that rule, arming the
// LATER rename of a pair left the crash free to roll the earlier one back and keep
// the later one — a durable prefix with a hole, reachable from the ordinary seed
// draw with no second arm.
func TestSimDiskDirentUndo_WritebackArmPinsThePrefix(t *testing.T) {
	for _, seed := range removeCrashSeeds() {
		d := NewSimDisk(NewSeed(seed), 0)
		stageDurableFile(t, d, "db/snapshot/manifest.json", []byte("old"))
		writeFile(t, d, "db/snapshot.tmp/manifest.json", []byte("new"))
		if err := d.DirSync("db/snapshot.tmp"); err != nil {
			t.Fatalf("seed %#x: DirSync staging: %v", seed, err)
		}
		if err := d.Rename("db/snapshot", "db/snapshot.bak"); err != nil {
			t.Fatalf("seed %#x: archive: %v", seed, err)
		}
		// Arm the LATER rename of the pair — the wrong one to arm, and the one
		// that used to manufacture the hole.
		d.ArmRenameWritebackForPath("db/snapshot")
		if err := d.Rename("db/snapshot.tmp", "db/snapshot"); err != nil {
			t.Fatalf("seed %#x: publish: %v", seed, err)
		}
		if got := d.RenameWritebackCount(); got != 1 {
			t.Fatalf("seed %#x: RenameWritebackCount = %d, want 1", seed, got)
		}
		if got := d.PendingRenameCount(); got != 0 {
			t.Fatalf("seed %#x: PendingRenameCount = %d, want 0: the write-back must declare the whole pending prefix durable",
				seed, got)
		}
		d.Crash()
		if !d.Exists("db/snapshot/manifest.json") || !d.Exists("db/snapshot.bak/manifest.json") {
			t.Fatalf("seed %#x: the armed publish is durable, so the archive before it must be too: live=%t bak=%t",
				seed, d.Exists("db/snapshot/manifest.json"), d.Exists("db/snapshot.bak/manifest.json"))
		}
	}
}

// TestSimDiskRemoveCrash_OutcomeIsSeedReproducible pins the determinism the whole
// harness rests on: the same seed must produce the same outcome every time, and
// the choice must come from the seed rather than from map iteration order or the
// clock. The sequence mixes a rename with two removals so the replay covers a log
// with records of both kinds.
func TestSimDiskRemoveCrash_OutcomeIsSeedReproducible(t *testing.T) {
	image := func(seed uint64) string {
		d := NewSimDisk(NewSeed(seed), 0)
		stageDurableFile(t, d, "a/src", []byte("moved"))
		stageDurableFile(t, d, "b/one", []byte("1"))
		stageDurableFile(t, d, "b/two", []byte("2"))
		if err := d.Rename("a/src", "a/dst"); err != nil {
			t.Fatalf("Rename: %v", err)
		}
		if err := d.Remove("b/one"); err != nil {
			t.Fatalf("Remove one: %v", err)
		}
		if err := d.Remove("b/two"); err != nil {
			t.Fatalf("Remove two: %v", err)
		}
		d.Crash()
		names := make([]string, 0, 4)
		for p := range d.Snapshot() {
			names = append(names, p)
		}
		sort.Strings(names)
		pending, restored := d.LastCrashRemoveOutcome()
		return fmt.Sprintf("%s (pending=%d restored=%d)", strings.Join(names, ","), pending, restored)
	}
	distinct := map[string]int{}
	for _, seed := range removeCrashSeeds()[:16] {
		first := image(seed)
		for rep := 0; rep < 3; rep++ {
			if again := image(seed); again != first {
				t.Fatalf("seed %#x replay %d: got %q, first run gave %q — the crash outcome is not a function of the seed",
					seed, rep, again, first)
			}
		}
		distinct[first]++
	}
	// Non-vacuity: if every seed produced the same image, replay equality would
	// hold on any implementation, including one that ignored the seed entirely.
	if len(distinct) < 2 {
		t.Fatalf("all sampled seeds produced the same durable image (%v): the reproducibility check cannot witness a defect", distinct)
	}
	t.Logf("%d distinct durable images across 16 seeds", len(distinct))
}

// TestSimDiskRemoveCrash_DoesNotPerturbTheFaultStream proves the removal model
// draws nothing from the disk's own [Seed]: two disks on the same seed, one of
// which removes files and one of which does not, must draw the same torn-write /
// Sync fault decisions. The dirent sub-stream is derived from the seed VALUE, so
// turning the model on must leave the fault sequence every other arm is careful
// to preserve exactly where it was.
func TestSimDiskRemoveCrash_DoesNotPerturbTheFaultStream(t *testing.T) {
	// A high fault rate so the stream is observable at all: at rate 0 every draw
	// returns false regardless of position, and the test could not fail.
	const rate = 0.5
	syncOutcomes := func(withRemovals bool) []bool {
		d := NewSimDisk(NewSeed(0x2536_F00D), rate)
		h, err := d.OpenFile("probe", os.O_CREATE|os.O_WRONLY)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer func() { _ = h.Close() }()
		out := make([]bool, 0, 32)
		for i := 0; i < 32; i++ {
			// BOTH arms create the file, because [SimFileHandle.Write] draws a
			// per-sector fault decision from the disk's Seed; only the removals
			// and the crash differ, which is what this test is about.
			name := fmt.Sprintf("dir/f%d", i)
			writeFileNoSync(t, d, name)
			if withRemovals {
				if err := d.Remove(name); err != nil {
					t.Fatalf("Remove: %v", err)
				}
			}
			out = append(out, h.Sync() != nil)
		}
		if withRemovals {
			d.Crash() // also exercise the crash draw itself
		}
		return out
	}
	plain, removed := syncOutcomes(false), syncOutcomes(true)
	if len(plain) != len(removed) {
		t.Fatalf("length mismatch: %d vs %d", len(plain), len(removed))
	}
	for i := range plain {
		if plain[i] != removed[i] {
			t.Fatalf("Sync fault stream diverged at draw %d (%t vs %t): the removal crash model is drawing from the disk's fault Seed",
				i, plain[i], removed[i])
		}
	}
	faults := 0
	for _, f := range plain {
		if f {
			faults++
		}
	}
	if faults == 0 || faults == len(plain) {
		t.Fatalf("the sampled Sync fault stream is constant (%d faults of %d): it cannot witness a perturbation",
			faults, len(plain))
	}
	t.Logf("fault stream identical across %d draws (%d faults)", len(plain), faults)
}
