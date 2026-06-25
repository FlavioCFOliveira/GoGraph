// Package wal implements a versioned, length-prefixed,
// CRC32C-checksummed Write-Ahead Log for the gograph durability
// stack.
//
// The on-disk format is documented in FORMAT.md alongside this
// package. Each frame is self-describing; readers stop cleanly at
// the first torn or corrupted frame and report the byte offset
// where the cut occurred, leaving the file otherwise untouched.
package wal

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// Magic is the 4-byte identifier prefix of every WAL frame: ASCII
// "GGWA".
var Magic = [4]byte{'G', 'G', 'W', 'A'}

// CurrentVersion is the WAL format version this package writes.
// Readers must accept all versions <= CurrentVersion; older versions
// are intentionally permitted so a fresh build can replay archives
// produced by previous releases.
const CurrentVersion uint16 = 1

// HeaderSize is the fixed number of bytes occupying the frame header
// (magic + version + length + crc32c).
const HeaderSize = 4 + 2 + 4 + 4

// maxFrameSize is the largest payload, in bytes, that [Decode] will
// allocate for a single frame. The frame's length field is a uint32,
// so the on-disk format already bounds a payload to ~4 GiB; this 1 GiB
// ceiling is a defence-in-depth (INFO finding I2) that caps the
// pathological case where a corrupted or crafted length field forces a
// large one-shot allocation before the CRC has had a chance to reject
// the frame. The ceiling is set well above any legitimate frame: WAL
// payloads carry single transactions, not bulk data, so 1 GiB cannot
// reject valid data — and a false rejection of a legitimately-large
// frame would be a worse failure than the allocation it guards against.
const maxFrameSize = 1 << 30

// Errors returned by the reader.
var (
	// ErrBadMagic indicates the next four bytes did not match Magic.
	ErrBadMagic = errors.New("wal: bad frame magic")
	// ErrUnsupportedVersion indicates the frame version is newer
	// than this build knows how to parse.
	ErrUnsupportedVersion = errors.New("wal: unsupported frame version")
	// ErrCRCMismatch indicates the frame's CRC32C did not match the
	// re-computed value.
	ErrCRCMismatch = errors.New("wal: crc32c mismatch")
	// ErrTornFrame indicates the underlying reader returned EOF
	// before the frame was fully read.
	ErrTornFrame = errors.New("wal: torn frame at end of input")
	// ErrTornFrameMasksData indicates a frame's declared payload length
	// over-declared past the end of input AND the bytes it would have
	// consumed contain at least one further valid (CRC-checking) frame.
	// This is genuine mid-stream corruption masquerading as a benign torn
	// tail: a corrupt length field swallowed durable frames that follow it.
	// Unlike [ErrTornFrame] (a benign final partial write), this is a hard
	// error — it MUST fail-stop so the durable frames the bad length hid are
	// never silently dropped. It is a DISTINCT sentinel (it deliberately does
	// not wrap [ErrTornFrame]) so recovery's corruption classifier treats it
	// as corruption rather than a benign tail.
	ErrTornFrameMasksData = errors.New("wal: torn frame hides later valid frames (corrupt length)")
	// ErrFrameTooLarge indicates the frame's declared payload length
	// exceeds maxFrameSize. A length field this large is treated as
	// corruption: the frame is rejected before any allocation, so a
	// crafted or corrupted length cannot force a large one-shot make.
	ErrFrameTooLarge = errors.New("wal: frame payload length exceeds maximum")
)

// castagnoli holds the precomputed CRC32C table used by every
// encode and decode. The polynomial is 0x1EDC6F41.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Frame is the in-memory representation of one WAL frame.
type Frame struct {
	Version uint16
	Payload []byte
}

// Encode writes f to w as a single binary frame. It returns the
// number of bytes written and any underlying writer error.
func Encode(w io.Writer, f Frame) (int, error) {
	defer metrics.Time("store.wal.Encode").Stop()
	if f.Version == 0 {
		f.Version = CurrentVersion
	}
	plen := uint32(len(f.Payload))

	// Build the 14-byte header on the stack — no per-frame heap allocation.
	// The frame stream is written as two contiguous Writes (header then
	// payload) instead of one concatenated buffer; the on-disk bytes are
	// byte-for-byte identical because the header layout and the CRC input
	// are unchanged (see below). The previous implementation allocated a
	// fresh HeaderSize+len(payload) slice per frame and copied the payload
	// into it; that allocation and copy are removed here (#1509).
	var header [HeaderSize]byte
	copy(header[0:4], Magic[:])
	binary.LittleEndian.PutUint16(header[4:6], f.Version)
	binary.LittleEndian.PutUint32(header[6:10], plen)
	// CRC is over magic+version+length+payload — the 4 crc bytes at
	// header[10:14] are NOT part of the input, exactly as before. Computing
	// it incrementally over header[0:10] then the payload reproduces the
	// identical checksum the single-buffer path produced.
	crc := crc32.Update(0, castagnoli, header[0:10])
	crc = crc32.Update(crc, castagnoli, f.Payload)
	binary.LittleEndian.PutUint32(header[10:14], crc)

	// Write the header, then the payload. bufio.Writer (the production
	// sink in Writer.Append) copies each Write into its internal buffer
	// synchronously before returning, so the caller's payload slice is
	// fully consumed when Encode returns — this is what makes the pooled
	// txn-layer scratch buffer safe to reuse for the next op (#1509).
	nh, err := w.Write(header[:])
	if err != nil {
		metrics.IncCounter("store.wal.Encode.errors", 1)
		return nh, err
	}
	np, err := w.Write(f.Payload)
	if err != nil {
		metrics.IncCounter("store.wal.Encode.errors", 1)
	}
	return nh + np, err
}

// Decode reads the next frame from r. It returns ErrTornFrame when
// the reader ends mid-frame (clean tail truncation), ErrBadMagic on
// a missing magic, ErrUnsupportedVersion on a newer-than-supported
// version, and ErrCRCMismatch on integrity failure. Any other error
// is propagated from the underlying reader.
func Decode(r io.Reader) (Frame, error) {
	defer metrics.Time("store.wal.Decode").Stop()
	var head [HeaderSize]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			metrics.IncCounter("store.wal.Decode.errors", 1)
			return Frame{}, ErrTornFrame
		}
		metrics.IncCounter("store.wal.Decode.errors", 1)
		return Frame{}, err
	}
	if head[0] != Magic[0] || head[1] != Magic[1] || head[2] != Magic[2] || head[3] != Magic[3] {
		metrics.IncCounter("store.wal.Decode.errors", 1)
		return Frame{}, ErrBadMagic
	}
	version := binary.LittleEndian.Uint16(head[4:6])
	if version > CurrentVersion {
		metrics.IncCounter("store.wal.Decode.errors", 1)
		return Frame{}, ErrUnsupportedVersion
	}
	plen := binary.LittleEndian.Uint32(head[6:10])
	expectCRC := binary.LittleEndian.Uint32(head[10:14])

	// Reject an implausibly large length before allocating. plen is a
	// uint32, so the format already caps a payload at ~4 GiB; this guard
	// tightens that to maxFrameSize (1 GiB) so a corrupted or crafted
	// length cannot force a large one-shot allocation ahead of the CRC
	// check below. See maxFrameSize for the rationale.
	if plen > maxFrameSize {
		metrics.IncCounter("store.wal.Decode.errors", 1)
		return Frame{}, ErrFrameTooLarge
	}

	payload := make([]byte, plen)
	if plen > 0 {
		if n, err := io.ReadFull(r, payload); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				metrics.IncCounter("store.wal.Decode.errors", 1)
				// The payload read ran short of the declared length, hitting
				// the end of input. This is normally a benign torn tail: the
				// writer crashed mid-write of the last frame. But a corrupted
				// length field that OVER-declares past EOF produces the same
				// EOF — and in that case the bytes the over-long read consumed
				// (payload[:n]) are not opaque payload at all, they are the
				// durable frame(s) that physically follow this one. If a valid,
				// CRC-checking frame begins anywhere inside those consumed
				// bytes, this "torn" frame is genuine mid-stream corruption
				// that would otherwise silently swallow durable committed data.
				// Promote it to a hard error so recovery fail-stops instead of
				// accepting a truncated prefix as clean. A benign tail's opaque
				// payload bytes do not form a CRC-valid frame except with the
				// ~2^-32 per-offset probability of a CRC collision, so a true
				// torn tail is not misclassified.
				if embedsValidFrame(payload[:n]) {
					metrics.IncCounter("store.wal.Decode.tornMasksData", 1)
					return Frame{}, ErrTornFrameMasksData
				}
				return Frame{}, ErrTornFrame
			}
			metrics.IncCounter("store.wal.Decode.errors", 1)
			return Frame{}, err
		}
	}
	gotCRC := crc32.Update(0, castagnoli, head[0:10])
	gotCRC = crc32.Update(gotCRC, castagnoli, payload)
	if gotCRC != expectCRC {
		metrics.IncCounter("store.wal.Decode.errors", 1)
		return Frame{}, ErrCRCMismatch
	}
	return Frame{Version: version, Payload: payload}, nil
}

// embedsValidFrame reports whether buf contains, at any byte offset, the start
// of a structurally complete and CRC-valid WAL frame. It is the discriminator
// used by [Decode] to tell a benign torn tail (the writer crashed mid-write of
// the final frame, so buf holds only that frame's opaque partial payload) from
// genuine corruption (a frame's length field over-declared past EOF, so the
// over-long read consumed the durable frames that physically follow it, and buf
// holds those frames' bytes).
//
// The check is deliberately strict: a candidate frame must have the magic, a
// supported version, a length that fits entirely within buf, AND a CRC32C that
// matches the bytes it covers. The CRC match is the load-bearing signal — it is
// what makes a false positive (opaque payload bytes accidentally read as a
// frame) a ~2^-32 per-offset event rather than a structural near-certainty.
//
// The scan offset advances one byte at a time because the start of the swallowed
// frame sits at the true (now-unknown) end of the corrupt frame's real payload,
// which need not be aligned to any boundary the reader can compute from the
// corrupt header. The scan is bounded by len(buf) and runs only on the torn
// path (once, during recovery), so its O(len(buf)) cost is not on any hot path.
func embedsValidFrame(buf []byte) bool {
	// A frame needs at least a full header plus the CRC bytes to be verifiable.
	for off := 0; off+HeaderSize <= len(buf); off++ {
		if buf[off] != Magic[0] || buf[off+1] != Magic[1] ||
			buf[off+2] != Magic[2] || buf[off+3] != Magic[3] {
			continue
		}
		version := binary.LittleEndian.Uint16(buf[off+4 : off+6])
		if version == 0 || version > CurrentVersion {
			continue
		}
		plen := binary.LittleEndian.Uint32(buf[off+6 : off+10])
		if plen > maxFrameSize {
			continue
		}
		end := off + HeaderSize + int(plen)
		if end > len(buf) || end < off {
			// The candidate frame would extend past the bytes we actually
			// have, so its CRC cannot be verified here. A genuinely swallowed
			// frame is fully present in buf (it was durable on disk before the
			// corrupt one), so an unverifiable candidate is not the signal we
			// want; keep scanning.
			continue
		}
		expectCRC := binary.LittleEndian.Uint32(buf[off+10 : off+14])
		gotCRC := crc32.Update(0, castagnoli, buf[off:off+10])
		gotCRC = crc32.Update(gotCRC, castagnoli, buf[off+HeaderSize:end])
		if gotCRC == expectCRC {
			return true
		}
	}
	return false
}
