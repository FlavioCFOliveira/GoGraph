package sim

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	pathpkg "path"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// sectorSize is the granularity of the fault bitmap. Each 512-byte sector can
// be independently marked faulted; a write into a faulted sector is corrupted
// deterministically.
const sectorSize = 512

// ErrSimFault is the sentinel returned by a simulated file operation that the
// seed-driven fault injector chose to fail. Callers match it with
// [errors.Is]. It models a durability fault: data the caller believed it was
// flushing did not reach stable storage.
var ErrSimFault = errors.New("sim: injected disk fault")

// walFile mirrors the (unexported) file-system interface that the WAL
// [github.com/FlavioCFOliveira/GoGraph/store/wal.Writer] requires of its
// underlying handle (see store/wal/writer_vfs.go). It cannot be imported by
// name because the WAL declares it unexported, so it is restated here verbatim;
// Go's structural typing means a [SimFileHandle] that satisfies this local copy
// also satisfies the WAL's own interface, which is what lets Phase 2 pass a
// SimFileHandle to wal.OpenWith. The compile-time assertion below guards the
// shape so a drift in either interface surfaces as a build failure here.
type walFile interface {
	io.Writer
	io.Reader
	io.Seeker
	Sync() error
	Truncate(size int64) error
	Close() error
}

// Compile-time assertion that a simulated file handle satisfies the WAL file
// interface, so SimDisk can stand in for *os.File once the engine is wired to
// it in Phase 2.
var _ walFile = (*SimFileHandle)(nil)

// Fidelity limits of the SimDisk crash model (rmp #2514).
//
// The model is deliberately narrower than a real filesystem. The notes below
// record, per operation, whether a crash can lose an effect that no real
// filesystem loses — the class of infidelity that makes the simulator ACCUSE the
// engine of a defect it does not have — or, conversely, whether it keeps an
// effect a real crash could take away, which lets an engine defect pass. The
// first class is a harness bug; the second is missing coverage. Both are
// recorded so a scenario author knows what the harness does and does not model.
//
// This is a summary of the limits that bear on CRASH OUTCOMES, written where the
// code is; it is not the complete audit of the type. In particular it does not
// cover the operations whose modelling gap is that they cannot FAIL (a Sync
// retry always succeeds after a fault, a write is never partial), which bound
// which engine error branches a scenario can reach rather than which durable
// images a crash can leave.
//
//	Rename          FIXED. A crash used to revoke the new directory entry while
//	                the old one had already been unlinked, losing BOTH names —
//	                an outcome rename(2)'s atomicity forbids, and the one that
//	                made the snapshot publish protocol look as if a crash
//	                between its two renames could destroy every copy of the
//	                graph. A crash now rolls the rename back instead, so the
//	                outcome is always one of the two legal ones. See
//	                [SimDisk.Crash].
//
//	Remove          FIXED (rmp #2536). An unlink is metadata, in the same
//	RemoveAll       durability class as the create and the rename above: until
//	                the parent directory is fsync'd a crash may legally leave the
//	                removed name in place. The model applied the unlink
//	                immediately and [SimDisk.Crash] could never restore it, so
//	                the harness only ever presented the EASIER input to
//	                recovery — a stale "<dir>/snapshot.bak" surviving the publish
//	                path's happy-path cleanup, and a stale "<dir>/snapshot.tmp"
//	                surviving recovery's own best-effort staging cleanup, were
//	                both unreachable from the removal side although recovery is
//	                written to tolerate them. A removal now records an undo (see
//	                [unlinkUndo]) in the SAME ordered log as a rename, so one
//	                durable-prefix draw adjudicates both and no crash can keep an
//	                unlink while discarding a rename issued before it.
//
//	Truncate        NOT MODELLED, coverage gap (never a false accusation).
//	TruncatePath    A shrink is applied immediately and a crash never restores
//	O_TRUNC         the previous length, so the harness cannot present a WAL
//	                whose prefix truncation was lost. Recovery would then have
//	                to re-read frames a checkpoint had already folded, which is
//	                exactly the idempotence it claims. The durable shadow added
//	                for rmp #2535 leaves this unchanged on purpose — a shrink
//	                lowers the durable image too ([simFile.truncateDurableTo]) —
//	                and that one function is the seam rmp #2542 needs to make a
//	                truncation itself survivable.
//
//	Write (data)    FIXED (rmp #2535). Bytes written but never Sync'd used to
//	                survive [SimDisk.Crash] intact, whereas a real crash loses
//	                whatever never left the page cache — so every "the commit
//	                was acked, therefore the bytes are recovered" assertion held
//	                irrespective of any fsync, and deleting the WAL commit fsync
//	                failed no scenario. Each file now carries a durable image
//	                advanced ONLY by a Sync that returns nil, and the crash
//	                primitive is split: [SimDisk.CrashHost] reverts to that
//	                image (power failure), [SimDisk.CrashProcess] keeps the
//	                bytes and the names (SIGKILL). [SimDisk.Crash] is CrashHost.
//
//	MkdirAll        NOT MODELLED, negligible. Directories are opaque keys and
//	                MkdirAll is a no-op, so a directory creation can never be
//	                lost. A real mkdir without a parent fsync can be. Nothing in
//	                the store depends on an empty directory existing.
//
//	DirSync         FIXED (rmp #2537), and NARROWER THAN REALITY in the safe
//	                direction. It could not FAIL: the body ended in an
//	                unconditional return nil while its sibling ParentDirSync
//	                already consulted an arm, so four snapshot staging-fsync
//	                error branches were unreachable under simulation — including
//	                the last durability gate before the publish renames. Both
//	                entry points now share ONE body ([SimDisk.dirSyncLocked]) and
//	                one arm mechanism ([dirSyncArm]), so neither the fault nor
//	                its effects can diverge again. What stays narrower is the
//	                SUCCESS path: it durabilises only the entries whose parent is
//	                exactly dir, whereas fsync(2) on a directory forces a journal
//	                commit that makes earlier metadata durable filesystem-wide.
//	                Fewer things become durable than would in reality, so the
//	                harness is stricter, never more forgiving.
//
// SimDisk is an in-memory filesystem with seed-driven fault injection. It backs
// the durability layer of the simulation: files live entirely in memory, and a
// per-sector fault bitmap plus a per-Sync fault probability let the simulator
// reproduce torn writes and failed flushes deterministically.
//
// SimDisk is built in Phase 1 but is not yet wired into the engine (that is
// Phase 2 work); it must compile, implement the WAL file interface, and be
// unit-tested standalone.
//
// # Concurrency contract
//
// SimDisk's directory operations are guarded by an internal mutex so the file
// table cannot be corrupted, but the simulation drives it from a single
// goroutine and the fault decisions draw from the shared single-goroutine
// [Seed]; it must not be used concurrently.
//
// The "Sim" prefix is part of the DST harness's deliberate naming scheme
// (SimDisk / SimFileHandle / SimReport), which reads clearly at call sites and
// matches the design specification; the apparent stutter is intentional.
//
//nolint:revive // intentional SimXxx naming scheme (see comment above).
type SimDisk struct {
	files map[string]*simFile
	seed  *Seed
	// dirs tracks the dirent durability of DIRECTORIES that were published by
	// a directory rename (the snapshot publish renames a whole staging
	// directory onto the live name). The value is whether the directory's own
	// name in its parent is durable; a directory absent from this map is
	// implicitly durable (it was created in place via MkdirAll, never via a
	// publish rename). A [SimDisk.Crash] drops every directory whose dirent is
	// not yet durable, taking its entire subtree with it.
	dirs map[string]bool
	// dirSyncFault / parentDirSyncFault are the two ONE-SHOT arms of the SAME
	// primitive — a directory fsync that fails — and they differ only in the KEY
	// each is matched against. dirSyncFault is keyed on the directory being
	// fsync'd ([SimDisk.DirSync]); parentDirSyncFault is keyed on the CHILD path
	// whose parent is being fsync'd ([SimDisk.ParentDirSync]). Both are consulted
	// by the one shared body, [SimDisk.dirSyncLocked], which is what stops them
	// from drifting apart: DirSync used to have no arm at all and ended in an
	// unconditional return nil, leaving four snapshot directory-fsync error
	// branches unreachable (rmp #2537).
	//
	// The two keys are deliberately NOT unified. ParentDirSync is keyed on the
	// exact childPath because a full-stack checkpoint issues a variable number of
	// parent fsyncs of the SAME directory ("db/snapshot" then "db/wal", both
	// children of "db"), so a directory-keyed arm would fire on whichever came
	// first rather than on the one the scenario means. Arming the directory key
	// is nonetheless the stronger statement — "every fsync of this directory
	// fails" — and it therefore also fires for a ParentDirSync whose parent is
	// that directory, because that call IS an fsync of it.
	dirSyncFault       dirSyncArm
	parentDirSyncFault dirSyncArm
	// dirSyncFaults counts how many times a directory-fsync fault actually FIRED,
	// through EITHER entry point — they are one primitive, so they share one
	// reachability observable ([SimDisk.DirSyncFaultCount]). An arm whose key
	// never matched is a silent no-op, and a scenario that depends on one firing
	// must be able to tell "the fault fired" from "the arm was ignored" rather
	// than diagnosing the resulting durable image as an engine defect. Mutated
	// only under d.mu.
	dirSyncFaults int64
	// capacityBytes, when > 0, bounds the total number of bytes the in-memory
	// filesystem may hold across all files. It models a finite disk so the DST
	// can drive the engine through a disk-full (ENOSPC) condition. Zero (the
	// default) means unbounded, so every pre-existing caller is byte-for-byte
	// unaffected. The budget check is a PURE function of the current byte total
	// and the requested growth — it draws NOTHING from the [Seed] — so turning a
	// capacity on never perturbs the reproducible torn-write/Sync fault stream.
	capacityBytes int64
	// syncCount counts every [SimFileHandle.Sync] call across all handles of
	// this disk. It is the ordinal a deterministic Sync-fault is armed against
	// ([SimDisk.ArmSyncFaultAt]) and is exposed via [SimDisk.SyncCount] so a
	// concurrent scenario can gate an action (e.g. a teardown) on durable
	// progress rather than on wall-clock time. It is mutated only under d.mu.
	syncCount   int64
	syncFaultAt int64
	// tornAppendArmed / tornAppendAt / tornAppendKeep implement a ONE-SHOT torn
	// append: the tornAppendAt-th GROWING write on this disk is marked so that
	// the next crash leaves only its first tornAppendKeep bytes intact, with the
	// remainder garbled. appendCount counts growing writes since the arm was set.
	tornAppendArmed bool
	tornAppendAt    int64
	tornAppendKeep  int64
	appendCount     int64
	faultRate       float64
	mu              sync.Mutex
	// syncFaultArmed / syncFaultAt implement a ONE-SHOT deterministic Sync
	// fault: when armed, the syncFaultAt-th Sync call returns [ErrSimFault]
	// exactly once (then disarms). Unlike the probabilistic faultRate path it
	// draws NOTHING from the [Seed] — the trigger is a pure function of the Sync
	// ordinal — so arming it never perturbs the reproducible torn-write/Sync
	// fault stream, exactly like [SimDisk.SetCapacity]. It models a fsync failure
	// on a CHOSEN commit (the WAL writer then poisons and discards the un-synced
	// suffix, store/wal/writer.go poison), which is the mid-flight durability
	// fault the durable-commit crash scenario needs. WHICH commit ends up being
	// the syncFaultAt-th is non-deterministic under concurrency (interleaving),
	// but that a fault fires at that ordinal is deterministic (the hybrid model).
	syncFaultArmed bool
	// enospcOnSync selects WHERE the out-of-space condition surfaces:
	//
	//   - false (eager mode, the default): a Write / Truncate / TruncatePath
	//     that would grow the total past capacityBytes returns an ENOSPC
	//     [os.PathError] and grows nothing — modelling allocate-on-write.
	//   - true (delayed-allocation mode): growth is buffered in memory and
	//     succeeds, but Sync returns ENOSPC whenever the total exceeds
	//     capacityBytes — modelling the common case where a full disk only
	//     surfaces at fsync. This is the harder path for the WAL commit contract.
	//
	// It has no effect when capacityBytes is 0.
	enospcOnSync bool
	// renameFaultPath / renameWritebackPath are the DESTINATION paths the two
	// one-shot Rename arms below are keyed on. Both are keyed on the destination
	// rather than a call ordinal for the same reason
	// [SimDisk.ArmParentDirSyncFaultForPath] is: one snapshot publish issues a
	// variable number of renames (every component writes through a sibling .tmp
	// and renames it into place) before the two renames of the publish protocol
	// itself, so an ordinal would be fragile against that count whereas the
	// destinations ("<dir>/snapshot", "<dir>/snapshot.bak") are stable.
	renameFaultPath     string
	renameWritebackPath string
	// renameFaults / renameWritebacks count how many times each armed Rename arm
	// actually FIRED. They are the reachability observable: an arm whose path
	// never matched is a silent no-op, and a scenario that depends on one firing
	// must be able to tell "the primitive fired" from "the primitive was ignored"
	// rather than diagnosing the resulting durable image as an engine defect.
	// Mutated only under d.mu.
	renameFaults     int64
	renameWritebacks int64
	// renameFaultArmed / renameFaultPath implement a ONE-SHOT deterministic fault
	// on [SimDisk.Rename]: when armed, the next Rename onto renameFaultPath
	// returns [ErrSimFault] and moves NOTHING, then disarms. See
	// [SimDisk.ArmRenameFaultForPath].
	renameFaultArmed bool
	// renameWritebackArmed / renameWritebackPath implement a ONE-SHOT selection
	// of the OTHER branch of the crash-window non-determinism a rename has: see
	// [SimDisk.ArmRenameWritebackForPath].
	renameWritebackArmed bool
	// direntUndos is the ONE ORDERED log of directory-entry mutations whose
	// effect is not yet on stable storage, newest last. It holds renames (rmp
	// #2514) and unlinks (rmp #2536) TOGETHER, and that is the load-bearing part
	// of its design: both are metadata mutations of the same directories, a
	// journalling filesystem commits metadata in ORDER, and therefore an unlink
	// issued after a rename cannot be durable while that rename is not. One log
	// with ONE durable-prefix draw is the only shape that guarantees it. Two
	// independent logs with two draws would let a crash keep an unlink while
	// rolling back a rename issued before it — an interleaving no filesystem can
	// produce, which is exactly the class of outcome this model exists to
	// eliminate.
	//
	// Each record carries what is needed to reverse its operation, including the
	// dirent durability the affected names carried at the instant of the
	// operation, so [SimDisk.Crash] can put the filesystem back exactly as it
	// was — and so that a name that was never durable is NOT resurrected,
	// because losing it is legal precisely when it had never been
	// crash-survivable.
	//
	// A record leaves the log when its effect becomes durable (a
	// [SimDisk.DirSync] of the affected parent directory) or when a later
	// operation makes its undo inapplicable (see
	// [SimDisk.pinDirentUndosLocked]). Both drop the record AND every record
	// before it, which is what keeps the surviving log a SUFFIX of the mutation
	// history, and both APPLY the durability the drop declares (see
	// [SimDisk.hardenDurableDirentLocked]) rather than only forgetting the
	// record.
	direntUndos []direntUndo
	// direntSeed is the deterministic sub-stream the crash-outcome choice draws
	// from. It is derived from the disk seed's VALUE (xor direntCrashSeedMix),
	// exactly as every call site derives the disk seed from the run seed
	// (NewSeed(cfg.Seed^diskSeedMix)), so the choice is a pure function of the
	// run seed and replays bit-for-bit — while drawing NOTHING from d.seed, so
	// turning the model on never perturbs the reproducible torn-write/Sync fault
	// stream that every other arm here is careful to leave alone.
	direntSeed *Seed
	// renameRollbackPath / renameRevokePath are the DESTINATION paths of the two
	// crash-outcome arms that pin a rename's crash outcome instead of letting
	// the seed choose it; renameRollbacks / renameRevokes are their fire counts,
	// the same reachability observable the other arms expose. See
	// [SimDisk.ArmRenameRollbackForPath] and
	// [SimDisk.ArmRenameRevokeBothForPath].
	renameRollbackPath  string
	renameRevokePath    string
	renameRollbacks     int64
	renameRevokes       int64
	renameRollbackArmed bool
	renameRevokeArmed   bool
	// removeWritebackPath / removeRollbackPath are the paths the two one-shot
	// unlink arms are keyed on, and removeWritebacks / removeRollbacks are their
	// fire counts — the same reachability observable every other arm here
	// exposes. They pin the two outcomes a crash immediately after unlink(2) can
	// produce, exactly as [SimDisk.ArmRenameWritebackForPath] and
	// [SimDisk.ArmRenameRollbackForPath] pin a rename's two. There is
	// deliberately no third, impossible-outcome arm: unlike a rename, BOTH of an
	// unlink's outcomes are legal, so there is no illegal one to reproduce.
	removeWritebackPath  string
	removeRollbackPath   string
	removeWritebacks     int64
	removeRollbacks      int64
	removeWritebackArmed bool
	removeRollbackArmed  bool
	// removeHits counts the removals that actually unlinked at least one existing
	// name, in total and per path. It answers a question no other observable can:
	// whether a TOLERANT cleanup — recovery's best-effort RemoveAll of a stale
	// staging directory, the publish path's RemoveAll of a stale backup — really
	// found something to clean, or ran as a no-op. A test that makes a removal
	// fail to stick and then asserts recovery "handled" the leftover proves
	// nothing unless the cleanup is known to have deleted it. Mutated only under
	// d.mu.
	//
	// removeHitsByPath is keyed on the paths a caller actually removes, which for
	// the store are the fixed names of its on-disk layout (wal.tmp,
	// snapshot.tmp, snapshot.bak and the staged component files), so its size is
	// bounded by that layout and not by the length of the run.
	removeHits       int64
	removeHitsByPath map[string]int64
	// crashPendingRenames / crashRolledBackRenames record what the LAST
	// [SimDisk.Crash] adjudicated about RENAMES: how many not-yet-durable ones
	// were in the log, and how many of them it rolled back.
	// crashPendingRemoves / crashRestoredRemoves are the same two numbers for
	// UNLINKS. They are the shape observable a non-vacuity gate reads to prove
	// the crash really landed inside a metadata window rather than after it, and
	// which of the legal branches it took. They are counted per kind even though
	// the log and the draw are shared, because a scenario asserts about the
	// operation it issued.
	crashPendingRenames    int
	crashRolledBackRenames int
	crashPendingRemoves    int
	crashRestoredRemoves   int
	// syncGate, when non-nil, is a ONE-SHOT rendezvous armed on a Sync ordinal by
	// [SimDisk.ArmSyncGateAt]: the syncGateAt-th Sync blocks inside the call until
	// the controlling goroutine releases it. It is the only primitive here that
	// controls sync TIMING rather than sync OUTCOME, and it exists because the WAL
	// group-commit leader is only a leader for as long as its fsync runs — against
	// an in-memory disk that window is far too short for another committer to
	// reach the follower path, so a multi-member group cannot otherwise be
	// constructed deterministically. Like every other arm here it draws NOTHING
	// from the [Seed]. See [SimDisk.ArmSyncGateAt] for the ordering rule against
	// [SimDisk.ArmSyncFaultAt].
	syncGate   *SyncGate
	syncGateAt int64
	// hostCrashGen counts [SimDisk.CrashHost] calls. A Sync captures it when the
	// fsync is ISSUED and re-checks it when the fsync COMPLETES: a host crash
	// landing in that window — which is exactly what [SimDisk.ArmSyncGateAt]
	// constructs — means the fsync never completed, so its durability effect is
	// dropped instead of being applied to an image the crash has already rolled
	// back. [SimDisk.CrashProcess] deliberately does NOT bump it: SIGKILL does
	// not abort an fsync already issued to the kernel, which owns the writeback.
	hostCrashGen uint64
	// crashGen counts crashes of EITHER kind, and is what invalidates an open
	// [SimFileHandle]. It is deliberately separate from hostCrashGen, which
	// models in-flight fsync invalidation and is bumped only by CrashHost: a
	// file descriptor does not survive its process, so a handle must die on a
	// CrashProcess too, while an fsync already handed to the kernel does not
	// (rmp #2544).
	crashGen uint64
	// lastCrashKind / lastCrashDiscardedBytes are the shape observables of the
	// most recent crash: which primitive ran, and how many
	// written-but-never-synced bytes [SimDisk.CrashHost] discarded from the files
	// that survived it. A non-vacuity gate reads the byte count to prove the
	// crash really landed on unsynced data rather than after a quiet fsync —
	// the difference between an oracle that can fail and one that cannot.
	lastCrashKind           CrashKind
	lastCrashDiscardedBytes int64
}

// CrashKind identifies which crash primitive a [SimDisk] last ran. It is the
// observable that lets a scenario state — and a gate check — which of the two
// physically distinct events it intended to model.
type CrashKind uint8

const (
	// CrashKindNone means no crash has been issued on this disk yet.
	CrashKindNone CrashKind = iota
	// CrashKindHost is a power failure: [SimDisk.CrashHost].
	CrashKindHost
	// CrashKindProcess is a SIGKILL of the process: [SimDisk.CrashProcess].
	CrashKindProcess
)

// String renders the crash kind for test output and reports.
func (k CrashKind) String() string {
	switch k {
	case CrashKindHost:
		return "host"
	case CrashKindProcess:
		return "process"
	default:
		return "none"
	}
}

// LastCrashKind reports which crash primitive this disk last ran, or
// [CrashKindNone] if it has not crashed. It is the observable a scenario asserts
// to prove it exercised the model it intended.
func (d *SimDisk) LastCrashKind() CrashKind {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastCrashKind
}

// LastCrashDiscardedBytes reports how many written-but-never-synced bytes the
// last [SimDisk.CrashHost] discarded, counted across the files that SURVIVED the
// crash (bytes lost with a revoked dirent are a name loss, not a data loss, and
// are not counted here). [SimDisk.CrashProcess] always leaves it at zero,
// because a process crash discards nothing.
//
// It is the non-vacuity observable for every durability oracle: a scenario that
// asserts "the unsynced tail is gone" must also be able to prove there WAS an
// unsynced tail, or it is asserting nothing.
func (d *SimDisk) LastCrashDiscardedBytes() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastCrashDiscardedBytes
}

// DurableSize reports how many leading bytes of the file at path are on stable
// storage — the length [SimDisk.CrashHost] would leave it at — and whether the
// file exists at all. It is the direct read of the durable watermark that lets a
// test distinguish "the engine fsync'd" from "the bytes merely exist".
func (d *SimDisk) DurableSize(path string) (int64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f, ok := d.files[path]
	if !ok {
		return 0, false
	}
	return f.durableSize(), true
}

// DurableImage returns a copy of the bytes the file at path would hold after a
// [SimDisk.CrashHost] — the durable image, as opposed to [SimDisk.ReadFile]'s
// live image, which includes writes the process has issued but never fsync'd. It
// returns an error wrapping fs.ErrNotExist when the file is absent.
func (d *SimDisk) DurableImage(path string) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f, ok := d.files[path]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	img := f.durableImage()
	out := make([]byte, len(img))
	copy(out, img)
	return out, nil
}

// SyncGate is a one-shot rendezvous on a chosen [SimFileHandle.Sync] call,
// returned by [SimDisk.ArmSyncGateAt]. The gated Sync blocks inside the call —
// with the disk lock RELEASED, so the rest of the disk stays usable — until
// [SyncGate.Release] is called, which is what lets a scenario hold the WAL
// group-commit leader inside its fsync while other committers arrive and become
// followers.
//
// # Concurrency contract
//
// SyncGate is safe for concurrent use. [SyncGate.Reached] may be received from
// any goroutine and [SyncGate.Release] may be called from any goroutine and more
// than once; the gated Sync itself runs on whichever goroutine issued it.
type SyncGate struct {
	reached   chan struct{}
	release   chan struct{}
	reachOnce sync.Once
	once      sync.Once
	fired     atomic.Bool
}

// reach marks the gate entered and wakes whoever is waiting on
// [SyncGate.Reached]. The one-shot claim in [SimDisk.syncOutcome] already
// guarantees a single caller; the Once makes that independent of it.
func (g *SyncGate) reach() {
	g.reachOnce.Do(func() {
		g.fired.Store(true)
		close(g.reached)
	})
}

// Reached returns a channel closed when the gated Sync has been entered and is
// blocked. Receiving from it is how a controlling goroutine learns the leader is
// parked inside its fsync, rather than guessing with a sleep.
func (g *SyncGate) Reached() <-chan struct{} { return g.reached }

// Release unblocks the gated Sync, which then returns whatever outcome the other
// arms selected for it (a [SimDisk.ArmSyncFaultAt] fault, an ENOSPC, or success).
// It is idempotent and safe to call even if the gate was never reached.
func (g *SyncGate) Release() { g.once.Do(func() { close(g.release) }) }

// Fired reports whether the gated Sync was actually entered. It is the
// reachability observable the rename arms established (rmp #2465): an ordinal
// that never matched is a silent no-op, and a scenario that depends on the gate
// firing must be able to tell "the gate held the leader" from "the gate was
// never reached" rather than misreading the resulting timing as an engine
// property.
func (g *SyncGate) Fired() bool { return g.fired.Load() }

// ArmSyncGateAt arms a ONE-SHOT rendezvous on the at-th [SimFileHandle.Sync]
// call on this disk, counted from the current [SimDisk.SyncCount]+1, and returns
// the gate. That Sync blocks — outside the disk lock — until
// [SyncGate.Release]; every other Sync is unaffected and the arm clears once it
// fires. A non-positive at disarms and returns nil.
//
// # Ordering against ArmSyncFaultAt
//
// Unlike [SimDisk.ArmSyncFaultAt] this does NOT reset the Sync counter, so that
// arming a gate never moves a fault ordinal already in place. To gate and fail
// the SAME Sync, arm the fault FIRST (it resets the counter) and the gate
// second, with the same ordinal.
//
// It draws nothing from the [Seed], so arming never perturbs the reproducible
// fault stream, and must be called from the controlling goroutine before the
// work that will trigger it begins.
func (d *SimDisk) ArmSyncGateAt(at int64) *SyncGate {
	d.mu.Lock()
	defer d.mu.Unlock()
	if at <= 0 {
		d.syncGate = nil
		d.syncGateAt = 0
		return nil
	}
	g := &SyncGate{reached: make(chan struct{}), release: make(chan struct{})}
	d.syncGate = g
	d.syncGateAt = d.syncCount + at
	return g
}

// simFile is the in-memory backing store for one path. data holds the file
// bytes; faulted marks, per sector index, whether writes into that sector are
// corrupted.
//
// direntDurable models POSIX directory-entry durability: it is the in-memory
// analogue of whether the name->inode link of this path is on stable storage.
// A create or rename links a name with direntDurable=false; only a
// [SimDisk.DirSync] of the containing directory (the fsync(2)-on-a-directory
// primitive the snapshot/csrfile publish protocol relies on) flips it true. A
// [SimDisk.Crash] drops every dirent that is not yet durable, exactly as a
// real crash within the kernel writeback window loses a rename whose parent
// directory was never fsync'd. The file's DATA durability is modelled
// separately by the per-Sync fault and the torn-write sector bitmap; this flag
// is only about the name's link surviving a crash.
type simFile struct {
	faulted map[int]bool
	data    []byte
	// durableData is the EXPLICIT durable image, materialised lazily and nil in
	// the common case. While it is nil the durable image is data[:durableLen] —
	// the O(1) watermark representation that costs an append-only writer such as
	// the WAL nothing per fsync. It is materialised (copy-on-write) the moment a
	// mutation would rewrite a byte the last successful Sync already placed on
	// stable storage, because from that instant the platter and the page cache
	// disagree and a watermark can no longer describe both.
	durableData []byte
	// durableLen is the number of leading bytes of data that a successful
	// [SimFileHandle.Sync] has placed on stable storage. It is meaningful only
	// while durableData is nil; use [simFile.durableSize] rather than reading it
	// directly. It advances ONLY when a Sync returns nil (rmp #2535).
	durableLen int64
	// tornTail* describe a PARTIAL trailing append that reached the platter
	// without a completed fsync: the write-back was in flight when the power
	// went. tornTailSet arms it; the record occupies [tornTailStart, tornTailEnd)
	// and only its first tornTailKeep bytes landed intact. See
	// [SimDisk.ArmTornAppendAt] for why this cannot emerge from the durable
	// shadow alone.
	tornTailSet   bool
	tornTailStart int64
	tornTailKeep  int64
	tornTailEnd   int64
	direntDurable bool
}

// durableSize returns the length of this file's durable image — the number of
// bytes a [SimDisk.CrashHost] would leave behind.
func (f *simFile) durableSize() int64 {
	if f.durableData != nil {
		return int64(len(f.durableData))
	}
	if f.durableLen > int64(len(f.data)) {
		return int64(len(f.data))
	}
	return f.durableLen
}

// durableImage returns the bytes a [SimDisk.CrashHost] would leave in this file.
// The returned slice may ALIAS f.data (the watermark representation), so callers
// that keep it must copy.
func (f *simFile) durableImage() []byte {
	if f.durableData != nil {
		return f.durableData
	}
	return f.data[:f.durableSize()]
}

// preserveDurableBelow pins an explicit copy of the durable image when a
// mutation at byte offset off would otherwise rewrite bytes the last successful
// Sync already placed on stable storage.
//
// It is the copy-on-write half of the watermark representation, and it is what
// makes the cheap representation exact rather than merely append-only-correct:
// an append (off at or beyond the watermark) costs nothing, and only a write
// back into already-durable bytes pays for one copy, which is then reset to the
// watermark form by the next successful Sync.
func (f *simFile) preserveDurableBelow(off int64) {
	if f.durableData != nil || off >= f.durableSize() {
		return
	}
	n := f.durableSize()
	buf := make([]byte, n)
	copy(buf, f.data[:n])
	f.durableData = buf
}

// truncateDurableTo shortens the durable image to size.
//
// It models a truncation as INSTANTLY durable, which is the pre-existing
// behaviour of [SimFileHandle.Truncate], [SimDisk.TruncatePath] and O_TRUNC and
// is deliberately left unchanged here: making a truncation itself survivable —
// so that a crash restores the longer prior image — is audit finding F8, filed
// separately as rmp #2542. This is the single seam that change needs.
func (f *simFile) truncateDurableTo(size int64) {
	if f.durableData != nil {
		if int64(len(f.durableData)) > size {
			f.durableData = f.durableData[:size]
		}
		return
	}
	if f.durableLen > size {
		f.durableLen = size
	}
}

// markSyncedTo records that an fsync covering the first n bytes completed
// successfully, so those bytes are now on stable storage.
//
// n is the length captured when the fsync was ISSUED, not the length now: an
// fsync makes durable what the file held when it started, and frames a
// concurrent committer appended while it ran belong to the next round. That is
// the same watermark discipline [github.com/FlavioCFOliveira/GoGraph/store/wal.Writer]
// applies when leadGroupSyncLocked snapshots appendedSize before its own fsync.
//
// The watermark never moves backwards: with a gate armed, fsyncs can complete
// out of order, and an earlier, smaller round completing last must not un-harden
// what a later one already made durable.
func (f *simFile) markSyncedTo(n int64) {
	if n > int64(len(f.data)) {
		n = int64(len(f.data))
	}
	if n < 0 {
		n = 0
	}
	if n <= f.durableSize() {
		// The fsync covered a prefix already deemed durable. It still REFRESHES
		// that prefix's content — a durable byte rewritten in place and then
		// fsync'd is durable again — so the explicit image, if there is one,
		// takes the new bytes.
		if f.durableData != nil {
			copy(f.durableData[:n], f.data[:n])
		}
		return
	}
	// Everything up to n is now both durable and current, so the explicit image
	// is redundant and the file returns to the cheap watermark representation.
	f.durableData = nil
	f.durableLen = n
}

// revertToDurable replaces the live bytes with the durable image and returns how
// many written-but-never-synced bytes that discarded. It is what
// [SimDisk.CrashHost] applies to every file that survives the crash.
//
// The per-sector fault bitmap is deliberately NOT touched: it models the
// device's bad sectors, which a power failure does not repair, and leaving it
// alone keeps a host crash's effect confined to the byte image. (That
// [SimFileHandle.Truncate] does clear marks for vanished sectors is a separate,
// pre-existing modelling choice — audit finding F13.)
func (f *simFile) revertToDurable() int64 {
	img := f.durableImage()
	buf := make([]byte, len(img))
	copy(buf, img)

	// A TORN APPEND survives the revert, because it describes bytes that reached
	// the platter without a completed fsync — which is more than the durable
	// image holds, not less. The record's first tornTailKeep bytes are the real
	// payload; the remainder is GARBAGE rather than zeroes, because zero-fill is
	// an unrealistically benign model of an interrupted write-back (ALICE,
	// Pillai et al., OSDI '14) and a reader that only ever sees zeros can pass
	// while mishandling the bytes a real platter would hold.
	//
	// It is applied only when it would EXTEND the durable image. A torn tail
	// below the durable watermark would mean discarding bytes an fsync already
	// hardened, which is not what an interrupted write-back does.
	if f.tornTailSet {
		if head := f.tornTailStart + f.tornTailKeep; head >= int64(len(buf)) &&
			f.tornTailEnd <= int64(len(f.data)) {
			out := make([]byte, f.tornTailEnd)
			copy(out, f.data[:head])
			fillGarbage(out[head:f.tornTailEnd], head)
			buf = out
		}
		f.tornTailSet = false
	}

	discarded := int64(len(f.data)) - int64(len(buf))
	f.data = buf
	f.durableData = nil
	f.durableLen = int64(len(buf))
	return discarded
}

// fillGarbage writes deterministic non-zero bytes into dst, as the residue of an
// interrupted write-back. off is the file offset dst starts at, so the pattern is
// a pure function of position and a run is reproducible without drawing from the
// disk's fault stream — the same rule [SimDisk.SetCapacity] follows.
//
// It never produces a zero byte: a zero is exactly the value a benign model would
// leave, and the point of this residue is to be the value a benign model would
// not.
func fillGarbage(dst []byte, off int64) {
	x := uint64(off)*0x9E3779B97F4A7C15 + 0xDEAD_BEEF_CAFE_F00D
	for i := range dst {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		b := byte(x >> 24)
		if b == 0 {
			b = 0xA5
		}
		dst[i] = b
	}
}

// direntCrashSeedMix decorrelates the dirent crash-outcome sub-stream from the
// disk's own fault stream, so the two draw independently from the same run seed.
// It follows the derivation convention every [NewSimDisk] call site already uses
// (NewSeed(cfg.Seed^diskSeedMix)).
//
// Its VALUE is unchanged from when the sub-stream carried renames alone (rmp
// #2514), so a seed that drew a given sequence of crash outcomes still draws that
// same sequence; what changed with rmp #2536 is only how many records a given
// crash has to adjudicate.
const direntCrashSeedMix uint64 = 0x2514_A70D_5E11_C0DE

// direntUndoKind identifies which operation a [direntUndo] reverses. The two
// kinds share one log and one durable-prefix draw (see the direntUndos field
// docs on [SimDisk]) and differ only in how a record is undone and in how it is
// recognised as durable.
type direntUndoKind uint8

const (
	// direntUndoRename reverses a [SimDisk.Rename]; the payload is rec.rename.
	direntUndoRename direntUndoKind = iota
	// direntUndoUnlink reverses a [SimDisk.Remove] or [SimDisk.RemoveAll]; the
	// payload is rec.unlink.
	direntUndoUnlink
)

// direntUndo is one entry of the shared ordered undo log: a directory-entry
// mutation whose effect has not yet reached stable storage, tagged with the kind
// of operation that produced it.
//
// The two payloads are held inline rather than behind a pointer or an interface:
// the log is bounded by the number of metadata operations inside one crash
// window (single digits in every scenario the harness drives), so the untaken
// payload costs nothing that matters, and keeping the concrete types visible
// keeps each one's own documentation and undo function where they belong.
type direntUndo struct {
	rename renameUndo
	unlink unlinkUndo
	kind   direntUndoKind
}

// pinsRollback reports whether an arm has pinned this record to the rolled-back
// branch, which lowers the durable-prefix boundary to it (see
// [SimDisk.rollbackDirentUndosLocked]).
func (u *direntUndo) pinsRollback() bool {
	if u.kind == direntUndoUnlink {
		return u.unlink.forceRestore
	}
	return u.rename.forceRollback
}

// unlinkUndo is one removal whose unlink has not yet reached stable storage,
// recorded so [SimDisk.Crash] can put the removed name — or the whole removed
// subtree — back.
//
// It exists because unlink(2) and rmdir(2) mutate a DIRECTORY, which puts them in
// the same durability class as the create and the rename this model already
// treats carefully. Neither POSIX nor the Linux man pages address removal
// durability directly; the applicable rule is the general one in Linux
// fsync(2) — an fsync of a file "does not necessarily ensure that the entry in
// the directory containing the file has also reached disk" — strengthened by
// POSIX Issue 8 XBD §3.368, which states that "an operation that modifies a
// directory is considered to be a write operation, and a directory's entries are
// considered to be the data read or written", bringing directory-entry changes,
// removals included, formally inside synchronized I/O. Two reference
// implementations treat a deletion as a durability event outright: RocksDB gives
// it its own reason code (DirFsyncOptions::FsyncReason::kFileDeleted, and
// PosixDirectory::FsyncWithDirOptions really does fsync the directory for it,
// even on btrfs where it elides the fsync for a newly synced file), and SQLite
// makes a deletion the commit point itself (atomiccommit.html §3.11: deleting the
// rollback journal "is the instant where the transaction commits", with §9.3
// flagging that assumption as load-bearing). ALICE decomposes unlink into
// "delete dir entry + truncate if last link", so a removal is not even atomic in
// its default persistence model.
//
// Unlike a rename, a removal has NO illegal crash outcome: the name is either
// still there or gone, and both are states a filesystem produces. That is why
// there is no unlink counterpart to [SimDisk.ArmRenameRevokeBothForPath].
type unlinkUndo struct {
	// path is the name the caller removed. For [SimDisk.RemoveAll] it is the root
	// of the removed subtree.
	path string
	// files / dirs are the file and directory-durability entries the removal
	// deleted, captured before the deletion. The *simFile values are shared, not
	// cloned — an undo restores NAMES, and the inode a name points at is the same
	// object either way — which is also why no separate dirent-durability field
	// is needed for the files: each simFile carries its own, so restoring the
	// object restores the durability the name had at the instant of the unlink.
	files map[string]*simFile
	dirs  map[string]bool
	// prunedDirs holds the directory-durability entries the removal deleted
	// because unlinking this name emptied the directory that held it (see
	// [SimDisk.pruneGhostDirEntriesLocked]). Restoring the removal puts a name
	// back INTO that directory, so the entry is put back to describe it again,
	// with its original value. It is nil in the common case, where the directory
	// still holds other names. This is the record [SimDisk.Remove] discarded
	// until rmp #2536, when it had no undo record to put it on.
	//
	// Stated precisely, because the obvious justification for it is no longer
	// true: restoring the entry is NOT what keeps the crash from losing less than
	// a real one, and it is not observable in any post-crash state. Measured over
	// 96 seeds, for this record and for [renameUndo.prunedDirs] alike, suppressing
	// the restore leaves the durable image identical. The reason is that every
	// record which leaves the undo log now has its durability APPLIED
	// ([SimDisk.dropDirentUndoPrefixLocked]), so "a non-durable directory entry
	// with no live record behind it" — the state whose restore would have
	// mattered — is unreachable by construction. It is kept because the undo pass
	// walks the log backwards over LIVE state ([SimDisk.undoRenameLocked] re-reads
	// the current subtree), so an intermediate state that misdescribed a
	// directory's durability would be a trap for the next record's undo.
	prunedDirs map[string]bool
	// subtree distinguishes a [SimDisk.RemoveAll] from a [SimDisk.Remove]. It is
	// carried for diagnostics and for the shape observables; the undo itself is
	// the same map restore either way.
	subtree bool
	// forceRestore pins this record's outcome to the restored branch instead of
	// letting the seed choose ([SimDisk.ArmRemoveRollbackForPath]).
	forceRestore bool
}

// renameUndo is one rename whose new directory entry is not yet durable,
// recorded so [SimDisk.Crash] can put the filesystem back the way it was before
// that rename.
//
// It exists because a crash immediately after rename(2) has exactly TWO legal
// outcomes — the new name is there, or the OLD name is still there — and the
// second one cannot be produced by revoking the new dirent alone: the old name
// was deleted when the rename was applied, so without a record of it the crash
// loses both names, which no filesystem does (rmp #2514).
//
// The undo restores names, never data. A rolled-back rename leaves the file's
// bytes exactly as they are: the inode is the same object either way, and what
// reached stable storage inside it is governed by the per-Sync fault and the
// torn-write sector bitmap, not by which name points at it.
type renameUndo struct {
	// oldPath / newPath are the rename's source and destination.
	oldPath string
	newPath string
	// replacedFiles / replacedDirs are the destination entries this rename
	// unlinked by replacing them (rename(2) replaces an existing destination).
	// Rolling the rename back must put them back, because the rename that
	// removed them never happened. Both are nil when the destination was empty,
	// which is the common case in the publish protocol.
	replacedFiles map[string]*simFile
	replacedDirs  map[string]bool
	// oldDurable is the dirent durability the SOURCE name carried at the instant
	// of the rename. Restoring it — rather than restoring the name as durable —
	// is what keeps the "both names lost" outcome available in the one case
	// where it IS legal: a source whose own name had never been fsync'd was
	// never crash-survivable, so the ordinary revoke pass drops it again.
	oldDurable bool
	// oldDirTracked reports whether d.dirs held an entry for oldPath before the
	// rename. A directory absent from d.dirs is implicitly durable, so restoring
	// "absent" and restoring "false" are different states and must not be
	// conflated.
	oldDirTracked bool
	// prunedDirs holds the directory-durability entries this rename deleted
	// because moving the name emptied the directory that held it (see
	// [SimDisk.pruneGhostDirEntriesLocked]). Rolling the rename back puts a name
	// back INTO that directory, so the entry is put back to describe it again,
	// with its original value. It is nil in the common case, where the directory
	// still holds other names.
	//
	// Its original justification — that without the restore a host crash would
	// keep a subtree whose own name never reached stable storage — was measured
	// FALSE once pinning became coherent, and the reasoning is set out in full on
	// [unlinkUndo.prunedDirs]: the restore changes no post-crash state, and the
	// field is kept only because the undo pass walks the log backwards over live
	// state, so an intermediate entry that misdescribed a directory's durability
	// would be a trap for the next record's undo.
	prunedDirs map[string]bool
	// dirRename distinguishes a whole-subtree rename from a single-file one.
	dirRename bool
	// forceRollback pins this record's outcome to the rolled-back branch instead
	// of letting the seed choose ([SimDisk.ArmRenameRollbackForPath]).
	forceRollback bool
	// revokeBoth additionally suppresses the restore of the old name, producing
	// the physically IMPOSSIBLE outcome on purpose
	// ([SimDisk.ArmRenameRevokeBothForPath]).
	revokeBoth bool
}

// NewSimDisk returns an empty in-memory filesystem. faultRate is the
// probability (clamped to [0,1]) that any individual Sync fails with
// [ErrSimFault] and that a freshly written sector is marked faulted. seed
// drives every fault decision so the fault sequence is reproducible.
func NewSimDisk(seed *Seed, faultRate float64) *SimDisk {
	if faultRate < 0 {
		faultRate = 0
	}
	if faultRate > 1 {
		faultRate = 1
	}
	return &SimDisk{
		files:            make(map[string]*simFile),
		faultRate:        faultRate,
		seed:             seed,
		dirs:             make(map[string]bool),
		removeHitsByPath: make(map[string]int64),
		// Derived sub-stream for the dirent crash-outcome choice: a pure
		// function of the same seed value, drawn independently of the fault
		// stream (see the direntSeed field docs).
		direntSeed: NewSeed(seed.Value() ^ direntCrashSeedMix),
	}
}

// FaultRate returns the per-sector / per-Sync fault probability the disk was
// constructed with (see [NewSimDisk]). It is an observability accessor for
// tests that assert the [DiskConfig.FaultRate] wiring in [New]; faultRate is
// immutable after construction, so it reads it without the mutex and mutates
// nothing.
func (d *SimDisk) FaultRate() float64 { return d.faultRate }

// CorruptRange deterministically corrupts n bytes of the ALREADY-DURABLE image
// of the file at path, starting at byte offset off, by flipping every byte
// (XOR 0xFF). It is the direct sector-corruption injector for bytes that are
// already on stable storage — the [SimFileHandle.Write] fault path only
// corrupts sectors as they are written, so it cannot damage a frame that was
// durably committed in an earlier session. Flipping a byte inside a committed
// WAL frame's header or payload makes that frame fail its CRC32C check on the
// next replay, modelling a bad disk sector under a durable frame.
//
// It draws NOTHING from the [Seed] and holds only [SimDisk]'s own mutex, so it
// never perturbs the reproducible fault stream. It returns an error wrapping
// fs.ErrNotExist when the file is absent and a range error when [off, off+n)
// does not lie wholly within the file. It must be called from the controlling
// goroutine while no handle is mid-write.
func (d *SimDisk) CorruptRange(path string, off int64, n int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	f, ok := d.files[path]
	if !ok {
		return &fs.PathError{Op: "corrupt", Path: path, Err: fs.ErrNotExist}
	}
	if off < 0 || n <= 0 || off+int64(n) > int64(len(f.data)) {
		return fmt.Errorf("sim: CorruptRange out of range: off=%d n=%d len=%d", off, n, len(f.data))
	}
	// Corrupt the DURABLE image as well as the live one. Without this the
	// injector would be silently undone by the next [SimDisk.CrashHost], which
	// restores the durable image over the live bytes — turning a bad-sector
	// scenario into a no-op. When durableData is nil the durable image aliases
	// data, so the loop below has already covered it; only the materialised
	// representation needs the second pass.
	for i := int64(0); i < int64(n); i++ {
		f.data[off+i] ^= 0xFF
	}
	if f.durableData != nil {
		for i := int64(0); i < int64(n) && off+i < int64(len(f.durableData)); i++ {
			f.durableData[off+i] ^= 0xFF
		}
	}
	return nil
}

// MarkDataDurable declares the CURRENT contents of the file at path to be on
// stable storage, advancing its durable watermark to the live length without
// issuing a Sync. It returns an error wrapping fs.ErrNotExist when the file is
// absent.
//
// It is the counterpart of [SimDisk.CorruptRange]: a harness primitive that
// edits the durable image directly, for a scenario that SUBSTITUTES a crafted
// component for a published one and needs the substitution to be what a crash
// leaves behind. Going through [SimFileHandle.Sync] instead would draw from the
// [Seed] — perturbing the reproducible fault stream — and could itself be failed
// by the injector, turning a fixture step into a spurious error.
//
// It is NOT a way for engine code to obtain durability it did not fsync for.
// Nothing outside a test fixture should call it; the durability an engine has is
// the durability its Syncs earned.
//
// It draws NOTHING from the [Seed] and holds only [SimDisk]'s own mutex, so it
// never perturbs the reproducible fault stream.
func (d *SimDisk) MarkDataDurable(path string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	f, ok := d.files[path]
	if !ok {
		return &fs.PathError{Op: "markdurable", Path: path, Err: fs.ErrNotExist}
	}
	f.markSyncedTo(int64(len(f.data)))
	return nil
}

// SetCapacity bounds the disk to capacityBytes total bytes across all files,
// modelling a finite disk. When enospcOnSync is false the out-of-space
// condition surfaces eagerly at the growing Write/Truncate; when true it
// surfaces at Sync (delayed allocation). A capacityBytes of 0 removes the bound
// (the default). It must be called before the store is driven, from the single
// simulation goroutine; it draws nothing from the seed, so it never perturbs
// the reproducible fault stream. See the field docs on [SimDisk] for the model.
func (d *SimDisk) SetCapacity(capacityBytes int64, enospcOnSync bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if capacityBytes < 0 {
		capacityBytes = 0
	}
	d.capacityBytes = capacityBytes
	d.enospcOnSync = enospcOnSync
}

// ArmSyncFaultAt schedules a ONE-SHOT durability fault: the at-th
// [SimFileHandle.Sync] call on this disk (counting from the current
// [SimDisk.SyncCount]+1) returns [ErrSimFault], then the arm clears so no
// further Sync is affected. It resets the Sync counter to zero so `at` is
// counted from the moment of arming, letting a scenario arm "the K-th commit's
// fsync" right before it starts issuing. A non-positive at disarms.
//
// It models an fsync failure on a chosen durable commit: the faulted Sync
// poisons the WAL writer, which discards its un-synced suffix and fails the
// commit (the client sees a wire FAILURE, never an ack) while every earlier
// acked commit stays durable.
//
// The faulted commit is not necessarily the ONLY one that fails. A commit's WAL
// fsync is a [wal.Writer.SyncGroup] round, and through the engine that round may
// carry followers (measured — see the group-commit note in
// durable_scenarios.go), in which case the poison fails every member of the
// group by design. This doc claimed the round was "always a solo leader" until
// rmp #2471; it is not, and a scenario must therefore treat the set of failed
// commits as a set rather than a singleton. It draws nothing from the [Seed], so arming never
// perturbs the reproducible fault stream. It must be called from the controlling
// goroutine before the workload that will trigger it begins.
func (d *SimDisk) ArmSyncFaultAt(at int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.syncCount = 0
	if at <= 0 {
		d.syncFaultArmed = false
		return
	}
	d.syncFaultArmed = true
	d.syncFaultAt = at
}

// ArmTornAppendAt schedules a ONE-SHOT TORN APPEND: the at-th growing write on
// this disk (counting from the current append total) is marked so that the next
// crash leaves only its first keepBytes intact, with the remainder of that
// record filled with garbage. A non-positive at disarms.
//
// # Why this cannot emerge from the durable shadow
//
// A truncated trailing frame is the single most important crash state a
// write-ahead log must survive, and before this the simulator could not produce
// one by any route (audit finding F7, rmp #2541). The durable shadow added for
// F1 does not deliver it: [SimDisk.CrashHost] reverts each file to
// data[:durableLen], and durableLen moves only on a SUCCESSFUL Sync — so a crash
// truncates a file at its last sync boundary, which for a WAL is a FRAME
// boundary. That is a cleanly LOST trailing frame, never a partial one, and the
// two exercise different branches of recovery: a reader that handles a clean
// truncation can still mishandle a frame whose header declares one length and
// whose body stops short.
//
// The short-count contract in [SimFileHandle.Write] makes a partial record
// emergent under a full disk; this arm makes one reachable at any capacity and
// at a chosen record, which is what a targeted recovery scenario needs.
//
// The kept bytes are the real payload and the remainder is GARBAGE rather than
// zeroes, per the ALICE finding that zero-fill is an unrealistically benign
// model of an interrupted write-back. It draws nothing from the seed, so it
// never perturbs the reproducible fault stream.
func (d *SimDisk) ArmTornAppendAt(at, keepBytes int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.appendCount = 0
	if at <= 0 {
		d.tornAppendArmed = false
		return
	}
	if keepBytes < 0 {
		keepBytes = 0
	}
	d.tornAppendArmed = true
	d.tornAppendAt = at
	d.tornAppendKeep = keepBytes
}

// AppendCount returns the number of growing writes performed across every handle
// of this disk since the last [SimDisk.ArmTornAppendAt] reset (or since
// construction). A scenario reads it to choose which append to tear.
func (d *SimDisk) AppendCount() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.appendCount
}

// SyncCount returns the number of [SimFileHandle.Sync] calls performed across
// every handle of this disk since the last [SimDisk.ArmSyncFaultAt] reset (or
// since construction). A concurrent scenario reads it to gate a teardown on
// durable progress (a bounded condition wait) rather than on wall-clock time.
func (d *SimDisk) SyncCount() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.syncCount
}

// totalBytesLocked returns the sum of every file's data length. The caller must
// hold d.mu. It is O(files); the simulated store holds only a handful of files
// (the WAL plus the checkpoint components), so the linear scan is negligible and
// avoids the bookkeeping-drift risk of a running counter.
func (d *SimDisk) totalBytesLocked() int64 {
	var total int64
	for _, f := range d.files {
		total += int64(len(f.data))
	}
	return total
}

// enospc builds the ENOSPC [os.PathError] the eager and delayed paths return.
// It matches the shape internal/testfs uses, so both errors.Is(err,
// syscall.ENOSPC) and the WAL's errno classifier recognise it.
func enospc(op, path string) error {
	return &os.PathError{Op: op, Path: path, Err: syscall.ENOSPC}
}

// wouldExceedLocked reports whether growing a file from oldLen to newLen bytes
// would push the disk's total past its capacity. It is a pure function of the
// current byte total (no seed draw). With no capacity, or in delayed-allocation
// mode (where growth is always buffered and Sync enforces the budget), it never
// blocks a write. The caller must hold d.mu.
func (d *SimDisk) wouldExceedLocked(oldLen, newLen int64) bool {
	if d.capacityBytes <= 0 || d.enospcOnSync {
		return false
	}
	if newLen <= oldLen {
		return false
	}
	return d.totalBytesLocked()-oldLen+newLen > d.capacityBytes
}

// OpenFile opens (creating when os.O_CREATE is set) the file at path and
// returns a handle positioned per the flags: at end when os.O_APPEND is set, at
// zero otherwise. When os.O_TRUNC is set the file's contents are discarded. It
// returns an error wrapping fs.ErrNotExist when the file is absent and
// os.O_CREATE is not set.
func (d *SimDisk) OpenFile(path string, flag int) (*SimFileHandle, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	f, ok := d.files[path]
	if !ok {
		if flag&os.O_CREATE == 0 {
			return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
		}
		// A freshly linked name's dirent is not yet durable: it survives a
		// crash only after a DirSync of its parent directory. The exception is
		// a root-level file (parent ".") such as the WAL: those are governed by
		// the long-standing data-durability model (per-Sync fault), not the
		// dirent model the snapshot/csrfile publish protocol exercises, so they
		// are treated as durably linked on creation to keep WAL-only crash
		// recovery byte-compatible with Phase 2.
		f = &simFile{faulted: make(map[int]bool), direntDurable: isRootLevel(path)}
		d.files[path] = f
	}
	if flag&os.O_TRUNC != 0 {
		f.data = f.data[:0]
		f.faulted = make(map[int]bool)
		// The truncation is modelled as instantly durable, exactly as it was
		// before the durable shadow existed; see [simFile.truncateDurableTo].
		f.truncateDurableTo(0)
	}
	h := &SimFileHandle{disk: d, file: f, path: path, crashGen: d.crashGen}
	if flag&os.O_APPEND != 0 {
		h.pos = int64(len(f.data))
	}
	return h, nil
}

// Rename atomically moves oldPath to newPath, replacing any existing
// destination. It handles both a single file (oldPath is a file key) and a
// directory (oldPath has child keys prefixed oldPath+"/"): the snapshot
// publish protocol renames a whole staging directory onto the live name, so a
// directory rename must move every child key, re-rooting its prefix. The moved
// dirent(s) become NOT durable — only a [SimDisk.DirSync] of the new parent
// makes the new name crash-survivable — which is what lets the simulator crash
// in the publish window between the rename and the parent-dir fsync. It returns
// an error wrapping fs.ErrNotExist when the source is absent.
//
// # Crash outcome
//
// A rename whose new name is not yet durable is recorded in an undo log (see
// [renameUndo]) so that a [SimDisk.Crash] lands on one of the TWO outcomes a
// real filesystem can produce — the new name, or the old name — rather than on
// the impossible third one of losing both. Which of the two a given crash
// selects is chosen deterministically from the seed unless an arm pins it
// ([SimDisk.ArmRenameWritebackForPath], [SimDisk.ArmRenameRollbackForPath],
// [SimDisk.ArmRenameRevokeBothForPath]).
func (d *SimDisk) Rename(oldPath, newPath string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// One-shot injected failure of the rename(2) call itself (see
	// [SimDisk.ArmRenameFaultForPath]). It fires before the source is looked up,
	// so it models the syscall failing rather than a missing source.
	if d.renameFaultArmed && d.renameFaultPath == newPath {
		d.renameFaultArmed = false
		d.renameFaultPath = ""
		d.renameFaults++
		return ErrSimFault
	}
	// One-shot selection of the "this rename reached stable storage" branch of
	// the crash window (see [SimDisk.ArmRenameWritebackForPath]).
	durable := isRootLevel(newPath)
	if d.renameWritebackArmed && d.renameWritebackPath == newPath {
		d.renameWritebackArmed = false
		d.renameWritebackPath = ""
		d.renameWritebacks++
		durable = true
	}
	// One-shot pinning of the OTHER legal branch, and of the deliberately
	// illegal one. Both are consumed here even when the rename turns out to be
	// durable (and therefore records nothing), so an arm is one-shot in the same
	// sense as every other arm on this disk.
	rec := renameUndo{oldPath: oldPath, newPath: newPath}
	if d.renameRollbackArmed && d.renameRollbackPath == newPath {
		d.renameRollbackArmed = false
		d.renameRollbackPath = ""
		d.renameRollbacks++
		rec.forceRollback = true
	}
	if d.renameRevokeArmed && d.renameRevokePath == newPath {
		d.renameRevokeArmed = false
		d.renameRevokePath = ""
		d.renameRevokes++
		rec.forceRollback = true
		rec.revokeBoth = true
	}
	// pin marks the records this rename invalidates. A rename consumes BOTH
	// names, so any pending record that created either of them — or an ancestor
	// of them — can no longer be rolled back without resurrecting a path this
	// call has just moved away; those records, and every record before them, go
	// into the durable prefix. It runs only once the rename is known to succeed:
	// a call that returns an error moved nothing and must therefore make nothing
	// durable.
	pin := func() {
		d.pinDirentUndosLocked(oldPath)
		d.pinDirentUndosLocked(newPath)
	}

	if f, ok := d.files[oldPath]; ok {
		// Single-file rename: replace any destination, link the new name with
		// a not-yet-durable dirent (unless root-level — see [isRootLevel] and
		// the rationale in OpenFile — so the WAL's own renames stay governed by
		// the data-durability model, not the dirent model).
		pin()
		delete(d.files, oldPath)
		if prev, replaced := d.files[newPath]; replaced {
			rec.replacedFiles = map[string]*simFile{newPath: prev}
		}
		rec.oldDurable = f.direntDurable
		f.direntDurable = durable
		d.files[newPath] = f
		// Moving the last name out of a directory published by rename empties
		// that directory, so its own entry must go with it or it becomes the
		// ghost of rmp #2538 — measured destroying a fully durable file at this
		// exact site. It runs AFTER the destination is installed, so a
		// same-directory rename (every rename the engine actually issues:
		// path+".tmp" -> path) leaves the directory populated and prunes nothing.
		rec.prunedDirs = d.pruneGhostDirEntriesLocked(oldPath)
		if durable {
			// This rename's dirent is already on stable storage, so by journal
			// order every pending mutation before it is too.
			d.pinAllDirentUndosLocked()
		} else {
			d.direntUndos = append(d.direntUndos, direntUndo{kind: direntUndoRename, rename: rec})
		}
		return nil
	}

	// Directory rename: move every child key under oldPath/ to newPath/,
	// preserving each child's own dirent durability — children travel inside
	// the moved directory inode, so a prior DirSync(oldPath) that made them
	// durable still holds. What is NOT yet durable is the directory's own name
	// in its parent: tracked in d.dirs, set false here and made durable only
	// by a later ParentDirSync(newPath). A crash before that parent fsync
	// drops the whole subtree (the rename is lost), exactly the window the
	// publish protocol's post-rename parent fsync closes.
	prefix := oldPath + "/"
	moved := make(map[string]*simFile)
	for p, f := range d.files {
		if strings.HasPrefix(p, prefix) {
			moved[newPath+"/"+p[len(prefix):]] = f
		}
	}
	if len(moved) == 0 {
		return &fs.PathError{Op: "rename", Path: oldPath, Err: fs.ErrNotExist}
	}
	// Directory entries nested inside the moved tree travel with it and keep
	// their own dirent durability, for the same reason the child files do: their
	// names live inside the moved inode, which this rename does not touch. They
	// are collected before anything is mutated, so a rename that returns an
	// error still moves nothing. Re-rooting them is also what makes the forward
	// rename symmetric with [SimDisk.undoRenameLocked], which already re-roots
	// them on the way back, and it stops the source path from keeping an entry
	// whose subtree has left (see [SimDisk.removeSubtreeLocked]).
	var movedDirs map[string]bool
	for dp, dur := range d.dirs {
		if strings.HasPrefix(dp, prefix) {
			if movedDirs == nil {
				movedDirs = make(map[string]bool)
			}
			movedDirs[newPath+"/"+dp[len(prefix):]] = dur
		}
	}
	rec.dirRename = true
	pin()
	// Capture the destination subtree BEFORE replace semantics drop it, so a
	// rolled-back rename can put it back.
	rec.replacedFiles, rec.replacedDirs = d.captureSubtreeLocked(newPath)
	// Drop any pre-existing destination subtree (replace semantics): both its
	// file keys and its directory-durability entries, since replacing a
	// directory unlinks the names inside it too.
	d.removeSubtreeLocked(newPath)
	rec.oldDurable, rec.oldDirTracked = d.dirs[oldPath]
	delete(d.dirs, oldPath)
	for p := range d.files {
		if strings.HasPrefix(p, prefix) {
			delete(d.files, p)
		}
	}
	for dp := range d.dirs {
		if strings.HasPrefix(dp, prefix) {
			delete(d.dirs, dp)
		}
	}
	for np, f := range moved {
		d.files[np] = f
	}
	for np, dur := range movedDirs {
		d.dirs[np] = dur
	}
	// Moving a whole subtree out of a published directory can empty it too; the
	// entry for oldPath itself is already gone above, so this catches only its
	// ancestors.
	rec.prunedDirs = d.pruneGhostDirEntriesLocked(oldPath)
	// The directory's own dirent under its parent is not durable until a
	// ParentDirSync(newPath); root-level directories are durably linked on
	// creation, mirroring root-level files (see [isRootLevel]). An armed
	// write-back (see above) links it durably here instead.
	d.dirs[newPath] = durable
	if durable {
		d.pinAllDirentUndosLocked()
	} else {
		d.direntUndos = append(d.direntUndos, direntUndo{kind: direntUndoRename, rename: rec})
	}
	return nil
}

// captureSubtreeLocked returns an independent copy of the file and directory
// entries at path and under path/, or (nil, nil) when there are none. It records
// the destination a rename is about to replace, so a rolled-back rename can put
// it back. The *simFile values are shared, not cloned: rolling a rename back
// restores NAMES, and the file objects those names point at are the same inodes
// either way. The caller holds d.mu.
func (d *SimDisk) captureSubtreeLocked(path string) (map[string]*simFile, map[string]bool) {
	var files map[string]*simFile
	var dirs map[string]bool
	prefix := path + "/"
	for p, f := range d.files {
		if p == path || strings.HasPrefix(p, prefix) {
			if files == nil {
				files = make(map[string]*simFile)
			}
			files[p] = f
		}
	}
	for dp, dur := range d.dirs {
		if dp == path || strings.HasPrefix(dp, prefix) {
			if dirs == nil {
				dirs = make(map[string]bool)
			}
			dirs[dp] = dur
		}
	}
	return files, dirs
}

// pinDirentUndosLocked pins into the durable prefix every pending RENAME whose
// new name is path, or lies under path — that is, every record whose undo would
// now resurrect a name this operation is about to move — together with every
// record BEFORE it, of either kind.
//
// Dropping the earlier records too is not incidental: a journalling filesystem
// commits its metadata in order, so the durable mutations are always a PREFIX of
// the issued ones. Keeping the log a suffix is what makes the crash outcomes
// compose — rolling back the publish rename of an archive/publish pair yields
// the stranded-backup state, and there is no interleaving in which the publish
// survives while the archive that made room for it does not.
//
// Only rename records are MATCHED. A removal's undo stays applicable when a later
// operation touches the same path, because the records are undone newest first:
// the later record is reversed before the earlier one, which puts the path back
// in the state that earlier undo expects. An unlink record therefore leaves the
// log only through the prefix rule, or when its own directory is fsync'd.
//
// The caller holds d.mu.
func (d *SimDisk) pinDirentUndosLocked(path string) {
	last := -1
	for i := range d.direntUndos {
		rec := &d.direntUndos[i]
		if rec.kind != direntUndoRename {
			continue
		}
		if rec.rename.newPath == path || strings.HasPrefix(rec.rename.newPath, path+"/") {
			last = i
		}
	}
	d.dropDirentUndoPrefixLocked(last)
}

// pruneDurableDirentUndosLocked drops every pending record whose effect has since
// become durable, and every record before it (see [SimDisk.pinDirentUndosLocked]
// for why the log stays a suffix). It is called after a directory fsync of dir,
// the only operation that makes a dirent durable.
//
// A rename is recognised as durable by inspecting the name it created. An unlink
// cannot be: the name is gone, so there is nothing left to carry a flag. What
// makes an unlink durable is an fsync of the directory the name was removed FROM,
// which is what dir is compared against here. The two recognitions have to live
// in the same pass, because the prefix rule spans both kinds.
//
// The caller holds d.mu.
func (d *SimDisk) pruneDurableDirentUndosLocked(dir string) {
	last := -1
	for i := range d.direntUndos {
		rec := &d.direntUndos[i]
		if rec.kind == direntUndoUnlink {
			if pathpkg.Dir(rec.unlink.path) == dir {
				last = i
			}
			continue
		}
		if rec.rename.dirRename {
			if dur, tracked := d.dirs[rec.rename.newPath]; tracked && dur {
				last = i
			}
			continue
		}
		if f, ok := d.files[rec.rename.newPath]; ok && f.direntDurable {
			last = i
		}
	}
	d.dropDirentUndoPrefixLocked(last)
}

// dropDirentUndoPrefixLocked removes records [0, last] from the log and APPLIES
// the durability that dropping them declares. A record with no undo left is a
// record whose effect a crash can no longer reverse, which is precisely the
// definition of durable, so the state has to say so too.
//
// Forgetting the record without hardening its dirent was a real defect, not a
// theoretical one: a chained rename (a -> b, then b -> c) pinned the first record
// while leaving b's dirent non-durable, so a crash that rolled back the second
// rename restored b and the ordinary revoke pass then deleted it — losing BOTH
// names of a rename, the exact outcome rmp #2514 exists to eliminate, reachable
// with no arming at all on 23 of 64 sampled seeds. The same hole opened through
// the prune path, where an fsync of one directory declares an earlier rename in a
// DIFFERENT directory durable by the prefix rule; and it would have opened a
// third time through rmp #2536, since a removal of a pending rename's destination
// pins that rename and the restored name would then have been dropped. Applying
// the durability at the single point where records leave the log closes all
// three. last < 0 means nothing matched. The caller holds d.mu.
// pinAllDirentUndosLocked declares the ENTIRE pending log durable. It is called
// when an operation's own effect is durable the instant it is issued — a
// root-level name (see [isRootLevel]) or an armed write-back
// ([SimDisk.ArmRenameWritebackForPath], [SimDisk.ArmRemoveWritebackForPath]) —
// because metadata reaches the journal in ORDER: if this mutation is on stable
// storage then every mutation issued before it is too.
//
// Without it a write-back arm manufactured the impossible interleaving it exists
// to avoid. Arming the publish rename of the snapshot protocol while the archive
// rename before it stayed pending left the crash free to roll the archive back
// while keeping the publish — a durable prefix with a hole in it, which no
// journalling filesystem produces — and it needed no second arm to do it, only
// the ordinary seed draw over the one remaining record. The caller holds d.mu,
// and must call it only once the operation is known to have succeeded: a call
// that returns an error mutated nothing and must therefore make nothing durable.
func (d *SimDisk) pinAllDirentUndosLocked() {
	d.dropDirentUndoPrefixLocked(len(d.direntUndos) - 1)
}

func (d *SimDisk) dropDirentUndoPrefixLocked(last int) {
	if last < 0 {
		return
	}
	for i := 0; i <= last; i++ {
		d.hardenDurableDirentLocked(&d.direntUndos[i])
	}
	d.direntUndos = append(d.direntUndos[:0:0], d.direntUndos[last+1:]...)
}

// hardenDurableDirentLocked marks the names a record created as crash-survivable,
// which is what "this record's effect reached stable storage" means in the state
// rather than merely in the log.
//
// For a rename it hardens the destination — for a DIRECTORY rename only the
// directory's OWN name, because the dirents inside it keep whatever durability
// they had: a staging tree published without its own directory fsync still loses
// its components, and a publish rename surviving never implies the component
// creates did. For an unlink there is nothing to harden: the name is absent, and
// a durable unlink is exactly an absence the crash can no longer undo. The caller
// holds d.mu.
func (d *SimDisk) hardenDurableDirentLocked(rec *direntUndo) {
	if rec.kind != direntUndoRename {
		return
	}
	if rec.rename.dirRename {
		if _, tracked := d.dirs[rec.rename.newPath]; tracked {
			d.dirs[rec.rename.newPath] = true
		}
		return
	}
	if f, ok := d.files[rec.rename.newPath]; ok {
		f.direntDurable = true
	}
}

// ArmRenameFaultForPath arms a ONE-SHOT fault on [SimDisk.Rename]: the next
// rename whose DESTINATION equals newPath returns [ErrSimFault] and moves
// nothing, then the arm clears so no further rename is affected. An empty
// newPath disarms. [SimDisk.RenameFaultCount] reports how many times it fired.
//
// It is the rename analogue of [SimDisk.ArmSyncFaultAt] and
// [SimDisk.ArmParentDirSyncFaultForPath], and closes the last gap in the
// snapshot publish protocol's fault surface: every other step of
//
//	write+fsync components -> fsync staging dir -> archive rename -> publish
//	rename -> fsync parent
//
// could already be made to fail under simulation, but the two renames could
// not, so the publish path's own archive-restore branch (store/snapshot, the
// best-effort Rename(bak, dir) after a failed publish rename) was unreachable.
// Arming the publish destination ("<dir>/snapshot") exercises that restore;
// arming the archive destination ("<dir>/snapshot.bak") aborts the publish
// before the live snapshot is touched at all.
//
// The fault fires before the source path is looked up, so it models the
// rename(2) call failing (EIO), not a missing source. It draws nothing from the
// [Seed], so arming never perturbs the reproducible fault stream, and it must be
// called from the controlling goroutine before the operation that will trigger
// it. It defaults disarmed: a [SimDisk] that never arms it behaves exactly as
// before.
func (d *SimDisk) ArmRenameFaultForPath(newPath string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if newPath == "" {
		d.renameFaultArmed = false
		d.renameFaultPath = ""
		return
	}
	d.renameFaultArmed = true
	d.renameFaultPath = newPath
}

// ArmRenameWritebackForPath arms a ONE-SHOT dirent WRITE-BACK on
// [SimDisk.Rename]: the next rename whose DESTINATION equals newPath links that
// destination with an ALREADY-DURABLE directory entry, as if the kernel had
// written the rename back to stable storage before the crash. The arm then
// clears. An empty newPath disarms. [SimDisk.RenameWritebackCount] reports how
// many times it fired.
//
// It exists because a crash immediately after a rename has TWO legal outcomes on
// a real filesystem — the rename reached stable storage, or it did not — and
// [SimDisk.Crash] models only the second (every not-yet-fsync'd dirent is
// revoked). That is sound but incomplete, and the incompleteness is
// load-bearing: the snapshot publish protocol issues two renames back to back
// with no fsync between them, so under the revoke-everything model a crash in
// the publish window drops BOTH the archived backup and the newly published
// snapshot. No real filesystem produces that outcome, and it is precisely the
// outcome recovery's interrupted-publish repair (store/recovery, promoting a
// stranded "<dir>/snapshot.bak" back to the live name) exists to handle — so
// without this arm that repair path is unreachable under simulation.
//
// Arming it on the archive destination therefore selects the "the archive
// rename reached disk, the publish rename did not" branch of the window, which
// is what strands a backup for recovery to promote.
//
// Firing it also declares every mutation still pending at that instant durable
// (see [SimDisk.pinAllDirentUndosLocked]), because metadata reaches the journal in
// order. Without that, arming the LATER rename of a pair — the publish rather
// than the archive — left the crash free to roll the earlier one back while
// keeping this one, a durable prefix with a hole in it that no filesystem
// produces. Arming the later step is therefore no longer a way to construct an
// impossible interleaving; it simply pins more of the window than intended, which
// [SimDisk.PendingRenameCount] makes visible.
//
// It draws nothing from the [Seed], so arming never perturbs the reproducible
// fault stream, and it must be called from the controlling goroutine before the
// rename it targets. It defaults disarmed: a [SimDisk] that never arms it
// behaves exactly as before.
func (d *SimDisk) ArmRenameWritebackForPath(newPath string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if newPath == "" {
		d.renameWritebackArmed = false
		d.renameWritebackPath = ""
		return
	}
	d.renameWritebackArmed = true
	d.renameWritebackPath = newPath
}

// RenameFaultCount returns how many armed rename faults actually fired since
// construction (see [SimDisk.ArmRenameFaultForPath]). A scenario reads it to
// prove the fault it armed was really reached rather than silently ignored
// because the destination never matched.
func (d *SimDisk) RenameFaultCount() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.renameFaults
}

// RenameWritebackCount returns how many armed rename write-backs actually fired
// since construction (see [SimDisk.ArmRenameWritebackForPath]). A scenario reads
// it to prove the crash window it selected was really entered.
func (d *SimDisk) RenameWritebackCount() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.renameWritebacks
}

// ArmRenameRollbackForPath arms a ONE-SHOT selection of the ROLLED-BACK branch
// of a rename's crash window: the next rename whose DESTINATION equals newPath
// is pinned so that a subsequent [SimDisk.Crash] undoes it — the new name goes
// away and the OLD name comes back with the dirent durability it had before the
// rename. The arm then clears. An empty newPath disarms.
// [SimDisk.RenameRollbackCount] reports how many times it fired.
//
// It is the exact counterpart of [SimDisk.ArmRenameWritebackForPath]: together
// the two pin the two outcomes a crash immediately after rename(2) can produce.
// Neither is needed for the model to be sound — an unarmed rename gets a
// seed-chosen outcome from the same two — but a test that must assert ONE of
// them unconditionally needs the choice pinned rather than sampled.
//
// Because the durable renames of a journalling filesystem are always a prefix of
// the issued ones, pinning a rename to the rolled-back branch also rolls back
// every LATER pending rename; it never rolls back an earlier one.
//
// It draws nothing from the [Seed] or from the rename sub-stream, so arming
// never perturbs either reproducible stream, and it must be called from the
// controlling goroutine before the rename it targets.
func (d *SimDisk) ArmRenameRollbackForPath(newPath string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if newPath == "" {
		d.renameRollbackArmed = false
		d.renameRollbackPath = ""
		return
	}
	d.renameRollbackArmed = true
	d.renameRollbackPath = newPath
}

// ArmRenameRevokeBothForPath arms a ONE-SHOT PHYSICALLY IMPOSSIBLE crash
// outcome on the next rename whose DESTINATION equals newPath: the crash revokes
// the new name AND does not restore the old one, so both names are lost. The arm
// then clears. An empty newPath disarms. [SimDisk.RenameRevokeBothCount] reports
// how many times it fired.
//
// No filesystem produces this outcome. rename(2) is atomic, so a crash leaves
// either the new name or the old one; losing both would mean the file was
// unlinked, which is not a partial outcome of a rename. It was nevertheless the
// simulator's DEFAULT until rmp #2514 — [SimDisk.Crash] revoked every
// un-fsync'd dirent and nothing put the source name back — which made the
// snapshot publish protocol look as if a crash between its two renames could
// destroy both copies of the graph, and made recovery's promote repair
// unreachable under simulation.
//
// It survives as an explicitly armed fault for exactly one purpose: testing the
// harness itself — proving that an oracle which would have accepted the
// impossible outcome now rejects it. Never arm it to reach a durable state a
// scenario needs; use [SimDisk.ArmRenameRollbackForPath] for that.
//
// It draws nothing from the [Seed] or from the rename sub-stream, and must be
// called from the controlling goroutine before the rename it targets.
func (d *SimDisk) ArmRenameRevokeBothForPath(newPath string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if newPath == "" {
		d.renameRevokeArmed = false
		d.renameRevokePath = ""
		return
	}
	d.renameRevokeArmed = true
	d.renameRevokePath = newPath
}

// RenameRollbackCount returns how many armed rename rollbacks actually fired
// since construction (see [SimDisk.ArmRenameRollbackForPath]). A scenario reads
// it to prove the branch it pinned was really reached rather than silently
// ignored because the destination never matched.
func (d *SimDisk) RenameRollbackCount() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.renameRollbacks
}

// RenameRevokeBothCount returns how many armed impossible-outcome revocations
// actually fired since construction (see [SimDisk.ArmRenameRevokeBothForPath]).
func (d *SimDisk) RenameRevokeBothCount() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.renameRevokes
}

// ArmRemoveWritebackForPath arms a ONE-SHOT unlink WRITE-BACK: the next
// [SimDisk.Remove] or [SimDisk.RemoveAll] of path is treated as having reached
// stable storage immediately, so no [SimDisk.Crash] can bring the name back. The
// arm then clears. An empty path disarms. [SimDisk.RemoveWritebackCount] reports
// how many times it fired.
//
// It pins the "the removal stuck" branch of the crash window, and is the exact
// counterpart of [SimDisk.ArmRemoveRollbackForPath]. Neither is needed for the
// model to be sound — an unarmed removal gets a seed-chosen outcome from the same
// two — but a test that must assert ONE of them unconditionally needs the choice
// pinned rather than sampled. Pinning it here rather than by fsyncing the parent
// directory is what keeps the rest of the scenario's dirents untouched: a
// [SimDisk.DirSync] would harden every name in that directory too, so the
// assertion would no longer be about the removal.
//
// Firing it also declares every mutation still pending at that instant durable
// (see [SimDisk.pinAllDirentUndosLocked]): an unlink that reached the journal
// implies everything issued before it did, so pinning the prefix is what stops
// this arm from producing the one interleaving the shared undo log exists to
// forbid — an unlink kept while a rename issued before it is rolled back.
//
// It draws nothing from the [Seed] or from the dirent sub-stream, so arming never
// perturbs either reproducible stream, and it must be called from the controlling
// goroutine before the removal it targets.
func (d *SimDisk) ArmRemoveWritebackForPath(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if path == "" {
		d.removeWritebackArmed = false
		d.removeWritebackPath = ""
		return
	}
	d.removeWritebackArmed = true
	d.removeWritebackPath = path
}

// ArmRemoveRollbackForPath arms a ONE-SHOT selection of the RESTORED branch of an
// unlink's crash window: the next [SimDisk.Remove] or [SimDisk.RemoveAll] of path
// is pinned so that a subsequent [SimDisk.Crash] puts the name — or the whole
// removed subtree — back, each entry with the dirent durability it carried at the
// instant of the removal. The arm then clears. An empty path disarms.
// [SimDisk.RemoveRollbackCount] reports how many times it fired.
//
// It is the arm that reaches the engine's leftover-artefact tolerance FROM THE
// REMOVAL SIDE, which no other primitive can: recovery's best-effort
// RemoveAll("<dir>/snapshot.tmp") and the publish path's stale-backup
// RemoveAll("<dir>/snapshot.bak") both exist to tolerate an artefact a previous
// crash left behind, and before rmp #2536 the harness could only reach that state
// by crashing BEFORE the removal, never by the removal failing to reach disk —
// which is the case a real deployment hits. [SimDisk.RemoveHitCountForPath] is
// how a scenario then proves the tolerant cleanup really found something rather
// than running as a no-op.
//
// Because the durable metadata mutations of a journalling filesystem are always a
// prefix of the issued ones, pinning a removal to the restored branch also rolls
// back every LATER pending record, rename or removal alike; it never rolls back an
// earlier one.
//
// It draws nothing from the [Seed] or from the dirent sub-stream, and must be
// called from the controlling goroutine before the removal it targets.
func (d *SimDisk) ArmRemoveRollbackForPath(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if path == "" {
		d.removeRollbackArmed = false
		d.removeRollbackPath = ""
		return
	}
	d.removeRollbackArmed = true
	d.removeRollbackPath = path
}

// RemoveWritebackCount returns how many armed unlink write-backs actually fired
// since construction (see [SimDisk.ArmRemoveWritebackForPath]). A scenario reads
// it to prove the branch it pinned was really reached rather than silently
// ignored because the path never matched.
func (d *SimDisk) RemoveWritebackCount() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.removeWritebacks
}

// RemoveRollbackCount returns how many armed unlink rollbacks actually fired
// since construction (see [SimDisk.ArmRemoveRollbackForPath]). A scenario reads
// it to prove the branch it pinned was really reached rather than silently
// ignored because the path never matched.
func (d *SimDisk) RemoveRollbackCount() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.removeRollbacks
}

// RemoveHitCount returns how many removals actually unlinked at least one
// existing name since construction. A removal of an absent path is a no-op and is
// not counted.
//
// It is the observable that separates a tolerant cleanup which really cleaned
// something from one that ran as a no-op — the difference between demonstrating
// that an engine branch handled a leftover artefact and merely asserting it.
func (d *SimDisk) RemoveHitCount() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.removeHits
}

// RemoveHitCountForPath returns how many removals of exactly path actually
// unlinked something (see [SimDisk.RemoveHitCount]). A scenario brackets it
// around the operation under test and reads the DELTA, so a cleanup that runs
// several times over the same name stays attributable.
func (d *SimDisk) RemoveHitCountForPath(path string) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.removeHitsByPath[path]
}

// PendingRemoveCount returns how many removals are currently recorded as
// not-yet-durable, i.e. how many a [SimDisk.Crash] issued now could put back. A
// scenario reads it to prove it really is INSIDE an unlink window before
// crashing, instead of assuming the timing worked out.
func (d *SimDisk) PendingRemoveCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.countPendingLocked(direntUndoUnlink)
}

// LastCrashRemoveOutcome reports what the most recent [SimDisk.Crash] adjudicated
// about REMOVALS: how many not-yet-durable ones it found pending, and how many of
// those it restored. The remainder (pending-restored) stuck. Both are zero before
// the first crash.
//
// It is the shape observable a non-vacuity gate reads: "the crash landed inside
// the unlink window" is pending > 0, and "the removal did not stick" is
// restored > 0. A verdict gate must never be conditioned on it — an unmet
// precondition is a reason to report, not to pass.
func (d *SimDisk) LastCrashRemoveOutcome() (pending, restored int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.crashPendingRemoves, d.crashRestoredRemoves
}

// PendingRenameCount returns how many renames are currently recorded as
// not-yet-durable, i.e. how many a [SimDisk.Crash] issued now would have to
// adjudicate. A scenario reads it to prove it really is INSIDE a rename window
// before crashing, instead of assuming the timing worked out.
//
// Renames and removals share ONE ordered log (see the direntUndos field docs on
// [SimDisk]), but they are counted separately here because a scenario asserts
// about the operation it issued.
func (d *SimDisk) PendingRenameCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.countPendingLocked(direntUndoRename)
}

// countPendingLocked returns how many records of the given kind the shared undo
// log currently holds. The caller holds d.mu.
func (d *SimDisk) countPendingLocked(kind direntUndoKind) int {
	n := 0
	for i := range d.direntUndos {
		if d.direntUndos[i].kind == kind {
			n++
		}
	}
	return n
}

// LastCrashRenameOutcome reports what the most recent [SimDisk.Crash]
// adjudicated: how many not-yet-durable renames it found pending, and how many
// of those it rolled back to the old name. The remainder (pending-rolledBack)
// were kept at the new name. Both are zero before the first crash.
//
// It is the shape observable a non-vacuity gate reads: "the crash landed between
// the two renames" is pending == 2, and "it took the stranded-backup branch" is
// rolledBack == 1. A verdict gate must never be conditioned on it — an unmet
// precondition is a reason to report, not to pass.
func (d *SimDisk) LastCrashRenameOutcome() (pending, rolledBack int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.crashPendingRenames, d.crashRolledBackRenames
}

// removeSubtreeLocked deletes path and every key under path/, files and
// directory-durability entries alike. The caller holds d.mu.
//
// Clearing d.dirs HERE, rather than at each call site, is what makes "no subtree
// removal leaves a d.dirs entry behind" hold by construction: every path that
// unlinks a whole subtree goes through this one body. The name-level unlinks are
// covered by [SimDisk.pruneGhostDirEntriesLocked]; together the two give the
// invariant that a non-durable entry never outlives its subtree. Maintaining it
// by hand was violable and duly violated — [SimDisk.Rename] and
// [SimDisk.undoRenameLocked]
// hand-deleted the entries while Remove/RemoveAll did not, so a directory that
// had once been published by rename and was then removed left an entry behind
// with its value false. If a directory was later created in place at that same
// path and fully fsync'd, [SimDisk.CrashHost] still honoured the stale entry and
// deleted the whole subtree — a loss of data whose bytes AND whose dirent were
// both on stable storage, which no filesystem produces. The simulator would then
// have manufactured a durability violation and the oracle would have charged it
// to the engine, the same false-accusation class as the rename model repaired
// under rmp #2514 (rmp #2538, audit finding F4).
func (d *SimDisk) removeSubtreeLocked(path string) {
	delete(d.files, path)
	delete(d.dirs, path)
	prefix := path + "/"
	for p := range d.files {
		if strings.HasPrefix(p, prefix) {
			delete(d.files, p)
		}
	}
	// A nested entry goes too, whatever its own durability: its name lives
	// inside a directory this call has just unlinked, so nothing under it is
	// reachable any more.
	for dp := range d.dirs {
		if strings.HasPrefix(dp, prefix) {
			delete(d.dirs, dp)
		}
	}
}

// MkdirAll is a no-op for the in-memory filesystem: there is no directory tree,
// paths are opaque keys. It exists to satisfy the filesystem surface the WAL
// and snapshot writers expect. perm is ignored.
func (d *SimDisk) MkdirAll(_ string, _ fs.FileMode) error { return nil }

// Remove deletes the file at path. Removing an absent path is a no-op, matching
// the tolerant cleanup the snapshot writer relies on.
//
// # Crash outcome
//
// The unlink is a mutation of the containing DIRECTORY, so until that directory
// is fsync'd a crash may legally leave the name in place. It is recorded in the
// shared dirent undo log (see [unlinkUndo] and the direntUndos field docs on
// [SimDisk]) so a [SimDisk.Crash] can restore it; which of the two legal
// outcomes a given crash selects is drawn from the seed unless an arm pins it
// ([SimDisk.ArmRemoveWritebackForPath], [SimDisk.ArmRemoveRollbackForPath]).
func (d *SimDisk) Remove(path string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	f, ok := d.files[path]
	if !ok {
		// Nothing was unlinked, so there is no metadata mutation to record and
		// no cleanup to count as having found anything.
		return nil
	}
	d.noteRemoveHitLocked(path)
	rec := unlinkUndo{path: path, files: map[string]*simFile{path: f}}
	durable := d.consumeRemoveArmsLocked(path, &rec)
	delete(d.files, path)
	// Recorded rather than discarded, which closes the note rmp #2538 left here: a
	// restored name returns into a directory whose entry this prune may have
	// deleted, so the entry travels on the record. See [unlinkUndo] for why the
	// post-crash state is nonetheless insensitive to it.
	rec.prunedDirs = d.pruneGhostDirEntriesLocked(path)
	if durable {
		// This unlink is already on stable storage, so by journal order every
		// pending mutation before it is too.
		d.pinAllDirentUndosLocked()
	} else {
		d.direntUndos = append(d.direntUndos, direntUndo{kind: direntUndoUnlink, unlink: rec})
	}
	return nil
}

// consumeRemoveArmsLocked applies the two one-shot unlink arms to a removal of
// path and reports whether the unlink is to be treated as already durable, in
// which case the caller records no undo at all.
//
// Both arms are consumed here even when the removal turns out to be durable for
// another reason, so an arm is one-shot in the same sense as every other arm on
// this disk. A root-level name is durable by the same rule that governs its
// creation and its renames (see [isRootLevel]): the WAL's own unlinks stay
// governed by the data-durability model, not the dirent model, which keeps
// WAL-only scenarios byte-compatible. The caller holds d.mu.
func (d *SimDisk) consumeRemoveArmsLocked(path string, rec *unlinkUndo) bool {
	durable := isRootLevel(path)
	if d.removeWritebackArmed && d.removeWritebackPath == path {
		d.removeWritebackArmed = false
		d.removeWritebackPath = ""
		d.removeWritebacks++
		durable = true
	}
	if d.removeRollbackArmed && d.removeRollbackPath == path {
		d.removeRollbackArmed = false
		d.removeRollbackPath = ""
		d.removeRollbacks++
		rec.forceRestore = true
	}
	return durable
}

// noteRemoveHitLocked records that a removal actually unlinked something, in
// total and for path. The caller holds d.mu.
func (d *SimDisk) noteRemoveHitLocked(path string) {
	d.removeHits++
	d.removeHitsByPath[path]++
}

// pruneGhostDirEntriesLocked drops every not-yet-durable directory entry that the
// removal of the name path has left without a subtree, and returns what it
// deleted so a caller able to undo the removal can put it back. It is the
// counterpart of [SimDisk.removeSubtreeLocked] for the paths that unlink a single
// NAME instead of a whole tree — [SimDisk.Remove] and a [SimDisk.Rename] that
// moves the last name out of a directory — and it closes the same hole: an entry
// whose value is false and whose subtree is gone destroys whatever is created at
// that path next (rmp #2538).
//
// Only path itself or an ancestor of path can be affected, and only entries that
// are still non-durable: a DURABLE entry left over an empty directory is a
// legitimate residue — a publish rename that reached stable storage while the
// components inside it did not — and, being durable, it can never delete
// anything. Forgetting a non-durable empty directory loses nothing observable,
// since an empty directory has no representation in the opaque-key model at all
// (see [SimDisk.Stat]). The caller holds d.mu.
func (d *SimDisk) pruneGhostDirEntriesLocked(path string) map[string]bool {
	var pruned map[string]bool
	for dp, durable := range d.dirs {
		if durable || (dp != path && !strings.HasPrefix(path, dp+"/")) {
			continue
		}
		if !d.hasSubtreeLocked(dp) {
			if pruned == nil {
				pruned = make(map[string]bool)
			}
			pruned[dp] = durable
			delete(d.dirs, dp)
		}
	}
	return pruned
}

// hasSubtreeLocked reports whether any file key sits at dir or under dir + "/",
// which is what "the directory exists" means in the opaque-key model. The caller
// holds d.mu.
func (d *SimDisk) hasSubtreeLocked(dir string) bool {
	prefix := dir + "/"
	for p := range d.files {
		if p == dir || strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// RemoveAll deletes path and every file under path/. Removing an absent path is
// a no-op, matching os.RemoveAll and the staging/backup cleanup the snapshot
// writer relies on.
//
// # Crash outcome
//
// Same as [SimDisk.Remove], extended to the subtree: the whole removal is one
// record in the shared dirent undo log, so a crash either keeps it or restores
// the entire subtree with each name's original dirent durability. It is one
// record and not one per name because the caller issued one removal, and a crash
// that restored half a subtree would model an interleaving os.RemoveAll's
// callers never see.
func (d *SimDisk) RemoveAll(path string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	files, dirs := d.captureSubtreeLocked(path)
	if len(files) == 0 && len(dirs) == 0 {
		return nil
	}
	if len(files) > 0 {
		// A subtree of directory entries alone (an emptied publish directory that
		// still carries its durability entry) is a metadata mutation worth
		// reversing, but it never removed a NAME a tolerant cleanup could have
		// found, so it is not counted as a hit.
		d.noteRemoveHitLocked(path)
	}
	rec := unlinkUndo{path: path, files: files, dirs: dirs, subtree: true}
	durable := d.consumeRemoveArmsLocked(path, &rec)
	d.removeSubtreeLocked(path)
	// The subtree's own root is gone, so only an ANCESTOR can be left describing
	// an empty directory; the capture above already holds every entry at or under
	// path (see [SimDisk.pruneGhostDirEntriesLocked]).
	rec.prunedDirs = d.pruneGhostDirEntriesLocked(path)
	if durable {
		d.pinAllDirentUndosLocked()
	} else {
		d.direntUndos = append(d.direntUndos, direntUndo{kind: direntUndoUnlink, unlink: rec})
	}
	return nil
}

// Stat reports a minimal [fs.FileInfo] for a file at path. It returns an error
// wrapping fs.ErrNotExist when no file is present, which is exactly the probe
// the snapshot and recovery paths rely on (testing for manifest.json / wal
// presence). Directories have no standalone entry in the opaque-key model;
// Stat reports a directory when any child key is prefixed path+"/".
func (d *SimDisk) Stat(path string) (fs.FileInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if f, ok := d.files[path]; ok {
		return simFileInfo{name: baseName(path), size: int64(len(f.data))}, nil
	}
	prefix := path + "/"
	for p := range d.files {
		if strings.HasPrefix(p, prefix) {
			return simFileInfo{name: baseName(path), dir: true}, nil
		}
	}
	return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
}

// ReadFile returns a copy of the whole contents of the file at path, or an
// error wrapping fs.ErrNotExist when absent. The copy keeps the returned slice
// independent of later writes, mirroring os.ReadFile.
func (d *SimDisk) ReadFile(path string) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f, ok := d.files[path]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	out := make([]byte, len(f.data))
	copy(out, f.data)
	return out, nil
}

// TruncatePath resizes the file at path to size bytes (zero-filling on grow),
// the path-based analogue of os.Truncate used by the csrfile writer. It returns
// an error wrapping fs.ErrNotExist when the file is absent.
func (d *SimDisk) TruncatePath(path string, size int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	f, ok := d.files[path]
	if !ok {
		return &fs.PathError{Op: "truncate", Path: path, Err: fs.ErrNotExist}
	}
	if size < 0 {
		return errors.New("sim: negative truncate size")
	}
	if size <= int64(len(f.data)) {
		f.data = f.data[:size]
		f.truncateDurableTo(size)
		return nil
	}
	if d.wouldExceedLocked(int64(len(f.data)), size) {
		return enospc("truncate", path)
	}
	grown := make([]byte, size)
	copy(grown, f.data)
	f.data = grown
	return nil
}

// dirSyncArm is ONE one-shot, path-keyed arm of the directory-fsync fault. Both
// directory-fsync entry points ([SimDisk.DirSync] and [SimDisk.ParentDirSync])
// hold one and consult it through the same body, so the arming, the matching,
// the one-shot disarm and the failure effects are written once and cannot drift
// between them (rmp #2537).
//
// It draws NOTHING from the [Seed]: the trigger is a pure function of the path,
// exactly like [SimDisk.ArmSyncFaultAt]'s ordinal, so arming one never perturbs
// the reproducible torn-write/Sync fault stream. Its zero value is disarmed.
type dirSyncArm struct {
	// path is the key this arm fires on: a directory for the DirSync arm, a
	// child path for the ParentDirSync arm. Empty means disarmed.
	path  string
	armed bool
}

// arm keys the arm on path, or disarms it when path is empty.
func (a *dirSyncArm) arm(path string) {
	a.path = path
	a.armed = path != ""
}

// fire reports whether this arm selects key, and disarms when it does — the
// ONE-SHOT rule. An empty key never matches, so an entry point that has no such
// key (DirSync has no child path) can pass "" and be sure the other arm stays
// out of its way.
func (a *dirSyncArm) fire(key string) bool {
	if !a.armed || key == "" || a.path != key {
		return false
	}
	a.path = ""
	a.armed = false
	return true
}

// ArmDirSyncFaultForPath arms a ONE-SHOT durability fault on [SimDisk.DirSync]:
// the next directory fsync of dir returns [ErrSimFault] and makes NO dirent
// durable, then the arm clears so no further directory fsync is affected. An
// empty dir disarms. [SimDisk.DirSyncFaultCount] reports how many times a
// directory-fsync fault has fired.
//
// Because a [SimDisk.ParentDirSync] of a child of dir IS an fsync of dir, it
// fires for that call too; the converse does not hold, since
// [SimDisk.ArmParentDirSyncFaultForPath] names one specific child (see the field
// docs on [SimDisk] for why the two keys stay distinct).
//
// It is the arm the snapshot publish protocol's directory fsyncs need. Audit
// finding F3 named four error branches that no fault could reach; this arm
// reaches the two on the full publish path — store/snapshot/full.go's staging
// fsync in writeCaptureCore and its indexes/ fsync in writeCapturedIndexes —
// through a real checkpoint. The remaining two (writer.go's legacy CSR staging
// fsync and indexes.go's standalone index writer) bind the OS backend at their
// entry points, so no in-memory disk can reach them however it is faulted; they
// are driven from inside store/snapshot instead.
//
// The staging fsync is the one that matters most: it is the LAST durability gate
// before the archive and publish renames, so it must RemoveAll the staging tree
// and abort rather than publish a directory whose dirents never reached stable
// storage. It draws nothing from the [Seed], so arming never perturbs the
// reproducible fault stream, and must be called from the controlling goroutine
// before the operation that will trigger it.
func (d *SimDisk) ArmDirSyncFaultForPath(dir string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dirSyncFault.arm(dir)
}

// ArmParentDirSyncFaultForPath arms a ONE-SHOT durability fault on
// [SimDisk.ParentDirSync]: the next call whose childPath equals childPath
// returns [ErrSimFault] and makes NO dirent durable, then the arm clears so no
// further ParentDirSync is affected. An empty childPath disarms.
//
// It is the same primitive as [SimDisk.ArmDirSyncFaultForPath] — one directory
// fsync, one shared body, one fire count — differing only in being keyed on the
// exact childPath rather than on the directory, so it targets a specific fsync
// robustly where several fsyncs of the SAME parent directory occur in one
// operation (see the field docs on [SimDisk]). It models the post-rename
// parent-directory fsync failing inside [wal.Writer.TruncatePrefix]: that
// failure must poison the WAL writer (store/wal/writer.go poisonAfterRename)
// while the on-disk suffix-only WAL — and any snapshot published before it —
// stays intact and recoverable. It draws nothing from the [Seed], so arming
// never perturbs the reproducible fault stream, and must be called from the
// controlling goroutine before the operation that will trigger it.
func (d *SimDisk) ArmParentDirSyncFaultForPath(childPath string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.parentDirSyncFault.arm(childPath)
}

// DirSyncFaultCount returns how many armed directory-fsync faults actually fired
// since the disk was created, counting both entry points ([SimDisk.DirSync] and
// [SimDisk.ParentDirSync]) because they are one primitive. It is the reachability
// observable a non-vacuity gate reads to prove an armed fault really bit rather
// than being silently ignored because its path never matched.
func (d *SimDisk) DirSyncFaultCount() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dirSyncFaults
}

// DirSync makes every dirent in directory dir durable: it is the in-memory
// analogue of fsync(2) on a directory descriptor. The snapshot/csrfile publish
// protocol calls it on the staging directory before the publish rename and on
// the parent directory after it; only after DirSync does a freshly created or
// renamed name survive a [SimDisk.Crash]. A DirSync of a directory with no
// entries is a harmless no-op (it models fsync of an empty directory).
//
// When a one-shot fault is armed for dir ([SimDisk.ArmDirSyncFaultForPath]) it
// returns [ErrSimFault] and durabilises NOTHING.
func (d *SimDisk) DirSync(dir string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dirSyncLocked(dir, "")
}

// ParentDirSync makes the dirent of childPath durable by fsyncing its parent
// directory. It is the analogue of the post-rename parent-directory fsync the
// publish protocols issue.
//
// It faults when a one-shot arm is set for this exact childPath
// ([SimDisk.ArmParentDirSyncFaultForPath]) OR for the parent directory it is
// about to fsync ([SimDisk.ArmDirSyncFaultForPath]) — the same body decides
// both, since this call IS an fsync of that directory.
func (d *SimDisk) ParentDirSync(childPath string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dirSyncLocked(pathpkg.Dir(childPath), childPath)
}

// dirSyncLocked is the ONE body behind both directory-fsync entry points: dir is
// the directory being fsync'd and child is the path whose dirent the caller is
// making durable, or "" when the caller named the directory itself. The caller
// holds d.mu.
//
// A failed directory fsync must compose with the two crash-model refinements
// that surround it, and it does so by returning BEFORE any of the durability
// work below:
//
//   - Dirents. Nothing becomes durable. POSIX states that when fsync fails
//     "outstanding I/O operations are not guaranteed to have been completed", so
//     the harness must assume no directory entry reached stable storage. This is
//     also the stricter direction, which is the standing rule for this model.
//
//   - Rename undo log (rmp #2514). The pending renames are NOT pruned. Pruning
//     is precisely the act of declaring a rename durable and therefore no longer
//     rollback-able; a fsync that failed declares the opposite. Were the log
//     pruned here, a later [SimDisk.CrashHost] would be obliged to KEEP a publish
//     rename whose staging tree was never made durable — which is exactly the
//     state the snapshot writer's staging-fsync branch exists to prevent, so the
//     harness would have manufactured the defect it is meant to detect.
//
//   - Durable shadow (rmp #2535). A directory fsync never advances a file's
//     durable watermark, on the success path or the failure path: fsync(2) on a
//     directory descriptor hardens metadata, not file contents, and only
//     [SimFileHandle.Sync] -> [SimDisk.completeSync] moves durableLen. So after a
//     failed DirSync a component can hold durable DATA under a name that is not
//     durable; [SimDisk.CrashHost] then deletes it outright, since it revokes
//     unsynced dirents before reverting data. That is the physically correct
//     outcome — blocks on the platter that no directory entry points at are
//     unreachable — and it is what makes the staging-fsync gate meaningful.
//
// Unlike [SimFileHandle.Sync] there is no issue/complete split and no
// hostCrashGen stamp: this body runs entirely under d.mu, which [SimDisk.Crash]
// also takes, so no crash can land inside a directory fsync.
func (d *SimDisk) dirSyncLocked(dir, child string) error {
	if d.dirSyncFault.fire(dir) || d.parentDirSyncFault.fire(child) {
		d.dirSyncFaults++
		return ErrSimFault
	}
	for p, f := range d.files {
		if pathpkg.Dir(p) == dir {
			f.direntDurable = true
		}
	}
	// A directory whose parent is dir has its own name made durable too: this
	// is how the post-publish ParentDirSync(live) durabilises the live
	// snapshot directory's name.
	for dp := range d.dirs {
		if pathpkg.Dir(dp) == dir {
			d.dirs[dp] = true
		}
	}
	// A mutation that has reached stable storage can no longer be reversed.
	d.pruneDurableDirentUndosLocked(dir)
	return nil
}

// Crash is an alias for [SimDisk.CrashHost], the STRONGER of the two crash
// models. Every scenario that has not consciously chosen otherwise therefore
// gets power-failure semantics, in which unsynced data is lost; a scenario that
// genuinely means SIGKILL calls [SimDisk.CrashProcess] and says so.
func (d *SimDisk) Crash() { d.CrashHost() }

// CrashProcess models a SIGKILL of the process (kill -9). The process dies; the
// kernel does not. Everything the process had already handed the kernel — every
// byte accepted by a write(2) and every directory entry created by a
// create(2)/rename(2)/unlink(2) — is still there for the next process to read,
// whether or not it was ever fsync'd. CrashProcess therefore discards NOTHING:
// no data revert, no dirent revocation, no rename rollback.
//
// It is the model [SimStore.Crash]'s own documentation used to describe while
// [SimDisk.Crash] actually applied dirent revocation, which is a host-crash
// effect. The two are now separate primitives so a scenario can state which
// event it means (rmp #2535).
//
// Pending renames stay pending: their dirents are in the page cache but not on
// stable storage, so a LATER [SimDisk.CrashHost] can still lose them. It must be
// driven from the single simulation goroutine.
func (d *SimDisk) CrashProcess() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastCrashKind = CrashKindProcess
	d.lastCrashDiscardedBytes = 0
	// Every file descriptor the dead process held is gone with it (rmp #2544).
	d.crashGen++
	// The undo log is untouched, but the shape observables still report what this
	// crash SAW, so a gate can prove it landed inside a publish window.
	d.crashPendingRenames = d.countPendingLocked(direntUndoRename)
	d.crashPendingRemoves = d.countPendingLocked(direntUndoUnlink)
	d.crashRolledBackRenames = 0
	d.crashRestoredRemoves = 0
	// hostCrashGen is deliberately NOT bumped: an fsync already issued to the
	// kernel completes even though the process that issued it is gone.
}

// CrashHost models a host crash — power failure, hard reset, hypervisor kill.
// Nothing that had not reached stable storage survives. It is the model
// [SimDisk.Crash] applies, and it has two halves that must agree with each other.
//
// # Data
//
// Every file reverts to its durable image: the bytes covered by a
// [SimFileHandle.Sync] that returned nil, and nothing else. A successful
// write(2) carries no durability whatever — Linux write(2) NOTES: "A successful
// return from write() does not make any guarantee that data has been committed
// to disk… The only way to be sure is to call fsync(2)" — and POSIX reaches
// durability solely through Successfully Transferred, i.e. via
// fsync/fdatasync/O_SYNC. Until rmp #2535 the model kept every byte ever
// written, which granted data the guarantee RocksDB's file abstraction reserves
// for Flush() ("should survive a process crash") while revoking names as if
// power had been lost — an internal contradiction no real event has, and one
// that made every "the commit was acked, so the bytes are recovered" assertion
// pass irrespective of any fsync.
//
// [SimDisk.LastCrashDiscardedBytes] reports how much this crash actually threw
// away, which is the non-vacuity observable an oracle needs to prove it was
// tested at all.
//
// # Names
//
// It drops every dirent that is not yet
// durable, exactly as a real crash within the kernel writeback window loses a
// create or rename whose parent directory was never fsync'd. A name becomes
// durable only after a [SimDisk.DirSync] of its parent; until then it is the
// load-bearing job of the publish protocol's directory fsyncs to make the name
// survive, and removing one of those fsyncs makes the corresponding name vanish
// here — which is what the non-vacuity guard test asserts.
//
// # Renames are rolled back, not revoked
//
// A rename is not a create, and revoking its new dirent is not enough: the old
// name was unlinked when the rename was applied, so revoking alone loses BOTH
// names. rename(2) is atomic, and a crash after it leaves either the new name or
// the old one — never neither. Crash therefore ROLLS BACK the renames it does
// not keep, restoring each source name with the dirent durability it carried at
// the instant of the rename (see [renameUndo]). A source whose own name was
// never durable is restored non-durable and then dropped by the ordinary revoke
// pass below, which is the one case in which losing both names IS legal: that
// name had never been crash-survivable.
//
// # Removals are restored, not applied unconditionally
//
// An unlink is a mutation of the containing directory, in the same durability
// class as the create and the rename above, so a crash before that directory is
// fsync'd may legally leave the removed name IN PLACE. Crash therefore restores
// the removals it does not keep, each entry with the dirent durability it carried
// at the instant of the unlink (see [unlinkUndo]). Both outcomes are legal here —
// unlike a rename there is no impossible third one — and the branch that matters
// is the restore: it is the only way the harness can reach the engine's tolerance
// of a leftover "<dir>/snapshot.tmp" or "<dir>/snapshot.bak" FROM THE REMOVAL
// SIDE, rather than by crashing before the removal was ever issued (rmp #2536).
//
// # One ordered log, one draw
//
// How much of the pending metadata is kept is drawn ONCE from the disk's dirent
// sub-stream over the renames and the removals TOGETHER, so it is a reproducible
// function of the run seed (see the direntSeed field docs) and the mutations that
// survive are always a PREFIX of the issued ones, as a journalling filesystem
// guarantees. Drawing per kind would let a crash keep an unlink while rolling back
// a rename issued before it, which is not an interleaving a filesystem can
// produce. An armed [SimDisk.ArmRenameRollbackForPath] or
// [SimDisk.ArmRemoveRollbackForPath] pins the boundary at or before that record
// instead of sampling it, and an armed [SimDisk.ArmRenameRevokeBothForPath]
// deliberately produces the impossible both-names-lost outcome for tests of the
// harness itself. Mutations already made durable — by a [SimDisk.DirSync] of the
// affected directory, by [SimDisk.ArmRenameWritebackForPath] or
// [SimDisk.ArmRemoveWritebackForPath], or by being root-level — are never
// reversed.
//
// CrashHost mutates the SimDisk in place. It is driven from the single
// simulation goroutine, but it may run while another goroutine is parked inside
// a gated [SimFileHandle.Sync]: that is the phantom-commit window, and the
// generation stamp on the in-flight fsync (see [SimDisk.completeSync]) is what
// stops the parked call from re-hardening bytes this crash has just discarded.
func (d *SimDisk) CrashHost() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastCrashKind = CrashKindHost
	// Invalidate every fsync currently in flight before anything else: the power
	// went out with the write-back incomplete.
	d.hostCrashGen++
	// And every open file descriptor with it (rmp #2544).
	d.crashGen++
	d.rollbackDirentUndosLocked()
	// Drop directories whose own dirent never became durable, taking their
	// whole subtree with them (a lost directory rename loses every child).
	for dp, durable := range d.dirs {
		if !durable {
			// removeSubtreeLocked drops the entry itself, and any nested entry,
			// along with the files: the invariant is maintained there so no call
			// site has to remember it (see [SimDisk.removeSubtreeLocked]).
			d.removeSubtreeLocked(dp)
		}
	}
	// Drop individual files whose dirent never became durable.
	for p, f := range d.files {
		if !f.direntDurable {
			delete(d.files, p)
		}
	}
	// Revert the DATA of every file whose name survived. Reverting is idempotent,
	// so a file reachable under two names (a rolled-back rename restores the
	// same inode under its old name) is safe to visit twice.
	var discarded int64
	for _, f := range d.files {
		discarded += f.revertToDurable()
	}
	d.lastCrashDiscardedBytes = discarded
}

// rollbackDirentUndosLocked undoes the trailing run of not-yet-durable dirent
// mutations the crash did not keep, newest first, and clears the log. The caller
// holds d.mu.
//
// keep is the number of leading records treated as durable. It is drawn ONCE,
// uniformly from [0, n] over the WHOLE log — renames and removals together —
// because a journalling filesystem commits metadata in order and the two are
// metadata mutations of the same directories: an unlink issued after a rename
// cannot be durable while that rename is not. A second, independent draw per kind
// would produce exactly that impossible interleaving. Every prefix length is
// equally likely, so for the publish protocol's archive/publish pair each of the
// three reachable durable images is sampled. The boundary is then lowered to the
// first record an arm pinned to the rolled-back branch, of either kind. The draw
// is taken whenever the log is non-empty, armed or not, so the sub-stream's
// position depends only on the sequence of crash log lengths and not on which arms
// a scenario happened to install.
//
// Undoing newest first is what lets records of different kinds compose: a removal
// of a name a pending rename created is reversed before that rename is, so the
// rename's undo finds the path in the state it expects.
func (d *SimDisk) rollbackDirentUndosLocked() {
	n := len(d.direntUndos)
	d.crashPendingRenames = d.countPendingLocked(direntUndoRename)
	d.crashPendingRemoves = d.countPendingLocked(direntUndoUnlink)
	d.crashRolledBackRenames = 0
	d.crashRestoredRemoves = 0
	if n == 0 {
		return
	}
	keep := d.direntSeed.IntN(n + 1)
	for i := range d.direntUndos {
		if d.direntUndos[i].pinsRollback() {
			// The earliest pinned record lowers the boundary; anything after it
			// is rolled back by the prefix rule anyway.
			if i < keep {
				keep = i
			}
			break
		}
	}
	for i := n - 1; i >= keep; i-- {
		rec := &d.direntUndos[i]
		if rec.kind == direntUndoUnlink {
			d.undoUnlinkLocked(&rec.unlink)
			d.crashRestoredRemoves++
			continue
		}
		d.undoRenameLocked(&rec.rename)
		d.crashRolledBackRenames++
	}
	// A record this crash KEPT is a mutation that reached stable storage, so the
	// name it created must survive the revoke pass that follows. Without this the
	// kept branch of a rename would revoke the new dirent while the old name has
	// already been unlinked — the impossible both-names-lost outcome this whole
	// model exists to eliminate. It is the same hardening a record gets when it
	// leaves the log through a pin or a prune, so the three paths cannot drift.
	for i := 0; i < keep; i++ {
		d.hardenDurableDirentLocked(&d.direntUndos[i])
	}
	d.direntUndos = nil
}

// undoUnlinkLocked reverses one recorded removal: it puts every file and
// directory-durability entry the removal deleted back where it was, each with the
// dirent durability it carried at the instant of the unlink.
//
// A restored name whose own dirent was never durable is put back non-durable and
// then dropped again by the ordinary revoke pass, which is correct rather than
// wasteful: that name had never been crash-survivable, so its absence after the
// crash is not the removal having stuck. The prunedDirs restore, by contrast,
// changes no post-crash state at all — it keeps the intermediate state honest for
// the next record's undo, and [unlinkUndo.prunedDirs] gives the measurement that
// withdrew the stronger claim once made here. The caller holds d.mu.
func (d *SimDisk) undoUnlinkLocked(rec *unlinkUndo) {
	for p, f := range rec.files {
		d.files[p] = f
	}
	for dp, dur := range rec.dirs {
		d.dirs[dp] = dur
	}
	for dp, dur := range rec.prunedDirs {
		d.dirs[dp] = dur
	}
}

// undoRenameLocked reverses one recorded rename: it moves whatever the new name
// currently designates back to the old name, restores that name's original
// dirent durability, and re-links the destination the rename had replaced.
//
// It re-reads the CURRENT contents of the new name rather than a snapshot taken
// at rename time, which is what makes nested renames compose: a directory
// published under a new name and then written into is moved back with the
// writes inside it, exactly as undoing the rename of a real directory inode
// would. Records are undone newest first, so any nested rename recorded after
// this one has already been reversed.
//
// A record armed with revokeBoth skips every restore, reproducing on purpose the
// physically impossible outcome this model exists to eliminate. The caller holds
// d.mu.
func (d *SimDisk) undoRenameLocked(rec *renameUndo) {
	if rec.dirRename {
		files, dirs := d.captureSubtreeLocked(rec.newPath)
		d.removeSubtreeLocked(rec.newPath)
		if !rec.revokeBoth {
			prefix := rec.newPath + "/"
			for p, f := range files {
				if p == rec.newPath {
					// A file sitting at the directory's own name cannot exist in
					// the opaque-key model; ignore it rather than mis-rooting it.
					continue
				}
				d.files[rec.oldPath+"/"+p[len(prefix):]] = f
			}
			for dp, dur := range dirs {
				if dp == rec.newPath {
					continue
				}
				d.dirs[rec.oldPath+"/"+dp[len(prefix):]] = dur
			}
			if rec.oldDirTracked {
				d.dirs[rec.oldPath] = rec.oldDurable
			}
		}
	} else if f, ok := d.files[rec.newPath]; ok {
		delete(d.files, rec.newPath)
		if !rec.revokeBoth {
			f.direntDurable = rec.oldDurable
			d.files[rec.oldPath] = f
		}
	}
	if rec.revokeBoth {
		return
	}
	for p, f := range rec.replacedFiles {
		d.files[p] = f
	}
	for dp, dur := range rec.replacedDirs {
		d.dirs[dp] = dur
	}
	// The source directory holds a name again, so the entry that described it
	// has to describe it again — including its NON-durability, which is what
	// makes the following revoke pass drop a subtree whose directory name never
	// reached stable storage.
	for dp, dur := range rec.prunedDirs {
		d.dirs[dp] = dur
	}
}

// baseName returns the final path element, for the synthetic [fs.FileInfo].
func baseName(p string) string { return pathpkg.Base(p) }

// isRootLevel reports whether p sits at the filesystem root (its parent is "."
// or "/" or itself). Root-level names are treated as durably linked on creation.
//
// # Why the exemption exists, and why it is no longer a blind spot
//
// A file at the root has no parent directory that anything could fsync, so
// without the exemption its name could never become durable and every crash
// would revoke it. MEASURED: with isRootLevel forced to false, 16 tests fail for
// exactly that reason. The exemption is therefore a necessity of the model, not
// a simplification of it — retiring it outright was tried and rejected.
//
// What WAS a blind spot is that the WAL-only layout used to sit at the bare key
// "wal" and take this exemption, which made wal.OpenFS's unconditional
// ParentDirSync inert in that mode: the call happened, changed nothing, and
// deleting it could not have failed a run. The WAL now lives under [simWALDir]
// and is governed by the dirent model like any other name, so the fsync is
// load-bearing in every mode. [TestWALDirentFsyncIsLoadBearing] proves it by
// withholding the fsync and requiring the crash to punish the omission, and
// [TestWALOnlyLayoutIsNotRootExempt] pins the layout so a move back to the root
// cannot restore the blindness silently (rmp #2539, audit finding F5).
func isRootLevel(p string) bool {
	dir := pathpkg.Dir(p)
	return dir == "." || dir == "/" || dir == p
}

// simFileInfo is the minimal [fs.FileInfo] SimDisk.Stat returns: callers only
// consult existence, Size, and IsDir.
type simFileInfo struct {
	name string
	size int64
	dir  bool
}

func (fi simFileInfo) Name() string { return fi.name }
func (fi simFileInfo) Size() int64  { return fi.size }
func (fi simFileInfo) Mode() fs.FileMode {
	if fi.dir {
		return fs.ModeDir | 0o755
	}
	return 0o600
}
func (fi simFileInfo) ModTime() time.Time { return time.Time{} }
func (fi simFileInfo) IsDir() bool        { return fi.dir }
func (fi simFileInfo) Sys() any           { return nil }

// Exists reports whether a file is present at path.
func (d *SimDisk) Exists(path string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.files[path]
	return ok
}

// Snapshot returns an independent deep copy of every file's contents keyed by
// path. Mutating the returned maps or slices never affects the live
// filesystem, so a caller can capture disk state for comparison after a crash.
func (d *SimDisk) Snapshot() map[string][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string][]byte, len(d.files))
	for path, f := range d.files {
		cp := make([]byte, len(f.data))
		copy(cp, f.data)
		out[path] = cp
	}
	return out
}

// SimFileHandle is an open handle onto a [SimDisk] file. It implements the WAL
// file interface (io.Reader, io.Writer, io.Seeker, Sync, Truncate, Close) so it
// can substitute for *os.File.
//
// # Concurrency contract
//
// SimFileHandle is NOT safe for concurrent use; it is driven from the single
// simulation goroutine.
//
//nolint:revive // "Sim" prefix is intentional (see SimDisk).
type SimFileHandle struct {
	disk *SimDisk
	file *simFile
	path string
	pos  int64
	// crashGen is the disk's crash counter as it stood when this handle was
	// opened. A crash of either kind bumps the disk's, so a stale stamp means
	// this handle outlived the process that opened it (rmp #2544).
	crashGen uint64
	closed   bool
}

// ErrCrashedDisk is returned by every [SimFileHandle] method whose handle was
// opened before a crash. It WRAPS [fs.ErrClosed], so code that already treats a
// closed handle as terminal keeps working unchanged, while errors.Is against
// this sentinel still tells a scenario author which of the two happened.
//
// It exists because the alternative was silent (rmp #2544). [SimDisk.CrashHost]
// drops entries from the name maps, but a handle holds its *simFile DIRECTLY, so
// a scenario that crashed while holding its own handle went on writing into an
// ORPHANED file. The bytes went nowhere, no error was returned, and the symptom
// presented as data missing from the engine — a false accusation pointing at the
// subject under test. Fail-stop beats silent discard: the author learns the
// handle is dead instead of debugging phantom data loss.
var ErrCrashedDisk = fmt.Errorf("sim: file handle was opened before a crash and did not survive it: %w", fs.ErrClosed)

// dead is [SimFileHandle.deadLocked] for a caller that does not already hold the
// disk lock.
func (h *SimFileHandle) dead() error {
	h.disk.mu.Lock()
	defer h.disk.mu.Unlock()
	return h.deadLocked()
}

// deadLocked reports whether this handle is unusable, and why. It must be called
// with d.mu held, because crashGen is disk state.
//
// The order matters: an explicitly closed handle reports fs.ErrClosed, because
// that is what the caller did; a handle killed by a crash reports the crash,
// because that is something the caller did NOT do and would otherwise have to
// infer from missing bytes.
func (h *SimFileHandle) deadLocked() error {
	if h.closed {
		return fs.ErrClosed
	}
	if h.crashGen != h.disk.crashGen {
		return ErrCrashedDisk
	}
	return nil
}

// Read copies up to len(p) bytes from the current position into p, advancing
// the position. It returns io.EOF when the position is at or past end of file.
func (h *SimFileHandle) Read(p []byte) (int, error) {
	h.disk.mu.Lock()
	defer h.disk.mu.Unlock()
	if err := h.deadLocked(); err != nil {
		return 0, err
	}
	if h.pos >= int64(len(h.file.data)) {
		return 0, io.EOF
	}
	n := copy(p, h.file.data[h.pos:])
	h.pos += int64(n)
	return n, nil
}

// Stat returns a minimal [fs.FileInfo] for the open handle, reporting the
// current file size. The snapshot index writer calls it to record the
// component's on-disk size in the manifest.
func (h *SimFileHandle) Stat() (fs.FileInfo, error) {
	h.disk.mu.Lock()
	defer h.disk.mu.Unlock()
	if err := h.deadLocked(); err != nil {
		return nil, err
	}
	return simFileInfo{name: baseName(h.path), size: int64(len(h.file.data))}, nil
}

// Write copies p to the file at the current position, growing the file as
// needed, and advances the position. Any byte written into a sector that the
// fault injector has marked faulted is corrupted deterministically (a single
// byte in that sector is flipped), modelling a torn or mis-directed write.
func (h *SimFileHandle) Write(p []byte) (int, error) {
	h.disk.mu.Lock()
	defer h.disk.mu.Unlock()
	// THE clause rmp #2544 exists for: without it these bytes landed in a simFile
	// no name reaches any more, and were discarded in silence.
	if err := h.deadLocked(); err != nil {
		return 0, err
	}

	end := h.pos + int64(len(p))
	oldLen := int64(len(h.file.data))
	partial := false
	if end > oldLen {
		// Disk full. A partially-satisfiable write TRANSFERS THE BYTES THAT FIT
		// and reports ENOSPC alongside the short count — it does not refuse
		// wholesale.
		//
		// This matters because returning (0, ENOSPC) meant a partial record could
		// never exist, which made a torn trailing WAL frame — the single most
		// important crash state a write-ahead log must survive — unreachable by
		// any route (rmp #2541, audit finding F7).
		//
		// The SHORT COUNT WITH A NON-NIL ERROR is the exact shape os.File.Write
		// produces, and the shape is not optional: SimFileHandle is used as an
		// io.Writer, and io.Writer's contract is that "Write must return a
		// non-nil error if it returns n < len(p)". The task prescribed returning
		// the count with a NIL error, on the POSIX write(2) reading; that was
		// tried and REFUTED by a real caller in this tree — the CSV export path
		// in the io-roundtrip-fault scenario detected the short write and
		// reported "short write, want a typed ENOSPC error". POSIX describes the
		// syscall; os.File is the layer that maps it to Go, and it attaches
		// io.ErrShortWrite or the wrapped errno to every short count
		// (os/file.go, File.Write).
		//
		// The partial record is emergent either way, because the bytes that fit
		// are in the file before the error is returned.
		if h.disk.wouldExceedLocked(oldLen, end) {
			fits := h.disk.bytesThatFitLocked(oldLen, h.pos)
			if fits <= 0 {
				return 0, enospc("write", h.path)
			}
			partial = true
			p = p[:fits]
			end = h.pos + fits
		}
		grown := make([]byte, end)
		copy(grown, h.file.data)
		h.file.data = grown
	}
	// A write back into already-durable bytes makes the page cache and the
	// platter disagree, so the durable image must be pinned before the bytes
	// change. A pure append — the WAL's whole access pattern — takes the early
	// return inside preserveDurableBelow and copies nothing.
	h.file.preserveDurableBelow(h.pos)
	copy(h.file.data[h.pos:end], p)

	// Decide, per touched sector, whether the write lands in a faulted sector
	// and corrupt it. Iterating sectors in ascending index keeps the draw
	// order deterministic.
	first := int(h.pos / sectorSize)
	last := int((end - 1) / sectorSize)
	for sec := first; sec <= last; sec++ {
		if !h.file.faulted[sec] && h.disk.seed.Bool(h.disk.faultRate) {
			h.file.faulted[sec] = true
		}
		if h.file.faulted[sec] {
			h.corruptSector(sec)
		}
	}
	// Count growing writes and, when this is the armed one, mark the tail as
	// torn so the next crash leaves it partial. The mark is taken AFTER the
	// bytes are in place, so the kept prefix is the real payload.
	if end > oldLen {
		h.disk.appendCount++
		if h.disk.tornAppendArmed && h.disk.appendCount == h.disk.tornAppendAt {
			h.disk.tornAppendArmed = false
			keep := h.disk.tornAppendKeep
			if keep < 0 {
				keep = 0
			}
			if n := end - h.pos; keep > n {
				keep = n
			}
			h.file.tornTailSet = true
			h.file.tornTailStart = h.pos
			h.file.tornTailKeep = keep
			h.file.tornTailEnd = end
		}
	}

	h.pos = end
	if partial {
		return len(p), enospc("write", h.path)
	}
	return len(p), nil
}

// bytesThatFitLocked returns how many bytes a write starting at pos can still
// transfer before the disk budget is reached, given the file's current length.
// It is never negative. The caller holds disk.mu.
//
// A write may start BEYOND the current end of file (a seek past the end), in
// which case the hole it creates counts against the budget too, exactly as the
// allocation would on a real filesystem.
func (d *SimDisk) bytesThatFitLocked(oldLen, pos int64) int64 {
	if d.capacityBytes <= 0 || d.enospcOnSync {
		return 1<<62 - 1
	}
	// The write consumes budget only for the bytes it adds beyond oldLen.
	headroom := d.capacityBytes - (d.totalBytesLocked() - oldLen)
	// headroom is the largest length this file may reach; the write may fill up
	// to that, starting from pos.
	if fits := headroom - pos; fits > 0 {
		return fits
	}
	return 0
}

// corruptSector flips the first byte of the given sector deterministically.
// The caller holds disk.mu.
func (h *SimFileHandle) corruptSector(sec int) {
	off := sec * sectorSize
	if off < len(h.file.data) {
		// The flip can reach BELOW the durable watermark even when the write
		// itself did not: a write starting mid-sector still corrupts that
		// sector's FIRST byte, which an earlier Sync may already have made
		// durable. Pin the durable image before flipping, so the corruption is
		// what the live process sees and what a later Sync would harden — never
		// a retroactive edit of what is already on the platter.
		h.file.preserveDurableBelow(int64(off))
		h.file.data[off] ^= 0xFF
	}
}

// Seek repositions the handle per the standard io.Seeker whence values and
// returns the resulting absolute offset. A negative resulting offset is an
// error.
func (h *SimFileHandle) Seek(offset int64, whence int) (int64, error) {
	h.disk.mu.Lock()
	defer h.disk.mu.Unlock()
	if err := h.deadLocked(); err != nil {
		return 0, err
	}
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = h.pos + offset
	case io.SeekEnd:
		abs = int64(len(h.file.data)) + offset
	default:
		return 0, errors.New("sim: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("sim: negative seek offset")
	}
	h.pos = abs
	return abs, nil
}

// Truncate resizes the file to size bytes, zero-filling when growing and
// dropping fault marks for sectors that no longer exist.
func (h *SimFileHandle) Truncate(size int64) error {
	if size < 0 {
		return errors.New("sim: negative truncate size")
	}
	h.disk.mu.Lock()
	defer h.disk.mu.Unlock()
	if err := h.deadLocked(); err != nil {
		return err
	}
	if size <= int64(len(h.file.data)) {
		h.file.data = h.file.data[:size]
		h.file.truncateDurableTo(size)
	} else {
		if h.disk.wouldExceedLocked(int64(len(h.file.data)), size) {
			return enospc("truncate", h.path)
		}
		grown := make([]byte, size)
		copy(grown, h.file.data)
		h.file.data = grown
	}
	lastSector := int((size - 1) / sectorSize)
	for sec := range h.file.faulted {
		if int64(size) == 0 || sec > lastSector {
			delete(h.file.faulted, sec)
		}
	}
	return nil
}

// Sync models flushing OS buffers to stable storage. With probability
// faultRate (drawn from the disk's seed) it fails with [ErrSimFault], modelling
// a durability fault. Each call consumes exactly one draw from the seed so the
// fault sequence is reproducible.
func (h *SimFileHandle) Sync() error {
	if err := h.dead(); err != nil {
		return err
	}
	gate, req, err := h.disk.syncOutcome(h.path, h.file)
	// Block on an armed gate with the disk lock RELEASED, so the rest of the disk
	// stays usable while this Sync is held. The outcome was already decided above
	// under the lock, so releasing it here selects the same result the ungated
	// call would have returned — the gate controls only WHEN this Sync returns.
	if gate != nil {
		// Announce that the leader is parked INSIDE the fsync, then hold it there.
		// The announcement must come before the wait, or the controlling goroutine
		// would block for a rendezvous that has already happened.
		gate.reach()
		<-gate.release
	}
	if err != nil {
		// A failed fsync advances nothing: the bytes it was carrying are still
		// only in the page cache, so a crash still loses them.
		return err
	}
	// The durability lands when the fsync RETURNS, not when it was issued. That
	// ordering is the whole point of the split: a host crash while this call was
	// parked means the fsync never completed, and completeSync then declines to
	// grant durability the platter never received.
	h.disk.completeSync(h.file, req)
	return nil
}

// syncRequest is the durability an in-flight Sync will grant if — and only if —
// it completes: the byte watermark captured when the fsync was issued, and the
// host-crash generation it was issued under.
type syncRequest struct {
	covers int64
	gen    uint64
}

// syncOutcome decides one Sync's result under the disk lock and reports whether
// that Sync is gated. It exists so [SimFileHandle.Sync] can block on a gate
// OUTSIDE the lock: holding d.mu across a rendezvous would wedge every other
// file operation on the disk, including the appends of the very committers a
// gated WAL leader is waiting for.
func (d *SimDisk) syncOutcome(path string, f *simFile) (*SyncGate, syncRequest, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Count this Sync first, so the ordinal a one-shot fault is armed against
	// (ArmSyncFaultAt) and the SyncCount observability surface both include it.
	d.syncCount++
	// One-shot rendezvous: claim the gate on its ordinal and disarm. It is
	// resolved by the caller after the lock is dropped.
	var gate *SyncGate
	if d.syncGate != nil && d.syncCount == d.syncGateAt {
		gate = d.syncGate
		d.syncGate = nil
	}
	// Capture what this fsync covers BEFORE the lock is released. Bytes a
	// concurrent committer appends while it runs belong to the next round.
	req := syncRequest{covers: int64(len(f.data)), gen: d.hostCrashGen}
	return gate, req, d.syncResultLocked(path)
}

// completeSync applies the durability an fsync granted by returning nil: it
// advances the file's durable watermark to the length the fsync covered.
//
// It declines when a [SimDisk.CrashHost] intervened between the fsync being
// issued and it completing. That is not a defensive check but the model itself —
// the machine lost power with the fsync in flight, so the write-back never
// happened, and the frames the caller was hardening are gone even though its
// Sync goes on to return nil. That combination (a nil Sync over a durable image
// that does not contain its bytes) is precisely the phantom-commit shape audit
// probe S1 constructed.
func (d *SimDisk) completeSync(f *simFile, req syncRequest) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.hostCrashGen != req.gen {
		return
	}
	f.markSyncedTo(req.covers)
}

// syncResultLocked returns the outcome the arms select for the Sync just
// counted. The caller holds d.mu.
func (d *SimDisk) syncResultLocked(path string) error {
	// One-shot deterministic Sync fault: fire ErrSimFault on the armed ordinal
	// exactly once, then disarm. Checked before the ENOSPC gate and the seed
	// draw and drawing nothing from the seed, so it neither depends on nor
	// perturbs the probabilistic fault stream.
	if d.syncFaultArmed && d.syncCount == d.syncFaultAt {
		d.syncFaultArmed = false
		return ErrSimFault
	}
	// Delayed-allocation disk-full: the bytes were buffered by Write but the
	// backing blocks cannot be allocated, so the out-of-space condition only
	// surfaces here at fsync. Checked before the seed draw and gated on a
	// non-zero capacity, so the default (capacity 0) Sync fault stream is
	// unchanged.
	if d.enospcOnSync && d.capacityBytes > 0 && d.totalBytesLocked() > d.capacityBytes {
		return enospc("fsync", path)
	}
	if d.seed.Bool(d.faultRate) {
		return ErrSimFault
	}
	return nil
}

// Close releases the handle. It is idempotent: a second Close is a no-op.
func (h *SimFileHandle) Close() error {
	// Deliberately NOT gated on [SimFileHandle.deadLocked]: closing a handle the
	// crash already killed is a no-op, not an error. Every caller closes with
	// `defer`, so returning ErrCrashedDisk here would turn the fail-stop of rmp
	// #2544 into a second, spurious failure on a path that is only cleaning up.
	h.closed = true
	return nil
}
