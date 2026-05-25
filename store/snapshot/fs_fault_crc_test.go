package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSnapshot_FaultCRCCorruption verifies that a single-byte flip at
// a specific early offset (byte 16, inside the CRC-protected body) in
// each required segment file causes [LoadSnapshotFull] to return
// [ErrCorrupted].
//
// This test is complementary to [TestSnapshot_SegmentCRCCorruption]
// (which flips the midpoint byte of each file). It pins the behaviour
// for a concrete, small offset that is past the magic/version header
// but well within the CRC-guarded body, ensuring the CRC path fires
// rather than the magic-check path.
func TestSnapshot_FaultCRCCorruption(t *testing.T) {
	t.Parallel()

	const corruptOffset = 16 // past 4-byte magic + version fields; inside CRC body

	segments := []string{
		CSRFile,
		LabelsFile,
		PropertiesFile,
		MapperFile,
	}

	goodDir := buildFullSnapshot(t)

	for _, seg := range segments {
		seg := seg
		t.Run(seg, func(t *testing.T) {
			t.Parallel()

			// Skip if the segment was not emitted (only MapperFile is
			// conditional on a string-keyed graph; buildFullSnapshot
			// always produces one, but guard defensively).
			if _, err := os.Stat(filepath.Join(goodDir, seg)); err != nil {
				t.Skipf("segment %q absent in reference snapshot, skipping", seg)
			}

			dir := copySnapshotDir(t, goodDir)
			segPath := filepath.Join(dir, seg)

			data, err := os.ReadFile(segPath) //nolint:gosec // path under t.TempDir
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", seg, err)
			}
			if len(data) <= corruptOffset {
				t.Skipf("segment %q too short (%d bytes) to corrupt at offset %d", seg, len(data), corruptOffset)
			}

			// Flip a single byte at the target offset.
			data[corruptOffset] ^= 0xFF
			if err := os.WriteFile(segPath, data, 0o600); err != nil { //nolint:gosec // path under t.TempDir
				t.Fatalf("WriteFile(%s): %v", seg, err)
			}

			_, err = LoadSnapshotFull(dir)
			if !errors.Is(err, ErrCorrupted) {
				t.Fatalf("LoadSnapshotFull(corrupted %s at offset %d) = %v, want ErrCorrupted",
					seg, corruptOffset, err)
			}
		})
	}
}
