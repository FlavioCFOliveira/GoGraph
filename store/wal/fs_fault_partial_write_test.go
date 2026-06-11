package wal_test

import (
	"bytes"
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
// Under the sync-failure poison contract (task #1333) the Sync whose
// flush trips the write budget physically discards the un-synced
// suffix and permanently poisons the writer, so the recovered file
// always ends at the last durably-synced frame boundary with a clean
// tail. Torn bytes that survive on disk (the crash case, where no
// writer is alive to scrub them) are exercised by
// crash_header_test.go, crash_payload_test.go and
// torn_offsets_test.go.
//
// Frame geometry with 40-byte payloads:
//
//	HeaderSize (14) + 40 = 54 bytes per frame
//
// Cut-off cases:
//
//	offset=10  → frame 1's own flush is cut (10 < 54): Sync #1 fails
//	             and rolls the file back to 0 bytes; 0 complete frames
//	offset=54  → frame 1 synced; frame 2's flush is refused at byte 0
//	             (budget exactly exhausted): Sync #2 fails, file stays
//	             at the 54-byte boundary; 1 complete frame
//	offset=81  → frame 1 synced; frame 2's flush is cut after 27
//	             bytes: Sync #2 fails and rolls the file back to byte
//	             54; 1 complete frame
func TestWALFault_PartialWrite_MultipleOffsets(t *testing.T) {
	t.Parallel()

	const payloadSize = 40
	// frame size = HeaderSize (14) + payloadSize (40) = 54
	const frameSize = 14 + payloadSize

	cases := []struct {
		name       string
		budget     int64
		wantFrames int
	}{
		{
			name:       "offset=10 (cut inside frame 1)",
			budget:     10,
			wantFrames: 0,
		},
		{
			name:       "offset=54 (exact frame boundary)",
			budget:     frameSize, // 54
			wantFrames: 1,
		},
		{
			name:       "offset=81 (mid-second-frame)",
			budget:     81,
			wantFrames: 1,
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

			// Append two frames; intermediate errors are tolerated —
			// the budget fault fires during one of the two Sync
			// flushes (or, once poisoned, the second round is
			// rejected outright).
			_ = w.Append(payload1)
			_ = w.Sync()
			_ = w.Append(payload2)
			_ = w.Sync()
			// Whichever Sync tripped the budget poisoned the writer:
			// every further append must be rejected.
			if err := w.Append(payload2); err == nil {
				t.Error("Append after failed Sync = nil; want sticky error (writer must be poisoned)")
			}
			_ = w.Close() // unclean shutdown: surfaces the sticky error

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

			// The poison discarded every partially-flushed byte: the
			// recovered tail must always be clean.
			if tailErr := r.TailError(); tailErr != nil {
				t.Errorf("TailError() = %v, want nil (poison must discard the partial suffix, budget=%d)", tailErr, tc.budget)
			}
		})
	}
}
