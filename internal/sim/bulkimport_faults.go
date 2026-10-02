package sim

// bulkimport_faults.go — fault injection into a bulk-import publish, through
// the store/bulkimport filesystem seam (rmp #2518).
//
// [bulkimport.PublishFS] and [bulkimport.ImportIntoFS] route the importer's
// empty-directory check, its store-directory creation and the whole snapshot
// write through a caller-supplied filesystem. This arm backs that filesystem
// with a [SimDisk] and drives a publish of the scenario's fixture into every
// fault regime the seam makes reachable:
//
//   - ENOSPC, surfaced eagerly at the growing write and, separately, at Sync
//     (delayed allocation).
//   - A failing fsync on the first component file and on the last one.
//   - A failing rename of `snapshot.tmp` onto `snapshot`, the instant the
//     publish's atomicity claim rests on.
//   - A process crash at EVERY filesystem operation of the publish — each
//     directory operation and each component file's Write, Sync and Close — in
//     turn, followed by a host crash of the disk.
//   - A crash immediately after the publish rename with that rename written
//     back ([SimDisk.ArmRenameWritebackForPath]), the other legal outcome of a
//     crash in the publish window.
//
// After each fault the disk is host-crashed and the store reopened through real
// recovery, and the outcome must be ALL OR NOTHING: either recovery finds no
// snapshot and an empty graph, or it finds the snapshot and the recovered graph
// equals the harness model exactly. Every regime also proves its fault FIRED —
// an errno matched, a fault counter advanced, a crash point reached — so a
// regime whose arm silently missed cannot pass as coverage.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"syscall"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/bulkimport"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// bulkImportFaultStoreDir is the store directory every fault regime publishes
// into on its SimDisk. It is nested: SimDisk exempts root-level names from crash
// revocation, so a root-level store directory would hide whether the publish
// makes the directory it creates durable. Nested, both "root" and
// "root/bistore" are created by the publish, and "root/bistore" survives a host
// crash only if the publish fsyncs its parent (rmp #2970).
const bulkImportFaultStoreDir = "root/bistore"

// bulkImportENOSPCCapacity is the disk bound for the ENOSPC regimes. It is half
// the non-vacuity floor on the published snapshot's size, so a publish of the
// fixture cannot fit.
const bulkImportENOSPCCapacity = bulkImportMinSnapshotBytes / 2

// Outcomes of one adjudicated regime.
const (
	bulkImportOutcomeEmpty    = "empty"
	bulkImportOutcomeComplete = "complete"
)

// errBulkImportCrashed is what every filesystem operation returns from the
// instant a [bulkImportCrashFS] crash point is reached: the process is dead, so
// nothing it attempts afterwards reaches the disk.
var errBulkImportCrashed = errors.New("sim: bulk-import publish process crashed")

// bulkImportFaultRegime is the measured result of one fault regime.
type bulkImportFaultRegime struct {
	// name identifies the regime in reports.
	name string
	// outcome is what recovery found after the host crash: empty or complete.
	outcome string
	// fired reports that the injected fault was observed to fire.
	fired bool
	// publishFailed reports that the publish returned an error.
	publishFailed bool
}

// bulkImportFaultEvidence is everything the fault arm measured.
type bulkImportFaultEvidence struct {
	// regimes are the single-fault regimes, in a fixed order.
	regimes []bulkImportFaultRegime
	// cleanOps is how many filesystem operations a clean publish performs; it is
	// the number of crash points the sweep covers.
	cleanOps int
	// cleanSyncs is how many component-file fsyncs a clean publish performs.
	cleanSyncs int64
	// renameOp is the 1-based index of the publish rename among cleanOps.
	renameOp int
	// controlOutcome is what recovery found after a CLEAN publish and a host
	// crash: the acknowledged import must survive.
	controlOutcome string
	// crashFired counts sweep points at which the crash actually fired;
	// crashEmpty and crashComplete count their adjudicated outcomes.
	crashFired    int
	crashEmpty    int
	crashComplete int
	// crashAcked counts sweep points at which the publish still returned nil
	// (the crash hit a best-effort operation after the publish was durable).
	crashAcked int
}

// bulkImportCrashFS wraps the SimDisk-backed bulk-import seam and kills the
// publishing process at its crashAt-th filesystem operation (1-based; 0 never
// crashes). Every operation counts: each directory operation and each Write,
// Sync and Close on a component file. From the crash point on, every operation
// fails with [errBulkImportCrashed] and touches nothing, which is what a dead
// process does to the disk.
//
// It also records the index of the publish rename, so a regime can place a
// crash immediately after it.
//
// Not safe for concurrent use; a publish is single-goroutine.
type bulkImportCrashFS struct {
	inner      simBulkImportFS
	renameDest string
	crashAt    int
	ops        int
	renameOp   int
	crashed    bool
}

// step accounts for one operation and reports whether the process is dead.
func (c *bulkImportCrashFS) step() error {
	if c.crashed {
		return errBulkImportCrashed
	}
	c.ops++
	if c.crashAt > 0 && c.ops == c.crashAt {
		c.crashed = true
		return errBulkImportCrashed
	}
	return nil
}

func (c *bulkImportCrashFS) ReadDir(dir string) ([]fs.DirEntry, error) {
	if err := c.step(); err != nil {
		return nil, err
	}
	return c.inner.ReadDir(dir)
}

func (c *bulkImportCrashFS) MkdirAll(dir string, perm fs.FileMode) error {
	if err := c.step(); err != nil {
		return err
	}
	return c.inner.MkdirAll(dir, perm)
}

func (c *bulkImportCrashFS) Create(path string) (snapshot.File, error) {
	if err := c.step(); err != nil {
		return nil, err
	}
	f, err := c.inner.Create(path)
	if err != nil {
		return nil, err
	}
	return &bulkImportCrashFile{File: f, fs: c}, nil
}

func (c *bulkImportCrashFS) OpenComponent(path string) (snapshot.ReadFile, error) {
	if err := c.step(); err != nil {
		return nil, err
	}
	return c.inner.OpenComponent(path)
}

func (c *bulkImportCrashFS) Open(path string) (snapshot.ReadFile, error) {
	if err := c.step(); err != nil {
		return nil, err
	}
	return c.inner.Open(path)
}

func (c *bulkImportCrashFS) Rename(oldPath, newPath string) error {
	if err := c.step(); err != nil {
		return err
	}
	if newPath == c.renameDest {
		c.renameOp = c.ops
	}
	return c.inner.Rename(oldPath, newPath)
}

func (c *bulkImportCrashFS) Remove(path string) error {
	if err := c.step(); err != nil {
		return err
	}
	return c.inner.Remove(path)
}

func (c *bulkImportCrashFS) RemoveAll(path string) error {
	if err := c.step(); err != nil {
		return err
	}
	return c.inner.RemoveAll(path)
}

func (c *bulkImportCrashFS) Stat(path string) (fs.FileInfo, error) {
	if err := c.step(); err != nil {
		return nil, err
	}
	return c.inner.Stat(path)
}

func (c *bulkImportCrashFS) DirSync(path string) error {
	if err := c.step(); err != nil {
		return err
	}
	return c.inner.DirSync(path)
}

func (c *bulkImportCrashFS) ParentDirSync(childPath string) error {
	if err := c.step(); err != nil {
		return err
	}
	return c.inner.ParentDirSync(childPath)
}

// bulkImportCrashFile is a component file whose Write, Sync and Close are crash
// points of the owning [bulkImportCrashFS]. Stat is not: it reads metadata and
// changes nothing on disk.
type bulkImportCrashFile struct {
	snapshot.File
	fs *bulkImportCrashFS
}

func (f *bulkImportCrashFile) Write(p []byte) (int, error) {
	if err := f.fs.step(); err != nil {
		return 0, err
	}
	return f.File.Write(p)
}

func (f *bulkImportCrashFile) Sync() error {
	if err := f.fs.step(); err != nil {
		return err
	}
	return f.File.Sync()
}

func (f *bulkImportCrashFile) Close() error {
	if err := f.fs.step(); err != nil {
		return err
	}
	return f.File.Close()
}

// bulkImportCheckFaults runs every fault regime against a finished builder of
// the scenario's fixture and adjudicates each outcome against the model.
func bulkImportCheckFaults(
	ctx context.Context, seed uint64, m *bulkImportModel,
	b *bulkimport.Builder[int64], nodes []bulkimport.Node, edges []bulkimport.Edge[int64],
) (bulkImportFaultEvidence, []Violation, error) {
	var (
		ev bulkImportFaultEvidence
		v  []Violation
	)
	snapDest := bulkImportFaultStoreDir + "/snapshot"
	newDisk := func() *SimDisk { return NewSimDisk(NewSeed(seed), 0) }

	// --- Control: a clean publish through the counting seam, then a host crash.
	// It sizes the crash sweep and locates the publish rename, and the
	// acknowledged import must survive the crash.
	disk := newDisk()
	disk.ArmSyncFaultAt(0) // resets the Sync counter without arming a fault
	count := &bulkImportCrashFS{inner: simBulkImportFS{simSnapshotFS{disk}}, renameDest: snapDest}
	if _, err := bulkimport.PublishFS[int64](ctx, count, bulkImportFaultStoreDir, b, nil); err != nil {
		return ev, nil, fmt.Errorf("sim: bulkimport-faults clean publish: %w", err)
	}
	ev.cleanOps, ev.renameOp, ev.cleanSyncs = count.ops, count.renameOp, disk.SyncCount()
	outcome, cv, err := bulkImportAdjudicate(disk, m, "clean publish, then a host crash")
	if err != nil {
		return ev, nil, err
	}
	ev.controlOutcome = outcome
	v = append(v, cv...)
	if outcome != bulkImportOutcomeComplete {
		v = append(v, bulkImportFaultViolation(fmt.Sprintf(
			"an acknowledged bulk-import publish did not survive a host crash: recovery found %s", outcome)))
	}

	// --- Single-fault regimes.
	regimes := []struct {
		arm   func(d *SimDisk)
		fired func(d *SimDisk, err error) bool
		name  string
		// viaImportInto publishes through ImportIntoFS rather than PublishFS,
		// so both seamed entry points are exercised.
		viaImportInto bool
	}{
		{
			name:          "ENOSPC at write",
			viaImportInto: true,
			arm:           func(d *SimDisk) { d.SetCapacity(bulkImportENOSPCCapacity, false) },
			fired:         func(_ *SimDisk, err error) bool { return errors.Is(err, syscall.ENOSPC) },
		},
		{
			name:  "ENOSPC at sync",
			arm:   func(d *SimDisk) { d.SetCapacity(bulkImportENOSPCCapacity, true) },
			fired: func(_ *SimDisk, err error) bool { return errors.Is(err, syscall.ENOSPC) },
		},
		{
			name: "fsync fault on the first component",
			arm:  func(d *SimDisk) { d.ArmSyncFaultAt(1) },
			fired: func(d *SimDisk, err error) bool {
				return errors.Is(err, ErrSimFault) && d.SyncCount() >= 1
			},
		},
		{
			name: "fsync fault on the last component",
			arm:  func(d *SimDisk) { d.ArmSyncFaultAt(ev.cleanSyncs) },
			fired: func(d *SimDisk, err error) bool {
				return errors.Is(err, ErrSimFault) && d.SyncCount() == ev.cleanSyncs
			},
		},
		{
			name:  "rename fault on snapshot.tmp -> snapshot",
			arm:   func(d *SimDisk) { d.ArmRenameFaultForPath(snapDest) },
			fired: func(d *SimDisk, _ error) bool { return d.RenameFaultCount() == 1 },
		},
	}
	for _, r := range regimes {
		d := newDisk()
		r.arm(d)
		fsys := simBulkImportFS{simSnapshotFS{d}}
		var perr error
		if r.viaImportInto {
			_, perr = bulkimport.ImportIntoFS[int64](ctx, fsys, bulkImportFaultStoreDir,
				bulkimport.Options{Directed: true, Multigraph: true, ExpectNodes: bulkImportNodes}, nodes, edges)
		} else {
			_, perr = bulkimport.PublishFS[int64](ctx, fsys, bulkImportFaultStoreDir, b, nil)
		}
		reg := bulkImportFaultRegime{name: r.name, publishFailed: perr != nil, fired: r.fired(d, perr)}
		d.SetCapacity(0, false) // lift the bound so recovery's own cleanup is not refused
		out, rv, aerr := bulkImportAdjudicate(d, m, r.name)
		if aerr != nil {
			return ev, nil, aerr
		}
		reg.outcome = out
		v = append(v, rv...)
		ev.regimes = append(ev.regimes, reg)
	}

	// --- Crash after the publish rename, with that rename written back.
	{
		d := newDisk()
		d.ArmRenameWritebackForPath(snapDest)
		c := &bulkImportCrashFS{
			inner: simBulkImportFS{simSnapshotFS{d}}, renameDest: snapDest, crashAt: ev.renameOp + 1,
		}
		_, perr := bulkimport.PublishFS[int64](ctx, c, bulkImportFaultStoreDir, b, nil)
		reg := bulkImportFaultRegime{
			name:          "crash after the publish rename, rename written back",
			publishFailed: perr != nil,
			fired:         c.crashed && d.RenameWritebackCount() == 1,
		}
		out, rv, aerr := bulkImportAdjudicate(d, m, reg.name)
		if aerr != nil {
			return ev, nil, aerr
		}
		reg.outcome = out
		v = append(v, rv...)
		ev.regimes = append(ev.regimes, reg)
	}

	// --- Crash sweep: the process dies at each operation in turn.
	for k := 1; k <= ev.cleanOps; k++ {
		d := newDisk()
		c := &bulkImportCrashFS{inner: simBulkImportFS{simSnapshotFS{d}}, renameDest: snapDest, crashAt: k}
		_, perr := bulkimport.PublishFS[int64](ctx, c, bulkImportFaultStoreDir, b, nil)
		if c.crashed {
			ev.crashFired++
		}
		out, rv, aerr := bulkImportAdjudicate(d, m, fmt.Sprintf("crash at operation %d of %d", k, ev.cleanOps))
		if aerr != nil {
			return ev, nil, aerr
		}
		v = append(v, rv...)
		// A crash at an operation whose failure the publish deliberately ignores
		// — the best-effort removal of a stale backup AFTER the publish is
		// durable — lets the call return nil in this model, where a real process
		// would simply be gone. Such a return is not a defect; acknowledging an
		// import that did NOT survive is.
		if perr == nil {
			ev.crashAcked++
			if out != bulkImportOutcomeComplete {
				v = append(v, bulkImportFaultViolation(fmt.Sprintf(
					"crash point %d of %d: the publish reported success but recovery found %s",
					k, ev.cleanOps, out)))
			}
		}
		switch out {
		case bulkImportOutcomeEmpty:
			ev.crashEmpty++
		case bulkImportOutcomeComplete:
			ev.crashComplete++
		}
	}

	v = append(v, bulkImportCheckFaultEvidence(&ev)...)
	return ev, v, nil
}

// bulkImportCheckFaultEvidence turns the measured fault evidence into
// violations: every regime must have fired and must have failed its publish,
// and the sweep must have reached every crash point. It is separate from the
// run so a test can prove each clause can fail.
func bulkImportCheckFaultEvidence(ev *bulkImportFaultEvidence) []Violation {
	var v []Violation
	if ev.cleanOps == 0 || ev.renameOp == 0 || ev.cleanSyncs == 0 {
		v = append(v, bulkImportFaultViolation(fmt.Sprintf(
			"the clean publish measured %d operations, rename at %d, %d fsyncs — the seam was not exercised",
			ev.cleanOps, ev.renameOp, ev.cleanSyncs)))
	}
	if ev.crashFired != ev.cleanOps {
		v = append(v, bulkImportFaultViolation(fmt.Sprintf(
			"the crash fired at %d of %d sweep points", ev.crashFired, ev.cleanOps)))
	}
	for _, r := range ev.regimes {
		if !r.fired {
			v = append(v, bulkImportFaultViolation(fmt.Sprintf(
				"regime %q: the injected fault was never observed to fire, so the regime proves nothing", r.name)))
		}
		if !r.publishFailed {
			v = append(v, bulkImportFaultViolation(fmt.Sprintf(
				"regime %q: the publish reported success although its fault fired", r.name)))
		}
	}
	return v
}

// bulkImportAdjudicate host-crashes disk, reopens the fault store through real
// recovery, and requires an all-or-nothing outcome: no snapshot and an empty
// graph, or the snapshot and a graph equal to the model.
func bulkImportAdjudicate(disk *SimDisk, m *bulkImportModel, what string) (string, []Violation, error) {
	disk.Crash()
	if !disk.Exists(bulkImportFaultStoreDir) {
		return bulkImportOutcomeEmpty, nil, nil
	}
	res, err := recovery.OpenFS[string, int64](simRecoveryFS{disk}, bulkImportFaultStoreDir,
		recovery.Options[string, int64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewInt64WeightCodec(),
		})
	if err != nil {
		return "", []Violation{bulkImportFaultViolation(fmt.Sprintf(
			"%s: recovery refused the store: %v", what, err))}, nil
	}
	if res.Graph == nil {
		return "", nil, fmt.Errorf("sim: bulkimport-faults %s: recovery returned a nil graph", what)
	}
	return bulkImportJudgeOutcome(m, res.SnapshotHit, res.WALOps, res.Graph, what)
}

// bulkImportJudgeOutcome classifies one recovered store as empty or complete,
// and reports a violation for anything in between.
func bulkImportJudgeOutcome(
	m *bulkImportModel, snapshotHit bool, walOps int, g *lpg.Graph[string, int64], what string,
) (string, []Violation, error) {
	if walOps != 0 {
		return "", []Violation{bulkImportFaultViolation(fmt.Sprintf(
			"%s: recovery replayed %d WAL ops, but a bulk import writes no WAL", what, walOps))}, nil
	}
	if !snapshotHit {
		if n := g.LiveOrderStored(); n != 0 {
			return "", []Violation{bulkImportFaultViolation(fmt.Sprintf(
				"%s: recovery found no snapshot yet a graph of %d nodes — a partial import is visible", what, n))}, nil
		}
		return bulkImportOutcomeEmpty, nil, nil
	}
	var scratch bulkImportEvidence
	if pv := bulkImportCheckParity(m, g, &scratch); len(pv) > 0 {
		return "", []Violation{bulkImportFaultViolation(fmt.Sprintf(
			"%s: recovery found a snapshot that does not equal the model (%d discrepancies, first: %s) — "+
				"a partial import is visible", what, len(pv), pv[0].Message))}, nil
	}
	return bulkImportOutcomeComplete, nil, nil
}

// bulkImportFaultViolation builds one fault-arm violation.
func bulkImportFaultViolation(msg string) Violation {
	return Violation{Kind: ViolationACIDAtomicity, Op: "<bulk import publish under fault>", Message: msg}
}
