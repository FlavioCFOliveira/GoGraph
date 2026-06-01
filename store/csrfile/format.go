// Package csrfile defines the on-disk binary format used by
// GoGraph's Tier 2 (out-of-core, mmap-backed) CSR storage.
//
// The format is stable and versioned; the full specification lives
// at docs/csrfile-v1.md alongside this repository. Tier 2 readers
// mmap the file and reinterpret the aligned sections as typed
// []uint64 slices without parsing.
package csrfile

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
)

// Magic is the 4-byte file identifier: ASCII "GGCS".
var Magic = [4]byte{'G', 'G', 'C', 'S'}

// CurrentVersion is the format version emitted by this build.
const CurrentVersion uint16 = 1

// HeaderSize is the fixed byte length of the file header.
const HeaderSize = 64

// Alignment is the section alignment in bytes; every typed section
// starts at an offset that is a multiple of this value.
const Alignment = 64

// WeightKind tags the on-disk type of the weights section.
type WeightKind uint8

// Supported weight kinds.
const (
	WeightAbsent  WeightKind = 0
	WeightUint32  WeightKind = 1
	WeightUint64  WeightKind = 2
	WeightFloat32 WeightKind = 3
	WeightFloat64 WeightKind = 4
)

// Size returns the byte size of one weight value for k.
func (k WeightKind) Size() int {
	switch k {
	case WeightUint32, WeightFloat32:
		return 4
	case WeightUint64, WeightFloat64:
		return 8
	}
	return 0
}

// Errors surfaced by the format helpers.
var (
	// ErrBadMagic indicates the first four bytes are not Magic.
	ErrBadMagic = errors.New("csrfile: bad magic")
	// ErrUnsupportedVersion indicates a newer-than-build version.
	ErrUnsupportedVersion = errors.New("csrfile: unsupported version")
	// ErrUnsupportedByteOrder indicates a non-little-endian file.
	ErrUnsupportedByteOrder = errors.New("csrfile: unsupported byte order")
	// ErrUnknownWeightKind indicates an out-of-range WeightKind.
	ErrUnknownWeightKind = errors.New("csrfile: unknown weight kind")
	// ErrFileCorrupted indicates the tail CRC32C check failed.
	ErrFileCorrupted = errors.New("csrfile: file corrupted")
	// ErrHeaderTooShort indicates a short read while parsing the header.
	ErrHeaderTooShort = errors.New("csrfile: header too short")
	// ErrHeaderInconsistent indicates the decoded header's counts and
	// offsets do not match the single canonical on-disk layout for
	// those counts (a malformed, hostile, or overflowing header). It
	// wraps [ErrFileCorrupted]: errors.Is(err, ErrFileCorrupted) is
	// true for any structural rejection, so callers that already test
	// for ErrFileCorrupted keep working.
	ErrHeaderInconsistent = fmt.Errorf("%w: header layout inconsistent", ErrFileCorrupted)
)

// Header is the in-memory representation of the 64-byte file
// preamble.
type Header struct {
	Version        uint16
	Alignment      uint8
	NVertices      uint64
	NEdges         uint64
	Weight         WeightKind
	VerticesOffset uint64
	EdgesOffset    uint64
	WeightsOffset  uint64
	TailCRCOffset  uint64
}

// AlignUp rounds n up to the next multiple of alignment.
func AlignUp(n, alignment uint64) uint64 {
	mask := alignment - 1
	return (n + mask) &^ mask
}

// mulOverflow returns a*b and whether the multiplication overflowed
// uint64. It is the overflow-safe primitive used by [Layout] when
// the operands originate from untrusted on-disk header fields.
func mulOverflow(a, b uint64) (product uint64, overflowed bool) {
	hi, lo := bits.Mul64(a, b)
	return lo, hi != 0
}

// addOverflow returns a+b and whether the addition overflowed uint64.
func addOverflow(a, b uint64) (sum uint64, overflowed bool) {
	s, carry := bits.Add64(a, b, 0)
	return s, carry != 0
}

// alignUpOverflow rounds n up to the next multiple of alignment,
// reporting overflow. alignment is a power of two (the package-wide
// [Alignment] constant), so the rounding adds at most alignment-1.
func alignUpOverflow(n, alignment uint64) (aligned uint64, overflowed bool) {
	mask := alignment - 1
	s, of := addOverflow(n, mask)
	if of {
		return 0, true
	}
	return s &^ mask, false
}

// Layout computes the on-disk byte offsets and total size for a
// header populated with NVertices, NEdges, and Weight. It returns
// the header with offsets filled and the total file size including
// the trailing CRC32C uint32.
//
// All arithmetic is overflow-safe: when NVertices, NEdges, or the
// derived section sizes would overflow a uint64 (as a hostile or
// corrupted header can request), Layout returns the zero Header and
// a zero totalBytes. Callers — both the writer and [Header.validate]
// — must treat a zero totalBytes as "this header is not representable
// on disk" and reject it rather than proceeding with bogus offsets.
func Layout(nVertices, nEdges uint64, weight WeightKind) (header Header, totalBytes uint64) {
	if weight > WeightFloat64 {
		return Header{}, 0
	}
	header = Header{
		Version:   CurrentVersion,
		Alignment: Alignment,
		NVertices: nVertices,
		NEdges:    nEdges,
		Weight:    weight,
	}

	// VerticesOffset: AlignUp(HeaderSize) — both constants, cannot overflow.
	off := AlignUp(HeaderSize, Alignment)
	header.VerticesOffset = off

	// EdgesOffset = AlignUp(VerticesOffset + 8*NVertices).
	vBytes, of := mulOverflow(8, nVertices)
	if of {
		return Header{}, 0
	}
	off, of = addOverflow(off, vBytes)
	if of {
		return Header{}, 0
	}
	off, of = alignUpOverflow(off, Alignment)
	if of {
		return Header{}, 0
	}
	header.EdgesOffset = off

	// Next offset = AlignUp(EdgesOffset + 8*NEdges).
	eBytes, of := mulOverflow(8, nEdges)
	if of {
		return Header{}, 0
	}
	off, of = addOverflow(off, eBytes)
	if of {
		return Header{}, 0
	}
	off, of = alignUpOverflow(off, Alignment)
	if of {
		return Header{}, 0
	}

	if weight != WeightAbsent {
		header.WeightsOffset = off
		// AlignUp(WeightsOffset + Weight.Size()*NEdges).
		wBytes, wof := mulOverflow(uint64(weight.Size()), nEdges)
		if wof {
			return Header{}, 0
		}
		off, wof = addOverflow(off, wBytes)
		if wof {
			return Header{}, 0
		}
		off, wof = alignUpOverflow(off, Alignment)
		if wof {
			return Header{}, 0
		}
	}
	header.TailCRCOffset = off

	// totalBytes = TailCRCOffset + 4 (the trailing CRC32C uint32).
	totalBytes, of = addOverflow(off, 4)
	if of {
		return Header{}, 0
	}
	return header, totalBytes
}

// EncodeHeader writes h into a fresh HeaderSize-byte slice.
func EncodeHeader(h Header) []byte {
	buf := make([]byte, HeaderSize)
	copy(buf[0:4], Magic[:])
	binary.LittleEndian.PutUint16(buf[4:6], h.Version)
	buf[6] = 0 // little-endian
	buf[7] = h.Alignment
	binary.LittleEndian.PutUint64(buf[8:16], h.NVertices)
	binary.LittleEndian.PutUint64(buf[16:24], h.NEdges)
	buf[24] = uint8(h.Weight)
	// 25..31 reserved (zero)
	binary.LittleEndian.PutUint64(buf[32:40], h.VerticesOffset)
	binary.LittleEndian.PutUint64(buf[40:48], h.EdgesOffset)
	binary.LittleEndian.PutUint64(buf[48:56], h.WeightsOffset)
	binary.LittleEndian.PutUint64(buf[56:64], h.TailCRCOffset)
	return buf
}

// DecodeHeader parses the first HeaderSize bytes into a Header.
func DecodeHeader(buf []byte) (Header, error) {
	if len(buf) < HeaderSize {
		return Header{}, ErrHeaderTooShort
	}
	if buf[0] != Magic[0] || buf[1] != Magic[1] || buf[2] != Magic[2] || buf[3] != Magic[3] {
		return Header{}, ErrBadMagic
	}
	version := binary.LittleEndian.Uint16(buf[4:6])
	if version > CurrentVersion {
		return Header{}, ErrUnsupportedVersion
	}
	if buf[6] != 0 {
		return Header{}, ErrUnsupportedByteOrder
	}
	kind := WeightKind(buf[24])
	if kind > WeightFloat64 {
		return Header{}, ErrUnknownWeightKind
	}
	return Header{
		Version:        version,
		Alignment:      buf[7],
		NVertices:      binary.LittleEndian.Uint64(buf[8:16]),
		NEdges:         binary.LittleEndian.Uint64(buf[16:24]),
		Weight:         kind,
		VerticesOffset: binary.LittleEndian.Uint64(buf[32:40]),
		EdgesOffset:    binary.LittleEndian.Uint64(buf[40:48]),
		WeightsOffset:  binary.LittleEndian.Uint64(buf[48:56]),
		TailCRCOffset:  binary.LittleEndian.Uint64(buf[56:64]),
	}, nil
}

// validate checks that h describes the one canonical on-disk layout
// for its (NVertices, NEdges, Weight) triple and that it exactly fits
// a file of fileLen bytes. It is the single structural-safety gate
// that makes the subsequent zero-copy slice reinterpretation in
// [Reader.bindSlices] provably in-bounds.
//
// The check is deliberately exact rather than a set of inequalities:
// [Layout] is the sole authority on where each section lives, so a
// header is sound only if every decoded offset equals the offset the
// writer would have produced for those counts. Because Layout is
// overflow-safe (it returns a zero totalBytes when the counts cannot
// be represented on disk), a hostile header — including the integer
// overflow case where 8*NEdges wraps to a small value — is rejected
// here with [ErrHeaderInconsistent] before any byte is sliced, rather
// than panicking on an out-of-range slice bound or building an
// unsafe.Slice view that reads past the mapped region.
//
// validate never panics: it performs only equality comparisons on
// already-decoded uint64 fields.
func (h Header) validate(fileLen int) error {
	want, total := Layout(h.NVertices, h.NEdges, h.Weight)
	if total == 0 {
		// Layout signalled an unrepresentable / overflowing header.
		return fmt.Errorf("%w: counts not representable (NVertices=%d NEdges=%d weight=%d)",
			ErrHeaderInconsistent, h.NVertices, h.NEdges, h.Weight)
	}
	if h.VerticesOffset != want.VerticesOffset ||
		h.EdgesOffset != want.EdgesOffset ||
		h.WeightsOffset != want.WeightsOffset ||
		h.TailCRCOffset != want.TailCRCOffset {
		return fmt.Errorf("%w: offsets %d/%d/%d/%d, canonical %d/%d/%d/%d",
			ErrHeaderInconsistent,
			h.VerticesOffset, h.EdgesOffset, h.WeightsOffset, h.TailCRCOffset,
			want.VerticesOffset, want.EdgesOffset, want.WeightsOffset, want.TailCRCOffset)
	}
	if uint64(total) != uint64(fileLen) {
		return fmt.Errorf("%w: file size %d, header layout requires %d",
			ErrHeaderInconsistent, fileLen, total)
	}
	return nil
}
