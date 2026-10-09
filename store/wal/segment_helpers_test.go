package wal

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// segmentPaths returns the segment files of the log at walPath, oldest first.
func segmentPaths(t testing.TB, walPath string) []string {
	t.Helper()
	names, err := readDirNames(SegmentDir(walPath))
	if err != nil {
		t.Fatalf("list segments of %s: %v", walPath, err)
	}
	var out []string
	for _, n := range names {
		if _, ok := parseSegmentName(n); ok {
			out = append(out, filepath.Join(SegmentDir(walPath), n))
		}
	}
	slices.Sort(out)
	return out
}

// tailSegment returns the newest segment of walPath holding a frame byte.
func tailSegment(t testing.TB, walPath string) string {
	t.Helper()
	segs := segmentPaths(t, walPath)
	for i := len(segs) - 1; i >= 0; i-- {
		fi, err := os.Stat(segs[i])
		if err != nil {
			t.Fatalf("stat %s: %v", segs[i], err)
		}
		if fi.Size() > segHeaderSize {
			return segs[i]
		}
	}
	if len(segs) == 0 {
		t.Fatalf("no segment under %s", SegmentDir(walPath))
	}
	return segs[0]
}

// segmentFrameBytes returns every segment's frame bytes, headers stripped,
// oldest first.
func segmentFrameBytes(t testing.TB, walPath string) []byte {
	t.Helper()
	var out []byte
	for _, p := range segmentPaths(t, walPath) {
		b, err := os.ReadFile(p) //nolint:gosec // test path under t.TempDir
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if len(b) > segHeaderSize {
			out = append(out, b[segHeaderSize:]...)
		}
	}
	return out
}
