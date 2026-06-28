package recovery

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestRecovery_MidWALLengthOverrun_IsHardError is the regression for the
// round-3 audit finding (task #1778): a corrupt frame whose length field
// OVER-declares past the end of the file used to masquerade as a benign torn
// tail. Because io.ReadFull hit EOF before the CRC (which covers the length
// field) was ever computed, Decode returned wal.ErrTornFrame, recovery
// classified it as the normal crash-after-fsync tail (IsClean() == true), and
// the reader stopped at the first error — silently dropping every durable
// committed frame that physically follows the corrupt one.
//
// The fix (store/wal/format.go: embedsValidFrame) inspects the bytes the
// over-long read consumed: if a valid CRC-checking frame begins inside them,
// the durable frames that follow were swallowed, so Decode returns
// wal.ErrTornFrameMasksData, which the recovery corruption classifier treats
// as a hard error.
//
// Pre-fix this test FAILS (IsClean() == true, TailErr is benign ErrTornFrame,
// later durable frames silently lost). Post-fix it PASSES.
func TestRecovery_MidWALLengthOverrun_IsHardError(t *testing.T) {
	t.Parallel()
	const n = 8

	dir := t.TempDir()
	keys := writeNCommittedNodes(t, dir, n)
	walPath := filepath.Join(dir, "wal")

	boundaries := frameBoundaries(t, walPath)
	if len(boundaries) < 2*n+1 {
		t.Fatalf("expected at least %d boundaries, got %d", 2*n+1, len(boundaries))
	}
	// Pick a frame well inside the file so durable committed frames exist on
	// BOTH sides of the corruption. Corrupt only its length field.
	midFrame := len(boundaries) / 2 // 1-based frame index near the middle
	frameStart := boundaries[midFrame-1]

	data, err := os.ReadFile(walPath) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("read WAL: %v", err)
	}
	// Over-declare the length: set it larger than the bytes remaining in the
	// file (so io.ReadFull hits EOF) but below maxFrameSize (so the too-large
	// guard does not trip first). Adding the whole remaining file length is a
	// guaranteed over-declaration.
	lenOff := frameStart + 6
	binary.LittleEndian.PutUint32(data[lenOff:lenOff+4], uint32(len(data)))
	if err := os.WriteFile(walPath, data, 0o600); err != nil { //nolint:gosec // path under t.TempDir
		t.Fatalf("write corrupt WAL: %v", err)
	}

	res, openErr := openCanonical(t, dir)

	if res.IsClean() {
		t.Fatalf("IsClean() == true for a mid-stream length over-declaration; the masquerade was NOT fixed (durable frames silently dropped, TailErr=%v)", res.TailErr)
	}
	if !errors.Is(res.TailErr, wal.ErrTornFrameMasksData) {
		t.Fatalf("Result.TailErr = %v, want errors.Is(TailErr, wal.ErrTornFrameMasksData)", res.TailErr)
	}
	// errors.Is against the benign sentinel must be FALSE: the masking variant
	// is a distinct sentinel, never confused with a plain torn tail.
	if errors.Is(res.TailErr, wal.ErrTornFrame) {
		t.Fatalf("Result.TailErr unexpectedly matches benign wal.ErrTornFrame: %v", res.TailErr)
	}
	if openErr == nil {
		t.Fatalf("Open returned nil error for a mid-stream length over-declaration; genuine corruption must be surfaced")
	}
	if !errors.Is(openErr, wal.ErrTornFrameMasksData) {
		t.Fatalf("Open error = %v, want errors.Is(err, wal.ErrTornFrameMasksData)", openErr)
	}
	if res.Graph == nil {
		t.Fatalf("Result.Graph must be non-nil even on corruption (diagnostics)")
	}
	// The committed prefix before the corruption is still available for
	// diagnostics; the point is that recovery did NOT report clean.
	if _, ok := res.Graph.AdjList().Mapper().Lookup(keys[0]); !ok {
		t.Errorf("committed prefix node %q should be present in the diagnostic graph", keys[0])
	}
}

// TestRecovery_BenignTornTail_StillClean is the control: a genuinely benign
// torn tail (the writer crashed mid-write of the final frame's payload) must
// still recover the committed prefix cleanly — IsClean() == true, Open returns
// nil — and must NOT be misclassified as the masking-corruption variant. This
// guards against the fix over-failing legitimate crash tails.
func TestRecovery_BenignTornTail_StillClean(t *testing.T) {
	t.Parallel()
	const n = 8

	dir := t.TempDir()
	keys := writeNCommittedNodes(t, dir, n)
	walPath := filepath.Join(dir, "wal")

	boundaries := frameBoundaries(t, walPath)
	if len(boundaries) < 2 {
		t.Fatalf("need at least one frame, got %d boundaries", len(boundaries))
	}
	data, err := os.ReadFile(walPath) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("read WAL: %v", err)
	}
	// Truncate inside the LAST frame's payload: keep its full header and the
	// first payload byte, then cut. This is the canonical crash-after-fsync
	// torn tail — the committed prefix (every earlier frame) is intact.
	// boundaries' final entry is the file end; the last frame starts at the
	// previous boundary.
	lastStart := boundaries[len(boundaries)-2]
	cut := lastStart + int64(wal.HeaderSize) + 1
	if cut >= int64(len(data)) {
		t.Fatalf("last frame too small to cut its payload (cut=%d len=%d)", cut, len(data))
	}
	if err := os.WriteFile(walPath, data[:cut], 0o600); err != nil { //nolint:gosec // path under t.TempDir
		t.Fatalf("write torn WAL: %v", err)
	}

	res, openErr := openCanonical(t, dir)

	if openErr != nil {
		t.Fatalf("Open returned error %v for a benign torn tail; want nil", openErr)
	}
	if !res.IsClean() {
		t.Fatalf("IsClean() == false for a benign torn tail (TailErr=%v); over-failed a legitimate crash tail", res.TailErr)
	}
	if errors.Is(res.TailErr, wal.ErrTornFrameMasksData) {
		t.Fatalf("benign torn tail misclassified as masking corruption: %v", res.TailErr)
	}
	if res.TailErr != nil && !errors.Is(res.TailErr, wal.ErrTornFrame) {
		t.Fatalf("benign torn tail TailErr = %v, want nil or wal.ErrTornFrame", res.TailErr)
	}
	// The committed prefix must be fully recovered.
	if _, ok := res.Graph.AdjList().Mapper().Lookup(keys[0]); !ok {
		t.Errorf("committed prefix node %q missing after benign torn tail", keys[0])
	}
}
