package snapshot

// manifest_integrity_test.go — coverage for the manifest CRC32C trailer
// (rmp #2520).
//
// Before the trailer, manifest.json carried the CRC32C of every OTHER component
// and none of its own. A byte flipped inside a JSON KEY NAME left a
// syntactically valid document whose renamed key encoding/json silently drops,
// so the field decoded as its zero value with no error anywhere. Measured over a
// published fixture, 360 of 1399 bytes (25.7%) were accepted silently, and the
// worst consequence was `commit_ts`: zeroing it drops the MVCC clock floor
// recovery restores, so a reopened graph re-mints instants the image already
// contains (the loss rmp #2309 exists to prevent).
//
// These tests pin the four properties the fix rests on: total byte coverage,
// backward compatibility with unframed manifests, forward compatibility for
// readers that predate the trailer, and refusal of a manifest whose trailer was
// removed rather than damaged.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"path/filepath"
	"testing"
	"time"
)

// framedManifest returns a representative manifest as WriteManifest frames it:
// a pretty-printed JSON document followed by the 16-byte trailer.
func framedManifest(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := Manifest{
		Version:     ManifestVersion,
		CreatedAt:   time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
		Order:       3,
		Size:        2,
		CommitTS:    20,
		GraphConfig: &GraphConfig{Directed: true, Multigraph: true},
		Files: []FileEntry{
			{Name: CSRFile, Size: 2122, CRC32C: 276024788},
			{Name: LabelsFile, Size: 64, CRC32C: 99},
		},
	}
	if err := WriteManifest(&buf, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	return buf.Bytes()
}

// TestManifestTrailer_RoundTripsAndReportsVerified is the baseline: the writer
// frames, the reader verifies, and the verification is reported to the caller
// rather than merely performed.
func TestManifestTrailer_RoundTripsAndReportsVerified(t *testing.T) {
	t.Parallel()
	data := framedManifest(t)

	got, err := LoadManifest(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if !got.IntegrityVerified {
		t.Error("IntegrityVerified = false over a freshly framed manifest")
	}
	if got.Integrity != IntegrityCRC32CTrailer {
		t.Errorf("Integrity = %q, want %q", got.Integrity, IntegrityCRC32CTrailer)
	}
	if got.CommitTS != 20 {
		t.Errorf("CommitTS = %d, want 20", got.CommitTS)
	}
	// The trailer is exactly the documented width, and it is the tail of the file.
	if n := len(data); n < manifestTrailerSize {
		t.Fatalf("framed manifest is %d bytes, shorter than the trailer", n)
	}
	tr := data[len(data)-manifestTrailerSize:]
	if !bytes.Equal(tr[0:8], manifestTrailerMagic[:]) {
		t.Errorf("trailer magic = %x, want %x", tr[0:8], manifestTrailerMagic)
	}
	if algo := binary.LittleEndian.Uint32(tr[8:12]); algo != manifestTrailerAlgoCRC32C {
		t.Errorf("trailer algorithm = %d, want %d", algo, manifestTrailerAlgoCRC32C)
	}
	// The checksum covers the payload and NOT itself: it is not self-referential.
	payload := data[:len(data)-manifestTrailerSize]
	if got, want := crc32.Checksum(payload, castagnoli), binary.LittleEndian.Uint32(tr[12:16]); got != want {
		t.Errorf("recomputed crc32c = %d, want %d", got, want)
	}
}

// TestManifestTrailer_EveryByteIsChecksummed is the census this task exists for:
// there must be NO byte of a framed manifest — payload or trailer — that can be
// flipped without LoadManifest refusing.
//
// It sweeps the whole file, so it covers the three regions the fix had to
// close together: the JSON key names (the original defect), the JSON values, and
// the trailer's own magic and algorithm identifier (which, without the
// "a non-empty tail MUST verify" rule, would have degraded a protected manifest
// back to an unverified one).
func TestManifestTrailer_EveryByteIsChecksummed(t *testing.T) {
	t.Parallel()
	orig := framedManifest(t)

	var accepted []int
	data := make([]byte, len(orig))
	for off := range orig {
		copy(data, orig)
		data[off] ^= 0xFF
		if _, err := LoadManifest(bytes.NewReader(data)); err == nil {
			accepted = append(accepted, off)
		} else if !errors.Is(err, ErrManifestCorrupted) {
			t.Errorf("byte %d flipped -> %v, want ErrManifestCorrupted", off, err)
		}
	}
	if len(accepted) != 0 {
		t.Fatalf("%d of %d manifest bytes were accepted after a flip (offsets %v): the trailer does not cover the whole file",
			len(accepted), len(orig), accepted)
	}
	t.Logf("manifest census: 0 of %d bytes can be flipped with no error from LoadManifest", len(orig))
}

// TestManifestTrailer_CommitTSKeyFlipIsRefused pins the specific consequence
// rmp #2467 measured: a byte flipped inside the `commit_ts` KEY NAME used to
// leave valid JSON whose renamed key encoding/json dropped, so the MVCC clock
// floor decoded as 0 and RestoreMVCCClock was skipped.
//
// TestManifestTrailer_EveryByteIsChecksummed subsumes this, but the sweep
// asserts a property while this asserts the DEFECT, so a future change that
// weakened only this path could not pass by narrowing the sweep.
func TestManifestTrailer_CommitTSKeyFlipIsRefused(t *testing.T) {
	t.Parallel()
	orig := framedManifest(t)

	idx := bytes.Index(orig, []byte(`"commit_ts"`))
	if idx < 0 {
		t.Fatal("the framed manifest carries no commit_ts key: the test would be vacuous")
	}
	// Establish the defect is reachable at all: without the trailer the flipped
	// key really does zero the timestamp.
	unframed := orig[:len(orig)-manifestTrailerSize]
	damaged := make([]byte, len(unframed))
	copy(damaged, unframed)
	damaged[idx+3] ^= 0xFF
	var probe Manifest
	if err := json.NewDecoder(bytes.NewReader(damaged)).Decode(&probe); err != nil {
		t.Fatalf("the key-flipped document no longer parses as JSON (%v): the test is not measuring the silent-drop path", err)
	}
	if probe.CommitTS != 0 {
		t.Fatalf("the key flip left CommitTS = %d, want 0: the silent-drop path this test guards has changed", probe.CommitTS)
	}

	// And with the trailer, the same flip is refused.
	framed := make([]byte, len(orig))
	copy(framed, orig)
	framed[idx+3] ^= 0xFF
	got, err := LoadManifest(bytes.NewReader(framed))
	if !errors.Is(err, ErrManifestCorrupted) {
		t.Fatalf("flipping a byte of the commit_ts key = (%+v, %v), want ErrManifestCorrupted", got, err)
	}
	if got.CommitTS != 0 || got.IntegrityVerified {
		t.Errorf("a refused manifest still handed back state: %+v", got)
	}
}

// TestManifestTrailer_LegacyManifestLoadsUnverified pins backward
// compatibility: a manifest written before the trailer existed carries no
// trailer region, still opens, and reports that it was NOT verified.
//
// The frozen v1 fixture is the real evidence here — it is on disk, unchanged,
// and predates every part of this mechanism.
func TestManifestTrailer_LegacyManifestLoadsUnverified(t *testing.T) {
	t.Parallel()

	m, err := ReadManifestFile(filepath.Join("testdata", "v1", "sample", "manifest.json"))
	if err != nil {
		t.Fatalf("the frozen v1 fixture no longer loads: %v", err)
	}
	if m.Version != 1 {
		t.Errorf("fixture Version = %d, want 1", m.Version)
	}
	if m.IntegrityVerified {
		t.Error("IntegrityVerified = true over an unframed legacy manifest")
	}
	if m.Integrity != "" {
		t.Errorf("Integrity = %q over a legacy manifest, want empty", m.Integrity)
	}

	// The same holds for any hand-written manifest, which is what keeps the
	// version guard reachable over a document that carries no framing at all.
	legacy := []byte(`{"version": 1, "files": [{"name": "csr.bin", "size": 0, "crc32c": 0}]}` + "\n")
	got, err := LoadManifest(bytes.NewReader(legacy))
	if err != nil {
		t.Fatalf("LoadManifest over an unframed manifest: %v", err)
	}
	if got.IntegrityVerified {
		t.Error("IntegrityVerified = true over an unframed manifest")
	}
}

// TestManifestTrailer_OldReaderIgnoresTheTrailer is the forward-compatibility
// proof, and the reason ManifestVersion is deliberately NOT bumped: a build that
// predates the trailer decodes a framed manifest exactly as before.
//
// It reproduces what such a build does — json.Decoder.Decode straight off the
// file — and requires it to succeed and to see every field. json.Decoder stops
// at the end of the first complete value, so the trailer is never read.
func TestManifestTrailer_OldReaderIgnoresTheTrailer(t *testing.T) {
	t.Parallel()
	data := framedManifest(t)

	// Exactly the pre-#2520 LoadManifest body.
	var m Manifest
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&m); err != nil {
		t.Fatalf("a reader that predates the trailer failed on a framed manifest: %v", err)
	}
	if m.Version != ManifestVersion || m.CommitTS != 20 || len(m.Files) != 2 {
		t.Errorf("a pre-trailer reader decoded %+v, want the full manifest", m)
	}
	if m.IntegrityVerified {
		t.Error("IntegrityVerified was set by a plain JSON decode: it must never come off the wire")
	}
}

// TestManifestTrailer_StrippedTrailerIsRefused covers the one damage the trailer
// cannot speak for on its own: removal rather than corruption. A framed manifest
// whose 16-byte tail is gone is byte-for-byte a legacy manifest, and would be
// accepted unverified were it not for the Integrity marker inside the document.
func TestManifestTrailer_StrippedTrailerIsRefused(t *testing.T) {
	t.Parallel()
	data := framedManifest(t)
	stripped := data[:len(data)-manifestTrailerSize]

	// Precondition: stripping really does leave a well-formed JSON document, so
	// the refusal below comes from the marker and not from a parse failure.
	var probe Manifest
	if err := json.NewDecoder(bytes.NewReader(stripped)).Decode(&probe); err != nil {
		t.Fatalf("the stripped manifest does not parse as JSON (%v): the test is not measuring trailer loss", err)
	}
	if probe.Integrity != IntegrityCRC32CTrailer {
		t.Fatalf("the stripped manifest declares integrity %q: the marker this test relies on is not being written", probe.Integrity)
	}

	if _, err := LoadManifest(bytes.NewReader(stripped)); !errors.Is(err, ErrManifestCorrupted) {
		t.Fatalf("a manifest that declares a trailer and carries none = %v, want ErrManifestCorrupted", err)
	}
}

// TestManifestTrailer_ShortAndGarbageTailsAreRefused covers the tail shapes a
// byte flip cannot produce but a truncating or appending write can: a tail too
// short to be a trailer, and a tail longer than one.
func TestManifestTrailer_ShortAndGarbageTailsAreRefused(t *testing.T) {
	t.Parallel()
	data := framedManifest(t)

	cases := []struct {
		name  string
		build func() []byte
	}{
		{"tail-truncated", func() []byte { return data[:len(data)-4] }},
		{"tail-extended", func() []byte { return append(append([]byte{}, data...), 'x') }},
		{"tail-is-garbage", func() []byte {
			out := append([]byte{}, data[:len(data)-manifestTrailerSize]...)
			return append(out, bytes.Repeat([]byte{'z'}, manifestTrailerSize)...)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := LoadManifest(bytes.NewReader(tc.build())); !errors.Is(err, ErrManifestCorrupted) {
				t.Fatalf("LoadManifest(%s) = %v, want ErrManifestCorrupted", tc.name, err)
			}
		})
	}
}

// TestManifestTrailer_WriteManifestFramesRegardlessOfCaller pins that the
// framing is not something a caller can forget or defeat: WriteManifest sets
// Integrity on its own copy, so a caller passing an empty or a wrong value still
// produces a manifest that verifies.
func TestManifestTrailer_WriteManifestFramesRegardlessOfCaller(t *testing.T) {
	t.Parallel()
	for _, declared := range []string{"", "none", "sha256-sidecar"} {
		var buf bytes.Buffer
		if err := WriteManifest(&buf, Manifest{Version: ManifestVersion, Integrity: declared}); err != nil {
			t.Fatalf("WriteManifest(Integrity=%q): %v", declared, err)
		}
		got, err := LoadManifest(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("LoadManifest after WriteManifest(Integrity=%q): %v", declared, err)
		}
		if !got.IntegrityVerified || got.Integrity != IntegrityCRC32CTrailer {
			t.Errorf("WriteManifest(Integrity=%q) produced %+v, want a verified crc32c trailer", declared, got)
		}
	}
}
