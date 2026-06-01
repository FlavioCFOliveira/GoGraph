package wal_test

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/testfs"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestWALFault_PartialWrite_MultipleOffsets is a complementary test
// to [TestWriterFault_TornFrameAtByte128]. It exercises three distinct
// cut-off offsets and verifies the number of complete frames the
// reader recovers in each case.
//
// Frame geometry with 40-byte payloads:
//
//	HeaderSize (14) + 40 = 54 bytes per frame
//
// Cut-off cases:
//
//	offset=10  → frame 1 header torn (10 < 14); 0 complete frames
//	offset=54  → frame 1 complete; frame 2 starts at 54 and is cut
//	             immediately (0 bytes of frame 2 reach disk for
//	             FailWritesAfterBytes=54 semantics: the 55th byte is
//	             refused, so only bytes 0–53 are written); 1 complete
//	             frame, TailError nil (clean EOF at frame boundary)
//	             — note: whether TailError is nil or a torn-frame
//	             error depends on whether the writer attempted a
//	             partial frame 2 header. We assert ≥1 complete frame.
//	offset=81  → frame 1 complete (bytes 0–53); frame 2 partially
//	             written (bytes 54–80, i.e. 27 bytes); 1 complete
//	             frame, TailError non-nil.
func TestWALFault_PartialWrite_MultipleOffsets(t *testing.T) {
	t.Parallel()

	const payloadSize = 40
	// frame size = HeaderSize (14) + payloadSize (40) = 54
	const frameSize = 14 + payloadSize

	cases := []struct {
		name           string
		budget         int64
		wantFrames     int
		wantTailErrNil bool // true → TailError must be nil (clean EOF)
	}{
		{
			name:       "offset=10 (header torn)",
			budget:     10,
			wantFrames: 0,
			// No complete frame; reader hits torn frame immediately.
			wantTailErrNil: false,
		},
		{
			name:       "offset=54 (exact frame boundary)",
			budget:     frameSize, // 54
			wantFrames: 1,
			// Cut at exact boundary: first frame complete, second
			// frame has 0 bytes (clean EOF). TailError nil.
			wantTailErrNil: true,
		},
		{
			name:       "offset=81 (mid-second-frame)",
			budget:     81,
			wantFrames: 1,
			// Frame 1 complete; frame 2 cut after 27 bytes → torn.
			wantTailErrNil: false,
		},
	}

	payload1 := bytes.Repeat([]byte{0xAA}, payloadSize)
	payload2 := bytes.Repeat([]byte{0xBB}, payloadSize)

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			walPath := filepath.Join(dir, "partial.wal")

			ff, err := testfs.New(walPath, testfs.Faults{FailWritesAfterBytes: tc.budget})
			if err != nil {
				t.Fatalf("testfs.New: %v", err)
			}

			w, err := wal.OpenWith(ff)
			if err != nil {
				_ = ff.Close()
				t.Fatalf("wal.OpenWith: %v", err)
			}

			// Append two frames; errors are tolerated — the fault may
			// fire during Append or during Sync.
			_ = w.Append(payload1)
			_ = w.Sync()
			_ = w.Append(payload2)
			_ = w.Sync()
			_ = w.Close()

			r, err := wal.OpenReader(walPath)
			if err != nil {
				if tc.wantFrames == 0 {
					// An empty/tiny file may cause OpenReader to fail.
					t.Logf("OpenReader: %v (accepted for 0-frame case)", err)
					return
				}
				t.Fatalf("OpenReader: %v", err)
			}
			defer func() { _ = r.Close() }()

			var decoded []wal.Frame
			for f := range r.Frames() {
				decoded = append(decoded, f)
			}

			if len(decoded) != tc.wantFrames {
				t.Errorf("decoded %d frame(s), want %d", len(decoded), tc.wantFrames)
			}

			tailErr := r.TailError()
			if tc.wantTailErrNil {
				if tailErr != nil {
					t.Errorf("TailError() = %v, want nil (clean EOF at frame boundary)", tailErr)
				}
			} else {
				if tailErr == nil {
					t.Errorf("TailError() = nil; want a torn-frame error for budget=%d", tc.budget)
				} else if !errors.Is(tailErr, wal.ErrTornFrame) &&
					!errors.Is(tailErr, wal.ErrBadMagic) &&
					!errors.Is(tailErr, wal.ErrCRCMismatch) {
					t.Errorf("TailError() = %v; want ErrTornFrame/ErrBadMagic/ErrCRCMismatch", tailErr)
				}
			}
		})
	}
}
