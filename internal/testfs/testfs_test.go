package testfs_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/testfs"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// tempFaultFile creates a FaultFile in t.TempDir() with the given faults.
func tempFaultFile(t *testing.T, faults testfs.Faults) *testfs.FaultFile {
	t.Helper()
	path := filepath.Join(t.TempDir(), "testfs_test.bin")
	ff, err := testfs.New(path, faults)
	if err != nil {
		t.Fatalf("testfs.New: %v", err)
	}
	t.Cleanup(func() { _ = ff.Close() })
	return ff
}

// TestPassThrough verifies that a zero-fault FaultFile is a
// transparent wrapper: write–seek–read round-trips correctly.
func TestPassThrough(t *testing.T) {
	ff := tempFaultFile(t, testfs.Faults{})
	data := []byte("hello testfs")

	n, err := ff.Write(data)
	if err != nil || n != len(data) {
		t.Fatalf("Write: n=%d err=%v", n, err)
	}
	if _, err := ff.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got := make([]byte, len(data))
	if _, err := io.ReadFull(ff, got); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("got %q, want %q", got, data)
	}
}

// TestCorruptOnRead_InvertsHeadByteFully is the #1771 gate: CorruptOnRead
// inverts ALL bits of the first byte of the read buffer (xor-delta 0xFF, not an
// 0x80 MSB-only flip) and leaves the rest of the buffer untouched (head-only).
func TestCorruptOnRead_InvertsHeadByteFully(t *testing.T) {
	ff := tempFaultFile(t, testfs.Faults{
		CorruptOnRead: func(_, _ int64) bool { return true },
	})
	orig := []byte{0x04, 0xBB, 0xCC, 0xDD}
	if _, err := ff.Write(orig); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := ff.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got := make([]byte, len(orig))
	if _, err := io.ReadFull(ff, got); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if delta := orig[0] ^ got[0]; delta != 0xFF {
		t.Errorf("head byte xor-delta = %#x, want 0xFF (full-byte invert, not an MSB-only 0x80 flip)", delta)
	}
	if !bytes.Equal(got[1:], orig[1:]) {
		t.Errorf("non-head bytes changed: got %x, want %x (corruption must be head-only)", got[1:], orig[1:])
	}
}

// TestFailWritesAfterBytes verifies that writes succeed up to the
// budget and then return ErrPartialWrite.
func TestFailWritesAfterBytes(t *testing.T) {
	const budget = 20
	ff := tempFaultFile(t, testfs.Faults{FailWritesAfterBytes: budget})

	// First write: fits entirely within budget.
	n, err := ff.Write([]byte("0123456789")) // 10 bytes
	if err != nil || n != 10 {
		t.Fatalf("first write: n=%d err=%v", n, err)
	}

	// Second write: also fits.
	n, err = ff.Write([]byte("0123456789")) // 10 bytes — hits budget exactly
	if err != nil || n != 10 {
		t.Fatalf("second write: n=%d err=%v", n, err)
	}

	// Third write: budget exhausted — any write must fail.
	n, err = ff.Write([]byte("X"))
	if n != 0 || !errors.Is(err, testfs.ErrPartialWrite) {
		t.Fatalf("third write: n=%d err=%v; want 0, ErrPartialWrite", n, err)
	}

	if !ff.BudgetExhausted() {
		t.Error("BudgetExhausted() = false, want true after budget reached")
	}
}

// TestFailWritesAfterBytes_PartialMidWrite verifies that when a
// write straddles the budget boundary, only the allowed prefix is
// written and ErrPartialWrite is returned.
func TestFailWritesAfterBytes_PartialMidWrite(t *testing.T) {
	const budget = 5
	ff := tempFaultFile(t, testfs.Faults{FailWritesAfterBytes: budget})

	// Write 8 bytes — only first 5 should land.
	n, err := ff.Write([]byte("ABCDEFGH"))
	if n != 5 || !errors.Is(err, testfs.ErrPartialWrite) {
		t.Fatalf("Write: n=%d err=%v; want 5, ErrPartialWrite", n, err)
	}

	// Verify only 5 bytes are on disk.
	if _, err := ff.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	buf := make([]byte, 16)
	rn, err := ff.Read(buf)
	if !errors.Is(err, io.EOF) && err != nil {
		t.Fatalf("Read: %v", err)
	}
	if rn != 5 || string(buf[:rn]) != "ABCDE" {
		t.Errorf("disk content = %q (n=%d), want %q (n=5)", buf[:rn], rn, "ABCDE")
	}
}

// TestReturnENOSPC verifies that every Write call returns ENOSPC
// when the flag is set.
func TestReturnENOSPC(t *testing.T) {
	ff := tempFaultFile(t, testfs.Faults{ReturnENOSPC: true})

	n, err := ff.Write([]byte("should fail"))
	if n != 0 || !testfs.IsENOSPC(err) {
		t.Fatalf("Write: n=%d err=%v; want 0, ENOSPC", n, err)
	}
}

// TestFsyncDelay verifies that Sync sleeps for at least the
// configured delay (with a generous 3× margin for scheduler jitter).
func TestFsyncDelay(t *testing.T) {
	delay := 5 * time.Millisecond
	ff := tempFaultFile(t, testfs.Faults{FsyncDelay: delay})

	if _, err := ff.Write([]byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	start := time.Now()
	if err := ff.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	elapsed := time.Since(start)

	// Must have waited at least the delay; allow 3× for CI jitter.
	if elapsed < delay {
		t.Errorf("Sync returned in %v, expected >= %v", elapsed, delay)
	}
}

// TestCorruptOnRead verifies that setting CorruptOnRead to always
// return true flips the first byte of every Read result.
func TestCorruptOnRead(t *testing.T) {
	ff := tempFaultFile(t, testfs.Faults{
		CorruptOnRead: func(_, _ int64) bool { return true },
	})

	if _, err := ff.Write([]byte{0x00, 0x01, 0x02}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := ff.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}

	buf := make([]byte, 3)
	if _, err := io.ReadFull(ff, buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	// First byte should be flipped (0x00 ^ 0xFF = 0xFF).
	if buf[0] != 0xFF {
		t.Errorf("buf[0] = 0x%02x, want 0xFF (flipped)", buf[0])
	}
	// Remaining bytes must be untouched.
	if buf[1] != 0x01 || buf[2] != 0x02 {
		t.Errorf("buf[1:] = % 02x, want [01 02]", buf[1:])
	}
}

// TestCorruptOnRead_Selective verifies that CorruptOnRead only
// corrupts at the caller-specified offset range.
func TestCorruptOnRead_Selective(t *testing.T) {
	// Corrupt only the second read (offset >= 3).
	ff := tempFaultFile(t, testfs.Faults{
		CorruptOnRead: func(offset, _ int64) bool { return offset >= 3 },
	})

	payload := []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE}
	if _, err := ff.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := ff.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}

	// First read (offset 0): must be clean.
	first := make([]byte, 3)
	if _, err := io.ReadFull(ff, first); err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if !bytes.Equal(first, payload[:3]) {
		t.Errorf("first read = % 02x, want % 02x", first, payload[:3])
	}

	// Second read (offset 3): first byte should be corrupted.
	second := make([]byte, 2)
	if _, err := io.ReadFull(ff, second); err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if second[0] != (0xDD ^ 0xFF) {
		t.Errorf("second[0] = 0x%02x, want 0x%02x (flipped 0xDD)", second[0], 0xDD^0xFF)
	}
}

// TestTruncate verifies that Truncate and Seek round-trip correctly
// through the FaultFile wrapper.
func TestTruncate(t *testing.T) {
	ff := tempFaultFile(t, testfs.Faults{})

	if _, err := ff.Write([]byte("hello world")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := ff.Truncate(5); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if _, err := ff.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	buf, err := io.ReadAll(ff)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(buf) != "hello" {
		t.Errorf("after truncate: %q, want %q", buf, "hello")
	}
}

// TestConcurrentWrites verifies that concurrent Write calls do not
// race and that the total bytes written is consistent.
func TestConcurrentWrites(t *testing.T) {
	const workers = 8
	const writesPerWorker = 100
	const chunkSize = 16

	ff := tempFaultFile(t, testfs.Faults{})

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			chunk := make([]byte, chunkSize)
			for j := 0; j < writesPerWorker; j++ {
				if _, err := ff.Write(chunk); err != nil {
					// Not using t.Fatal in goroutines; record failure.
					t.Errorf("Write: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	expected := int64(workers * writesPerWorker * chunkSize)
	if got := ff.Written(); got != expected {
		t.Errorf("Written() = %d, want %d", got, expected)
	}
}

// TestWrap verifies that Wrap takes ownership of an existing *os.File.
func TestWrap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wrap_test.bin")
	raw, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	ff := testfs.Wrap(raw, testfs.Faults{})
	t.Cleanup(func() { _ = ff.Close() })

	if _, err := ff.Write([]byte("wrapped")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if ff.Unwrap() != raw {
		t.Error("Unwrap() returned wrong file pointer")
	}
}

// TestIsENOSPC verifies the ENOSPC helper.
func TestIsENOSPC(t *testing.T) {
	ff := tempFaultFile(t, testfs.Faults{ReturnENOSPC: true})
	_, err := ff.Write([]byte("x"))
	if !testfs.IsENOSPC(err) {
		t.Errorf("IsENOSPC(%v) = false, want true", err)
	}
	if testfs.IsENOSPC(nil) {
		t.Error("IsENOSPC(nil) = true, want false")
	}
}

// TestSync_NoDelay verifies that Sync with zero delay passes through
// cleanly even when the underlying write cache is empty.
func TestSync_NoDelay(t *testing.T) {
	ff := tempFaultFile(t, testfs.Faults{})
	if err := ff.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}

// TestFailSyncAfter verifies that the first N Sync calls succeed,
// every subsequent call returns ErrSyncFailed (wrapping syscall.EIO),
// and the bytes written since the last successful Sync are discarded.
func TestFailSyncAfter(t *testing.T) {
	ff := tempFaultFile(t, testfs.Faults{FailSyncAfter: 1})

	if _, err := ff.Write([]byte("tx1")); err != nil {
		t.Fatalf("Write tx1: %v", err)
	}
	if err := ff.Sync(); err != nil {
		t.Fatalf("first Sync: %v", err)
	}

	if _, err := ff.Write([]byte("tx2")); err != nil {
		t.Fatalf("Write tx2: %v", err)
	}
	err := ff.Sync()
	if !errors.Is(err, testfs.ErrSyncFailed) {
		t.Fatalf("second Sync: err=%v; want ErrSyncFailed", err)
	}
	if !errors.Is(err, syscall.EIO) {
		t.Errorf("second Sync: err=%v does not wrap syscall.EIO", err)
	}

	// The un-synced suffix must be gone: only tx1 survives.
	if _, err := ff.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, err := io.ReadAll(ff)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "tx1" {
		t.Errorf("file content = %q, want %q (unsynced suffix not discarded)", got, "tx1")
	}

	// Every later Sync keeps failing.
	if err := ff.Sync(); !errors.Is(err, testfs.ErrSyncFailed) {
		t.Errorf("third Sync: err=%v; want ErrSyncFailed", err)
	}
}

// TestFailSyncAfter_Concurrent verifies the fault trips atomically:
// with FailSyncAfter=N, exactly N of M concurrent Sync calls succeed
// and the rest observe ErrSyncFailed.
func TestFailSyncAfter_Concurrent(t *testing.T) {
	const limit = 3
	const callers = 16

	ff := tempFaultFile(t, testfs.Faults{FailSyncAfter: limit})

	var succeeded atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch err := ff.Sync(); {
			case err == nil:
				succeeded.Add(1)
			case !errors.Is(err, testfs.ErrSyncFailed):
				t.Errorf("Sync: unexpected error %v", err)
			}
		}()
	}
	wg.Wait()

	if got := succeeded.Load(); got != limit {
		t.Errorf("successful Syncs = %d, want %d", got, limit)
	}
}

// TestReturnEIOOnSync verifies that every Sync fails immediately and
// that nothing written since open survives the fault.
func TestReturnEIOOnSync(t *testing.T) {
	ff := tempFaultFile(t, testfs.Faults{ReturnEIOOnSync: true})

	if _, err := ff.Write([]byte("never durable")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := ff.Sync(); !errors.Is(err, testfs.ErrSyncFailed) {
		t.Fatalf("Sync: err=%v; want ErrSyncFailed", err)
	}

	if _, err := ff.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, err := io.ReadAll(ff)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("file content = %q, want empty (no Sync ever succeeded)", got)
	}
}

// TestReturnEIOOnSync_PreservesPreexistingContent verifies that the
// discard rolls back to the size at open — not to zero — when the
// file already holds previously-durable data.
func TestReturnEIOOnSync_PreservesPreexistingContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preexisting.bin")
	if err := os.WriteFile(path, []byte("durable"), 0o600); err != nil {
		t.Fatal(err)
	}
	ff, err := testfs.New(path, testfs.Faults{ReturnEIOOnSync: true})
	if err != nil {
		t.Fatalf("testfs.New: %v", err)
	}
	t.Cleanup(func() { _ = ff.Close() })

	if _, err := ff.Seek(0, io.SeekEnd); err != nil {
		t.Fatalf("Seek end: %v", err)
	}
	if _, err := ff.Write([]byte("+lost")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := ff.Sync(); !errors.Is(err, testfs.ErrSyncFailed) {
		t.Fatalf("Sync: err=%v; want ErrSyncFailed", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "durable" {
		t.Errorf("file content = %q, want %q (pre-open prefix must survive)", got, "durable")
	}
}

// TestResetWritten verifies the ResetWritten helper.
func TestResetWritten(t *testing.T) {
	const budget = 8
	ff := tempFaultFile(t, testfs.Faults{FailWritesAfterBytes: budget})
	if _, err := ff.Write(make([]byte, budget)); err != nil {
		t.Fatalf("initial Write: %v", err)
	}
	if !ff.BudgetExhausted() {
		t.Fatal("BudgetExhausted() = false before reset")
	}
	ff.ResetWritten()
	if ff.BudgetExhausted() {
		t.Fatal("BudgetExhausted() = true after ResetWritten")
	}
	// Should be able to write again.
	if _, err := ff.Write([]byte("ok")); err != nil {
		t.Fatalf("Write after reset: %v", err)
	}
}
