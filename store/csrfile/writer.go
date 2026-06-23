package csrfile

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash"
	"hash/crc32"
	"io"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// WriteToFile serialises c into the path atomically and durably: data
// lands in path + ".tmp" first, the temp file's contents are fsync'd,
// the file is renamed onto path, and finally the PARENT directory is
// fsync'd so the rename's directory entry survives a crash. Concurrent
// readers see either the previous file or the new file, never a partial
// write.
//
// On return with a nil error the published file is crash-durable: it
// survives process crash, host crash, and kill -9. This guarantee
// matters because WriteToFile is the bulk loader's sole durability
// mechanism — the bulk path bypasses the WAL, so there is no replay and
// no later checkpoint of this artefact to recover a lost rename. Without
// the parent-directory fsync, a crash within the kernel's writeback
// window after a successful return could lose the rename's directory
// entry and with it the entire bulk load. The parent fsync is a no-op on
// platforms without a directory-fsync primitive (Windows); see
// [parentDirFsync].
//
// W must be one of the supported weight kinds (int32/uint32/float32
// for 4-byte; int/uint/int64/uint64/float64/uintptr for 8-byte) or
// struct{} for unweighted graphs. Unsupported types produce
// [ErrUnknownWeightKind].
func WriteToFile[W any](path string, c *csr.CSR[W]) (Header, error) {
	return writeToFileWith(osFS{}, path, c)
}

// WriteToFileWith is [WriteToFile] over a caller-supplied filesystem
// backend. It exists for the deterministic-simulation harness
// (internal/sim), which passes an in-memory backend so it can crash
// between any two of the write/fsync/rename/parent-fsync steps and replay
// the result. The backend parameter type is unexported (mirroring
// [github.com/FlavioCFOliveira/GoGraph/store/wal.OpenWith]); production
// code calls [WriteToFile], which supplies the OS backend. Passing the OS
// backend here is byte-for-byte equivalent to [WriteToFile].
func WriteToFileWith[W any](fsys fs, path string, c *csr.CSR[W]) (Header, error) {
	return writeToFileWith(fsys, path, c)
}

// writeToFileWith is the seam-threaded core of [WriteToFile]: every
// filesystem operation goes through fsys. The production caller passes
// [osFS], whose methods delegate verbatim to the os.* calls the function
// used before the seam existed, so the published-file bytes and the
// durability ordering (write -> fsync file -> rename -> fsync parent) are
// unchanged. The deterministic-simulation harness passes an in-memory
// backend so it can crash between any two of those steps.
func writeToFileWith[W any](fsys fs, path string, c *csr.CSR[W]) (Header, error) {
	weightKind, err := weightKindOf[W]()
	if err != nil {
		return Header{}, err
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	if weightKind != WeightAbsent && len(c.WeightsSlice()) == 0 {
		// CSR has no weights at runtime; downgrade to unweighted.
		weightKind = WeightAbsent
	}

	header, total := Layout(uint64(len(verts)), uint64(len(edges)), weightKind)

	tmp := path + ".tmp"
	// Create the temp file mode 0600: the CSR payload contains full
	// edge and weight data, so it must not be world- or group-readable.
	// os.Rename preserves the mode, so the published file is 0600 too.
	f, err := fsys.Create(tmp)
	if err != nil {
		return Header{}, err
	}
	if err := fsys.Truncate(tmp, int64(total)); err != nil {
		_ = f.Close()        // best-effort: already on error path, truncate err preserved
		_ = fsys.Remove(tmp) // best-effort: tmp file cleanup, truncate err preserved
		return Header{}, err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	h := crc32.New(castagnoli)
	tee := io.MultiWriter(bw, h)

	if err := writeSections(tee, h, header, verts, edges, c.WeightsSlice()); err != nil {
		_ = f.Close()        // best-effort: already on error path, writeSections err preserved
		_ = fsys.Remove(tmp) // best-effort: tmp file cleanup, writeSections err preserved
		return Header{}, err
	}

	// Append the trailing CRC32C over every preceding byte.
	if err := binary.Write(bw, binary.LittleEndian, h.Sum32()); err != nil {
		_ = f.Close()        // best-effort: already on error path, CRC write err preserved
		_ = fsys.Remove(tmp) // best-effort: tmp file cleanup, CRC write err preserved
		return Header{}, err
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()        // best-effort: already on error path, flush err preserved
		_ = fsys.Remove(tmp) // best-effort: tmp file cleanup, flush err preserved
		return Header{}, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()        // best-effort: already on error path, sync err preserved
		_ = fsys.Remove(tmp) // best-effort: tmp file cleanup, sync err preserved
		return Header{}, err
	}
	if err := f.Close(); err != nil {
		_ = fsys.Remove(tmp) // best-effort: tmp file cleanup, close err preserved
		return Header{}, err
	}
	if err := fsys.Rename(tmp, path); err != nil {
		_ = fsys.Remove(tmp) // best-effort: tmp file cleanup, rename err preserved
		return Header{}, fmt.Errorf("csrfile: publish rename: %w", err)
	}
	notePublishStep("rename", path)
	// Make the rename durable: fsync the parent directory so the new
	// directory entry survives a crash within the journal writeback
	// window. tmp is created alongside path (path + ".tmp"), so it shares
	// the parent directory and this single post-rename fsync covers both
	// the unlink of tmp and the link of path. No-op on platforms that
	// lack a directory-fsync primitive (Windows); see [parentDirFsync].
	if err := fsys.ParentDirSync(path); err != nil {
		return Header{}, fmt.Errorf("csrfile: publish parent fsync: %w", err)
	}
	notePublishStep("parent-fsync", path)
	return header, nil
}

// writeSections writes the header + each section + padding so the
// next section begins on its required alignment boundary.
func writeSections[W any](w io.Writer, h hash.Hash32, header Header, verts []uint64, edges []graph.NodeID, weights []W) error {
	if _, err := w.Write(EncodeHeader(header)); err != nil {
		return err
	}
	if err := writePadding(w, h, header.VerticesOffset-HeaderSize); err != nil {
		return err
	}
	// Stream the vertex and edge columns through zero-copy little-endian byte
	// views (#1597). Both are 8-byte native-endian words on a little-endian
	// host, so the views are byte-identical to binary.Write(LittleEndian, ...)
	// — but with no transient buffer. In particular the edge column previously
	// paid a full `make([]uint64, len(edges))` no-op widening copy even though
	// graph.NodeID IS uint64.
	if err := streamLE(w, uint64sAsBytes(verts)); err != nil {
		return err
	}
	wrote := header.VerticesOffset + 8*uint64(len(verts))
	if err := writePadding(w, h, header.EdgesOffset-wrote); err != nil {
		return err
	}
	if err := streamLE(w, nodeIDsAsBytes(edges)); err != nil {
		return err
	}
	wrote = header.EdgesOffset + 8*uint64(len(edges))
	if header.Weight != WeightAbsent {
		if err := writePadding(w, h, header.WeightsOffset-wrote); err != nil {
			return err
		}
		if err := binary.Write(w, binary.LittleEndian, weights); err != nil {
			return err
		}
		wrote = header.WeightsOffset + uint64(header.Weight.Size())*uint64(len(edges))
	}
	// Pad up to the CRC trailer offset.
	if err := writePadding(w, h, header.TailCRCOffset-wrote); err != nil {
		return err
	}
	return nil
}

func writePadding(w io.Writer, _ hash.Hash32, n uint64) error {
	if n == 0 {
		return nil
	}
	pad := make([]byte, n)
	_, err := w.Write(pad)
	return err
}

// weightKindOf maps the Go type W to a [WeightKind]. Returns
// [ErrUnknownWeightKind] when W is not one of the supported numeric
// types or struct{}.
func weightKindOf[W any]() (WeightKind, error) {
	var zero W
	switch any(zero).(type) {
	case struct{}:
		return WeightAbsent, nil
	case int32, uint32:
		return WeightUint32, nil
	case float32:
		return WeightFloat32, nil
	case int, uint, int64, uint64, uintptr:
		return WeightUint64, nil
	case float64:
		return WeightFloat64, nil
	}
	return WeightAbsent, ErrUnknownWeightKind
}
