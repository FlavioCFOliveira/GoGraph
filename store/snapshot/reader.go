package snapshot

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
)

// ErrCorrupted is returned by [Open] when a component file CRC32C
// disagrees with the manifest, or when a referenced file is missing
// or shorter than expected.
var ErrCorrupted = errors.New("snapshot: directory corrupted")

// LoadedCSR is the result of [LoadCSR] / [Open]: the parsed CSR
// arrays plus the manifest entry that produced them.
type LoadedCSR struct {
	Manifest Manifest
	CSR      CSRReadback
}

// Open verifies and loads the snapshot rooted at dir. It reads the
// manifest, then reads csr.bin and verifies its CRC32C matches the
// manifest entry. Future versions may load additional components
// (labels.bin, properties.bin, schema.bin) by extending Manifest.Files.
func Open(dir string) (LoadedCSR, error) {
	manifestPath := filepath.Join(dir, "manifest.json")
	m, err := ReadManifestFile(manifestPath)
	if err != nil {
		return LoadedCSR{}, err
	}
	var csrEntry *FileEntry
	for k := range m.Files {
		if m.Files[k].Name == CSRFile {
			csrEntry = &m.Files[k]
			break
		}
	}
	if csrEntry == nil {
		return LoadedCSR{}, fmt.Errorf("%w: manifest missing %q", ErrCorrupted, CSRFile)
	}
	csrPath := filepath.Join(dir, CSRFile)
	f, err := os.Open(csrPath) //nolint:gosec // caller-supplied path
	if err != nil {
		return LoadedCSR{}, err
	}
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(f, hasher)
	parsed, err := ReadCSR(tee)
	if err != nil {
		return LoadedCSR{}, fmt.Errorf("%w: %v", ErrCorrupted, err)
	}
	// Drain any trailing bytes through the hasher (e.g., padding) so
	// the CRC matches the full on-disk file.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return LoadedCSR{}, fmt.Errorf("%w: %v", ErrCorrupted, err)
	}
	if got := hasher.Sum32(); got != csrEntry.CRC32C {
		return LoadedCSR{}, fmt.Errorf("%w: %s crc32c=%d want=%d",
			ErrCorrupted, CSRFile, got, csrEntry.CRC32C)
	}
	return LoadedCSR{Manifest: m, CSR: parsed}, nil
}
