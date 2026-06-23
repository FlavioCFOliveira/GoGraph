package snapshot

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"path/filepath"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
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
	return openWith(osBackend{}, dir)
}

// openWith is the filesystem-seam implementation behind [Open]: the
// manifest and csr.bin reads route through fsys, so the OS backend
// reproduces the historical behaviour exactly (csr.bin via a plain open
// without O_NOFOLLOW, as before) while the simulator can supply an
// in-memory disk.
func openWith(fsys fileSystem, dir string) (LoadedCSR, error) {
	defer metrics.Time("store.snapshot.Open")()
	manifestPath := filepath.Join(dir, "manifest.json")
	m, err := readManifestFileWith(fsys, manifestPath)
	if err != nil {
		metrics.IncCounter("store.snapshot.Open.errors", 1)
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
		metrics.IncCounter("store.snapshot.Open.errors", 1)
		return LoadedCSR{}, fmt.Errorf("%w: manifest missing %q", ErrCorrupted, CSRFile)
	}
	csrPath := filepath.Join(dir, CSRFile)
	f, err := fsys.Open(csrPath)
	if err != nil {
		metrics.IncCounter("store.snapshot.Open.errors", 1)
		return LoadedCSR{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(f, hasher)
	// Pass the manifest-recorded size as the precise remaining-bytes
	// bound: a header that declares more vertices/edges/weights than
	// csrEntry.Size bytes could hold is rejected before any allocation.
	parsed, err := readCSRLimited(tee, csrEntry.Size)
	if err != nil {
		metrics.IncCounter("store.snapshot.Open.errors", 1)
		return LoadedCSR{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	// Drain any trailing bytes through the hasher (e.g., padding) so
	// the CRC matches the full on-disk file.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		metrics.IncCounter("store.snapshot.Open.errors", 1)
		return LoadedCSR{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if got := hasher.Sum32(); got != csrEntry.CRC32C {
		metrics.IncCounter("store.snapshot.Open.errors", 1)
		return LoadedCSR{}, fmt.Errorf("%w: %s crc32c=%d want=%d",
			ErrCorrupted, CSRFile, got, csrEntry.CRC32C)
	}
	return LoadedCSR{Manifest: m, CSR: parsed}, nil
}
