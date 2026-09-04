package csrfile

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ErrPublishedNotDurable reports that the publish RENAME already succeeded — the
// new generation is visible and the previous one is gone — but the parent
// directory fsync that makes that rename survive a crash did not.
//
// It exists because the two failure modes were indistinguishable from the
// caller's side (rmp #2580's sibling, rmp #2581). Every earlier step's failure
// leaves the previous generation intact and the temp file removed, so an error
// from [WriteToFile] read naturally as "not published". A parent-fsync failure
// returned an error over a state where publication HAD occurred, and a caller
// that reacted by assuming the old file survived would be wrong.
//
// Wrap-and-return is the deliberate choice among the two shapes prior art takes:
// RocksDB returns the survivable error (file/filename.cc, v9.7.3), while
// PostgreSQL escalates a failed fsync to PANIC rather than let a caller carry on
// (src/backend/storage/file/fd.c, REL_17_STABLE), on the reasoning that a LATER
// fsync may falsely report success once the kernel has dropped the dirty page.
// GoGraph is an embedded library and cannot take the process down on its
// embedder's behalf, so it returns — but it returns something the embedder can
// TELL APART, and documents that retrying the fsync, or failing the process, are
// the two sound responses. Treating it as "not published" is not one of them.
//
// Test for it with errors.Is; the underlying filesystem error is wrapped and
// remains reachable.
var ErrPublishedNotDurable = errors.New("csrfile: published but durability unproven")

// WriteToFile serialises c into the path atomically and durably: data
// lands in path + ".tmp" first, the temp file's contents are fsync'd,
// the file is renamed onto path, and finally the PARENT directory is
// fsync'd so the rename's directory entry survives a crash. Concurrent
// readers see either the previous file or the new file, never a partial
// write.
//
// On return with a NIL error the published file's CONTENTS are durable
// everywhere — the temp file is fsync'd before the rename — and on
// linux/darwin/freebsd/netbsd/openbsd the rename's DIRECTORY ENTRY is durable
// too, so the publication survives process crash, host crash and kill -9.
//
// # The platform scope of that sentence (rmp #2582)
//
// Outside that build set [parentDirFsync] is an unconditional no-op, so this
// function performs no barrier after the rename and the durability of the
// directory ENTRY is not established by anything GoGraph does. What happens to
// it is then a property of the filesystem, and this godoc deliberately makes no
// claim either way: the audit that raised this had inferred a Windows answer
// from the absent barrier rather than measuring one, and found no normative
// documentation on NTFS journal-commit ordering with respect to MoveFileEx.
// Stating where the barrier IS is a fact; stating what other platforms do
// without one would be a guess.
//
// Process crash and kill -9 are unaffected by the platform: the rename is a
// single kernel operation and survives the death of the process that issued it
// everywhere.
//
// On return with a NON-NIL error, which of the two states holds depends on the
// error, and the caller must not guess:
//
//   - errors.Is(err, [ErrPublishedNotDurable]) — the rename SUCCEEDED. The new
//     generation is visible and the previous one is gone; only the parent
//     directory fsync failed, so the rename may not survive a crash. Retry the
//     fsync or fail the process; do NOT assume the previous file survived.
//   - any other error — nothing was published. The previous generation is intact
//     and the temp file has been removed.
//
// This guarantee
// matters because WriteToFile is the bulk loader's sole durability
// mechanism — the bulk path bypasses the WAL, so there is no replay and
// no later checkpoint of this artefact to recover a lost rename. Without
// the parent-directory fsync, a crash within the kernel's writeback
// window after a successful return could lose the rename's directory
// entry and with it the entire bulk load. The parent fsync is a no-op on
// platforms without a directory-fsync primitive (Windows); see
// [parentDirFsync].
//
// W must be one of the supported weight kinds, which are the same set
// store/snapshot persists so a weight type accepted by one durable path is
// accepted by the other (rmp #2529):
//
//	struct{}                       unweighted, 0 bytes
//	int8, uint8, bool              1 byte
//	int16, uint16                  2 bytes
//	int32, uint32, float32         4 bytes
//	int, uint, uintptr             8 bytes — see the note below
//	int64, uint64, float64         8 bytes
//
// Any other type produces an error wrapping [ErrUnknownWeightKind] and NAMING
// the type, so the limit is discoverable without reading this list.
//
// A CSR whose section counts have no on-disk representation produces an error
// wrapping [ErrNotRepresentable], and nothing is written. This is unreachable
// for any CSR that fits in memory — it needs section counts on the order of
// 2^61 — but it is checked rather than assumed, because [Layout] requires every
// caller to check and proceeding would panic rather than write a bad file
// (rmp #2744); see [layoutForWrite].
//
// int, uint and uintptr are PLATFORM-DEPENDENT widths, persisted at 8 bytes
// deliberately. They are 8 bytes on every platform GoGraph builds for today and
// store/snapshot already made this choice, so the two formats agree. The cost is
// stated rather than hidden: such a file, written on a 64-bit build, would be
// misread by a 32-bit one. Use an explicitly-sized weight type for a file that
// must cross word sizes.
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

	header, total, err := layoutForWrite(uint64(len(verts)), uint64(len(edges)), weightKind)
	if err != nil {
		return Header{}, err
	}

	tmp := path + ".tmp"
	// Create the temp file mode 0600: the CSR payload contains full
	// edge and weight data, so it must not be world- or group-readable.
	// os.Rename preserves the mode, so the published file is 0600 too.
	f, err := fsys.Create(tmp)
	if err != nil {
		return Header{}, err
	}
	// On the DESCRIPTOR, not the path (rmp #2580). A path-based os.Truncate here
	// re-resolved a predictable name moments after the create, which is a TOCTOU
	// window an attacker who can write the directory could step into.
	//nolint:gosec // G115: total comes from layoutForWrite, which fail-stops above on Layout's zero-totalBytes signal, so total is at least HeaderSize+4 here; reaching 1<<63 needs ~2^60 in-memory elements, and a negative length fail-stops on Truncate's EINVAL
	if err := f.Truncate(int64(total)); err != nil {
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
		// THE RENAME ALREADY HAPPENED. The new generation is visible and the
		// previous one is gone, so this error must not be read as "not published"
		// (rmp #2581). ErrPublishedNotDurable is what lets a caller tell this
		// apart from every earlier failure, each of which leaves the previous
		// generation intact.
		return Header{}, fmt.Errorf("%w: parent fsync of %q: %w", ErrPublishedNotDurable, path, err)
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
		// The raw view, not binary.Write, which refuses int/uint/uintptr/bool
		// (rmp #2529). Byte-identical on a little-endian host for every kind that
		// already worked.
		if err := streamLE(w, weightsAsBytes(weights, header.Weight.Size())); err != nil {
			return err
		}
		//nolint:gosec // G115: WeightKind.Size (format.go:61) is a total switch returning only 0,1,2,4,8, so the int is non-negative and the conversion is exact
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
	case int8, uint8, bool:
		return WeightUint8, nil
	case int16, uint16:
		return WeightUint16, nil
	case int32, uint32:
		return WeightUint32, nil
	case float32:
		return WeightFloat32, nil
	case int, uint, int64, uint64, uintptr:
		// PLATFORM-DEPENDENT WIDTHS, persisted at 8 bytes DELIBERATELY (rmp
		// #2529). int, uint and uintptr are 8 bytes on every platform GoGraph
		// builds for today, and store/snapshot already made the same choice, so
		// the two durable paths accept the same set. The cost is stated rather
		// than hidden: a csrfile carrying these weights, written on a 64-bit
		// build, would be misread by a 32-bit one. A caller who needs a file that
		// crosses word sizes should use an explicitly-sized weight type.
		return WeightUint64, nil
	case float64:
		return WeightFloat64, nil
	}
	return WeightAbsent, fmt.Errorf("%w: %T", ErrUnknownWeightKind, zero)
}
