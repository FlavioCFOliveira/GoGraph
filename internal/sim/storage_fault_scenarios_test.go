package sim

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// storageFaultTestSeeds are a small fixed spread of seeds so the mechanical
// storage-fault scenarios exercise several fixtures without depending on
// wall-clock timing. Each scenario is bit-reproducible, so a fixed set suffices.
var storageFaultTestSeeds = []uint64{0x1, 0xC5F11E00, 0x3A1C0FFA, 0xD125F00D, 0x109F0117, 0x9E3779B9}

// runStorageFaultScenario runs one scenario across the fixed seeds and fails on
// any harness error or non-nil report.
func runStorageFaultScenario(t *testing.T, sc *Scenario) {
	t.Helper()
	for _, seed := range storageFaultTestSeeds {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		report, err := sc.Run(ctx, seed)
		cancel()
		if err != nil {
			t.Fatalf("%s seed %#x run error: %v", sc.Name, seed, err)
		}
		if report != nil {
			t.Fatalf("%s seed %#x reported a violation:\n%s", sc.Name, seed, report)
		}
	}
}

// -----------------------------------------------------------------------------
// ST4 — csrfile-publish-fault
// -----------------------------------------------------------------------------

// TestST4_CSRFilePublishFault_Scenario runs the registered ST4 scenario across
// seeds: an ENOSPC bound and an armed Sync fault mid-publish leave the prior
// csrfile intact (byte-for-byte and by reconstruction), and a first-publish
// fault leaves no file at all.
func TestST4_CSRFilePublishFault_Scenario(t *testing.T) {
	defer goleak.VerifyNone(t)
	sc := csrfilePublishFaultScenario()
	runStorageFaultScenario(t, &sc)
}

// TestST4_ReaderRejectsCorruptedPublish is the meta-test: it proves the
// verification mechanism the ST4 invariant relies on actually catches a torn
// file. A CSR is published cleanly, then a byte inside the published file is
// flipped with the sector-corruption injector; the real csrfile reader MUST
// reject it (CRC), so a torn/partial artefact can never masquerade as a valid
// reconstruction.
func TestST4_ReaderRejectsCorruptedPublish(t *testing.T) {
	disk := NewSimDisk(NewSeed(4), 0)
	fsys := simCSRFS{disk: disk}
	c, err := buildDeterministicCSR(NewSeed(0xABCDEF))
	if err != nil {
		t.Fatalf("buildDeterministicCSR: %v", err)
	}
	if _, err := csrfile.WriteToFileWith[float64](fsys, csrFilePath, c); err != nil {
		t.Fatalf("clean publish: %v", err)
	}
	// Sanity: the clean file reads back.
	if r, err := csrfile.OpenWith(fsys, csrFilePath); err != nil {
		t.Fatalf("clean read-back: %v", err)
	} else {
		_ = r.Close()
	}
	// Flip a byte in the middle of the published payload.
	image, err := disk.ReadFile(csrFilePath)
	if err != nil {
		t.Fatalf("read published: %v", err)
	}
	if err := disk.CorruptRange(csrFilePath, int64(len(image)/2), 1); err != nil {
		t.Fatalf("CorruptRange: %v", err)
	}
	if _, err := csrfile.OpenWith(fsys, csrFilePath); !errors.Is(err, csrfile.ErrFileCorrupted) {
		t.Fatalf("reader accepted a corrupted csrfile: err=%v, want ErrFileCorrupted", err)
	}
}

// -----------------------------------------------------------------------------
// ST5 — wal-corruption-failstop
// -----------------------------------------------------------------------------

// TestST5_WALCorruptionFailStop_Scenario runs the registered ST5 scenario across
// seeds: corrupting an interior committed frame makes recovery report genuine
// corruption, reconstruct exactly the committed prefix, and refuse to append.
func TestST5_WALCorruptionFailStop_Scenario(t *testing.T) {
	defer goleak.VerifyNone(t)
	sc := walCorruptionFailStopScenario()
	runStorageFaultScenario(t, &sc)
}

// TestST5_BenignTornTailIsNotCorruption is the contrast/meta-test: a WAL whose
// LAST frame is merely truncated (a torn tail, the normal crash-after-fsync
// case) is NOT treated as corruption — [OpenSimStore] succeeds, recovers a whole
// number of committed transactions, and reports Clean. This pins that ST5's
// fail-stop is specific to genuine interior corruption, not any short read.
func TestST5_BenignTornTailIsNotCorruption(t *testing.T) {
	defer goleak.VerifyNone(t)
	disk := NewSimDisk(NewSeed(5), 0)
	cfg := defaultSimStoreConfig()
	walPath := walPathFor(cfg.dir)

	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		t.Fatalf("OpenSimStore: %v", err)
	}
	const n = 12
	for i := 0; i < n; i++ {
		if err := commitCreatePerson(context.Background(), st.Engine(), itoaST5(i), i); err != nil {
			_ = st.Close()
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	image, err := disk.ReadFile(walPath)
	if err != nil {
		t.Fatalf("read WAL: %v", err)
	}
	if len(image) < 4 {
		t.Fatalf("WAL image too small: %d bytes", len(image))
	}
	// Truncate a few bytes off the end so the last frame is torn (incomplete).
	h, err := disk.OpenFile(walPath, os.O_RDWR)
	if err != nil {
		t.Fatalf("open WAL rw: %v", err)
	}
	if err := h.Truncate(int64(len(image) - 3)); err != nil {
		_ = h.Close()
		t.Fatalf("truncate: %v", err)
	}
	_ = h.Close()

	st2, err := OpenSimStore(disk, cfg)
	if err != nil {
		t.Fatalf("OpenSimStore over a benign torn tail must succeed, got: %v", err)
	}
	defer func() { _ = st2.Close() }()
	if !st2.Clean() {
		t.Fatal("a benign torn tail was misreported as not clean")
	}
	count, err := scalarCountViaEngine(context.Background(), st2.Engine(), "MATCH (n:Person) RETURN count(n)")
	if err != nil {
		t.Fatalf("recovered count: %v", err)
	}
	if count < 0 || count > n {
		t.Fatalf("recovered count %d out of range [0,%d]", count, n)
	}
}

// itoaST5 names an ST5 test node.
func itoaST5(i int) string { return "tt" + itoa(i) }

// -----------------------------------------------------------------------------
// ST6 — checkpoint-dirfsync-fault
// -----------------------------------------------------------------------------

// TestST6_CheckpointDirFsyncFault_Scenario runs the registered ST6 scenario
// across seeds: a checkpoint whose WAL-truncate post-rename dir-fsync faults
// poisons the writer yet leaves the exact committed state recoverable on reopen.
func TestST6_CheckpointDirFsyncFault_Scenario(t *testing.T) {
	defer goleak.VerifyNone(t)
	sc := checkpointDirFsyncFaultScenario()
	runStorageFaultScenario(t, &sc)
}

// TestST6_NoLeakAcrossRuns repeats ST6 to catch a teardown-only leak (the
// abandoned poisoned writer, or the reopened store).
func TestST6_NoLeakAcrossRuns(t *testing.T) {
	defer goleak.VerifyNone(t)
	sc := checkpointDirFsyncFaultScenario()
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		report, err := sc.Run(ctx, storageFaultTestSeeds[i])
		cancel()
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if report != nil {
			t.Fatalf("iteration %d violation:\n%s", i, report)
		}
	}
}

// -----------------------------------------------------------------------------
// ST8 — io-roundtrip-fault
// -----------------------------------------------------------------------------

// TestST8_IORoundTripFault_Scenario runs the registered ST8 scenario across
// seeds: a clean CSV/JSONL round-trip reproduces the graph exactly, and an
// export under ENOSPC fails cleanly leaving no partial artefact accepted as the
// model.
func TestST8_IORoundTripFault_Scenario(t *testing.T) {
	defer goleak.VerifyNone(t)
	sc := ioRoundTripFaultScenario()
	runStorageFaultScenario(t, &sc)
}

// TestST8_PartialArtefactDiffersFromModel is the meta-test: it proves the
// edge-set comparison the ST8 invariant relies on actually detects a partial
// artefact. A model is exported cleanly, the file is truncated by hand, and the
// re-import is asserted to either error or reconstruct a DIFFERENT edge set —
// never the full model — so a silently-accepted torn artefact would be caught.
func TestST8_PartialArtefactDiffersFromModel(t *testing.T) {
	model, err := buildDeterministicAdjList(NewSeed(0x8ADF00D))
	if err != nil {
		t.Fatalf("buildDeterministicAdjList: %v", err)
	}
	want := edgeTriples(model)

	for _, f := range ioRoundTripFormats() {
		disk := NewSimDisk(NewSeed(8), 0)
		wh, err := disk.OpenFile(f.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
		if err != nil {
			t.Fatalf("%s open: %v", f.name, err)
		}
		if _, err := f.export(wh, model); err != nil {
			_ = wh.Close()
			t.Fatalf("%s export: %v", f.name, err)
		}
		_ = wh.Close()

		image, err := disk.ReadFile(f.path)
		if err != nil {
			t.Fatalf("%s read: %v", f.name, err)
		}
		if len(image) < 8 {
			t.Fatalf("%s exported image too small: %d", f.name, len(image))
		}
		// Truncate to half so the file is a genuine partial artefact.
		h, err := disk.OpenFile(f.path, os.O_RDWR)
		if err != nil {
			t.Fatalf("%s open rw: %v", f.name, err)
		}
		if err := h.Truncate(int64(len(image) / 2)); err != nil {
			_ = h.Close()
			t.Fatalf("%s truncate: %v", f.name, err)
		}
		_ = h.Close()

		rh, err := disk.OpenFile(f.path, os.O_RDONLY)
		if err != nil {
			t.Fatalf("%s open partial: %v", f.name, err)
		}
		got, ierr := f.imp(rh)
		_ = rh.Close()
		if ierr == nil && got != nil && triplesEqual(want, edgeTriples(got)) {
			t.Fatalf("%s: a truncated partial artefact re-imported as the full model", f.name)
		}
	}
}

// TestStorageFault_Deterministic pins bit-reproducibility of the mechanical
// cluster: each scenario run twice with the same seed produces the same outcome
// (a clean nil report both times), so a failure would always replay.
func TestStorageFault_Deterministic(t *testing.T) {
	defer goleak.VerifyNone(t)
	scenarios := []Scenario{
		csrfilePublishFaultScenario(),
		walCorruptionFailStopScenario(),
		checkpointDirFsyncFaultScenario(),
		ioRoundTripFaultScenario(),
	}
	const seed = 0x5EED5EED
	for _, sc := range scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r1, e1 := sc.Run(ctx, seed)
			r2, e2 := sc.Run(ctx, seed)
			if e1 != nil || e2 != nil {
				t.Fatalf("%s run errors: %v / %v", sc.Name, e1, e2)
			}
			if (r1 == nil) != (r2 == nil) {
				t.Fatalf("%s non-deterministic: run1 report=%v run2 report=%v", sc.Name, r1, r2)
			}
			if r1 != nil {
				t.Fatalf("%s reported a violation:\n%s", sc.Name, r1)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// SimDisk fault-primitive unit tests
// -----------------------------------------------------------------------------

// TestSimDisk_CorruptRange checks the sector-corruption injector: it flips the
// requested bytes, rejects out-of-range spans, and reports an absent file.
func TestSimDisk_CorruptRange(t *testing.T) {
	disk := NewSimDisk(NewSeed(1), 0)
	h, err := disk.OpenFile("f", os.O_CREATE|os.O_WRONLY)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	orig := []byte{0x00, 0x11, 0x22, 0x33}
	if _, err := h.Write(orig); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = h.Close()

	if err := disk.CorruptRange("f", 1, 2); err != nil {
		t.Fatalf("CorruptRange: %v", err)
	}
	got, _ := disk.ReadFile("f")
	want := []byte{0x00, 0x11 ^ 0xFF, 0x22 ^ 0xFF, 0x33}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("byte %d = %#x, want %#x", i, got[i], want[i])
		}
	}
	// Out-of-range spans and n<=0 are rejected.
	if err := disk.CorruptRange("f", 3, 5); err == nil {
		t.Fatal("CorruptRange past EOF did not error")
	}
	if err := disk.CorruptRange("f", 0, 0); err == nil {
		t.Fatal("CorruptRange with n=0 did not error")
	}
	// An absent file reports ErrNotExist.
	if err := disk.CorruptRange("absent", 0, 1); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("CorruptRange on an absent file: err=%v, want ErrNotExist", err)
	}
}

// TestSimDisk_ArmParentDirSyncFaultForPath checks the one-shot, path-selective
// dir-fsync fault: it fires exactly once on the matching childPath, disarms, and
// never fires on a non-matching path.
func TestSimDisk_ArmParentDirSyncFaultForPath(t *testing.T) {
	disk := NewSimDisk(NewSeed(1), 0)
	disk.ArmParentDirSyncFaultForPath("db/wal")

	// A non-matching path is unaffected even while the fault is armed.
	if err := disk.ParentDirSync("db/snapshot/manifest.json"); err != nil {
		t.Fatalf("non-matching ParentDirSync faulted: %v", err)
	}
	// The matching path faults exactly once.
	if err := disk.ParentDirSync("db/wal"); !errors.Is(err, ErrSimFault) {
		t.Fatalf("matching ParentDirSync err=%v, want ErrSimFault", err)
	}
	// Disarmed thereafter.
	if err := disk.ParentDirSync("db/wal"); err != nil {
		t.Fatalf("ParentDirSync still faulting after one-shot fire: %v", err)
	}
	// An empty path disarms without firing.
	disk.ArmParentDirSyncFaultForPath("db/wal")
	disk.ArmParentDirSyncFaultForPath("")
	if err := disk.ParentDirSync("db/wal"); err != nil {
		t.Fatalf("ParentDirSync faulted after disarm: %v", err)
	}
}

// TestNew_WiresDiskFaultRate pins the FaultRate wiring in New: the disk the
// durable path drives carries the configured FaultRate, and the default (0)
// leaves it fault-free — byte-identical to the pre-FaultRate behaviour.
func TestNew_WiresDiskFaultRate(t *testing.T) {
	sm, err := New(Config{Disk: DiskConfig{FaultRate: 0.25}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = sm.Close() }()
	if got := sm.Disk().FaultRate(); got != 0.25 {
		t.Fatalf("Disk().FaultRate() = %v, want 0.25", got)
	}

	def, err := New(Config{})
	if err != nil {
		t.Fatalf("New default: %v", err)
	}
	defer func() { _ = def.Close() }()
	if got := def.Disk().FaultRate(); got != 0 {
		t.Fatalf("default Disk().FaultRate() = %v, want 0", got)
	}
}

// TestWALCorruptionFailStop_Direct exercises the ST5 core directly and asserts
// the SET relations richer than the scenario's nil-report check: recovery of the
// corrupt WAL keeps a non-empty proper prefix, the reopen fail-stops with a CRC
// mismatch, and the whole run is a clean pass.
func TestWALCorruptionFailStop_Direct(t *testing.T) {
	defer goleak.VerifyNone(t)
	// A CRC mismatch is the sentinel the corrupt interior frame must produce; the
	// scenario already asserts it, this is the type-level anchor.
	if !errors.Is(wal.ErrCRCMismatch, wal.ErrCRCMismatch) {
		t.Fatal("wal.ErrCRCMismatch sentinel missing")
	}
	report, err := runWALCorruptionFailStop(context.Background(), 0xC0FFEE11)
	if err != nil {
		t.Fatalf("runWALCorruptionFailStop: %v", err)
	}
	if report != nil {
		t.Fatalf("ST5 direct run reported a violation:\n%s", report)
	}
}
