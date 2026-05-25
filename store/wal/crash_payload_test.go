package wal

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

// TestWAL_CrashMidPayload simulates a crash that wrote a complete frame
// header (claiming a 100-byte payload) but only 10 bytes of the payload
// before the process died.
//
// Recovery must:
//   - Return exactly 3 complete frames (the pre-crash durable state).
//   - Report ErrTornFrame via TailError().
func TestWAL_CrashMidPayload(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "complete.wal")

	// Write 3 complete frames and sync.
	w, err := Open(src)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := w.Append([]byte("payload-crash-frame")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	good, err := os.ReadFile(src) //nolint:gosec // t.TempDir-rooted
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Construct a valid-looking header claiming plen=100, but only supply 10
	// payload bytes. Decode will attempt io.ReadFull(r, make([]byte,100)) and
	// receive io.ErrUnexpectedEOF, which maps to ErrTornFrame.
	//
	// The CRC is computed over header[0:10] + the full 100-byte payload (as a
	// legitimate encoder would). The reader never reaches CRC validation because
	// ReadFull fails first, but using the correct CRC keeps the test clean.
	const claimedLen = 100
	fullPayload := make([]byte, claimedLen)
	for i := range fullPayload {
		fullPayload[i] = byte(i)
	}

	var tornHeader [HeaderSize]byte
	copy(tornHeader[0:4], Magic[:])
	binary.LittleEndian.PutUint16(tornHeader[4:6], CurrentVersion)
	binary.LittleEndian.PutUint32(tornHeader[6:10], claimedLen)
	// CRC covers header[0:10] + full payload (mirrors Encode's algorithm).
	crc := crc32.Update(0, castagnoli, tornHeader[0:10])
	crc = crc32.Update(crc, castagnoli, fullPayload)
	binary.LittleEndian.PutUint32(tornHeader[10:14], crc)

	// Build augmented bytes without chained appends (avoids appendAssign lint).
	augmented := make([]byte, len(good)+HeaderSize+10)
	copy(augmented, good)
	copy(augmented[len(good):], tornHeader[:])
	copy(augmented[len(good)+HeaderSize:], fullPayload[:10])

	dst := filepath.Join(dir, "torn_payload.wal")
	if err := os.WriteFile(dst, augmented, 0o600); err != nil { //nolint:gosec // testdata
		t.Fatalf("WriteFile: %v", err)
	}

	// Open and replay.
	r, err := OpenReader(dst)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()

	count := 0
	if err := r.Replay(func(_ Frame) error {
		count++
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	if count != 3 {
		t.Fatalf("decoded %d frames, want 3", count)
	}

	tailErr := r.TailError()
	if !errors.Is(tailErr, ErrTornFrame) {
		t.Fatalf("TailError = %v, want ErrTornFrame", tailErr)
	}
}
