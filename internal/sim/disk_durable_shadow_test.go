package sim

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// disk_durable_shadow_test.go — harness-fidelity gates for the DATA half of the
// SimDisk crash model (rmp #2535, audit finding F1).
//
// A successful write(2) carries no durability whatever. Linux write(2) NOTES:
// "A successful return from write() does not make any guarantee that data has
// been committed to disk… The only way to be sure is to call fsync(2)." POSIX
// reaches durability only through Successfully Transferred (XBD §3.377), i.e.
// via fsync/fdatasync/O_SYNC. Until #2535 [SimDisk.Crash] kept every byte ever
// written while revoking names as if power had been lost — host-crash semantics
// for metadata, process-crash semantics for data, a shape no real event has.
// Every DST assertion of the form "the commit was acked, therefore the bytes
// are recovered" was consequently satisfied irrespective of any fsync.
//
// The gates below are written to the project's three-gate shape: an
// UNCONDITIONAL verdict gate that fails on an illegal outcome; a SEPARATE
// shape-only gate proving the situation actually arose (an oracle that cannot
// fail proves nothing); and witness detail via t.Logf only, because an unmet
// precondition is a fact to report, not a defect.

// walShapedFrames appends n fixed-size frames to path through one handle and
// returns the handle, the number of bytes appended, and the frame size. It
// reproduces the WAL's access pattern — open once, append repeatedly — because
// that pattern is what the durable watermark is optimised for and what audit
// probes Q3 and P1b used.
func walShapedFrames(t *testing.T, d *SimDisk, path string, n int) (*SimFileHandle, int64) {
	t.Helper()
	h, err := d.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		t.Fatalf("OpenFile %s: %v", path, err)
	}
	var total int64
	for i := 0; i < n; i++ {
		frame := []byte(fmt.Sprintf("FRAME-%010d", i)) // 16 bytes, as probe Q3
		got, werr := h.Write(frame)
		if werr != nil {
			t.Fatalf("Write frame %d: %v", i, werr)
		}
		total += int64(got)
	}
	return h, total
}

// TestSimDisk_HostCrashDiscardsEveryUnsyncedByte is audit probe Q3, encoded as a
// regression gate: 100 WAL-shaped frames totalling 1600 bytes, NO Sync at all,
// one ParentDirSync so only the NAME is durable, then a crash.
//
// The probe measured 1600 of 1600 bytes surviving. On the fixed model the file
// must survive as an empty file: its dirent is durable, its contents are not.
// This test fails on the pre-#2535 code and passes after it.
func TestSimDisk_HostCrashDiscardsEveryUnsyncedByte(t *testing.T) {
	d := NewSimDisk(NewSeed(0x2535), 0) // no data faults: isolate the durability model

	h, appended := walShapedFrames(t, d, "db/wal", 100)
	if err := d.ParentDirSync("db/wal"); err != nil {
		t.Fatalf("ParentDirSync: %v", err)
	}
	syncsBefore := d.SyncCount()
	d.Crash()
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	after, err := d.ReadFile("db/wal")
	if err != nil {
		t.Fatalf("ReadFile after crash: %v", err)
	}
	t.Logf("appended=%d syncs=%d survived=%d discarded=%d kind=%s",
		appended, syncsBefore, len(after), d.LastCrashDiscardedBytes(), d.LastCrashKind())

	// (i) UNCONDITIONAL verdict gate: not one unsynced byte may survive.
	if len(after) != 0 {
		t.Errorf("host crash kept %d of %d bytes that were never fsync'd; "+
			"a successful write carries no durability (Linux write(2) NOTES, POSIX XBD "+
			"Successfully Transferred), so every one of them must be gone",
			len(after), appended)
	}
	// The name is a separate concern and must NOT have been lost: the parent was
	// fsync'd. Losing it too would be a different infidelity.
	if !d.Exists("db/wal") {
		t.Errorf("host crash lost the NAME db/wal although its parent was fsync'd; "+
			"only the DATA was unsynced (discarded=%d)", d.LastCrashDiscardedBytes())
	}

	// (ii) SEPARATE shape-only non-vacuity gate: prove the situation arose at all.
	// Without these the verdict above would hold vacuously on an empty file.
	if appended != 1600 {
		t.Errorf("shape: appended %d bytes, want the probe's 1600", appended)
	}
	if syncsBefore != 0 {
		t.Errorf("shape: SyncCount=%d before the crash, want 0 — the probe fsyncs nothing, "+
			"so a non-zero count means the setup, not the model, made the bytes durable", syncsBefore)
	}
	if got := d.LastCrashDiscardedBytes(); got != appended {
		t.Errorf("shape: crash reported %d discarded bytes, want %d", got, appended)
	}
	if got := d.LastCrashKind(); got != CrashKindHost {
		t.Errorf("shape: LastCrashKind=%s, want %s — Crash must alias CrashHost", got, CrashKindHost)
	}
}

// TestSimDisk_HostCrashKeepsTheSyncedPrefixAndDropsTheTail is the other half of
// the contract: fsync is load-bearing in BOTH directions. Bytes an fsync
// covered must survive a power failure, and only those.
func TestSimDisk_HostCrashKeepsTheSyncedPrefixAndDropsTheTail(t *testing.T) {
	d := NewSimDisk(NewSeed(0x2535_01), 0)

	h, err := d.OpenFile("wal", os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	durable := []byte("ACKED-COMMIT-1;")
	if _, err := h.Write(durable); err != nil {
		t.Fatalf("Write durable: %v", err)
	}
	if err := h.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	lost := []byte("UNACKED-COMMIT-2;")
	if _, err := h.Write(lost); err != nil {
		t.Fatalf("Write lost: %v", err)
	}
	watermark, _ := d.DurableSize("wal")
	live, err := d.ReadFile("wal")
	if err != nil {
		t.Fatalf("ReadFile before crash: %v", err)
	}
	d.Crash()
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	after, err := d.ReadFile("wal")
	if err != nil {
		t.Fatalf("ReadFile after crash: %v", err)
	}
	t.Logf("live=%q durable-watermark=%d after-crash=%q discarded=%d",
		live, watermark, after, d.LastCrashDiscardedBytes())

	// (i) UNCONDITIONAL verdict gate.
	if !bytes.Equal(after, durable) {
		t.Errorf("after host crash the file is %q, want exactly the fsync'd prefix %q", after, durable)
	}

	// (ii) SEPARATE shape-only non-vacuity gate.
	if len(live) != len(durable)+len(lost) {
		t.Errorf("shape: live image is %d bytes, want %d — the unsynced tail was never written",
			len(live), len(durable)+len(lost))
	}
	if watermark != int64(len(durable)) {
		t.Errorf("shape: durable watermark is %d, want %d — the Sync did not advance it as expected",
			watermark, len(durable))
	}
	if got := d.LastCrashDiscardedBytes(); got != int64(len(lost)) {
		t.Errorf("shape: crash discarded %d bytes, want %d", got, len(lost))
	}
}

// TestSimDisk_ProcessCrashRetainsDataAndNames pins the OTHER primitive. A
// SIGKILL kills the process, not the kernel: every byte a write(2) accepted and
// every dirent a create/rename produced is still there for the next process,
// fsync'd or not. RocksDB's filesystem abstraction names exactly this level —
// FSWritableFile::Flush, "the data should survive a process crash but is not
// necessarily persisted to stable storage. Use Sync() for that guarantee."
func TestSimDisk_ProcessCrashRetainsDataAndNames(t *testing.T) {
	d := NewSimDisk(NewSeed(0x2535_02), 0)

	h, appended := walShapedFrames(t, d, "db/wal", 100)
	// A name whose parent was NEVER fsync'd: a host crash would revoke it, a
	// process crash must not.
	writeUnsyncedName(t, d, "db/snapshot.tmp/manifest.json", []byte("staged"))
	d.CrashProcess()
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	after, err := d.ReadFile("db/wal")
	if err != nil {
		t.Fatalf("ReadFile after process crash: %v", err)
	}
	t.Logf("appended=%d survived=%d staging-present=%t kind=%s discarded=%d",
		appended, len(after), d.Exists("db/snapshot.tmp/manifest.json"),
		d.LastCrashKind(), d.LastCrashDiscardedBytes())

	// (i) UNCONDITIONAL verdict gate: nothing is discarded by a process crash.
	if int64(len(after)) != appended {
		t.Errorf("process crash kept %d of %d written bytes; kill -9 does not empty the "+
			"kernel page cache, so every byte write(2) accepted must survive", len(after), appended)
	}
	if !d.Exists("db/snapshot.tmp/manifest.json") {
		t.Error("process crash revoked a never-fsync'd dirent; dirent revocation is a " +
			"HOST-crash effect and must not appear here")
	}
	if got := d.LastCrashDiscardedBytes(); got != 0 {
		t.Errorf("process crash reported %d discarded bytes, want 0", got)
	}

	// (ii) SEPARATE shape-only non-vacuity gate.
	if appended == 0 {
		t.Error("shape: nothing was written, so the retention claim is vacuous")
	}
	// The retention claim is only interesting for bytes that were NEVER fsync'd,
	// so read the WAL's own watermark rather than the disk-wide SyncCount — the
	// staging file this scenario also creates is deliberately fsync'd, and a
	// global counter would conflate the two.
	if watermark, ok := d.DurableSize("db/wal"); !ok || watermark != 0 {
		t.Errorf("shape: db/wal durable watermark is %d (present=%t), want 0 — the bytes the "+
			"verdict above says survived must be bytes no fsync ever covered", watermark, ok)
	}
	if got := d.LastCrashKind(); got != CrashKindProcess {
		t.Errorf("shape: LastCrashKind=%s, want %s", got, CrashKindProcess)
	}
}

// writeUnsyncedName creates path with contents and fsyncs the FILE but never its
// parent directory, so the name is live but not crash-durable.
func writeUnsyncedName(t *testing.T, d *SimDisk, path string, data []byte) {
	t.Helper()
	h, err := d.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		t.Fatalf("OpenFile %s: %v", path, err)
	}
	if _, err := h.Write(data); err != nil {
		t.Fatalf("Write %s: %v", path, err)
	}
	if err := h.Sync(); err != nil {
		t.Fatalf("Sync %s: %v", path, err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close %s: %v", path, err)
	}
}

// TestSimDisk_CrashPrimitivesAreDistinguishable drives ONE identical setup
// through each primitive and requires the two durable images to differ. If they
// agreed, the split would be decorative and a scenario's declaration of which
// event it means would carry no information.
func TestSimDisk_CrashPrimitivesAreDistinguishable(t *testing.T) {
	stage := func(t *testing.T) (*SimDisk, *SimFileHandle, int64) {
		t.Helper()
		d := NewSimDisk(NewSeed(0x2535_03), 0)
		h, appended := walShapedFrames(t, d, "db/wal", 50)
		// Make the WAL's NAME durable so the data axis is measured on a file that
		// survives both primitives; without this a host crash revokes the name and
		// the comparison degenerates into "the file is gone".
		if err := d.ParentDirSync("db/wal"); err != nil {
			t.Fatalf("ParentDirSync: %v", err)
		}
		writeUnsyncedName(t, d, "db/snapshot.tmp/manifest.json", []byte("staged"))
		return d, h, appended
	}

	hostDisk, hostHandle, appended := stage(t)
	hostDisk.CrashHost()
	if err := hostHandle.Close(); err != nil {
		t.Fatalf("Close host: %v", err)
	}
	hostBytes, err := hostDisk.ReadFile("db/wal")
	if err != nil {
		t.Fatalf("ReadFile host: %v", err)
	}
	hostStaging := hostDisk.Exists("db/snapshot.tmp/manifest.json")

	procDisk, procHandle, _ := stage(t)
	procDisk.CrashProcess()
	if err := procHandle.Close(); err != nil {
		t.Fatalf("Close process: %v", err)
	}
	procBytes, err := procDisk.ReadFile("db/wal")
	if err != nil {
		t.Fatalf("ReadFile process: %v", err)
	}
	procStaging := procDisk.Exists("db/snapshot.tmp/manifest.json")

	t.Logf("appended=%d host: bytes=%d staging=%t | process: bytes=%d staging=%t",
		appended, len(hostBytes), hostStaging, len(procBytes), procStaging)

	// (i) UNCONDITIONAL verdict gate: the two models must disagree, on BOTH axes.
	if len(hostBytes) == len(procBytes) {
		t.Errorf("both primitives left %d WAL bytes; a host crash must discard the unsynced "+
			"data a process crash keeps, or the split is decorative", len(hostBytes))
	}
	if hostStaging == procStaging {
		t.Errorf("both primitives left staging-present=%t; a host crash must revoke the "+
			"never-fsync'd dirent a process crash keeps", hostStaging)
	}

	// (ii) SEPARATE shape-only non-vacuity gate: the setup really did create both
	// an unsynced byte range and an unsynced dirent.
	if appended == 0 {
		t.Error("shape: no bytes were appended, so the data axis is vacuous")
	}
	if !procStaging {
		t.Error("shape: the process arm lost the staging name, so the dirent axis measured " +
			"something other than the intended difference")
	}
}

// TestSimDisk_FailedSyncDoesNotAdvanceTheDurableImage: an fsync that returns an
// error hardens nothing. The bytes it was carrying are still only in the page
// cache, so a host crash must still lose them.
//
// Scope note: this pins the FAILURE path of the watermark only. That a RETRY of
// a failed fsync then reports success while the data is gone — the 20-year
// PostgreSQL bug — is audit finding F6, filed separately as rmp #2540.
func TestSimDisk_FailedSyncDoesNotAdvanceTheDurableImage(t *testing.T) {
	d := NewSimDisk(NewSeed(0x2535_04), 0)

	h, err := d.OpenFile("wal", os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := h.Write([]byte("BEFORE;")); err != nil {
		t.Fatalf("Write before: %v", err)
	}
	if err := h.Sync(); err != nil {
		t.Fatalf("Sync before: %v", err)
	}
	d.ArmSyncFaultAt(1)
	if _, err := h.Write([]byte("AFTER-FAILED-FSYNC;")); err != nil {
		t.Fatalf("Write after: %v", err)
	}
	syncErr := h.Sync()
	watermark, _ := d.DurableSize("wal")
	d.Crash()
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	after, err := d.ReadFile("wal")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	t.Logf("sync err=%v watermark=%d after-crash=%q discarded=%d",
		syncErr, watermark, after, d.LastCrashDiscardedBytes())

	// (i) UNCONDITIONAL verdict gate.
	if !bytes.Equal(after, []byte("BEFORE;")) {
		t.Errorf("after a FAILED fsync and a host crash the file is %q, want %q — a failed "+
			"fsync must harden nothing", after, "BEFORE;")
	}

	// (ii) SEPARATE shape-only non-vacuity gate: the fault really fired.
	if !errors.Is(syncErr, ErrSimFault) {
		t.Errorf("shape: Sync returned %v, want ErrSimFault — the arm did not fire, so the "+
			"verdict above tested the ordinary success path instead", syncErr)
	}
	if d.LastCrashDiscardedBytes() == 0 {
		t.Error("shape: the crash discarded nothing, so the verdict holds vacuously")
	}
}

// TestSimDisk_OverwriteBelowTheWatermarkKeepsTheDurableBytes exercises the
// copy-on-write half of the cheap watermark representation. A watermark alone
// would describe the durable image as "the first N bytes of the CURRENT buffer",
// which silently rewrites history the moment a write lands under N. The durable
// image must keep the bytes the platter actually holds.
func TestSimDisk_OverwriteBelowTheWatermarkKeepsTheDurableBytes(t *testing.T) {
	d := NewSimDisk(NewSeed(0x2535_05), 0)

	h, err := d.OpenFile("f", os.O_CREATE|os.O_WRONLY)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := h.Write([]byte("AAAABBBB")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := h.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// Rewrite the first four bytes in place and never fsync again.
	if _, err := h.Seek(0, 0); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if _, err := h.Write([]byte("ZZZZ")); err != nil {
		t.Fatalf("Write overwrite: %v", err)
	}
	live, err := d.ReadFile("f")
	if err != nil {
		t.Fatalf("ReadFile live: %v", err)
	}
	durableBefore, err := d.DurableImage("f")
	if err != nil {
		t.Fatalf("DurableImage: %v", err)
	}
	d.Crash()
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after, err := d.ReadFile("f")
	if err != nil {
		t.Fatalf("ReadFile after: %v", err)
	}
	t.Logf("live=%q durable=%q after-crash=%q", live, durableBefore, after)

	// (i) UNCONDITIONAL verdict gate.
	if !bytes.Equal(after, []byte("AAAABBBB")) {
		t.Errorf("after host crash the file is %q, want the last fsync'd content %q", after, "AAAABBBB")
	}

	// (ii) SEPARATE shape-only non-vacuity gate: the live image really did diverge.
	if !bytes.Equal(live, []byte("ZZZZBBBB")) {
		t.Errorf("shape: live image is %q, want %q — the overwrite did not happen, so the "+
			"verdict tested nothing", live, "ZZZZBBBB")
	}
}

// TestSimDisk_HostCrashDuringAnInFlightFsyncGrantsNoDurability is the
// disk-level phantom-commit gate (audit probe S1). ArmSyncGateAt parks a Sync
// inside the fsync; the machine loses power while it is parked; the call then
// returns nil because the arm selected success. That nil is a lie the model must
// not make true: the write-back never completed, so the bytes are gone.
//
// This is the mechanism that makes the WAL's own phantom-commit window
// (leadGroupSyncLocked fsyncs with w.mu released) expressible at all.
func TestSimDisk_HostCrashDuringAnInFlightFsyncGrantsNoDurability(t *testing.T) {
	d := NewSimDisk(NewSeed(0x2535_06), 0)

	h, err := d.OpenFile("wal", os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := h.Write([]byte("ACKED;")); err != nil {
		t.Fatalf("Write acked: %v", err)
	}
	if err := h.Sync(); err != nil {
		t.Fatalf("Sync acked: %v", err)
	}
	if _, err := h.Write([]byte("IN-FLIGHT;")); err != nil {
		t.Fatalf("Write in-flight: %v", err)
	}

	gate := d.ArmSyncGateAt(1)
	if gate == nil {
		t.Fatal("ArmSyncGateAt returned nil")
	}
	done := make(chan error, 1)
	go func() { done <- h.Sync() }()
	<-gate.Reached()
	// The power fails with the fsync in flight.
	d.CrashHost()
	gate.Release()
	parkedErr := <-done

	after, err := d.ReadFile("wal")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	watermark, _ := d.DurableSize("wal")
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	t.Logf("gate fired=%t parked Sync returned=%v after-crash=%q watermark=%d discarded=%d",
		gate.Fired(), parkedErr, after, watermark, d.LastCrashDiscardedBytes())

	// (i) UNCONDITIONAL verdict gate: the in-flight frames are NOT durable.
	if bytes.Contains(after, []byte("IN-FLIGHT;")) {
		t.Errorf("the durable image %q contains bytes whose fsync was still in flight when the "+
			"host crashed; that fsync never completed, so granting them durability manufactures "+
			"a phantom commit", after)
	}
	if !bytes.Equal(after, []byte("ACKED;")) {
		t.Errorf("durable image is %q, want the previously acked prefix %q", after, "ACKED;")
	}
	// The parked call completing after the crash must not re-harden them either.
	if watermark != int64(len("ACKED;")) {
		t.Errorf("durable watermark is %d after the parked Sync returned, want %d — the "+
			"completing fsync re-hardened bytes the crash had already discarded",
			watermark, len("ACKED;"))
	}

	// (ii) SEPARATE shape-only non-vacuity gate: the crash really landed INSIDE
	// the fsync window, and that fsync really did report success.
	if !gate.Fired() {
		t.Error("shape: the gate never fired, so the crash did not land inside an fsync and " +
			"the verdict above tested an ordinary post-fsync crash")
	}
	if parkedErr != nil {
		t.Errorf("shape: the parked Sync returned %v, want nil — the phantom-commit shape "+
			"requires a fsync that REPORTS SUCCESS over bytes that are gone", parkedErr)
	}
}

// TestSimDisk_CorruptRangeSurvivesAHostCrash guards a trap the durable shadow
// creates: [SimDisk.CorruptRange] documents itself as corrupting the
// ALREADY-DURABLE image, and if it touched only the live bytes then the next
// host crash would restore the durable image over them and silently reduce the
// bad-sector injector to a no-op.
func TestSimDisk_CorruptRangeSurvivesAHostCrash(t *testing.T) {
	d := NewSimDisk(NewSeed(0x2535_07), 0)

	h, err := d.OpenFile("wal", os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	clean := []byte("DURABLE-FRAME;")
	if _, err := h.Write(clean); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := h.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// A write BELOW the watermark first, so the durable image is materialised and
	// the second (explicit) representation is the one under test.
	if _, err := h.Seek(0, 0); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if _, err := h.Write([]byte("X")); err != nil {
		t.Fatalf("Write overwrite: %v", err)
	}
	if err := d.CorruptRange("wal", 3, 4); err != nil {
		t.Fatalf("CorruptRange: %v", err)
	}
	// The exact image the crash must leave: the fsync'd content with ONLY the
	// four CorruptRange bytes flipped. The unsynced overwrite at offset 0 is not
	// part of it.
	want := append([]byte(nil), clean...)
	for i := 3; i < 7; i++ {
		want[i] ^= 0xFF
	}
	d.Crash()
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after, err := d.ReadFile("wal")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	t.Logf("clean=%q want=%q after-crash=%q", clean, want, after)

	// (i) UNCONDITIONAL verdict gate: exactly the corrupted durable image, and
	// nothing else. Equality catches both failure directions at once — a crash
	// that repaired the sector (after == clean) and a crash that kept the
	// unsynced overwrite as well.
	if !bytes.Equal(after, want) {
		t.Errorf("after host crash the file is %q, want %q; CorruptRange must damage the "+
			"DURABLE image (or a crash silently repairs the sector) while the unsynced "+
			"in-place overwrite must NOT survive", after, want)
	}

	// (ii) SEPARATE shape-only non-vacuity gate: the injector really flipped bytes,
	// so the equality above is not comparing two pristine images.
	if bytes.Equal(want, clean) {
		t.Error("shape: the expected image equals the clean one, so CorruptRange was asked " +
			"to flip nothing and the verdict holds vacuously")
	}
}

// TestSimDisk_UnsyncedTruncationDoesNotSurviveACrash is the F8 contract (rmp
// #2542): a truncation is a metadata-and-data change that does not reach stable
// storage until an fsync, so a crash in between restores the longer prior image.
//
// This test used to assert the OPPOSITE and named #2542 as the place it would
// change. It is inverted rather than deleted, because the boundary it marks is
// still worth pinning — it is simply on the other side of it now.
func TestSimDisk_UnsyncedTruncationDoesNotSurviveACrash(t *testing.T) {
	d := NewSimDisk(NewSeed(0x2535_08), 0)

	h, err := d.OpenFile("wal", os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := h.Write([]byte("LONG-PREVIOUS-CONTENT")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := h.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := h.Truncate(4); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	watermark, _ := d.DurableSize("wal")
	d.Crash()
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after, err := d.ReadFile("wal")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	t.Logf("watermark-after-truncate=%d after-crash=%q", watermark, after)

	// A truncation is a metadata-and-data change that does not reach stable
	// storage until an fsync, so a crash in between restores the LONGER prior
	// image (#2542). This test pinned the opposite until that landed, and named
	// #2542 as the place it would change.
	const prior = "LONG-PREVIOUS-CONTENT"
	if string(after) != prior {
		t.Errorf("after truncate+crash the file is %q (%d bytes), want the longer prior image "+
			"%q (%d bytes): the truncation was never fsynced, so it did not happen",
			after, len(after), prior, len(prior))
	}
	if watermark != int64(len(prior)) {
		t.Errorf("durable watermark is %d after an unsynced truncation to 4, want %d — the "+
			"durable image legitimately EXCEEDS the live length here, because the platter still "+
			"holds the previous generation's bytes", watermark, len(prior))
	}
}

// TestSimDisk_TruncationBecomesDurableOnTheNextSync is the other half: once the
// truncation is fsynced it is real, so the crash keeps the SHORT file. Without
// this arm the test above is satisfied by a model in which a truncation never
// becomes durable at all.
func TestSimDisk_TruncationBecomesDurableOnTheNextSync(t *testing.T) {
	d := NewSimDisk(NewSeed(0x2542_01), 0)

	h, err := d.OpenFile("wal", os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := h.Write([]byte("LONG-PREVIOUS-CONTENT")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := h.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := h.Truncate(4); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	// THIS is what makes the truncation real.
	if err := h.Sync(); err != nil {
		t.Fatalf("Sync after truncate: %v", err)
	}
	watermark, _ := d.DurableSize("wal")
	d.Crash()
	_ = h.Close()

	after, err := d.ReadFile("wal")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(after) != 4 {
		t.Errorf("after truncate+SYNC+crash the file is %d bytes (%q), want 4: an fsync flushes "+
			"metadata, so it is what makes a truncation durable", len(after), after)
	}
	if watermark != 4 {
		t.Errorf("durable watermark is %d after a SYNCED truncation to 4, want 4", watermark)
	}
}

// TestSimDisk_DurableWatermarkNeverExceedsTheLiveLength is a cheap invariant
// sweep over the operations that can shorten a file, run with the shortening
// FSYNCED so the truncation is durable.
//
// The invariant it guards is representational: while the durable image is the
// watermark form (data[:durableLen]) a watermark above the live length would
// slice past the buffer. An UNSYNCED truncation deliberately breaks that
// relationship — the platter still holds the longer previous generation, which
// is the whole point of #2542 — and the representation handles it by pinning an
// explicit durable image instead of a watermark. So this sweep syncs, which is
// what makes the watermark form the live representation again.
func TestSimDisk_DurableWatermarkNeverExceedsTheLiveLength(t *testing.T) {
	cases := []struct {
		name   string
		shrink func(t *testing.T, d *SimDisk, h *SimFileHandle)
	}{
		{"handle-truncate", func(t *testing.T, _ *SimDisk, h *SimFileHandle) {
			if err := h.Truncate(3); err != nil {
				t.Fatalf("Truncate: %v", err)
			}
		}},
		{"truncate-path", func(t *testing.T, d *SimDisk, _ *SimFileHandle) {
			if err := d.TruncatePath("wal", 3); err != nil {
				t.Fatalf("TruncatePath: %v", err)
			}
		}},
		{"reopen-o-trunc", func(t *testing.T, d *SimDisk, _ *SimFileHandle) {
			h2, err := d.OpenFile("wal", os.O_WRONLY|os.O_TRUNC)
			if err != nil {
				t.Fatalf("OpenFile O_TRUNC: %v", err)
			}
			if err := h2.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewSimDisk(NewSeed(0x2535_09), 0)
			h, err := d.OpenFile("wal", os.O_CREATE|os.O_WRONLY|os.O_APPEND)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			if _, err := h.Write([]byte("0123456789")); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if err := h.Sync(); err != nil {
				t.Fatalf("Sync: %v", err)
			}
			tc.shrink(t, d, h)
			// Make the shortening DURABLE, so the watermark form applies. Without
			// this the durable image is the longer prior one by design (#2542) and
			// the invariant below would be measuring the wrong model.
			syncPathForTest(t, d, "wal")
			watermark, ok := d.DurableSize("wal")
			live, err := d.ReadFile("wal")
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			d.Crash()
			after, err := d.ReadFile("wal")
			if err != nil {
				t.Fatalf("ReadFile after: %v", err)
			}
			if err := h.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			t.Logf("%s: live=%d watermark=%d after-crash=%d", tc.name, len(live), watermark, len(after))

			// (i) UNCONDITIONAL verdict gate.
			if !ok {
				t.Fatal("the file vanished; the shrink was not the operation under test")
			}
			if watermark > int64(len(live)) {
				t.Errorf("durable watermark %d exceeds the live length %d", watermark, len(live))
			}
			if len(after) > len(live) {
				t.Errorf("the crash GREW the file from %d to %d bytes", len(live), len(after))
			}

			// (ii) SEPARATE shape-only non-vacuity gate: a shrink really happened.
			if len(live) >= 10 {
				t.Errorf("shape: the file is still %d bytes, so no shrink occurred and the "+
					"invariant was not exercised", len(live))
			}
		})
	}
}

// TestWALPhantomCommit_CrashInsideTheLeaderFsyncLosesTheFlushedSuffix is audit
// probe S1 driven through the REAL [github.com/FlavioCFOliveira/GoGraph/store/wal.Writer],
// not the bare disk.
//
// The window is structural: leadGroupSyncLocked flushes the bufio buffer under
// w.mu and then fsyncs with w.mu RELEASED. Between those two steps the
// committer's frames — and its OpCommit marker — are in the kernel's hands but
// on nobody's platter, and the committer has not been acknowledged. Probe S1
// measured, on the pre-#2535 model:
//
//	S1 gate fired=true  committer's Sync returned=<nil> (never acked)
//	S1 durable image after crash = "TXN-1;COMMIT-1;TXN-2;COMMIT-2;"
//	S1 unacked TXN-2 bytes present in the durable image: true
//
// The frames were in the durable image because EVERY byte was, whatever the
// fsync did — which is what made the harness look as though the engine had
// manufactured a phantom commit (the shape charged to it under rmp #2322). With
// the durable shadow the flushed-but-unsynced suffix is simply gone, so the
// engine is no longer accused of a window it does not have.
//
// Recovery replaying MORE than was acknowledged is not itself a defect — the
// ACID clause is "acked implies durable", not its converse — so the oracle here
// is the sharp one: bytes whose fsync never completed must not be durable.
func TestWALPhantomCommit_CrashInsideTheLeaderFsyncLosesTheFlushedSuffix(t *testing.T) {
	defer goleak.VerifyNone(t)

	const path = "phantom_commit.wal"
	disk := NewSimDisk(NewSeed(0x2535_5001), 0) // faultRate 0: only the gate acts
	w, err := wal.OpenFS(simWALFS{disk: disk}, path)
	if err != nil {
		t.Fatalf("OpenFS: %v", err)
	}

	// (1) An acknowledged, durable commit. It is the control: whatever the crash
	// does to the in-flight suffix, this must still be there.
	ackedMark, err := w.AppendRun(func(emit func([]byte) error) error { return emit(groupCommitFrame(0xC0)) })
	if err != nil {
		t.Fatalf("append acked: %v", err)
	}
	if err := w.SyncGroup(ackedMark); err != nil {
		t.Fatalf("sync acked: %v", err)
	}
	ackedImage, err := disk.DurableImage(path)
	if err != nil {
		t.Fatalf("DurableImage after acked commit: %v", err)
	}

	// (2) The in-flight commit: appended, never acknowledged.
	inflightMark, err := w.AppendRun(func(emit func([]byte) error) error { return emit(groupCommitFrame(0xA0)) })
	if err != nil {
		t.Fatalf("append in-flight: %v", err)
	}

	gate := disk.ArmSyncGateAt(1)
	if gate == nil {
		t.Fatal("ArmSyncGateAt returned nil")
	}
	done := make(chan error, 1)
	go func() { done <- w.SyncGroup(inflightMark) }()

	// (3) Park the leader INSIDE its fsync. Its flush has already pushed the
	// frames into the disk's live image; nothing below them is durable.
	<-gate.Reached()
	liveDuringFsync, err := disk.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile during fsync: %v", err)
	}
	durableDuringFsync, err := disk.DurableImage(path)
	if err != nil {
		t.Fatalf("DurableImage during fsync: %v", err)
	}

	// (4) The machine loses power with the fsync in flight.
	disk.CrashHost()
	gate.Release()
	leaderErr := <-done

	afterCrash, err := disk.DurableImage(path)
	if err != nil {
		t.Fatalf("DurableImage after crash: %v", err)
	}
	live, err := disk.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after crash: %v", err)
	}
	t.Logf("gate fired=%t leader SyncGroup returned=%v acked-image=%dB live-during-fsync=%dB "+
		"durable-during-fsync=%dB after-crash=%dB(live %dB) discarded=%d",
		gate.Fired(), leaderErr, len(ackedImage), len(liveDuringFsync),
		len(durableDuringFsync), len(afterCrash), len(live), disk.LastCrashDiscardedBytes())

	// (i) UNCONDITIONAL verdict gate: the durable image is the acknowledged
	// prefix and nothing more. The in-flight frames' fsync never completed.
	if !bytes.Equal(afterCrash, ackedImage) {
		t.Errorf("durable image after the crash is %d bytes, want the %d-byte acknowledged "+
			"prefix; bytes whose fsync was still in flight when power failed must not be durable",
			len(afterCrash), len(ackedImage))
	}
	if bytes.Contains(afterCrash, groupCommitFrame(0xA0)) {
		t.Error("the durable image contains the IN-FLIGHT committer's frame although its fsync " +
			"never completed — that is the phantom-commit shape the harness must not manufacture")
	}
	// The acknowledged commit is the other half of the contract and must survive.
	if !bytes.Contains(afterCrash, groupCommitFrame(0xC0)) {
		t.Error("the durable image lost the ACKNOWLEDGED commit's frame; a crash may discard " +
			"only what no completed fsync covered")
	}

	// (ii) SEPARATE shape-only non-vacuity gates. Without these the verdict could
	// hold because the leader never reached the window, or because its flush
	// never actually put the frames on the disk — either of which would make the
	// oracle untestable rather than satisfied.
	if !gate.Fired() {
		t.Error("shape: the gate never fired, so the crash did not land inside the leader's " +
			"fsync and the verdict tested an ordinary post-commit crash")
	}
	if !bytes.Contains(liveDuringFsync, groupCommitFrame(0xA0)) {
		t.Error("shape: the in-flight frame was NOT in the disk's live image while the leader " +
			"was parked, so leadGroupSyncLocked's flush-then-fsync window was never entered " +
			"and there was nothing for the crash to discard")
	}
	if len(durableDuringFsync) != len(ackedImage) {
		t.Errorf("shape: the durable image had already grown to %d bytes while the fsync was "+
			"still parked (want %d) — the watermark advanced at fsync ISSUE rather than at "+
			"completion, which would make the window unobservable",
			len(durableDuringFsync), len(ackedImage))
	}
	if disk.LastCrashDiscardedBytes() == 0 {
		t.Error("shape: the crash discarded no bytes, so the verdict holds vacuously")
	}

	// Tear the writer down without letting its Close fsync anything back: the
	// process it belonged to is dead.
	_ = w.Close()
}

// syncPathForTest fsyncs the file at path through a fresh handle, so a test can
// make a change durable without holding on to the handle that made it.
func syncPathForTest(t *testing.T, d *SimDisk, path string) {
	t.Helper()
	sh, err := d.OpenFile(path, os.O_WRONLY)
	if err != nil {
		t.Fatalf("open %s to fsync it: %v", path, err)
	}
	if err := sh.Sync(); err != nil {
		t.Fatalf("Sync %s: %v", path, err)
	}
	if err := sh.Close(); err != nil {
		t.Fatalf("Close %s: %v", path, err)
	}
}

// TestSimDisk_UnsyncedTruncationAtEverySite is acceptance criterion 1: the same
// outcome at all THREE truncation sites, which is what proves they share one
// mechanism rather than three.
//
// The staging-file creators in this codebase all open with O_TRUNC
// (store/snapshot/safe_create.go, store/csrfile/fs.go) as does the WAL suffix
// temporary (store/wal/writer.go), so "the .tmp I truncated still holds a
// previous generation's bytes" is a state this module can genuinely reach.
func TestSimDisk_UnsyncedTruncationAtEverySite(t *testing.T) {
	const prior = "PREVIOUS-GENERATION-BYTES"

	for _, tc := range []struct {
		name  string
		trunc func(t *testing.T, d *SimDisk, h *SimFileHandle)
	}{
		{"handle-truncate", func(t *testing.T, _ *SimDisk, h *SimFileHandle) {
			if err := h.Truncate(5); err != nil {
				t.Fatalf("Truncate: %v", err)
			}
		}},
		{"truncate-path", func(t *testing.T, d *SimDisk, _ *SimFileHandle) {
			if err := d.TruncatePath("stage.tmp", 5); err != nil {
				t.Fatalf("TruncatePath: %v", err)
			}
		}},
		{"reopen-o-trunc", func(t *testing.T, d *SimDisk, _ *SimFileHandle) {
			h2, err := d.OpenFile("stage.tmp", os.O_WRONLY|os.O_TRUNC)
			if err != nil {
				t.Fatalf("OpenFile O_TRUNC: %v", err)
			}
			if _, err := h2.Write([]byte("NEW")); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if err := h2.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewSimDisk(NewSeed(0x2542_02), 0)
			h, err := d.OpenFile("stage.tmp", os.O_CREATE|os.O_RDWR|os.O_APPEND)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			if _, err := h.Write([]byte(prior)); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if err := h.Sync(); err != nil {
				t.Fatalf("Sync: %v", err)
			}

			tc.trunc(t, d, h)
			live, err := d.ReadFile("stage.tmp")
			if err != nil {
				t.Fatalf("ReadFile (live): %v", err)
			}
			// Read-your-writes: the truncation is visible to this process at once.
			if len(live) >= len(prior) {
				t.Fatalf("the live file is %d bytes after truncating a %d-byte file; the "+
					"truncation must be visible immediately", len(live), len(prior))
			}

			d.Crash()
			_ = h.Close()

			after, err := d.ReadFile("stage.tmp")
			if err != nil {
				t.Fatalf("ReadFile (after crash): %v", err)
			}
			if string(after) != prior {
				t.Errorf("after an UNSYNCED truncation and a crash the file is %q (%d bytes), "+
					"want the longer prior image %q (%d bytes). All three truncation sites go "+
					"through one rule, so a difference here means they have drifted (#2542)",
					after, len(after), prior, len(prior))
			}
		})
	}
}

// TestSimDisk_TruncatePathDropsFaultMarks settles F13. TruncatePath used to keep
// the fault marks for sectors past the new end of file while the handle Truncate
// dropped them, with no reason recorded for the difference.
//
// Keeping them is wrong: a fault mark names a SECTOR OF THIS FILE, and a sector
// past the end of file does not exist. A later grow that happens to place bytes
// there would be corrupted by a fault that never touched them.
func TestSimDisk_TruncatePathDropsFaultMarks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		trunc func(t *testing.T, d *SimDisk, h *SimFileHandle)
	}{
		{"handle-truncate", func(t *testing.T, _ *SimDisk, h *SimFileHandle) {
			if err := h.Truncate(0); err != nil {
				t.Fatalf("Truncate: %v", err)
			}
		}},
		{"truncate-path", func(t *testing.T, d *SimDisk, _ *SimFileHandle) {
			if err := d.TruncatePath("f", 0); err != nil {
				t.Fatalf("TruncatePath: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// faultRate 1.0: every written sector is marked.
			d := NewSimDisk(NewSeed(0x2542_03), 1.0)
			h, err := d.OpenFile("f", os.O_CREATE|os.O_RDWR|os.O_APPEND)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			if _, err := h.Write(bytes.Repeat([]byte{'x'}, 3*sectorSize)); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if n := d.FaultedSectorCount("f"); n == 0 {
				t.Fatalf("no sector was faulted, so this test cannot observe the marks being " +
					"dropped")
			}

			tc.trunc(t, d, h)

			if n := d.FaultedSectorCount("f"); n != 0 {
				t.Errorf("%d fault marks survive a truncation to zero. A mark names a sector of "+
					"this file, and no sector exists past the end of it; keeping the mark would "+
					"corrupt bytes a later grow places there (F13, #2542)", n)
			}
		})
	}
}
