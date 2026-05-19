package snapshot

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"time"

	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/internal/metrics"
)

// LoadedSnapshot is the result of [LoadSnapshotFull]: the parsed CSR
// arrays, the parsed labels readback (empty for v1 snapshots), and
// the manifest that produced them.
type LoadedSnapshot struct {
	Manifest Manifest
	CSR      CSRReadback
	Labels   LabelsReadback
}

// WriteSnapshotFull is the v2 high-level helper: it lays out a
// snapshot directory containing csr.bin (legacy v1 component),
// labels.bin (new v2 component), and a v2 manifest indexing both.
// Atomic publication is achieved by assembling the snapshot under
// dir + ".tmp" and renaming it to dir on success — the same protocol
// used by [WriteSnapshotCSR].
//
// Callers that do not need durable LPG labels can keep using
// [WriteSnapshotCSR]; it writes a v1-shaped directory that future
// readers (including this one) accept transparently.
func WriteSnapshotFull[N comparable, W any](dir string, c *csr.CSR[W], g *lpg.Graph[N, W]) error {
	defer metrics.Time("store.snapshot.WriteSnapshotFull")()
	err := WriteSnapshotFullCtx(context.Background(), dir, c, g)
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFull.errors", 1)
	}
	return err
}

// WriteSnapshotFullCtx is the context-aware variant of
// [WriteSnapshotFull]. ctx.Err() is checked at four stage boundaries:
// before the CSR write, before the labels write, before the manifest
// write, and before the atomic rename. On cancellation the temporary
// staging directory is cleaned up and the wrapped ctx.Err is
// returned.
//
//nolint:gocyclo // snapshot publish: dir prep + CSR write + labels write + manifest write + atomic rename + ctx ticks
func WriteSnapshotFullCtx[N comparable, W any](
	ctx context.Context,
	dir string,
	c *csr.CSR[W],
	g *lpg.Graph[N, W],
) error {
	defer metrics.Time("store.snapshot.WriteSnapshotFullCtx")()
	if err := ctx.Err(); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	tmp := dir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// csr.bin
	csrPath := filepath.Join(tmp, CSRFile)
	csrSize, csrCRC, err := writeAndSync(csrPath, func(w io.Writer) (int64, uint32, error) {
		return WriteCSR(w, c)
	})
	if err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	if err := ctx.Err(); err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// labels.bin
	labelsPath := filepath.Join(tmp, LabelsFile)
	labelsSize, labelsCRC, err := writeAndSync(labelsPath, func(w io.Writer) (int64, uint32, error) {
		return WriteLabels(w, g)
	})
	if err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	if err := ctx.Err(); err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	m := Manifest{
		Version:   ManifestVersion,
		CreatedAt: time.Now().UTC(),
		Order:     c.Order(),
		Size:      c.Size(),
		Files: []FileEntry{
			{Name: CSRFile, Size: csrSize, CRC32C: csrCRC},
			{Name: LabelsFile, Size: labelsSize, CRC32C: labelsCRC},
		},
	}

	manifestPath := filepath.Join(tmp, "manifest.json")
	mf, err := os.Create(manifestPath) //nolint:gosec // caller-controlled directory
	if err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	if err := WriteManifest(mf, m); err != nil {
		_ = mf.Close()
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	if err := mf.Sync(); err != nil {
		_ = mf.Close()
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	if err := mf.Close(); err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	if err := ctx.Err(); err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return fmt.Errorf("snapshot: publish rename: %w", err)
	}
	return nil
}

// writeAndSync creates path, hands the file handle to write, fsyncs
// and closes the file. It returns the (size, crc) tuple computed by
// write so the caller can record them in the manifest. The caller's
// path must reside under the staging .tmp directory; the function
// removes the file on any error (best effort) so a half-written
// component never lingers.
func writeAndSync(
	path string,
	write func(io.Writer) (int64, uint32, error),
) (size int64, crc uint32, err error) {
	f, err := os.Create(path) //nolint:gosec // caller-controlled directory
	if err != nil {
		return 0, 0, err
	}
	size, crc, werr := write(f)
	if werr != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return 0, 0, werr
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return 0, 0, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return 0, 0, err
	}
	return size, crc, nil
}

// LoadSnapshotFull verifies and loads the snapshot rooted at dir,
// returning both the CSR and the labels readback. v1 snapshots are
// accepted transparently: their manifest has no labels.bin entry,
// and the returned [LoadedSnapshot.Labels] is the zero value (empty
// strings table, no records). v2 snapshots additionally read and
// CRC-validate labels.bin.
//
// CSR CRC verification mirrors [Open]; labels CRC verification uses
// the same TeeReader pattern so a corrupted labels.bin surfaces as
// [ErrCorrupted].
func LoadSnapshotFull(dir string) (LoadedSnapshot, error) {
	defer metrics.Time("store.snapshot.LoadSnapshotFull")()
	manifestPath := filepath.Join(dir, "manifest.json")
	m, err := ReadManifestFile(manifestPath)
	if err != nil {
		metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
		return LoadedSnapshot{}, err
	}

	csrEntry, labelsEntry := findEntries(m.Files)
	if csrEntry == nil {
		metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
		return LoadedSnapshot{}, fmt.Errorf("%w: manifest missing %q", ErrCorrupted, CSRFile)
	}

	csrParsed, err := readVerifiedCSR(filepath.Join(dir, CSRFile), csrEntry.CRC32C)
	if err != nil {
		metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
		return LoadedSnapshot{}, err
	}

	var labelsParsed LabelsReadback
	if labelsEntry != nil {
		labelsParsed, err = readVerifiedLabels(filepath.Join(dir, LabelsFile), labelsEntry.CRC32C)
		if err != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, err
		}
	}

	return LoadedSnapshot{
		Manifest: m,
		CSR:      csrParsed,
		Labels:   labelsParsed,
	}, nil
}

// findEntries returns pointers to the csr.bin and labels.bin entries
// in files, or nil for either when absent. The slice is walked once
// and pointers index into the original storage so the caller can
// inspect them without copying.
func findEntries(files []FileEntry) (csrEntry, labelsEntry *FileEntry) {
	for k := range files {
		switch files[k].Name {
		case CSRFile:
			csrEntry = &files[k]
		case LabelsFile:
			labelsEntry = &files[k]
		}
	}
	return csrEntry, labelsEntry
}

// readVerifiedCSR opens path, runs the file bytes through CRC32C and
// the structural CSR reader simultaneously, and returns the parsed
// snapshot iff the CRC matches expected. Any disagreement surfaces
// as [ErrCorrupted].
func readVerifiedCSR(path string, expected uint32) (CSRReadback, error) {
	f, err := os.Open(path) //nolint:gosec // caller-supplied path
	if err != nil {
		return CSRReadback{}, err
	}
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(f, hasher)
	parsed, err := ReadCSR(tee)
	if err != nil {
		return CSRReadback{}, fmt.Errorf("%w: %v", ErrCorrupted, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return CSRReadback{}, fmt.Errorf("%w: %v", ErrCorrupted, err)
	}
	if got := hasher.Sum32(); got != expected {
		return CSRReadback{}, fmt.Errorf("%w: %s crc32c=%d want=%d",
			ErrCorrupted, CSRFile, got, expected)
	}
	return parsed, nil
}

// readVerifiedLabels is the dual of [readVerifiedCSR] for labels.bin.
func readVerifiedLabels(path string, expected uint32) (LabelsReadback, error) {
	f, err := os.Open(path) //nolint:gosec // caller-supplied path
	if err != nil {
		return LabelsReadback{}, err
	}
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(f, hasher)
	parsed, err := ReadLabels(tee)
	if err != nil {
		return LabelsReadback{}, fmt.Errorf("%w: %v", ErrCorrupted, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return LabelsReadback{}, fmt.Errorf("%w: %v", ErrCorrupted, err)
	}
	if got := hasher.Sum32(); got != expected {
		return LabelsReadback{}, fmt.Errorf("%w: %s crc32c=%d want=%d",
			ErrCorrupted, LabelsFile, got, expected)
	}
	return parsed, nil
}
