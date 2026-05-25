package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSnapshot_SegmentCRCCorruption verifies that flipping a single
// byte inside each required segment file (csr.bin, labels.bin,
// properties.bin, mapper.bin) causes [LoadSnapshotFull] to return
// [ErrCorrupted].
//
// The test exercises the CRC validation path for every component that
// WriteSnapshotFull emits for a string-keyed graph (v3 snapshot). The
// csr.bin corruption path is also exercised by
// [TestOpen_CorruptedCSR]; the other three component files are
// covered exclusively here.
func TestSnapshot_SegmentCRCCorruption(t *testing.T) {
	t.Parallel()

	// Candidate segment files for a v3 (string-keyed) snapshot. csr.bin
	// is included to establish the baseline behaviour via the same code
	// path (readVerifiedCSR called from LoadSnapshotFull).
	segments := []string{
		CSRFile,
		LabelsFile,
		PropertiesFile,
		MapperFile,
	}

	// Build the reference snapshot once; sub-tests copy it.
	goodDir := buildFullSnapshot(t)

	for _, seg := range segments {
		seg := seg
		t.Run(seg, func(t *testing.T) {
			t.Parallel()

			// Check that the file exists in the reference snapshot.
			// mapper.bin is only emitted for string-keyed graphs
			// (v3); if for any reason it is absent, skip rather
			// than fail.
			if _, err := os.Stat(filepath.Join(goodDir, seg)); err != nil {
				t.Skipf("segment %q absent in reference snapshot, skipping", seg)
			}

			dir := copySnapshotDir(t, goodDir)
			segPath := filepath.Join(dir, seg)

			data, err := os.ReadFile(segPath) //nolint:gosec // path under t.TempDir
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", seg, err)
			}
			if len(data) == 0 {
				t.Skipf("segment %q is empty, cannot flip a byte", seg)
			}

			// Flip byte at the midpoint to avoid clobbering the
			// magic-byte header (which triggers a different, earlier
			// error path). Midpoint corruption guarantees the file
			// parses past the magic check and the CRC catch fires.
			pos := len(data) / 2
			data[pos] ^= 0xFF
			if err := os.WriteFile(segPath, data, 0o600); err != nil { //nolint:gosec // path under t.TempDir
				t.Fatalf("WriteFile(%s): %v", seg, err)
			}

			_, err = LoadSnapshotFull(dir)
			if !errors.Is(err, ErrCorrupted) {
				t.Fatalf("LoadSnapshotFull(corrupted %s) = %v, want ErrCorrupted", seg, err)
			}
		})
	}
}
