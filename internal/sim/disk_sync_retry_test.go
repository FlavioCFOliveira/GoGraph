package sim

// disk_sync_retry_test.go — a failed fsync is permanent, and a retry is a lie
// (rmp #2540, audit finding F6).
//
// Sync used to inject a one-shot fault and touch no data on either path, so a
// failed fsync left the bytes intact and a retry simply succeeded. Real systems
// do not behave that way: a failed write-back marks the affected pages CLEAN and
// reports the error to exactly one caller, so a retried fsync returns success
// over data that is already gone.
//
// There is no live defect this protects against. No current caller retries — the
// WAL poisons fail-stop, and RocksDB's equivalent is structural. What the model
// could not express was the consequence of INTRODUCING a retry, and that is what
// these tests make expressible.
//
// Sources, per the audit's insistence that no man page actually warns against
// retrying fsync: Rebello et al., "Can Applications Recover from fsync
// Failures?" (USENIX ATC 2020), and PostgreSQL's response to fsyncgate, which
// was to PANIC rather than retry.

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

// syncRetryFixture writes payload, then fsyncs with the given attempt count,
// arming the FIRST fsync on the disk to fail. It returns the errors each attempt
// produced.
func syncRetryFixture(t *testing.T, d *SimDisk, payload []byte, attempts int) []error {
	t.Helper()
	h, err := d.OpenFile("/data", os.O_CREATE|os.O_RDWR|os.O_APPEND)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = h.Close() }()

	d.ArmSyncFaultAt(1)
	if _, err := h.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := make([]error, 0, attempts)
	for i := 0; i < attempts; i++ {
		out = append(out, h.Sync())
	}
	return out
}

// TestSyncRetryAfterFailureLosesData is the acceptance criterion: the retry
// SUCCEEDS and the data is gone anyway.
func TestSyncRetryAfterFailureLosesData(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	payload := []byte("committed-and-acknowledged")

	errs := syncRetryFixture(t, d, payload, 2)

	if !errors.Is(errs[0], ErrSimFault) {
		t.Fatalf("the first fsync returned %v, want ErrSimFault; the fault arm did not fire and "+
			"this test is not driving the branch it exists for", errs[0])
	}
	if errs[1] != nil {
		t.Fatalf("the retried fsync returned %v, want SUCCESS. The whole hazard is that a retry "+
			"looks like it worked", errs[1])
	}

	// The retry reported success, so a caller that trusted it would have
	// acknowledged the write. The crash shows what that acknowledgement was worth.
	d.CrashHost()

	got, err := d.ReadFile("/data")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadFile: %v", err)
	}
	if bytes.Contains(got, payload) {
		t.Errorf("the data survived a crash after a FAILED fsync whose retry returned success. " +
			"A failed write-back marks the pages clean and drops them, so the retry granted " +
			"nothing; if this passes, a retry-before-poison optimisation would look safe under " +
			"simulation and lose data in production (#2540)")
	}
}

// TestSyncSucceedsAndKeepsDataWithoutAFault is the CONTROL. Without it, the test
// above is satisfied by a disk on which no fsync ever grants durability.
func TestSyncSucceedsAndKeepsDataWithoutAFault(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	payload := []byte("committed-and-acknowledged")

	h, err := d.OpenFile("/data", os.O_CREATE|os.O_RDWR|os.O_APPEND)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := h.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := h.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	_ = h.Close()
	d.CrashHost()

	got, err := d.ReadFile("/data")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("an UNFAULTED fsync did not make the data durable: got %q, want %q. Without "+
			"this control, TestSyncRetryAfterFailureLosesData would pass on a disk where fsync "+
			"never grants anything", got, payload)
	}
}

// TestSyncRetryPoisonIsPermanent pins that the freeze outlives the retry: bytes
// written AFTER the failure cannot become durable either, because a durable
// prefix is contiguous and the bytes before them are gone.
func TestSyncRetryPoisonIsPermanent(t *testing.T) {
	d := NewSimDisk(NewSeed(2), 0)

	h, err := d.OpenFile("/data", os.O_CREATE|os.O_RDWR|os.O_APPEND)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	d.ArmSyncFaultAt(1)
	if _, err := h.Write([]byte("lost")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := h.Sync(); !errors.Is(err, ErrSimFault) {
		t.Fatalf("first Sync = %v, want ErrSimFault", err)
	}

	// Keep going as an unaware caller would: more writes, more fsyncs, all
	// reporting success.
	for i := 0; i < 3; i++ {
		if _, err := h.Write([]byte("later")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := h.Sync(); err != nil {
			t.Fatalf("later Sync %d = %v, want success", i, err)
		}
	}
	_ = h.Close()
	d.CrashHost()

	got, _ := d.ReadFile("/data")
	if len(got) != 0 {
		t.Errorf("the file holds %q after the crash, want nothing. Every fsync after the failed "+
			"one returned success and must have granted NOTHING: the durable image is a "+
			"contiguous prefix, and the bytes at its head were dropped", got)
	}
}

// TestRetryBeforePoisonOptimisationIsCaught is acceptance criterion 2. It writes
// the optimisation the audit warned about — retry the fsync once, and treat
// success as success — and requires the simulator to catch it.
//
// This is the whole point of the finding. Before #2540 the retry genuinely
// worked in simulation, so this writer would have passed every durability
// assertion in the suite while losing data on a real kernel.
func TestRetryBeforePoisonOptimisationIsCaught(t *testing.T) {
	// syncWithOneRetry is the tempting optimisation: an fsync failure is often
	// transient, so try once more before giving up and poisoning the writer.
	syncWithOneRetry := func(h *SimFileHandle) error {
		if err := h.Sync(); err != nil {
			return h.Sync() // "it was probably transient"
		}
		return nil
	}

	d := NewSimDisk(NewSeed(3), 0)
	h, err := d.OpenFile("/wal", os.O_CREATE|os.O_RDWR|os.O_APPEND)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	payload := []byte("txn-1")
	d.ArmSyncFaultAt(1)
	if _, err := h.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// The optimisation reports success, so this commit is ACKNOWLEDGED.
	if err := syncWithOneRetry(h); err != nil {
		t.Fatalf("the retry-before-poison writer reported %v; for this test to mean anything it "+
			"must believe the commit succeeded", err)
	}
	_ = h.Close()
	d.CrashHost()

	got, _ := d.ReadFile("/wal")
	if bytes.Contains(got, payload) {
		t.Fatalf("the acknowledged commit survived, so the simulator did NOT catch a " +
			"retry-before-poison writer. That writer is a data-loss bug on a real kernel and the " +
			"model exists to make it fail here first (#2540)")
	}
	t.Logf("the retry-before-poison writer acknowledged a commit that the crash lost — caught, " +
		"which is what this model is for")
}
