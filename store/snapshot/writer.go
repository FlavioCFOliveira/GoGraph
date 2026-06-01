package snapshot

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/bits"
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

// ErrCSRCorrupted is returned by [ReadCSR] when the csr.bin payload is
// structurally malformed: an implausible vertex/edge count, an
// out-of-range weight-element size, or a weights-array byte length that
// overflows. It mirrors the per-component corruption sentinels used by
// the sibling readers ([ErrLabelsCorrupted], [ErrPropertiesCorrupted],
// [ErrMapperCorrupted]) so callers can classify a corrupt CSR the same
// way. [readVerifiedCSR] / [Open] wrap it under [ErrCorrupted].
var ErrCSRCorrupted = errors.New("snapshot: csr.bin corrupted")

// maxCSRCount is the absolute backstop cap on the declared vertex and
// edge counts read from the header. Each vertex and each edge consumes
// at least 8 bytes of input, so a count above this bound cannot
// correspond to a file of any plausible size and is treated as
// corruption. The bound matches the absolute cap the sibling readers
// apply to their per-8-byte-record counts (1<<40 entries ≈ 8 TiB),
// keeping the validation style consistent across the package.
//
// It is the *backstop*, not the primary guard. On every real load path
// the bytes flow through [Open] / [readVerifiedCSR], which pass the
// manifest-recorded file size (FileEntry.Size) to [readCSRLimited] as a
// precise remaining-bytes bound; the count is then rejected the moment
// it exceeds what that many bytes could hold. The backstop only applies
// when a caller invokes the bare exported [ReadCSR] over an io.Reader of
// unknown length, where no manifest size is available.
const maxCSRCount = 1 << 40

// maxInt is the largest value representable by the platform int. It
// bounds the overflow-safe weights-size computation in [ReadCSR] before
// the uint64 -> int conversion that precedes the make().
const maxInt = uint64(^uint(0) >> 1)

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
//
// Untrusted input: a bare ReadCSR over an io.Reader of unknown length
// cannot know the true remaining-bytes bound, so the declared vertex,
// edge, and weight sizes are checked only against an absolute backstop
// cap (see [maxCSRCount]) plus the overflow-safe weights computation.
// That stops an unbounded pre-EOF allocation, but the precise bound
// requires the file size. Callers loading an untrusted snapshot should
// prefer the [Open] / [LoadSnapshotFull] path, which supplies the
// manifest-recorded size (FileEntry.Size) so the count is rejected the
// moment it exceeds what that many bytes could possibly hold; bounding
// a bare reader otherwise remains the caller's responsibility.
func ReadCSR(r io.Reader) (CSRReadback, error) {
	// maxBytes <= 0 selects the absolute backstop cap; the overflow
	// guard still applies. See readCSRLimited.
	return readCSRLimited(r, -1)
}

// readCSRLimited is the structural CSR reader behind [ReadCSR]. It
// rejects implausible vertex/edge/weight sizes BEFORE any make(), so a
// flipped or hostile header byte cannot drive a multi-gigabyte
// allocation before binary.Read even reaches EOF.
//
// maxBytes is the precise upper bound on the bytes the reader may
// legitimately contain — on the real load paths this is the
// manifest-recorded file size (FileEntry.Size) passed by [Open] and
// [readVerifiedCSR]. When maxBytes > 0, each declared count is rejected
// the moment it exceeds what maxBytes bytes could hold (every vertex and
// every edge needs 8 bytes; each weight needs wsize bytes). When
// maxBytes <= 0 (the bare [ReadCSR] entry point, whose reader length is
// unknown) the absolute backstop cap [maxCSRCount] is used instead. The
// effective per-count limit is the smaller of the two so the precise
// bound never relaxes the backstop.
func readCSRLimited(r io.Reader, maxBytes int64) (CSRReadback, error) {
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

	// vertexCap / edgeCap is the largest count the available bytes can
	// hold, computed overflow-safely (never multiply count*8 first). When
	// a manifest size is known we use maxBytes (precise bound); otherwise
	// 8*maxCSRCount (the absolute backstop). Each vertex and each edge
	// needs 8 bytes; the header (18 bytes) is part of the file but
	// counting it against the cap can only make the bound tighter, so we
	// conservatively allow the full byte budget for records. Mirrors the
	// sibling readers' cap checks (ReadLabels/ReadProperties/ReadMapper*).
	byteBudget := uint64(8 * maxCSRCount)
	if maxBytes > 0 && uint64(maxBytes) < byteBudget {
		byteBudget = uint64(maxBytes)
	}
	recordCap := byteBudget / 8 // max vertices or edges these bytes could hold
	if recordCap > maxCSRCount {
		recordCap = maxCSRCount
	}
	if nV > recordCap {
		metrics.IncCounter("store.snapshot.ReadCSR.errors", 1)
		return CSRReadback{}, fmt.Errorf("%w: implausible vertex count %d (max %d for %d bytes)",
			ErrCSRCorrupted, nV, recordCap, byteBudget)
	}
	if nE > recordCap {
		metrics.IncCounter("store.snapshot.ReadCSR.errors", 1)
		return CSRReadback{}, fmt.Errorf("%w: implausible edge count %d (max %d for %d bytes)",
			ErrCSRCorrupted, nE, recordCap, byteBudget)
	}
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
		nbytes, err := weightsByteLen(wsize, nE, maxBytes, byteBudget)
		if err != nil {
			metrics.IncCounter("store.snapshot.ReadCSR.errors", 1)
			return CSRReadback{}, err
		}
		weightBytes = make([]byte, nbytes)
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

// weightsByteLen computes the weights-array byte length overflow-safely
// and bounds it before the make(). wsize is a 0-255 byte and nE is a
// full uint64, so int(wsize)*int(nE) could wrap int to a small or
// negative value, yielding a short buffer (a silent truncation) or a
// make() panic. bits.Mul64 surfaces any overflow in the high word; the
// low word is then bounded against (in order) the precise manifest-size
// budget when known, the absolute backstop byteBudget, and the platform
// int range before the conversion. Any failure returns [ErrCSRCorrupted].
func weightsByteLen(wsize uint8, nE uint64, maxBytes int64, byteBudget uint64) (int, error) {
	hi, lo := bits.Mul64(uint64(wsize), nE)
	if hi != 0 || lo > uint64(maxInt) {
		return 0, fmt.Errorf("%w: weights size overflow: wsize=%d nE=%d",
			ErrCSRCorrupted, wsize, nE)
	}
	// The weights array alone cannot exceed the file's byte budget. When a
	// manifest size is known, byteBudget == maxBytes (the whole file), an
	// even tighter bound than the backstop.
	if maxBytes > 0 && lo > byteBudget {
		return 0, fmt.Errorf("%w: weights bytes %d exceed file budget %d: wsize=%d nE=%d",
			ErrCSRCorrupted, lo, byteBudget, wsize, nE)
	}
	return int(lo), nil
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
	f, err := createSnapshotFile(csrPath)
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
	mf, err := createSnapshotFile(manifestPath)
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
