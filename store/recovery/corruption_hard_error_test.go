package recovery

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// writeNCommittedNodes writes n separate single-node transactions ("k0" ..
// "k<n-1>") to a fresh WAL under dir and returns the node keys in commit
// order. Each transaction is one OpAddNode v3 frame followed by an OpCommit
// marker frame, so the WAL holds 2*n frames with every node fully durable.
// It is the fixture for the corruption-vs-torn-tail battery below: a
// per-transaction layout lets a test corrupt a frame that has committed
// transactions on BOTH sides of it (a genuine mid-WAL corruption) rather
// than a single fat transaction whose torn batch is merely discarded.
func writeNCommittedNodes(t *testing.T, dir string, n int) []string {
	t.Helper()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := txn.NewStoreWithOptions[string, int64](g, w, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	keys := make([]string, n)
	for i := range keys {
		keys[i] = "k" + itoa(i)
		tx := s.Begin()
		if err := tx.AddNode(keys[i]); err != nil {
			t.Fatalf("AddNode(%s): %v", keys[i], err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit(%s): %v", keys[i], err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
	return keys
}

// openCanonical opens dir through recovery.Open with the canonical
// string+int64 codecs and returns the full Result and error so a test can
// inspect both the function error and the diagnostic Result.
func openCanonical(t *testing.T, dir string) (Result[string, int64], error) {
	t.Helper()
	return Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
}

// TestRecovery_MidWALCRCMismatch_IsHardError is the headline regression for
// task #1289: a CRC mismatch in a NON-tail (middle) frame is genuine
// corruption and must be surfaced as the function error, not silently
// swallowed into Result.TailErr while Open returns nil.
//
// Pre-fix this test FAILS: openCodec returned (res, nil) for every tail
// error, so the CRC mismatch reached the caller only via res.TailErr.
// Post-fix Open returns a non-nil error that errors.Is(err,
// wal.ErrCRCMismatch), Result.IsClean() reports false, and the committed
// prefix that pre-dates the bad frame is still placed in Result.Graph for
// diagnostics.
func TestRecovery_MidWALCRCMismatch_IsHardError(t *testing.T) {
	t.Parallel()
	const n = 8

	refDir := t.TempDir()
	_ = writeNCommittedNodes(t, refDir, n)
	walPath := filepath.Join(refDir, "wal")
	orig, err := os.ReadFile(walPath) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("read WAL: %v", err)
	}
	boundaries := frameBoundaries(t, walPath)
	// 2*n frames -> 2*n+1 boundaries (including offset 0). Pick a frame
	// safely inside the file so committed transactions exist on both sides.
	if len(boundaries) < 2*n+1 {
		t.Fatalf("expected at least %d boundaries, got %d", 2*n+1, len(boundaries))
	}
	midFrame := len(boundaries) / 2 // a frame index near the middle
	payloadStart := boundaries[midFrame-1] + int64(wal.HeaderSize)
	if payloadStart >= boundaries[midFrame] {
		t.Fatalf("frame %d has no payload byte to flip", midFrame)
	}

	dir := t.TempDir()
	corrupt := append([]byte(nil), orig...)
	corrupt[payloadStart] ^= 0xFF                                                   // flip a payload byte -> CRC32C mismatch
	if err := os.WriteFile(filepath.Join(dir, "wal"), corrupt, 0o600); err != nil { //nolint:gosec // path under t.TempDir
		t.Fatalf("write corrupt WAL: %v", err)
	}

	res, err := openCanonical(t, dir)
	if err == nil {
		t.Fatal("Open returned nil error for a mid-WAL CRC mismatch; genuine corruption must be surfaced")
	}
	if !errors.Is(err, wal.ErrCRCMismatch) {
		t.Fatalf("Open error = %v, want errors.Is(err, wal.ErrCRCMismatch)", err)
	}
	if res.IsClean() {
		t.Fatal("Result.IsClean() = true for a mid-WAL CRC mismatch, want false")
	}
	if !errors.Is(res.TailErr, wal.ErrCRCMismatch) {
		t.Fatalf("Result.TailErr = %v, want errors.Is(TailErr, wal.ErrCRCMismatch)", res.TailErr)
	}
	// The committed prefix that pre-dates the bad frame must still be
	// available for diagnostics, and nothing past the bad frame may apply.
	if res.Graph == nil {
		t.Fatal("Result.Graph must be non-nil even on corruption (diagnostics)")
	}
	if _, ok := res.Graph.AdjList().Mapper().Lookup("k0"); !ok {
		t.Error("committed prefix node k0 should be present in the diagnostic graph")
	}
	lastKey := "k" + itoa(n-1)
	if _, ok := res.Graph.AdjList().Mapper().Lookup(lastKey); ok {
		t.Errorf("node %q from a frame past the corruption must not be recovered", lastKey)
	}
}

// TestRecovery_CleanTornTail_IsBenign asserts the dual of the corruption
// case: a WAL truncated mid-frame (the normal crash-after-fsync state) is
// NOT corruption. Open returns a nil error, Result.IsClean() reports true,
// and the benign torn-tail sentinel is still surfaced via Result.TailErr
// for callers that wish to log it. The committed prefix is fully recovered
// and the torn last op is dropped.
func TestRecovery_CleanTornTail_IsBenign(t *testing.T) {
	t.Parallel()
	const n = 6

	refDir := t.TempDir()
	keys := writeNCommittedNodes(t, refDir, n)
	walPath := filepath.Join(refDir, "wal")
	orig, err := os.ReadFile(walPath) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("read WAL: %v", err)
	}
	boundaries := frameBoundaries(t, walPath)
	// Truncate one byte short of the final frame boundary so the last
	// frame is torn mid-payload.
	last := boundaries[len(boundaries)-1]
	tearOff := last - 1
	if tearOff <= boundaries[len(boundaries)-2] {
		t.Fatalf("cannot tear last frame: boundaries too close (%d <= %d)",
			tearOff, boundaries[len(boundaries)-2])
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wal"), orig[:tearOff], 0o600); err != nil { //nolint:gosec // path under t.TempDir
		t.Fatalf("write torn WAL: %v", err)
	}

	res, err := openCanonical(t, dir)
	if err != nil {
		t.Fatalf("Open returned %v for a clean torn tail; a torn tail must be benign", err)
	}
	if !res.IsClean() {
		t.Fatalf("Result.IsClean() = false for a clean torn tail, want true (TailErr=%v)", res.TailErr)
	}
	if res.TailErr != nil && !errors.Is(res.TailErr, wal.ErrTornFrame) {
		t.Fatalf("Result.TailErr = %v, want nil or wal.ErrTornFrame", res.TailErr)
	}
	// Every fully-committed transaction except the torn last one survives.
	for _, k := range keys[:n-1] {
		if _, ok := res.Graph.AdjList().Mapper().Lookup(k); !ok {
			t.Errorf("committed node %q should survive a torn tail", k)
		}
	}
}

// TestRecovery_UnsupportedRecordVersion_IsHardError covers the second
// genuine-corruption sentinel from the AC: a CRC-valid WAL frame whose
// transaction-record version byte is neither v2 (0xFE) nor v3 (0xFD) — a
// future or garbage version. Decode rejects it with
// ErrUnsupportedRecordVersion, which the new contract surfaces as the
// function error (pre-fix it was swallowed into Result.TailErr with Open
// returning nil). Result.IsClean() reports false.
func TestRecovery_UnsupportedRecordVersion_IsHardError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	// A well-formed WAL frame (correct CRC) whose transaction payload begins
	// with 0x7F — not OpRecordV1 (0x00), V2 (0xFE), or V3 (0xFD) — so the
	// frame survives the WAL reader but Decode rejects its content.
	payload := []byte{0x7F, byte(txn.OpAddNode), 0x00, 0x00}
	if err := w.Append(payload); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	res, err := openCanonical(t, dir)
	if err == nil {
		t.Fatal("Open returned nil error for an unsupported record version; it must be surfaced")
	}
	if !errors.Is(err, ErrUnsupportedRecordVersion) {
		t.Fatalf("Open error = %v, want errors.Is(err, ErrUnsupportedRecordVersion)", err)
	}
	if res.IsClean() {
		t.Fatal("Result.IsClean() = true for an unsupported record version, want false")
	}
	if !errors.Is(res.TailErr, ErrUnsupportedRecordVersion) {
		t.Fatalf("Result.TailErr = %v, want errors.Is(TailErr, ErrUnsupportedRecordVersion)", res.TailErr)
	}
}

// TestRecovery_EmptyAndCleanWAL_IsClean is a positive control: a WAL that
// reaches a clean EOF with no torn tail and no corruption yields a nil
// error, IsClean()==true, and a nil TailErr.
func TestRecovery_EmptyAndCleanWAL_IsClean(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keys := writeNCommittedNodes(t, dir, 4)

	res, err := openCanonical(t, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !res.IsClean() {
		t.Fatalf("Result.IsClean() = false on a clean WAL, want true (TailErr=%v)", res.TailErr)
	}
	if res.TailErr != nil {
		t.Fatalf("Result.TailErr = %v on a clean WAL, want nil", res.TailErr)
	}
	for _, k := range keys {
		if _, ok := res.Graph.AdjList().Mapper().Lookup(k); !ok {
			t.Errorf("committed node %q missing after a clean recovery", k)
		}
	}
}
