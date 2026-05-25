package wal

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// TestWAL_TornFrameAtEveryOffset builds a 5-frame buffer in memory (short
// payloads to keep total size under 200 bytes), then truncates it at every
// offset from 1 to len-1 and verifies:
//
//   - Only complete frames are returned (no partial frames decoded).
//   - TailError() is ErrTornFrame for non-boundary truncations.
//   - TailError() is nil for clean-boundary truncations.
//
// At least one truncation must produce TailError() == ErrTornFrame.
func TestWAL_TornFrameAtEveryOffset(t *testing.T) {
	t.Parallel()

	// Build a reference buffer with 5 frames.
	payloads := [][]byte{
		[]byte("frame-0"),
		[]byte("frame-1"),
		[]byte("frame-2"),
		[]byte("frame-3"),
		[]byte("frame-4"),
	}

	var ref bytes.Buffer
	for _, p := range payloads {
		if _, err := Encode(&ref, Frame{Payload: p}); err != nil {
			t.Fatalf("Encode: %v", err)
		}
	}
	full := ref.Bytes()
	frameSize := HeaderSize + len(payloads[0]) // all payloads are same length (7 bytes)

	// Precompute the byte offset at which each complete frame ends.
	// frameBoundaries[i] = byte offset just after frame i (0-indexed).
	frameBoundaries := make([]int, len(payloads))
	for i := range payloads {
		frameBoundaries[i] = (i + 1) * frameSize
	}

	sawTornFrame := false

	for cut := 1; cut < len(full); cut++ {
		buf := bytes.NewReader(full[:cut])
		r := NewReader(buf, io.NopCloser(buf))

		seen := 0
		if err := r.Replay(func(_ Frame) error {
			seen++
			return nil
		}); err != nil {
			t.Fatalf("cut=%d: Replay returned unexpected error: %v", cut, err)
		}

		// Determine expected complete frame count for this cut.
		want := 0
		for _, boundary := range frameBoundaries {
			if cut >= boundary {
				want++
			}
		}

		if seen != want {
			t.Fatalf("cut=%d: decoded %d frames, want %d", cut, seen, want)
		}

		// Determine whether cut lands exactly on a frame boundary.
		isBoundary := false
		for _, boundary := range frameBoundaries {
			if cut == boundary {
				isBoundary = true
				break
			}
		}

		tailErr := r.TailError()
		if isBoundary {
			// A cut at a frame boundary is a clean truncation: no partial
			// frame was started, so TailError must be nil.
			if tailErr != nil {
				t.Fatalf("cut=%d (boundary): TailError = %v, want nil", cut, tailErr)
			}
		} else {
			// A non-boundary cut starts a frame but does not finish it.
			if !errors.Is(tailErr, ErrTornFrame) {
				t.Fatalf("cut=%d: TailError = %v, want ErrTornFrame", cut, tailErr)
			}
			sawTornFrame = true
		}
	}

	if !sawTornFrame {
		t.Fatal("no truncation produced ErrTornFrame; test did not exercise the torn-frame path")
	}
}
