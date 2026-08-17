package sim

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// promoteFsyncCounter is the metric store/recovery increments after it fsyncs
// the parent directory of a snapshot it promoted from a stranded backup. It is
// the recovery package's OWN observable that the interrupted-publish repair
// branch ran, independent of what this package observes on the durable image.
// The name must match the counter emitted in recovery.openCodec.
const promoteFsyncCounter = "store.recovery.snapshot.promoteParentFsync"

// promoteCaptureBackend is a [metrics.Backend] that counts increments of a
// single named counter. It is atomic because the global metrics sink is shared:
// installing it must never race another goroutine's emission.
type promoteCaptureBackend struct {
	target string
	count  atomic.Uint64
}

func (c *promoteCaptureBackend) IncCounter(name string, delta uint64) {
	if name == c.target {
		c.count.Add(delta)
	}
}

func (c *promoteCaptureBackend) ObserveLatency(string, time.Duration) {}

func (c *promoteCaptureBackend) SetGauge(string, float64) {}

// -----------------------------------------------------------------------------
// Reachability of the new SimDisk primitives
// -----------------------------------------------------------------------------

// TestSimDisk_ArmRenameFaultForPath_FiresAndIsOneShot proves the rename fault
// is real: an armed rename onto the keyed destination fails with [ErrSimFault]
// and moves nothing, the fire count records it, and the arm is consumed so the
// retry succeeds. Without the arm the same rename succeeds, which is the
// control that stops the test passing on a disk that simply cannot rename.
func TestSimDisk_ArmRenameFaultForPath_FiresAndIsOneShot(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	writeFile(t, d, "dir/src/f", []byte("payload"))

	d.ArmRenameFaultForPath("dir/dst")
	if err := d.Rename("dir/src", "dir/dst"); !errors.Is(err, ErrSimFault) {
		t.Fatalf("armed Rename returned %v, want ErrSimFault", err)
	}
	if got := d.RenameFaultCount(); got != 1 {
		t.Fatalf("RenameFaultCount = %d, want 1", got)
	}
	// The failed rename must have moved nothing.
	if !d.Exists("dir/src/f") || d.Exists("dir/dst/f") {
		t.Fatalf("a faulted rename moved data: src present=%t dst present=%t",
			d.Exists("dir/src/f"), d.Exists("dir/dst/f"))
	}
	// One-shot: the arm is consumed, so the retry succeeds.
	if err := d.Rename("dir/src", "dir/dst"); err != nil {
		t.Fatalf("retry after a one-shot fault: %v", err)
	}
	if !d.Exists("dir/dst/f") {
		t.Fatal("the retry did not move the subtree")
	}
	if got := d.RenameFaultCount(); got != 1 {
		t.Fatalf("RenameFaultCount = %d after the retry, want it to stay 1", got)
	}
}

// TestSimDisk_RenameArms_InertByDefault is the byte-identity guard: a disk that
// arms nothing must rename exactly as it always did and report zero fires, so
// adding the primitives cannot have perturbed any pre-existing scenario.
func TestSimDisk_RenameArms_InertByDefault(t *testing.T) {
	d := NewSimDisk(NewSeed(2), 0)
	writeFile(t, d, "dir/src/f", []byte("payload"))
	if err := d.Rename("dir/src", "dir/dst"); err != nil {
		t.Fatalf("unarmed Rename: %v", err)
	}
	if !d.Exists("dir/dst/f") {
		t.Fatal("unarmed Rename did not move the subtree")
	}
	if f, w := d.RenameFaultCount(), d.RenameWritebackCount(); f != 0 || w != 0 {
		t.Fatalf("unarmed disk reported fires: faults=%d writebacks=%d, want 0/0", f, w)
	}
	// Disarming with an empty path is accepted and leaves the disk inert.
	d.ArmRenameFaultForPath("")
	d.ArmRenameWritebackForPath("")
	if err := d.Rename("dir/dst", "dir/dst2"); err != nil {
		t.Fatalf("Rename after an empty-path disarm: %v", err)
	}
}

// TestSimDisk_ArmRenameWritebackForPath_SurvivesCrash proves the write-back arm
// changes the crash outcome, with its own control. Both arms stage the subtree
// exactly as the snapshot publish protocol does — write and fsync the
// components, then fsync the staging directory, then rename — so the only
// difference between them is whether the rename's own dirent reached stable
// storage. Unarmed, [SimDisk.Crash] revokes it and the subtree goes with it;
// armed, it survives. Asserting both halves stops the test passing on a disk
// where Crash simply never revokes anything.
func TestSimDisk_ArmRenameWritebackForPath_SurvivesCrash(t *testing.T) {
	stage := func(d *SimDisk) {
		t.Helper()
		writeFile(t, d, "dir/src/f", []byte("payload"))
		// The publish protocol fsyncs the staging directory before the rename,
		// which is what makes the component's own dirent durable; without it
		// the child would be revoked for its own sake and the rename's dirent
		// durability would not be what the test measures.
		if err := d.DirSync("dir/src"); err != nil {
			t.Fatalf("DirSync staging: %v", err)
		}
	}

	// Control: no arm -> the renamed directory's name is revoked.
	ctrl := NewSimDisk(NewSeed(3), 0)
	stage(ctrl)
	if err := ctrl.Rename("dir/src", "dir/dst"); err != nil {
		t.Fatalf("control Rename: %v", err)
	}
	ctrl.Crash()
	if ctrl.Exists("dir/dst/f") {
		t.Fatal("control: an un-fsynced directory rename survived the crash, so the write-back arm below would prove nothing")
	}

	// Armed: the rename is treated as having reached stable storage.
	d := NewSimDisk(NewSeed(3), 0)
	stage(d)
	d.ArmRenameWritebackForPath("dir/dst")
	if err := d.Rename("dir/src", "dir/dst"); err != nil {
		t.Fatalf("armed Rename: %v", err)
	}
	if got := d.RenameWritebackCount(); got != 1 {
		t.Fatalf("RenameWritebackCount = %d, want 1", got)
	}
	d.Crash()
	if !d.Exists("dir/dst/f") {
		t.Fatal("an armed rename write-back did not survive the crash")
	}
}

// -----------------------------------------------------------------------------
// The scenario
// -----------------------------------------------------------------------------

// TestCheckpointCrashStorm_Scenario_Passes runs the registered scenario: a
// crash inside the snapshot publish, at each of the three points of the
// crash-atomic swap, must lose no acknowledged commit and must never load a
// half-published snapshot.
func TestCheckpointCrashStorm_Scenario_Passes(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioCheckpointCrashStorm)
	if !ok {
		t.Fatalf("%s scenario not registered", ScenarioCheckpointCrashStorm)
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("%s run: %v", ScenarioCheckpointCrashStorm, err)
	}
	if report != nil {
		t.Fatalf("%s reported a violation:\n%s", ScenarioCheckpointCrashStorm, report)
	}
}

// TestCheckpointCrashStorm_NonVacuous is the measured-evidence gate: it asserts
// the run really entered every publish window, that each window's armed
// primitive fired, that the publish was raced by live committers, and — the
// crux — that the stranded-backup cycle drove recovery's promote repair, read
// off the durable image rather than assumed.
func TestCheckpointCrashStorm_NonVacuous(t *testing.T) {
	defer goleak.VerifyNone(t)
	ev, report, err := runCheckpointCrashStormWith(context.Background(),
		checkpointCrashStormScenario().DefaultSeed, shortCheckpointStorm(), defaultCheckpointStormOptions())
	if err != nil {
		t.Fatalf("runCheckpointCrashStormWith: %v", err)
	}
	if report != nil {
		t.Fatalf("violation:\n%s", report)
	}
	if len(ev.cycles) != len(checkpointStormWindows()) {
		t.Fatalf("run drove %d cycles, want %d", len(ev.cycles), len(checkpointStormWindows()))
	}
	if ev.acked == 0 {
		t.Fatal("no commit was acknowledged: the durability oracle adjudicated nothing")
	}

	promotes, raced := 0, 0
	for i, c := range ev.cycles {
		t.Logf("cycle %d window=%-16s cpErr=%v faults=%d writebacks=%d syncsDuringCheckpoint=%d "+
			"live/bak before=%t/%t after=%t/%t promoted=%t",
			i, c.window, c.cpErr, c.renameFaults, c.renameWritebacks, c.syncsDuringCheckpoint,
			c.liveBeforeReopen, c.bakBeforeReopen, c.liveAfterReopen, c.bakAfterReopen, c.promoted())
		if c.cpErr == nil {
			t.Errorf("cycle %d (%s): the checkpoint succeeded, so the publish window was never entered", i, c.window)
		} else if !errors.Is(c.cpErr, ErrSimFault) {
			t.Errorf("cycle %d (%s): checkpoint failed with %v, want the injected ErrSimFault", i, c.window, c.cpErr)
		}
		if c.syncsDuringCheckpoint > 0 {
			raced++
		}
		if c.promoted() {
			promotes++
		}
	}
	if promotes == 0 {
		t.Fatal("no cycle drove recovery's snapshot-promote repair: the interrupted-publish branch stayed unexercised")
	}
	if raced == 0 {
		t.Fatal("no interrupted checkpoint had a durable commit land while it ran: the publish window was never raced")
	}
	t.Logf("acked=%d issued=%d recovered=%d (promotes=%d raced=%d)", ev.acked, ev.issued, ev.recovered, promotes, raced)
}

// TestCheckpointCrashStorm_PromoteBranchEnteredMetric corroborates the durable-
// image evidence with store/recovery's OWN observable. It builds the minimal
// interrupted-publish state — a clean checkpoint, then a publish whose parent
// fsync fails with the archive rename written back — crashes, and reopens with
// a recording metrics backend installed, requiring the promote path's
// parent-fsync counter to move.
//
// It reproduces the state directly instead of running the whole scenario
// because the metrics sink is global (see [MetricsOracle]) and must be
// bracketed around serial work only; here the bracket covers just the reopen.
// It must not call t.Parallel for the same reason.
func TestCheckpointCrashStorm_PromoteBranchEnteredMetric(t *testing.T) {
	ctx := context.Background()
	disk := NewSimDisk(NewSeed(0x2465), 0)
	cfg := fullStackStoreConfig()
	snapDir := cfg.dir + "/" + simSnapshotName

	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	eng := NewEngineAdapter(st.Engine())
	writeBatch := func(prefix string) {
		t.Helper()
		for i := 0; i < 4; i++ {
			params := map[string]any{"name": prefix + string(rune('a'+i)), "age": int64(i)}
			if !runWriteCommitted(ctx, eng, "CREATE (:Person {name:$name, age:$age})", params) {
				t.Fatalf("write %v was refused", params["name"])
			}
		}
	}

	// A clean checkpoint publishes the live snapshot the next publish archives,
	// and truncates the WAL prefix holding these commits — so losing the
	// snapshot would lose them for real.
	writeBatch("pre-")
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("clean checkpoint: %v", err)
	}
	writeBatch("post-")

	windowStrandedBackup.arm(disk, snapDir)
	if err := st.Checkpoint(); !errors.Is(err, ErrSimFault) {
		t.Fatalf("interrupted checkpoint returned %v, want the injected ErrSimFault", err)
	}
	st.Crash()

	// The crash must have stranded the backup: that is the precondition of the
	// branch under test, so assert it before claiming the branch ran.
	if disk.Exists(snapDir + "/manifest.json") {
		t.Fatal("the live snapshot survived: the crash did not land in the publish window")
	}
	if !disk.Exists(snapDir + ".bak/manifest.json") {
		t.Fatal("no stranded backup: the archive rename write-back did not take effect")
	}

	capture := &promoteCaptureBackend{target: promoteFsyncCounter}
	metrics.SetBackend(capture)
	st2, reopenErr := OpenSimStore(disk, cfg)
	metrics.SetBackend(nil)
	if reopenErr != nil {
		t.Fatalf("reopen: %v", reopenErr)
	}
	defer func() { _ = st2.Close() }()

	if got := capture.count.Load(); got == 0 {
		t.Fatalf("%s did not move across the reopen: recovery never ran the snapshot-promote repair", promoteFsyncCounter)
	}
	// The image-side observable must agree with the metric.
	if !disk.Exists(snapDir+"/manifest.json") || disk.Exists(snapDir+".bak/manifest.json") {
		t.Fatalf("after the reopen live=%t bak=%t, want the backup promoted onto the live name",
			disk.Exists(snapDir+"/manifest.json"), disk.Exists(snapDir+".bak/manifest.json"))
	}
	// And the commits the lost snapshot folded must be back.
	recovered, _, err := recoveredPersonNames(ctx, st2.Engine())
	if err != nil {
		t.Fatalf("read recovered graph: %v", err)
	}
	for _, name := range []string{"pre-a", "pre-b", "pre-c", "pre-d", "post-a", "post-d"} {
		if _, ok := recovered[name]; !ok {
			t.Errorf("acknowledged commit %q lost across the interrupted publish (recovered %d nodes)", name, len(recovered))
		}
	}
}

// -----------------------------------------------------------------------------
// Sensitivity: the oracles can fail
// -----------------------------------------------------------------------------

// TestCheckpointCrashStorm_SensitivityLostCommit proves the durability oracle
// is live. The stranded backup is destroyed after the crash, so recovery has
// neither a live snapshot nor a backup to promote while the clean checkpoint
// has already truncated the WAL prefix those commits lived in — a genuine lost
// acknowledged commit, produced by damaging the durable image rather than by
// doctoring the oracle's input. The run MUST report a durability violation.
func TestCheckpointCrashStorm_SensitivityLostCommit(t *testing.T) {
	defer goleak.VerifyNone(t)
	opts := defaultCheckpointStormOptions()
	opts.windows = []publishWindow{windowStrandedBackup}
	opts.discardStrandedBackup = true

	_, report, err := runCheckpointCrashStormWith(context.Background(),
		checkpointCrashStormScenario().DefaultSeed, shortCheckpointStorm(), opts)
	if err != nil {
		t.Fatalf("runCheckpointCrashStormWith: %v", err)
	}
	if report == nil {
		t.Fatal("destroying the stranded backup lost acknowledged commits, but the durability oracle stayed silent")
	}
	var durability int
	for _, v := range report.Violations {
		if v.Kind == ViolationACIDDurability {
			durability++
		}
	}
	if durability == 0 {
		t.Fatalf("expected an ACID durability violation, got:\n%s", report)
	}
	t.Logf("sensitivity seam produced %d durability violations", durability)
}

// TestCheckpointCrashStorm_SensitivityDegeneratePlan proves the terminal
// non-vacuity gate is really wired into the run: a plan that drives only one
// window must be rejected for leaving the other two — and the promote repair —
// unexercised, rather than passing because it asserted nothing.
func TestCheckpointCrashStorm_SensitivityDegeneratePlan(t *testing.T) {
	defer goleak.VerifyNone(t)
	opts := defaultCheckpointStormOptions()
	opts.windows = []publishWindow{windowArchiveRename}

	_, report, err := runCheckpointCrashStormWith(context.Background(),
		checkpointCrashStormScenario().DefaultSeed, shortCheckpointStorm(), opts)
	if err != nil {
		t.Fatalf("runCheckpointCrashStormWith: %v", err)
	}
	if report == nil {
		t.Fatal("a degenerate one-window plan passed: the non-vacuity gate is not wired into the run")
	}
	msg := report.String()
	for _, want := range []string{windowStrandedBackup.String(), windowPublishRename.String(), "promote"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the non-vacuity report does not mention %q:\n%s", want, msg)
		}
	}
}
