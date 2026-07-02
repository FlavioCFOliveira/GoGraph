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
	defer metrics.Time("store.snapshot.Open").Stop()
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
	// OpenComponent (not Open) applies O_NOFOLLOW on unix, so a symlinked
	// csr.bin in an untrusted snapshot directory is rejected rather than
	// followed — consistent with the hardened LoadSnapshotFull path (CWE-59).
	f, err := fsys.OpenComponent(csrPath)
	if err != nil {
		metrics.IncCounter("store.snapshot.Open.errors", 1)
		return LoadedCSR{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(f, hasher)
	// Bound the allocation by min(manifest size, real on-disk size): a header
	// that declares more vertices/edges/weights than the file could hold — even
	// under a manifest that lies about the size (e.g. size:0 to force the
	// backstop) — is rejected before any allocation. The bound comes from an
	// fstat of the open fd (see [safeCSRAllocBound]), so there is no TOCTOU.
	bound, err := safeCSRAllocBound(f, csrEntry.Size)
	if err != nil {
		metrics.IncCounter("store.snapshot.Open.errors", 1)
		return LoadedCSR{}, err
	}
	parsed, err := readCSRLimited(tee, bound)
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
