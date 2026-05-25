package wal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// TestWAL_CRCCorruptionAtEveryByte builds an 8-frame buffer and for each
// frame flips the first payload byte, then verifies:
//   - Replay returns ErrCRCMismatch.
//   - All frames preceding the corrupted frame were decoded successfully.
//
// It also tests magic-byte corruption (→ ErrBadMagic) and version-field
// corruption (→ ErrUnsupportedVersion) on frame 0.
func TestWAL_CRCCorruptionAtEveryByte(t *testing.T) {
	t.Parallel()

	const numFrames = 8
	payload := []byte("payload!") // 8 bytes — all frames same length

	// Build the reference buffer.
	var ref bytes.Buffer
	for i := 0; i < numFrames; i++ {
		p := make([]byte, len(payload))
		copy(p, payload)
		p[0] = byte(i) // make each frame's payload distinguishable
		if _, err := Encode(&ref, Frame{Payload: p}); err != nil {
			t.Fatalf("Encode frame %d: %v", i, err)
		}
	}
	full := ref.Bytes()

	frameSize := HeaderSize + len(payload) // every frame is the same size

	// frameOffset returns the byte index of the start of frame i.
	frameOffset := func(i int) int { return i * frameSize }
	// payloadOffset returns the byte index of the first payload byte of frame i.
	payloadOffset := func(i int) int { return frameOffset(i) + HeaderSize }

	t.Run("payload_corruption", func(t *testing.T) {
		t.Parallel()
		for frameIdx := 0; frameIdx < numFrames; frameIdx++ {
			corrupt := append([]byte(nil), full...)
			// Flip the first payload byte of this frame.
			corrupt[payloadOffset(frameIdx)] ^= 0xff

			buf := bytes.NewReader(corrupt)
			r := NewReader(buf, io.NopCloser(buf))

			seen := 0
			err := r.Replay(func(_ Frame) error {
				seen++
				return nil
			})

			if !errors.Is(err, ErrCRCMismatch) {
				t.Errorf("frame %d corrupted: Replay = %v, want ErrCRCMismatch", frameIdx, err)
			}
			if seen != frameIdx {
				t.Errorf("frame %d corrupted: decoded %d frames before error, want %d", frameIdx, seen, frameIdx)
			}
		}
	})

	t.Run("magic_corruption", func(t *testing.T) {
		t.Parallel()
		// Flip the first magic byte of frame 0 — reader must return ErrBadMagic.
		corrupt := append([]byte(nil), full...)
		corrupt[frameOffset(0)] ^= 0xff // flip byte 0 of the header

		buf := bytes.NewReader(corrupt)
		r := NewReader(buf, io.NopCloser(buf))

		seen := 0
		err := r.Replay(func(_ Frame) error {
			seen++
			return nil
		})

		if !errors.Is(err, ErrBadMagic) {
			t.Errorf("magic corruption: Replay = %v, want ErrBadMagic", err)
		}
		if seen != 0 {
			t.Errorf("magic corruption: decoded %d frames, want 0", seen)
		}
	})

	t.Run("version_corruption", func(t *testing.T) {
		t.Parallel()
		// Set the version field of frame 0 to 0xFFFF (far beyond CurrentVersion).
		corrupt := append([]byte(nil), full...)
		// Header layout: magic(4) | version(2 LE) | length(4 LE) | crc(4)
		binary.LittleEndian.PutUint16(corrupt[frameOffset(0)+4:], 0xFFFF)

		buf := bytes.NewReader(corrupt)
		r := NewReader(buf, io.NopCloser(buf))

		seen := 0
		err := r.Replay(func(_ Frame) error {
			seen++
			return nil
		})

		if !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("version corruption: Replay = %v, want ErrUnsupportedVersion", err)
		}
		if seen != 0 {
			t.Errorf("version corruption: decoded %d frames, want 0", seen)
		}
	})
}
