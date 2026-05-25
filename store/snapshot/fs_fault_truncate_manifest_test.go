package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSnapshot_TruncatedManifest verifies that [LoadSnapshotFull]
// surfaces a parse error (and not a panic) when the manifest.json
// is truncated to various lengths.
//
// The related test [TestOpen_CorruptedManifestJSON] covers the case
// where the manifest contains invalid JSON (`{not json`). This test
// is complementary: it exercises physical truncation at three
// different points and calls [LoadSnapshotFull] (rather than [Open]),
// ensuring both entry-points handle a damaged manifest gracefully.
func TestSnapshot_TruncatedManifest(t *testing.T) {
	t.Parallel()

	goodDir := buildFullSnapshot(t)

	cases := []struct {
		name      string
		keepBytes int64 // truncate manifest to this many bytes
	}{
		{
			// Completely empty: no bytes at all.
			name:      "empty",
			keepBytes: 0,
		},
		{
			// Partial: only the opening brace and the first character
			// of the first key (`{"`). Not a valid JSON object.
			name:      "after-opening-brace",
			keepBytes: 2,
		},
		{
			// 10 bytes: enough for `{"version` but not a closing `}`.
			name:      "first-10-bytes",
			keepBytes: 10,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := copySnapshotDir(t, goodDir)
			manifestPath := filepath.Join(dir, "manifest.json")

			if err := os.Truncate(manifestPath, tc.keepBytes); err != nil {
				t.Fatalf("Truncate manifest to %d bytes: %v", tc.keepBytes, err)
			}

			_, err := LoadSnapshotFull(dir)
			if err == nil {
				t.Fatalf("LoadSnapshotFull(manifest truncated to %d bytes) = nil; want error", tc.keepBytes)
			}

			// The error must wrap either ErrManifestCorrupted or
			// ErrCorrupted; either is acceptable because different
			// truncation lengths trip different parse branches.
			if !errors.Is(err, ErrManifestCorrupted) && !errors.Is(err, ErrCorrupted) {
				t.Errorf("LoadSnapshotFull(truncated manifest) = %v; want ErrManifestCorrupted or ErrCorrupted", err)
			}
		})
	}
}
