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

// Layout computes the on-disk byte offsets and total size for a
// header populated with NVertices, NEdges, and Weight. It returns
// the header with offsets filled and the total file size including
// the trailing CRC32C uint32.
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
	off := AlignUp(HeaderSize, Alignment)
	header.VerticesOffset = off
	off = AlignUp(off+8*nVertices, Alignment)
	header.EdgesOffset = off
	off = AlignUp(off+8*nEdges, Alignment)
	if weight != WeightAbsent {
		header.WeightsOffset = off
		off = AlignUp(off+uint64(weight.Size())*nEdges, Alignment)
	}
	header.TailCRCOffset = off
	totalBytes = off + 4
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
