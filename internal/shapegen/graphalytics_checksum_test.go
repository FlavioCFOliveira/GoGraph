package shapegen

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// graphalytics_checksum_test.go is the SHORT-layer, network-free
// counterpart to graphalytics_soak_test.go. It exercises the
// checksum-verification policy of [verifyGraphalyticsChecksum]
// against a small synthetic "archive" written to a temp file,
// proving the SHA-256-preferred / MD5-fallback contract without
// touching the SURF mirror.
//
// The real dataset digests in [GraphalyticsDatasets] cannot be
// asserted here (they require multi-GB downloads on the soak layer),
// so this test computes the real MD5 and SHA-256 of its own synthetic
// payload at runtime via [digestPair]. The verify policy is the unit
// under test; the synthetic digests are ground truth derived from the
// payload itself, never fabricated constants.

// writeSyntheticArchive writes content to a temp file standing in for
// a Graphalytics .tar.zst archive and returns its path together with
// the real SHA-256 and MD5 of the bytes on disk.
func writeSyntheticArchive(t *testing.T, content []byte) (path, sha, md5 string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "synthetic.tar.zst")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write synthetic archive: %v", err)
	}
	sha, md5, err := digestPair(path)
	if err != nil {
		t.Fatalf("digestPair(%q): %v", path, err)
	}
	return path, sha, md5
}

// TestVerifyGraphalyticsChecksum_SHA256Preferred proves that when the
// registry entry carries a non-empty SHA-256, that digest is the one
// enforced — even if the registered MD5 is deliberately wrong. This
// pins AC#2: SHA-256 is preferred over MD5 whenever it is present.
func TestVerifyGraphalyticsChecksum_SHA256Preferred(t *testing.T) {
	path, sha, _ := writeSyntheticArchive(t, []byte("graphalytics synthetic payload — sha256 preferred"))

	ds := GraphalyticsDataset{
		Name:   "synthetic-sha-preferred",
		SHA256: sha,
		// A wrong MD5 must be ignored when SHA-256 is present: if the
		// verifier consulted MD5 here it would reject, so passing
		// proves SHA-256 is the digest actually being checked.
		MD5: "00000000000000000000000000000000",
	}
	if err := verifyGraphalyticsChecksum(path, ds); err != nil {
		t.Fatalf("verifyGraphalyticsChecksum with matching SHA-256 (wrong MD5): got %v, want nil", err)
	}
}

// TestVerifyGraphalyticsChecksum_MD5Fallback proves that when the
// registry entry has an empty SHA-256 (the current state of all three
// pinned datasets), verification falls back to MD5 and accepts a
// matching MD5. This pins AC#3: existing MD5-only caches stay valid.
func TestVerifyGraphalyticsChecksum_MD5Fallback(t *testing.T) {
	path, _, md5 := writeSyntheticArchive(t, []byte("graphalytics synthetic payload — md5 fallback"))

	ds := GraphalyticsDataset{
		Name:   "synthetic-md5-fallback",
		SHA256: "", // empty — forces the MD5 fallback path.
		MD5:    md5,
	}
	if err := verifyGraphalyticsChecksum(path, ds); err != nil {
		t.Fatalf("verifyGraphalyticsChecksum with empty SHA-256 and matching MD5: got %v, want nil", err)
	}
}

// TestVerifyGraphalyticsChecksum_SHA256Mismatch proves that a SHA-256
// that does not match the file is rejected with the typed checksum
// error. The MD5 is set correctly to confirm the SHA-256 branch
// rejects without ever consulting (and being rescued by) MD5.
func TestVerifyGraphalyticsChecksum_SHA256Mismatch(t *testing.T) {
	path, _, md5 := writeSyntheticArchive(t, []byte("graphalytics synthetic payload — sha256 mismatch"))

	ds := GraphalyticsDataset{
		Name:   "synthetic-sha-mismatch",
		SHA256: "deadbeef" + strings.Repeat("0", 56), // 64 hex chars, wrong.
		MD5:    md5,                                  // correct, but must not rescue.
	}
	err := verifyGraphalyticsChecksum(path, ds)
	if !errors.Is(err, ErrGraphalyticsChecksumMismatch) {
		t.Fatalf("verifyGraphalyticsChecksum with wrong SHA-256: got %v, want ErrGraphalyticsChecksumMismatch", err)
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("SHA-256 mismatch error should name the sha256 digest, got %q", err.Error())
	}
}

// TestVerifyGraphalyticsChecksum_MD5Mismatch proves that on the
// fallback path (empty SHA-256) a wrong MD5 is rejected with the
// typed checksum error, completing the verification matrix.
func TestVerifyGraphalyticsChecksum_MD5Mismatch(t *testing.T) {
	path, _, _ := writeSyntheticArchive(t, []byte("graphalytics synthetic payload — md5 mismatch"))

	ds := GraphalyticsDataset{
		Name:   "synthetic-md5-mismatch",
		SHA256: "",
		MD5:    "ffffffffffffffffffffffffffffffff", // wrong.
	}
	err := verifyGraphalyticsChecksum(path, ds)
	if !errors.Is(err, ErrGraphalyticsChecksumMismatch) {
		t.Fatalf("verifyGraphalyticsChecksum with wrong MD5 on fallback: got %v, want ErrGraphalyticsChecksumMismatch", err)
	}
	if !strings.Contains(err.Error(), "md5") {
		t.Errorf("MD5 mismatch error should name the md5 digest, got %q", err.Error())
	}
}

// TestVerifyGraphalyticsChecksum_NoDigestRegistered proves that an
// entry with neither digest is rejected rather than silently accepted,
// guarding against an un-pinned dataset slipping through verification.
func TestVerifyGraphalyticsChecksum_NoDigestRegistered(t *testing.T) {
	path, _, _ := writeSyntheticArchive(t, []byte("graphalytics synthetic payload — no digest"))

	ds := GraphalyticsDataset{Name: "synthetic-no-digest", SHA256: "", MD5: ""}
	if err := verifyGraphalyticsChecksum(path, ds); !errors.Is(err, ErrGraphalyticsChecksumMismatch) {
		t.Fatalf("verifyGraphalyticsChecksum with no registered digest: got %v, want ErrGraphalyticsChecksumMismatch", err)
	}
}

// TestGraphalyticsDatasets_SHA256LeftEmpty documents the externally
// blocked AC#1: the three pinned datasets must keep empty SHA-256
// fields until a real soak fetch back-populates them. Fabricating a
// digest here (or in the registry) would defeat integrity checking,
// so this test fails loudly if a non-empty SHA-256 ever appears
// without the accompanying real-fetch evidence.
func TestGraphalyticsDatasets_SHA256LeftEmpty(t *testing.T) {
	for name, ds := range GraphalyticsDatasets {
		if ds.SHA256 != "" {
			t.Errorf("dataset %q has SHA256=%q; it must stay empty until a real soak fetch supplies the audited digest (AC#1 is externally blocked — never fabricate)", name, ds.SHA256)
		}
		if ds.MD5 == "" {
			t.Errorf("dataset %q has empty MD5; the MD5 fallback is the only integrity check while SHA-256 is unpinned", name)
		}
	}
}
