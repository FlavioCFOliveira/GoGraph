package snapshot

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"time"

	"gograph/graph"
	"gograph/graph/csr"
	"gograph/internal/metrics"
)

// CSRFile is the conventional file name carrying the CSR triplet
// (vertices + edges + optional weights) inside a snapshot directory.
const CSRFile = "csr.bin"

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// WriteCSR serialises c to w, returning the number of bytes written
// and the CRC32C of the serialised payload. The on-disk layout is:
//
//	uint64 nVertices      (little-endian)
//	uint64 nEdges
//	uint8  hasWeights     (1 = weights array present)
//	uint8  weightSizeBytes (0 when hasWeights = 0)
//	[vertices]            (nVertices * 8 bytes)
//	[edges]               (nEdges * 8 bytes)
//	[weights]             (nEdges * weightSizeBytes bytes, when present)
func WriteCSR[W any](w io.Writer, c *csr.CSR[W]) (size int64, crc uint32, err error) {
	defer metrics.Time("store.snapshot.WriteCSR")()
	bw := bufio.NewWriterSize(w, 1<<20)
	hasher := crc32.New(castagnoli)
	tee := io.MultiWriter(bw, hasher)

	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	weights := c.WeightsSlice()

	if err := binary.Write(tee, binary.LittleEndian, uint64(len(verts))); err != nil {
		metrics.IncCounter("store.snapshot.WriteCSR.errors", 1)
		return 0, 0, err
	}
	if err := binary.Write(tee, binary.LittleEndian, uint64(len(edges))); err != nil {
		metrics.IncCounter("store.snapshot.WriteCSR.errors", 1)
		return 0, 0, err
	}
	wsize := uint8(0)
	if weights != nil {
		wsize = csrWeightSize[W]()
	}
	hasW := uint8(0)
	if wsize > 0 {
		hasW = 1
	}
	if _, err := tee.Write([]byte{hasW, wsize}); err != nil {
		metrics.IncCounter("store.snapshot.WriteCSR.errors", 1)
		return 0, 0, err
	}
	if err := binary.Write(tee, binary.LittleEndian, verts); err != nil {
		metrics.IncCounter("store.snapshot.WriteCSR.errors", 1)
		return 0, 0, err
	}
	// Write edges as []uint64 by casting through binary.Write.
	tmp := make([]uint64, len(edges))
	for i, e := range edges {
		tmp[i] = uint64(e)
	}
	if err := binary.Write(tee, binary.LittleEndian, tmp); err != nil {
		metrics.IncCounter("store.snapshot.WriteCSR.errors", 1)
		return 0, 0, err
	}
	if hasW == 1 {
		if err := binary.Write(tee, binary.LittleEndian, weights); err != nil {
			metrics.IncCounter("store.snapshot.WriteCSR.errors", 1)
			return 0, 0, err
		}
	}
	if err := bw.Flush(); err != nil {
		metrics.IncCounter("store.snapshot.WriteCSR.errors", 1)
		return 0, 0, err
	}
	total := int64(8+8+2+8*len(verts)+8*len(edges)) + int64(int(wsize))*int64(len(edges))
	return total, hasher.Sum32(), nil
}

// csrWeightSize returns the size in bytes of one weight value, or 0
// when W is struct{} (unweighted). For unsupported weight types we
// return 0 and rely on the writer to skip the weights section.
func csrWeightSize[W any]() uint8 {
	var zero W
	switch any(zero).(type) {
	case struct{}:
		return 0
	case int8, uint8, bool:
		return 1
	case int16, uint16:
		return 2
	case int32, uint32, float32:
		return 4
	case int, uint, int64, uint64, float64, uintptr:
		return 8
	}
	return 0
}

// CSRReadback parses a CSR previously serialised by [WriteCSR]. It
// returns the parsed vertices, edges, and (optional) raw weight
// bytes plus the on-disk CRC32C of the payload.
type CSRReadback struct {
	Vertices    []uint64
	Edges       []graph.NodeID
	HasWeights  bool
	WeightSize  uint8
	WeightBytes []byte
}

// ReadCSR parses a CSR previously written by [WriteCSR] from r.
// The caller is responsible for verifying the surrounding manifest
// CRC; this function only enforces the structural contract.
func ReadCSR(r io.Reader) (CSRReadback, error) {
	defer metrics.Time("store.snapshot.ReadCSR")()
	br := bufio.NewReader(r)
	var nV, nE uint64
	if err := binary.Read(br, binary.LittleEndian, &nV); err != nil {
		metrics.IncCounter("store.snapshot.ReadCSR.errors", 1)
		return CSRReadback{}, err
	}
	if err := binary.Read(br, binary.LittleEndian, &nE); err != nil {
		metrics.IncCounter("store.snapshot.ReadCSR.errors", 1)
		return CSRReadback{}, err
	}
	flag := make([]byte, 2)
	if _, err := io.ReadFull(br, flag); err != nil {
		metrics.IncCounter("store.snapshot.ReadCSR.errors", 1)
		return CSRReadback{}, err
	}
	hasW := flag[0] == 1
	wsize := flag[1]
	verts := make([]uint64, nV)
	if err := binary.Read(br, binary.LittleEndian, verts); err != nil {
		metrics.IncCounter("store.snapshot.ReadCSR.errors", 1)
		return CSRReadback{}, err
	}
	raw := make([]uint64, nE)
	if err := binary.Read(br, binary.LittleEndian, raw); err != nil {
		metrics.IncCounter("store.snapshot.ReadCSR.errors", 1)
		return CSRReadback{}, err
	}
	edges := make([]graph.NodeID, nE)
	for i, v := range raw {
		edges[i] = graph.NodeID(v)
	}
	var weightBytes []byte
	if hasW {
		weightBytes = make([]byte, int(wsize)*int(nE))
		if _, err := io.ReadFull(br, weightBytes); err != nil {
			metrics.IncCounter("store.snapshot.ReadCSR.errors", 1)
			return CSRReadback{}, err
		}
	}
	return CSRReadback{
		Vertices:    verts,
		Edges:       edges,
		HasWeights:  hasW,
		WeightSize:  wsize,
		WeightBytes: weightBytes,
	}, nil
}

// WriteSnapshotCSR is the legacy high-level helper that lays a
// snapshot directory containing a v1 manifest plus the CSR. It is
// retained for backward compatibility: callers that also need LPG
// label durability must use [WriteSnapshotFull] which writes a v2
// manifest with both csr.bin and labels.bin. Atomic publication is
// achieved by assembling the snapshot under dir + ".tmp" and
// renaming it to dir on success.
func WriteSnapshotCSR[W any](dir string, c *csr.CSR[W]) error {
	defer metrics.Time("store.snapshot.WriteSnapshotCSR")()
	err := WriteSnapshotCSRCtx(context.Background(), dir, c)
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotCSR.errors", 1)
	}
	return err
}

// WriteSnapshotCSRCtx is the context-aware variant of
// [WriteSnapshotCSR]. ctx.Err() is checked at three stage boundaries:
// before the CSR write, before the manifest write, and before the
// atomic rename. On cancellation the temporary staging directory is
// cleaned up and the wrapped ctx.Err is returned.
//
//nolint:gocyclo // snapshot publish: dir prep + CSR write + manifest write + atomic rename + ctx ticks
func WriteSnapshotCSRCtx[W any](ctx context.Context, dir string, c *csr.CSR[W]) error {
	defer metrics.Time("store.snapshot.WriteSnapshotCSRCtx")()
	if err := ctx.Err(); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}
	tmp := dir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}
	csrPath := filepath.Join(tmp, CSRFile)
	f, err := os.Create(csrPath) //nolint:gosec // caller-controlled directory
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}
	size, csum, err := WriteCSR(f, c)
	if err != nil {
		_ = f.Close() // best-effort: already on error path, WriteCSR err preserved
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close() // best-effort: already on error path, sync err preserved
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}
	if err := f.Close(); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}

	m := Manifest{
		Version:   manifestVersionLegacy,
		CreatedAt: time.Now().UTC(),
		Order:     c.Order(),
		Size:      c.Size(),
		Files: []FileEntry{
			{Name: CSRFile, Size: size, CRC32C: csum},
		},
	}
	if err := ctx.Err(); err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}
	manifestPath := filepath.Join(tmp, "manifest.json")
	mf, err := os.Create(manifestPath) //nolint:gosec // caller-controlled directory
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}
	if err := WriteManifest(mf, m); err != nil {
		_ = mf.Close() // best-effort: already on error path, WriteManifest err preserved
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}
	if err := mf.Sync(); err != nil {
		_ = mf.Close() // best-effort: already on error path, sync err preserved
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}
	if err := mf.Close(); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}

	if err := ctx.Err(); err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return fmt.Errorf("snapshot: publish rename: %w", err)
	}
	// Make the rename durable: fsync the parent directory so the
	// new directory entry survives a crash within the journal
	// writeback window. No-op on platforms that lack a directory
	// fsync primitive (Windows).
	if err := parentDirFsync(dir); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return fmt.Errorf("snapshot: publish parent fsync: %w", err)
	}
	return nil
}
