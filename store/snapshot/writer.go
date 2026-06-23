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

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
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
// corruption.
//
// It is set to 1<<34 (≈ 1.7e10 records ⇒ a ≈ 128 GiB vertex or edge
// array): far beyond any legitimate single in-memory CSR — a graph with
// 17 billion vertices is not one this engine builds — yet low enough that
// the bare backstop alone cannot let a hostile header drive a multi-TiB
// make() on the unbounded entry point. Real reference engines
// (RocksDB/LMDB) never expose an unbounded parse entry point; this tight
// ceiling is the bare reader's analogue of their recorded-extent bound.
//
// It remains the *backstop*, not the primary guard. On every real load
// path the bytes flow through [Open] / [readVerifiedCSR], which pass the
// manifest-recorded file size (FileEntry.Size) to [readCSRLimited] as a
// precise remaining-bytes bound; the effective per-count limit is the
// smaller of that precise bound and this backstop, so lowering the
// backstop only ever tightens the real path, never relaxes it. The
// backstop alone applies when a caller invokes the bare exported
// [ReadCSR] over an io.Reader of unknown length, where no manifest size
// is available.
const maxCSRCount = 1 << 34

// maxInt is the largest value representable by the platform int. It
// bounds the overflow-safe weights-size computation in [ReadCSR] before
// the uint64 -> int conversion that precedes the make().
const maxInt = uint64(^uint(0) >> 1)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// capHint returns a safe initial capacity for an append-grown slice whose
// element count is an untrusted, validated-but-large header value. It is
// min(count, maxCap): the count has already been rejected against the
// decoder's implausibility ceiling, and clamping the eager reservation to
// maxCap means a hostile-but-under-ceiling count cannot drive a multi-gigabyte
// make() before the per-record reads fail on a truncated body. The append loop
// then grows the slice to the true count for a legitimate file, costing only a
// few re-grows beyond maxCap. Shared by the snapshot record decoders
// (ReadLabels / ReadProperties / ReadMapper* / readEdgeHandleStrTable),
// mirroring the inline clamp tombstones.go and edgehandles.go already apply.
func capHint(count uint64, maxCap int) int {
	if count < uint64(maxCap) {
		return int(count)
	}
	return maxCap
}

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
//	uint8  hasHandles      (OPTIONAL trailing block; 1 = handle array present)
//	[handles]             (nEdges * 8 bytes, when hasHandles = 1)
//
// The trailing handles block (Stage 2 of the stable-edge-handle work) is
// emitted ONLY when the source CSR carries a per-slot handle column
// ([csr.CSR.HandlesSlice] != nil). A graph that never used AddEdgeH produces
// no trailing block, so its csr.bin is byte-identical to one written before
// this column existed — the v1 golden and the cross-process byte-equality
// fixtures are unaffected. [readCSRLimited] detects the block by attempting
// to read one more byte after the weights array: present → handles follow,
// EOF → none (the backward-compatible read branch).
func WriteCSR[W any](w io.Writer, c *csr.CSR[W]) (size int64, crc uint32, err error) {
	defer metrics.Time("store.snapshot.WriteCSR")()
	bw := bufio.NewWriterSize(w, 1<<20)
	hasher := crc32.New(castagnoli)
	tee := io.MultiWriter(bw, hasher)

	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	weights := c.WeightsSlice()
	handles := c.HandlesSlice()

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
	// Vertices, edges, weights and handles are streamed to the tee as their
	// raw little-endian byte views in bounded csrWriteChunk-sized slices,
	// instead of via binary.Write. The prior path widened edges into a whole
	// `tmp []uint64` copy (graph.NodeID IS uint64, so the copy was a no-op
	// widening) and let binary.Write's fast path allocate a make([]byte, 8*len)
	// scratch buffer per whole-slice call — together ~1.92x the payload of
	// transient heap at checkpoint time. Reinterpreting each native slice as
	// bytes (see csr_codec_bytes.go; the on-disk format is little-endian and
	// the engine runs only on little-endian hosts) and streaming in 64 KiB
	// chunks keeps the writer's working set O(chunk). The byte stream — and
	// therefore the on-disk layout and the tee'd CRC32C — is byte-identical to
	// the prior binary.Write output, which the v1 golden fixture and the CSR
	// cross-process byte-equality tests pin.
	if err := streamLE(tee, uint64sAsBytes(verts)); err != nil {
		metrics.IncCounter("store.snapshot.WriteCSR.errors", 1)
		return 0, 0, err
	}
	if err := streamLE(tee, nodeIDsAsBytes(edges)); err != nil {
		metrics.IncCounter("store.snapshot.WriteCSR.errors", 1)
		return 0, 0, err
	}
	if hasW == 1 {
		if err := streamLE(tee, weightsAsBytes(weights, int(wsize))); err != nil {
			metrics.IncCounter("store.snapshot.WriteCSR.errors", 1)
			return 0, 0, err
		}
	}
	// Optional trailing handle block. Emitted only when the CSR carries a
	// handle column; absent otherwise so a handle-less graph's bytes match
	// the pre-Stage-2 layout exactly.
	handleBytes := int64(0)
	if handles != nil {
		if _, err := tee.Write([]byte{1}); err != nil {
			metrics.IncCounter("store.snapshot.WriteCSR.errors", 1)
			return 0, 0, err
		}
		if err := streamLE(tee, uint64sAsBytes(handles)); err != nil {
			metrics.IncCounter("store.snapshot.WriteCSR.errors", 1)
			return 0, 0, err
		}
		handleBytes = 1 + 8*int64(len(handles))
	}
	if err := bw.Flush(); err != nil {
		metrics.IncCounter("store.snapshot.WriteCSR.errors", 1)
		return 0, 0, err
	}
	total := int64(8+8+2+8*len(verts)+8*len(edges)) + int64(int(wsize))*int64(len(edges)) + handleBytes
	return total, hasher.Sum32(), nil
}

// csrWeightSize returns the size in bytes of one weight value, or 0
// when W is struct{} (unweighted). For unsupported weight types we
// return 0 and rely on the writer to skip the weights section.
//
// The int / uint / uintptr cases report 8 bytes, matching the read-side
// decode in [decodeCSRWeight]. The prior writer serialised weights with
// binary.Write, which rejects these variable-width Go types with "some
// values are not fixed-sized" — so a CSR with int/uint/uintptr weights was
// never persistable (WriteCSR returned an error). Since [WriteCSR] now
// streams the raw 8-byte little-endian view ([weightsAsBytes]), these
// weights round-trip correctly through the symmetric decode. This is a
// strictly additive capability: no pre-existing on-disk file can contain an
// int/uint/uintptr weights section, so there is no migration concern and the
// byte layout for every previously-writable weight type is unchanged.
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
	// Handles is the optional per-slot stable-edge-handle column, aligned
	// slot-for-slot with Edges (handles[i] is the stable handle of the edge
	// at edges[i]). It is nil when the snapshot predates the column or its
	// source graph carried no handles; when non-nil it has the same length
	// as Edges. See the trailing-block note on [WriteCSR].
	Handles []uint64
}

// ReadCSR parses a CSR previously written by [WriteCSR] from r.
// The caller is responsible for verifying the surrounding manifest
// CRC; this function only enforces the structural contract.
//
// Untrusted input: a bare ReadCSR over an io.Reader of unknown length
// cannot know the true remaining-bytes bound, so the declared vertex,
// edge, and weight sizes are checked only against the absolute backstop
// cap [maxCSRCount] plus the overflow-safe weights computation. That cap
// is deliberately tight (1<<34 records ⇒ a ≈ 128 GiB array): it bounds
// the worst-case eager reservation on this entry point well below the
// multi-TiB an unbounded ceiling would permit, while still admitting any
// CSR this engine could legitimately produce. It is, however, only a
// backstop, not a precise bound — that requires the file size. Callers
// loading an untrusted snapshot should prefer the [Open] /
// [LoadSnapshotFull] path, which supplies the manifest-recorded size
// (FileEntry.Size) so the count is rejected the moment it exceeds what
// that many bytes could possibly hold; bounding a bare reader more
// tightly than the backstop otherwise remains the caller's
// responsibility.
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
	// Edges are read directly into the destination []graph.NodeID via its
	// raw byte view — graph.NodeID IS uint64 (graph/graph.go), so the prior
	// `raw []uint64` + per-element copy into a second `edges []graph.NodeID`
	// array was a pure widening no-op that doubled the readback's transient
	// working set (an extra whole edge column). Reading the little-endian
	// bytes straight into the final slice mirrors the established mmap
	// precedent in store/csrfile (Reader.bindSlices): the on-disk format is
	// explicitly little-endian and the engine runs only on little-endian
	// hosts, so the raw bytes land as native uint64s with no byte-swap and no
	// alignment hazard — a []graph.NodeID from make is always 8-byte aligned.
	//
	// The edge count nE was already rejected against the validated byte budget
	// (recordCap) ABOVE, before this allocation — the OOM / decoder-bomb guard
	// from the 2026-06-14 security audits is preserved: no make() here can be
	// driven past what the file's bytes could hold.
	edges := make([]graph.NodeID, nE)
	if _, err := io.ReadFull(br, nodeIDsAsBytes(edges)); err != nil {
		metrics.IncCounter("store.snapshot.ReadCSR.errors", 1)
		return CSRReadback{}, err
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
	// Optional trailing handle block. Peek one byte: EOF means the snapshot
	// predates the column (the backward-compatible read branch); a 1 byte
	// means a handle array of exactly nE uint64s follows. Any other flag
	// byte is a corrupt frame. The handle count equals the edge count, which
	// readCSRLimited already bounded against the byte budget, so no extra cap
	// check is needed.
	var handles []uint64
	flagByte, perr := br.ReadByte()
	switch {
	case errors.Is(perr, io.EOF):
		// No trailing block: handle-less snapshot. Leave handles nil.
	case perr != nil:
		metrics.IncCounter("store.snapshot.ReadCSR.errors", 1)
		return CSRReadback{}, perr
	case flagByte == 1:
		handles = make([]uint64, nE)
		if err := binary.Read(br, binary.LittleEndian, handles); err != nil {
			metrics.IncCounter("store.snapshot.ReadCSR.errors", 1)
			return CSRReadback{}, err
		}
	default:
		metrics.IncCounter("store.snapshot.ReadCSR.errors", 1)
		return CSRReadback{}, fmt.Errorf("%w: bad handle-block flag %#x", ErrCSRCorrupted, flagByte)
	}
	return CSRReadback{
		Vertices:    verts,
		Edges:       edges,
		HasWeights:  hasW,
		WeightSize:  wsize,
		WeightBytes: weightBytes,
		Handles:     handles,
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
func WriteSnapshotCSRCtx[W any](ctx context.Context, dir string, c *csr.CSR[W]) error {
	return writeSnapshotCSRCtxWith(ctx, osBackend{}, dir, c)
}

// writeSnapshotCSRCtxWith is the filesystem-seam implementation behind
// [WriteSnapshotCSRCtx]: every filesystem operation of the legacy CSR-only
// snapshot publish routes through fsys, so the OS backend reproduces the
// historical bytes and durability ordering exactly while the simulator can
// supply an in-memory disk.
//
//nolint:gocyclo // snapshot publish: dir prep + CSR write + manifest write + atomic rename + ctx ticks
func writeSnapshotCSRCtxWith[W any](ctx context.Context, fsys fileSystem, dir string, c *csr.CSR[W]) error {
	defer metrics.Time("store.snapshot.WriteSnapshotCSRCtx")()
	if err := ctx.Err(); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}
	if err := fsys.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}
	tmp := dir + ".tmp"
	if err := fsys.RemoveAll(tmp); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}
	if err := fsys.MkdirAll(tmp, 0o750); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}
	csrPath := filepath.Join(tmp, CSRFile)
	f, err := fsys.Create(csrPath)
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
		_ = fsys.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}
	manifestPath := filepath.Join(tmp, "manifest.json")
	mf, err := fsys.Create(manifestPath)
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
		_ = fsys.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return err
	}
	// Make the staging directory's own inode durable BEFORE the publish
	// rename so the dirents linking csr.bin and manifest.json into it
	// survive a crash even on a filesystem that does not flush a renamed
	// directory's child dirents as part of the rename. Mirrors the
	// canonical crash-safe ordering documented on the v2/v3 path
	// ([writeSnapshotFullCore]): write+fsync components -> fsync staging
	// dir -> rename -> fsync parent. No-op on Windows (see [dirFsync]).
	if err := fsys.DirSync(tmp); err != nil {
		_ = fsys.RemoveAll(tmp) // best-effort: staging cleanup, fsync err preserved
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return fmt.Errorf("snapshot: staging dir fsync: %w", err)
	}
	notePublishStep("staging-fsync", tmp)
	// Crash-atomic publish. Mirrors the v2/v3 path (writeSnapshotFullCore):
	// the previous RemoveAll(dir) -> Rename(tmp, dir) sequence left NO live
	// snapshot on disk if a crash hit between the two calls, and — with the
	// WAL already truncated by an earlier checkpoint — recovery would
	// silently rebuild an empty graph. Archive the live snapshot to
	// dir+".bak", rename the staging directory into place, and drop the
	// backup only after the publish rename has been made durable, so at
	// every instant at least one complete snapshot exists on disk. Recovery
	// promotes a stranded backup back to the live name (see store/recovery).
	bak := dir + ".bak"
	// Clean up a stale backup from a prior interrupted publish (idempotent;
	// recovery may already have promoted or discarded it).
	_ = fsys.RemoveAll(bak) // best-effort: stale backup cleanup
	notePublishStep("archive", bak)
	// Atomically archive the current live snapshot. When dir does not yet
	// exist (first checkpoint), Rename fails with os.ErrNotExist — fine.
	if err := fsys.Rename(dir, bak); err != nil && !errors.Is(err, os.ErrNotExist) {
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return fmt.Errorf("snapshot: archive live snapshot: %w", err)
	}
	notePublishStep("rename", tmp)
	if err := fsys.Rename(tmp, dir); err != nil {
		// Restore: undo the archive so the caller retries against an
		// intact live snapshot.
		_ = fsys.Rename(bak, dir) // best-effort: archive restore, rename err preserved
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return fmt.Errorf("snapshot: publish rename: %w", err)
	}
	// Make the rename durable: fsync the parent directory so the
	// new directory entry survives a crash within the journal
	// writeback window. No-op on platforms that lack a directory
	// fsync primitive (Windows).
	if err := fsys.ParentDirSync(dir); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotCSRCtx.errors", 1)
		return fmt.Errorf("snapshot: publish parent fsync: %w", err)
	}
	// Drop the backup only AFTER the parent-dir fsync: a crash after the
	// publish rename but before the fsync may lose the new dirent, and the
	// backup is then the only surviving copy of the previous snapshot.
	_ = fsys.RemoveAll(bak) // best-effort: happy-path backup cleanup
	return nil
}
