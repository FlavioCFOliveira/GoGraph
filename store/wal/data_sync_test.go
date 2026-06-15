package wal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/testfs"
)

// TestDataSync_RealFile_DurableRoundTrip exercises the production dataSync
// path against a real *os.File: on Linux this calls fdatasync(2), elsewhere
// it calls fsync. Either way the appended bytes — and, crucially for an
// append, the grown file size — must be durable and readable back. The test
// is platform-agnostic on purpose: it asserts the durability contract dataSync
// promises, which both fdatasync and fsync satisfy.
func TestDataSync_RealFile_DurableRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "data_sync_real")

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = f.Close() }()

	payload := []byte("the quick brown fox commits durably")
	if _, err := f.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// The unit under test: make the appended data and the new file size
	// durable via the platform-appropriate sync.
	if err := dataSync(f); err != nil {
		t.Fatalf("dataSync(*os.File): %v", err)
	}

	// Read the bytes back through an independent handle. This proves the
	// data AND the file-size growth reached the file system — the exact
	// pair fdatasync must make durable on an append.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("read back %q; want %q", got, payload)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Size() != int64(len(payload)) {
		t.Fatalf("durable size = %d; want %d", fi.Size(), len(payload))
	}
}

// TestDataSync_FallsBackToInterfaceSync confirms that for any walFile that is
// not a concrete *os.File, dataSync delegates to the value's own Sync method
// rather than reaching for a file descriptor. This is the seam that keeps the
// fault-injection (*testfs.FaultFile) and benchmark (discardFile) paths
// behaving exactly as before #1510 on every platform — including Linux, where
// the *os.File branch would otherwise call fdatasync and bypass the injected
// fault.
func TestDataSync_FallsBackToInterfaceSync(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "data_sync_fault")

	// Arm a sync fault that fires on the very first Sync.
	ff, err := testfs.New(path, testfs.Faults{ReturnEIOOnSync: true})
	if err != nil {
		t.Fatalf("testfs.New: %v", err)
	}
	defer func() { _ = ff.Close() }()

	// dataSync must route through FaultFile.Sync (not fdatasync on a raw fd),
	// so the injected fault surfaces unchanged.
	if err := dataSync(ff); !errors.Is(err, testfs.ErrSyncFailed) {
		t.Fatalf("dataSync(FaultFile with ReturnEIOOnSync) = %v; want ErrSyncFailed", err)
	}
}

// TestDataSync_FallbackSucceedsWithoutFault confirms the non-*os.File branch
// also returns nil cleanly when no fault is armed, i.e. the fallback is a
// faithful pass-through to the wrapped Sync.
func TestDataSync_FallbackSucceedsWithoutFault(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "data_sync_clean")

	ff, err := testfs.New(path, testfs.Faults{})
	if err != nil {
		t.Fatalf("testfs.New: %v", err)
	}
	defer func() { _ = ff.Close() }()

	if _, err := ff.Write([]byte("durable")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := dataSync(ff); err != nil {
		t.Fatalf("dataSync(clean FaultFile) = %v; want nil", err)
	}
}
