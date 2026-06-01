package snapshot

import (
	"bytes"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

// TestValidateIndexName_TableDriven exercises the path-traversal guard
// directly: every escaping or malformed name must be rejected with
// ErrManifestCorrupted, and every legitimate single-element name must be
// accepted. Finding H5.
func TestValidateIndexName_TableDriven(t *testing.T) {
	t.Parallel()
	reject := []string{
		"",
		".",
		"..",
		"../secret",
		"../../../../tmp/pwned",
		"a/b",
		"a\\b",
		"sub/idx",
		"./idx",
		"idx/",
		"/etc/passwd",
		"/abs/name",
		"foo/../bar",
		"..hidden/../x",
	}
	for _, name := range reject {
		if err := validateIndexName(name); err == nil {
			t.Errorf("validateIndexName(%q) = nil, want error", name)
		} else if !errors.Is(err, ErrManifestCorrupted) {
			t.Errorf("validateIndexName(%q) = %v, want wrapping ErrManifestCorrupted", name, err)
		}
	}

	accept := []string{
		"labels.nodes",
		"hash.email",
		"btree.age",
		"index_42",
		"a",
		"my-index.v2",
	}
	for _, name := range accept {
		if err := validateIndexName(name); err != nil {
			t.Errorf("validateIndexName(%q) = %v, want nil", name, err)
		}
	}
}

// TestLoadIndexes_RejectTraversalName confirms LoadIndexes returns
// ErrManifestCorrupted for a relative traversal name and never reads the
// file the name would have escaped to. Finding H5.
func TestLoadIndexes_RejectTraversalName(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "snap")
	idxDir := filepath.Join(dir, IndexesDir)
	if err := os.MkdirAll(idxDir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Plant a sentinel OUTSIDE idxDir that the traversal name would reach.
	// filepath.Join(idxDir, "../../secret.bin") resolves under root.
	const secret = "TOP-SECRET-MUST-NOT-BE-READ"
	secretPath := filepath.Join(root, "secret.bin")
	if err := os.WriteFile(secretPath, []byte(secret), 0o600); err != nil {
		t.Fatalf("WriteFile sentinel: %v", err)
	}

	entries := []IndexFileEntry{
		// ".bin" is appended by LoadIndexes, so the on-disk target of
		// this name without the guard would be root/../secret -> the
		// sentinel one level up from idxDir's parent.
		{Name: "../../secret", Size: int64(len(secret)), CRC32C: 0},
	}
	got, err := LoadIndexes(dir, entries)
	if err == nil {
		t.Fatal("LoadIndexes with traversal name = nil error, want ErrManifestCorrupted")
	}
	if !errors.Is(err, ErrManifestCorrupted) {
		t.Fatalf("LoadIndexes error = %v, want wrapping ErrManifestCorrupted", err)
	}
	if got != nil {
		t.Fatalf("LoadIndexes returned %v readbacks on rejection, want nil", got)
	}
	// Defence in depth: no readback ever carried the sentinel bytes.
	for _, rb := range got {
		if string(rb.Bytes) == secret {
			t.Fatal("sentinel file outside idxDir was read")
		}
	}
}

// TestLoadIndexes_RejectAbsoluteName confirms LoadIndexes returns
// ErrManifestCorrupted for an absolute index name. Finding H5.
func TestLoadIndexes_RejectAbsoluteName(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "snap")
	idxDir := filepath.Join(dir, IndexesDir)
	if err := os.MkdirAll(idxDir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// An absolute name; on a real attack this would be /etc/anything.
	abs := filepath.Join(root, "outside")
	entries := []IndexFileEntry{{Name: abs}}
	got, err := LoadIndexes(dir, entries)
	if err == nil {
		t.Fatal("LoadIndexes with absolute name = nil error, want ErrManifestCorrupted")
	}
	if !errors.Is(err, ErrManifestCorrupted) {
		t.Fatalf("LoadIndexes error = %v, want wrapping ErrManifestCorrupted", err)
	}
	if got != nil {
		t.Fatalf("LoadIndexes returned %v readbacks on rejection, want nil", got)
	}
}

// TestLoadIndexes_ValidNameUnchanged confirms a legitimate index name
// still loads its bytes when the on-disk file and CRC match — the guard
// does not perturb the happy path. Finding H5.
func TestLoadIndexes_ValidNameUnchanged(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "snap")
	idxDir := filepath.Join(dir, IndexesDir)
	if err := os.MkdirAll(idxDir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	payload := []byte("legit-index-bytes")
	if err := os.WriteFile(filepath.Join(idxDir, "good.bin"), payload, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	crc := crc32.Checksum(payload, castagnoli)
	got, err := LoadIndexes(dir, []IndexFileEntry{{Name: "good", Size: int64(len(payload)), CRC32C: crc}})
	if err != nil {
		t.Fatalf("LoadIndexes(valid) = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(readbacks) = %d, want 1", len(got))
	}
	if !bytes.Equal(got[0].Bytes, payload) {
		t.Fatalf("bytes = %q, want %q", got[0].Bytes, payload)
	}
}
