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

	"gograph/graph"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/internal/metrics"
)

// LoadedSnapshot is the result of [LoadSnapshotFull]: the parsed CSR
// arrays, the parsed labels readback (empty for v1 snapshots), the
// parsed properties readback (empty when properties.bin is absent),
// the parsed mapper readback (empty when mapper.bin is absent, which
// is the case for every v1 and v2 snapshot), the optional per-index
// byte payloads (one entry per indexes/<name>.bin file referenced by
// the manifest), and the manifest that produced them.
//
// Each [IndexReadback].Bytes may be nil even when the manifest
// references the index — that signals the file was missing or its
// CRC32C did not validate. Callers must treat nil bytes as "rebuild
// from LPG" rather than as a fatal error; the corruption was already
// metered by [LoadIndexes] under `store.snapshot.indexes.corrupted`.
type LoadedSnapshot struct {
	Manifest   Manifest
	CSR        CSRReadback
	Labels     LabelsReadback
	Properties PropertiesReadback
	Mapper     MapperReadback
	Indexes    []IndexReadback
}

// WriteSnapshotFull is the v2/v3 high-level helper: it lays out a
// snapshot directory containing csr.bin (legacy v1 component),
// labels.bin (v2 component), properties.bin (v2 component) and a
// manifest indexing them. When the underlying [graph.Mapper] is
// string-keyed (N=string) the writer additionally emits mapper.bin —
// the durable (NodeID -> natural key) interning table — and the
// manifest is stamped at [ManifestVersion] (v3). For any other N the
// writer falls back to the v2 layout (no mapper.bin) and the manifest
// records [manifestVersionV2]; recovery from a v2 snapshot continues
// to rely on WAL replay to re-intern keys.
//
// Atomic publication is achieved by assembling the snapshot under
// dir + ".tmp" and renaming it to dir on success — the same protocol
// used by [WriteSnapshotCSR].
//
// When g carries a non-nil [index.Manager] (set via
// [lpg.Graph.SetIndexManager]) with at least one registered index
// that implements [index.Serializer], an indexes/ sub-directory is
// also produced — one file per registered serializable index, each
// referenced from the manifest's Indexes field. Subscribers that do
// not implement [index.Serializer] are skipped (rebuild-on-restart).
//
// Callers that do not need durable LPG labels or properties can keep
// using [WriteSnapshotCSR]; it writes a v1-shaped directory that
// future readers (including this one) accept transparently.
func WriteSnapshotFull[N comparable, W any](dir string, c *csr.CSR[W], g *lpg.Graph[N, W]) error {
	defer metrics.Time("store.snapshot.WriteSnapshotFull")()
	err := WriteSnapshotFullCtx(context.Background(), dir, c, g)
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFull.errors", 1)
	}
	return err
}

// WriteSnapshotFullCtx is the context-aware variant of
// [WriteSnapshotFull]. ctx.Err() is checked at five stage boundaries:
// before the CSR write, before the labels write, before the
// properties write, before the manifest write, and before the
// atomic rename. On cancellation the temporary staging directory is
// cleaned up and the wrapped ctx.Err is returned.
//
//nolint:gocyclo // snapshot publish: dir prep + CSR + labels + properties + manifest + atomic rename + ctx ticks
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

	// properties.bin
	propertiesPath := filepath.Join(tmp, PropertiesFile)
	propsSize, propsCRC, err := writeAndSync(propertiesPath, func(w io.Writer) (int64, uint32, error) {
		return WriteProperties(w, g)
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

	// mapper.bin — durable (NodeID -> natural key) table. Only emitted
	// when the graph's Mapper is string-keyed: that is the only N for
	// which the codec is implemented today and the only N exercised by
	// production call sites (txn.NewStoreWithOptions, the social_cli
	// example, every Cypher TCK persistence test). Non-string N falls
	// back to the v2 layout (no mapper.bin) and recovery continues to
	// rebuild the mapper from the WAL — the documented v2 contract.
	mapperSize, mapperCRC, haveMapper, err := writeMapperIfStringKeyed(ctx, tmp, g)
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

	// indexes/<name>.bin — one file per registered index that
	// implements [index.Serializer]. Subscribers without serializer
	// support are silently skipped (rebuild-on-restart contract).
	var idxEntries []IndexFileEntry
	if mgr := g.IndexManager(); mgr != nil && mgr.Count() > 0 {
		entries, ierr := WriteIndexes(tmp, mgr)
		if ierr != nil {
			_ = os.RemoveAll(tmp)
			metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
			return ierr
		}
		idxEntries = entries
	}

	if err := ctx.Err(); err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// Manifest version is v3 only when mapper.bin was emitted; non-
	// string-keyed graphs continue to produce v2 manifests so existing
	// recovery tests (which compare Manifest.Version against the
	// build's [ManifestVersion]) keep passing for every shape that
	// already worked before this change.
	manifestVersion := manifestVersionV2
	files := []FileEntry{
		{Name: CSRFile, Size: csrSize, CRC32C: csrCRC},
		{Name: LabelsFile, Size: labelsSize, CRC32C: labelsCRC},
		{Name: PropertiesFile, Size: propsSize, CRC32C: propsCRC},
	}
	if haveMapper {
		manifestVersion = ManifestVersion
		files = append(files, FileEntry{Name: MapperFile, Size: mapperSize, CRC32C: mapperCRC})
	}

	m := Manifest{
		Version:   manifestVersion,
		CreatedAt: time.Now().UTC(),
		Order:     c.Order(),
		Size:      c.Size(),
		Files:     files,
		Indexes:   idxEntries,
	}

	manifestPath := filepath.Join(tmp, "manifest.json")
	mf, err := os.Create(manifestPath) //nolint:gosec // caller-controlled directory
	if err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	if err := WriteManifest(mf, m); err != nil {
		_ = mf.Close()        // best-effort: already on error path, WriteManifest err preserved
		_ = os.RemoveAll(tmp) // best-effort: tmp dir cleanup, WriteManifest err preserved
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	if err := mf.Sync(); err != nil {
		_ = mf.Close()        // best-effort: already on error path, sync err preserved
		_ = os.RemoveAll(tmp) // best-effort: tmp dir cleanup, sync err preserved
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	if err := mf.Close(); err != nil {
		_ = os.RemoveAll(tmp) // best-effort: tmp dir cleanup, close err preserved
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
	// Make the rename durable: fsync the parent directory so the
	// new directory entry survives a crash within the journal
	// writeback window. No-op on platforms that lack a directory
	// fsync primitive (Windows).
	if err := parentDirFsync(dir); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return fmt.Errorf("snapshot: publish parent fsync: %w", err)
	}
	return nil
}

// writeMapperIfStringKeyed inspects g's mapper and, when N=string,
// serialises it to mapper.bin under tmp. Returns (size, crc, true,
// nil) on success, (0, 0, false, nil) when N is not string (callers
// fall back to v2), or a non-nil error on a write failure (callers
// must clean tmp and surface the error).
//
// The function uses a type-switch on a sentinel pointer so the
// compiler can prove the conversion is well-typed without resorting
// to unsafe; it returns false for any N that is not the canonical
// string type. The fallback to v2 is documented at the writer
// godoc — non-string graphs keep producing the same on-disk layout as
// before this change.
func writeMapperIfStringKeyed[N comparable, W any](
	ctx context.Context,
	tmp string,
	g *lpg.Graph[N, W],
) (size int64, crc uint32, ok bool, err error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, false, err
	}
	adj := g.AdjList()
	mapper := adj.Mapper()
	// Reflection-free probe: the writer only knows how to serialise
	// the string-keyed mapper. We tunnel the concrete pointer through
	// a type assertion on any() to avoid pulling in reflect for a one-
	// shot dispatch on the hot path.
	stringMapper, ok := any(mapper).(*graph.Mapper[string])
	if !ok {
		return 0, 0, false, nil
	}
	mapperPath := filepath.Join(tmp, MapperFile)
	mSize, mCRC, werr := writeAndSync(mapperPath, func(w io.Writer) (int64, uint32, error) {
		return WriteMapperString(w, stringMapper)
	})
	if werr != nil {
		return 0, 0, false, werr
	}
	return mSize, mCRC, true, nil
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
		_ = f.Close()       // best-effort: already on error path, write err preserved
		_ = os.Remove(path) // best-effort: partial file cleanup, write err preserved
		return 0, 0, werr
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()       // best-effort: already on error path, sync err preserved
		_ = os.Remove(path) // best-effort: partial file cleanup, sync err preserved
		return 0, 0, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path) // best-effort: partial file cleanup, close err preserved
		return 0, 0, err
	}
	return size, crc, nil
}

// LoadSnapshotFull verifies and loads the snapshot rooted at dir,
// returning the CSR, the labels readback, and the properties
// readback. v1 snapshots are accepted transparently: their manifest
// has no labels.bin or properties.bin entry, and the returned
// [LoadedSnapshot.Labels] / [LoadedSnapshot.Properties] are zero
// values (empty tables, no records). v2 snapshots may carry any
// combination of labels.bin and properties.bin; each component is
// CRC-validated only when its manifest entry is present.
//
// CSR CRC verification mirrors [Open]; labels and properties CRC
// verification use the same TeeReader pattern so a corrupted
// component surfaces as [ErrCorrupted].
func LoadSnapshotFull(dir string) (LoadedSnapshot, error) {
	defer metrics.Time("store.snapshot.LoadSnapshotFull")()
	manifestPath := filepath.Join(dir, "manifest.json")
	m, err := ReadManifestFile(manifestPath)
	if err != nil {
		metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
		return LoadedSnapshot{}, err
	}

	csrEntry, labelsEntry, propsEntry, mapperEntry := findEntries(m.Files)
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

	var propsParsed PropertiesReadback
	if propsEntry != nil {
		propsParsed, err = readVerifiedProperties(filepath.Join(dir, PropertiesFile), propsEntry.CRC32C)
		if err != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, err
		}
	}

	var mapperParsed MapperReadback
	if mapperEntry != nil {
		mapperParsed, err = readVerifiedMapper(filepath.Join(dir, MapperFile), mapperEntry.CRC32C)
		if err != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, err
		}
	}

	// indexes/<name>.bin — best-effort load. Corruption surfaces as
	// nil Bytes on the IndexReadback so the recovery path can rebuild
	// from the LPG rather than aborting.
	var idxReadback []IndexReadback
	if len(m.Indexes) > 0 {
		idxReadback, err = LoadIndexes(dir, m.Indexes)
		if err != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, err
		}
	}

	return LoadedSnapshot{
		Manifest:   m,
		CSR:        csrParsed,
		Labels:     labelsParsed,
		Properties: propsParsed,
		Mapper:     mapperParsed,
		Indexes:    idxReadback,
	}, nil
}

// findEntries returns pointers to the csr.bin, labels.bin,
// properties.bin and mapper.bin entries in files, or nil for any that
// are absent. The slice is walked once and pointers index into the
// original storage so the caller can inspect them without copying.
func findEntries(files []FileEntry) (csrEntry, labelsEntry, propsEntry, mapperEntry *FileEntry) {
	for k := range files {
		switch files[k].Name {
		case CSRFile:
			csrEntry = &files[k]
		case LabelsFile:
			labelsEntry = &files[k]
		case PropertiesFile:
			propsEntry = &files[k]
		case MapperFile:
			mapperEntry = &files[k]
		}
	}
	return csrEntry, labelsEntry, propsEntry, mapperEntry
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
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(f, hasher)
	parsed, err := ReadCSR(tee)
	if err != nil {
		return CSRReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return CSRReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
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
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(f, hasher)
	parsed, err := ReadLabels(tee)
	if err != nil {
		return LabelsReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return LabelsReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if got := hasher.Sum32(); got != expected {
		return LabelsReadback{}, fmt.Errorf("%w: %s crc32c=%d want=%d",
			ErrCorrupted, LabelsFile, got, expected)
	}
	return parsed, nil
}

// readVerifiedProperties is the dual of [readVerifiedCSR] for
// properties.bin.
func readVerifiedProperties(path string, expected uint32) (PropertiesReadback, error) {
	f, err := os.Open(path) //nolint:gosec // caller-supplied path
	if err != nil {
		return PropertiesReadback{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(f, hasher)
	parsed, err := ReadProperties(tee)
	if err != nil {
		return PropertiesReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return PropertiesReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if got := hasher.Sum32(); got != expected {
		return PropertiesReadback{}, fmt.Errorf("%w: %s crc32c=%d want=%d",
			ErrCorrupted, PropertiesFile, got, expected)
	}
	return parsed, nil
}
