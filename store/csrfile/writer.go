package csrfile

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"

	"gograph/graph"
	"gograph/graph/csr"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// WriteToFile serialises c into the path atomically: data lands in
// path + ".tmp" first, the file is fsync'd, and only then renamed
// onto path. Concurrent readers see either the previous file or the
// new file, never a partial write.
//
// W must be one of the supported weight kinds (int32/uint32/float32
// for 4-byte; int/uint/int64/uint64/float64/uintptr for 8-byte) or
// struct{} for unweighted graphs. Unsupported types produce
// [ErrUnknownWeightKind].
func WriteToFile[W any](path string, c *csr.CSR[W]) (Header, error) {
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
	f, err := os.Create(tmp) //nolint:gosec // caller-supplied path
	if err != nil {
		return Header{}, err
	}
	if err := os.Truncate(tmp, int64(total)); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return Header{}, err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	h := crc32.New(castagnoli)
	tee := io.MultiWriter(bw, h)

	if err := writeSections(tee, h, header, verts, edges, c.WeightsSlice()); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return Header{}, err
	}

	// Append the trailing CRC32C over every preceding byte.
	if err := binary.Write(bw, binary.LittleEndian, h.Sum32()); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return Header{}, err
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return Header{}, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return Header{}, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return Header{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return Header{}, fmt.Errorf("csrfile: publish rename: %w", err)
	}
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
	if err := binary.Write(w, binary.LittleEndian, verts); err != nil {
		return err
	}
	wrote := header.VerticesOffset + 8*uint64(len(verts))
	if err := writePadding(w, h, header.EdgesOffset-wrote); err != nil {
		return err
	}
	tmp := make([]uint64, len(edges))
	for i, e := range edges {
		tmp[i] = uint64(e)
	}
	if err := binary.Write(w, binary.LittleEndian, tmp); err != nil {
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
