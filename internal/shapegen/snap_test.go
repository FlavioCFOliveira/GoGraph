package shapegen

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSNAP_RegistryHasCanonicalDatasets pins the three SNAP entries
// the task requires (web-Google, soc-LiveJournal1, cit-HepPh) and
// asserts every entry carries a 64-char lowercase hex SHA-256, an
// HTTPS URL, and non-zero metadata.
func TestSNAP_RegistryHasCanonicalDatasets(t *testing.T) {
	t.Parallel()
	want := []string{"web-Google", "soc-LiveJournal1", "cit-HepPh"}
	for _, name := range want {
		ds, ok := SNAPDatasets[name]
		if !ok {
			t.Fatalf("SNAPDatasets missing %q", name)
		}
		if ds.Name != name {
			t.Errorf("%s: ds.Name = %q, want %q", name, ds.Name, name)
		}
		if !strings.HasPrefix(ds.URL, "https://") {
			t.Errorf("%s: URL %q is not HTTPS", name, ds.URL)
		}
		if len(ds.SHA256) != 64 {
			t.Errorf("%s: SHA256 has length %d, want 64", name, len(ds.SHA256))
		}
		if strings.ToLower(ds.SHA256) != ds.SHA256 {
			t.Errorf("%s: SHA256 %q is not lowercase", name, ds.SHA256)
		}
		if ds.Nodes == 0 || ds.Edges == 0 {
			t.Errorf("%s: zero metadata: nodes=%d edges=%d", name, ds.Nodes, ds.Edges)
		}
	}
}

// TestSNAP_LoadUnknown surfaces the typed error for an unregistered
// dataset name.
func TestSNAP_LoadUnknown(t *testing.T) {
	t.Parallel()
	_, err := LoadSNAP("never-registered", t.TempDir())
	if !errors.Is(err, ErrSNAPUnknownDataset) {
		t.Fatalf("LoadSNAP(unknown) err = %v, want ErrSNAPUnknownDataset", err)
	}
}

// TestSNAP_DefaultCacheDir verifies the fallback chain
// (GOGRAPH_SNAP_DIR > $HOME/.cache/gograph-snap > $TMPDIR fallback).
func TestSNAP_DefaultCacheDir(t *testing.T) {
	// Not Parallel: mutates the process environment.
	t.Setenv(snapCacheDirEnv, "/tmp/sentinel-snap-dir")
	if got := SNAPDefaultCacheDir(); got != "/tmp/sentinel-snap-dir" {
		t.Fatalf("env override ignored: %q", got)
	}
	t.Setenv(snapCacheDirEnv, "")
	got := SNAPDefaultCacheDir()
	if got == "" {
		t.Fatal("SNAPDefaultCacheDir returned empty path")
	}
	if !strings.Contains(got, "gograph-snap") {
		t.Fatalf("default cache dir %q missing gograph-snap segment", got)
	}
}

// TestSNAP_ParseHandCraftedArchive exercises the parser against an
// in-memory gzip archive that mimics the SNAP wire format. It pins:
//   - lines starting with # are skipped;
//   - the parser tolerates tab and space separators;
//   - empty lines are ignored;
//   - the resulting graph is directed;
//   - Order and Size match the distinct-id count and the edge count.
func TestSNAP_ParseHandCraftedArchive(t *testing.T) {
	t.Parallel()
	const body = "" +
		"# Directed graph (each unordered pair of nodes is saved once): hand.txt \n" +
		"# Comment with random text\n" +
		"# Nodes: 4 Edges: 5\n" +
		"# FromNodeId\tToNodeId\n" +
		"\n" +
		"1\t2\n" +
		"1\t3\n" +
		"2 3\n" +
		"3\t4\n" +
		"4\t1\n"
	path := writeGzipFixture(t, "hand.txt.gz", body)
	g, err := parseSNAPArchive(path)
	if err != nil {
		t.Fatalf("parseSNAPArchive: %v", err)
	}
	if got, want := g.AdjList().Order(), uint64(4); got != want {
		t.Errorf("Order = %d, want %d", got, want)
	}
	if got, want := g.AdjList().Size(), uint64(5); got != want {
		t.Errorf("Size = %d, want %d", got, want)
	}
	if !g.AdjList().Directed() {
		t.Error("parsed graph is not Directed()")
	}
}

// TestSNAP_ChecksumMismatchDetected pins the SHA-256 guard: a
// fixture whose hash differs from a deliberately wrong expectation
// must surface ErrSNAPChecksumMismatch.
func TestSNAP_ChecksumMismatchDetected(t *testing.T) {
	t.Parallel()
	path := writeGzipFixture(t, "checksum.txt.gz", "0\t1\n")
	// Compute the real digest, twist one nibble, ensure the verifier
	// rejects it.
	actualSum := sha256Hex(t, path)
	twisted := flipFirstHexNibble(actualSum)
	err := verifySNAPChecksum(path, twisted)
	if !errors.Is(err, ErrSNAPChecksumMismatch) {
		t.Fatalf("verifySNAPChecksum err = %v, want ErrSNAPChecksumMismatch", err)
	}
	// Sanity-check that the real digest passes.
	if err := verifySNAPChecksum(path, actualSum); err != nil {
		t.Fatalf("verifySNAPChecksum(real) = %v, want nil", err)
	}
}

// writeGzipFixture compresses body and writes it to a temporary
// gzip file. It returns the path.
func writeGzipFixture(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- t.TempDir() controlled path.
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	gw := gzip.NewWriter(f)
	if _, err := gw.Write([]byte(body)); err != nil {
		t.Fatalf("gzip.Write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip.Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("file.Close: %v", err)
	}
	return path
}

// sha256Hex returns the lowercase hex SHA-256 of the file at path.
func sha256Hex(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- t.TempDir() controlled path.
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// flipFirstHexNibble returns h with the first character XORed against
// a non-zero nibble so the result is guaranteed != h.
func flipFirstHexNibble(h string) string {
	if h == "" {
		return h
	}
	first := h[0]
	switch {
	case first >= '0' && first <= '9':
		first = '0' + (first-'0'+1)%10
	case first >= 'a' && first <= 'f':
		first = 'a' + (first-'a'+1)%6
	}
	return string(first) + h[1:]
}

// TestSNAP_SplitEdgeRejectsMalformedLines exercises the parser's
// error path on inputs that lack a valid separator or carry a
// non-integer endpoint.
func TestSNAP_SplitEdgeRejectsMalformedLines(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		line string
	}{
		{"missing separator", "123"},
		{"non integer from", "abc\t1"},
		{"non integer to", "1\txyz"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			body := c.line + "\n"
			var buf bytes.Buffer
			gw := gzip.NewWriter(&buf)
			_, _ = gw.Write([]byte(body))
			_ = gw.Close()
			path := writeGzipFixture(t, "bad.txt.gz", body)
			_, err := parseSNAPArchive(path)
			if err == nil {
				t.Fatalf("parseSNAPArchive accepted %q", c.line)
			}
		})
	}
}
