// Package wal implements a versioned, length-prefixed,
// CRC32C-checksummed Write-Ahead Log for the gograph durability
// stack.
//
// The on-disk format is documented in FORMAT.md alongside this
// package. A store's log is a directory of numbered segment files
// described by a durable control file; every frame carries its
// logical position, a link to its predecessor and the store's
// identity, so a reader refuses a frame from another store, a frame
// moved to another position, and a frame from an older generation.
// Readers stop cleanly at the first torn or corrupted frame.
package wal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// Magic is the 4-byte identifier prefix of every WAL frame: ASCII
// "GGWA".
var Magic = [4]byte{'G', 'G', 'W', 'A'}

// CurrentVersion is the WAL frame version this package writes: the
// 36-byte header of [HeaderSizeV2], which carries the frame's logical
// position, the distance to its predecessor and the store id.
// Readers accept every version <= CurrentVersion, so a fresh build
// replays the single-file logs ([LegacyVersion]) of previous releases.
const CurrentVersion uint16 = 2

// LegacyVersion is the frame version of the single-file log written by
// releases before WAL v2: the 14-byte header of [HeaderSize], with no
// position and no store identity. [Encode] still writes it for a
// [Frame] whose Version is 0 or LegacyVersion, which is how test
// fixtures of the legacy format are built.
const LegacyVersion uint16 = 1

// HeaderSize is the fixed number of bytes occupying a [LegacyVersion]
// frame header (magic + version + length + crc32c).
const HeaderSize = 4 + 2 + 4 + 4

// HeaderSizeV2 is the fixed number of bytes occupying a [CurrentVersion]
// frame header: magic (4), version (2), flags (2), length (4),
// position (8), prevLen (4), store id (8) and crc32c (4).
const HeaderSizeV2 = 36

// headerSizeFor returns the header size of a frame of version v, or 0 for a
// version this build does not know.
func headerSizeFor(v uint16) int {
	switch v {
	case LegacyVersion:
		return HeaderSize
	case CurrentVersion:
		return HeaderSizeV2
	default:
		return 0
	}
}

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

// framePayloadEagerCap bounds the up-front allocation when reading a frame
// payload. A payload up to this size is pre-sized exactly in one make (the
// common case — WAL frames carry single transactions); a larger declared plen
// grows as bytes actually arrive. This keeps a forged/tampered plen that
// over-declares past EOF (the poisoned-store-directory threat model) from
// forcing a speculative allocation up to maxFrameSize before the short read
// fails — the same defence [readLenPrefixedValue] gives the snapshot readers.
const framePayloadEagerCap = 1 << 20

// readFramePayload reads exactly plen payload bytes from r into a fresh slice
// without eagerly reserving an untrusted plen. For plen within
// [framePayloadEagerCap] it pre-sizes exactly (the historical fast path); above
// that it grows a bytes.Buffer as bytes arrive so a plen that over-declares past
// EOF fails on the short read with peak transient ~2x the bytes truly present,
// never ~plen. The returned slice always holds exactly the bytes consumed (len
// == plen on success, or the short-read prefix on an EOF-class error) so
// [Decode]'s torn-vs-corruption discrimination sees the real consumed bytes.
func readFramePayload(r io.Reader, plen uint32) ([]byte, error) {
	if plen == 0 {
		return nil, nil
	}
	if plen <= framePayloadEagerCap {
		payload := make([]byte, plen)
		n, err := io.ReadFull(r, payload)
		return payload[:n], err
	}
	var b bytes.Buffer
	b.Grow(framePayloadEagerCap)
	if _, err := io.CopyN(&b, r, int64(plen)); err != nil {
		return b.Bytes(), err
	}
	return b.Bytes(), nil
}

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
	// ErrFrameTooLarge indicates a frame payload longer than maxFrameSize.
	//
	// On DECODE the length is a declared one and is treated as corruption: the
	// frame is rejected before any allocation, so a crafted or corrupted length
	// cannot force a large one-shot make.
	//
	// On ENCODE it is a real payload, and the rejection is a durability guard
	// (rmp #2742): a frame this large could be written but never read back, so
	// [Encode] refuses it before any byte reaches the writer and the commit
	// fails rather than being acknowledged.
	ErrFrameTooLarge = errors.New("wal: frame payload length exceeds maximum")
)

// castagnoli holds the precomputed CRC32C table used by every
// encode and decode. The polynomial is 0x1EDC6F41.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Frame is the in-memory representation of one WAL frame.
//
// A Frame carries no synchronisation of its own, so its concurrency contract
// follows the ownership of Payload. A Frame returned by [Decode] owns its
// Payload outright — the decoder allocates a fresh slice per frame and never
// aliases the reader's buffer — so it is safe to hand to another goroutine and
// to read concurrently. A Frame passed to [Encode] merely borrows the caller's
// Payload for the duration of that call, which is what lets the transaction
// layer re-use one pooled scratch buffer for every op; such a Frame must not be
// retained or shared past the call that consumed it.
//
// Pos, PrevLen and StoreID are meaningful only for a [CurrentVersion] frame:
// Pos is the frame's logical position (the count of frame bytes the store's
// log held before it, independent of segments and never reset), PrevLen is
// Pos minus the predecessor's Pos (0 only for the store's first frame), and
// StoreID is the store's identity from the control file. A [LegacyVersion]
// frame carries none of them and decodes with all three zero.
type Frame struct {
	Payload []byte
	Pos     uint64
	StoreID uint64
	PrevLen uint32
	Version uint16
}

// Encode writes f to w as a single binary frame. It returns the
// number of bytes written and any underlying writer error.
//
// A Frame whose Version is 0 or [LegacyVersion] is written in the legacy
// 14-byte layout; a Frame whose Version is [CurrentVersion] is written in the
// 36-byte layout with its Pos, PrevLen and StoreID. Any other version is
// refused with [ErrUnsupportedVersion]. The [Writer] always writes
// [CurrentVersion] frames; the legacy layout exists so fixtures of logs
// written by earlier releases can still be built.
func Encode(w io.Writer, f Frame) (int, error) {
	defer metrics.Time("store.wal.Encode").Stop()
	// AGGREGATE BOUND (rmp #2742). The per-field guards in store/txn bound each
	// string a frame carries, but only the framer sees the assembled payload: a
	// property list of many individually-legal elements still adds up. Refusing
	// here, before a byte is written, is what stops a commit being acknowledged
	// for a frame [Decode] is then required to refuse — the "assembled but not
	// replayable" record PostgreSQL closed off in commit 8fcb32db98 by bounding
	// the whole record in XLogRecordAssemble against XLogRecordMaxSize, for the
	// same reason. Above 4 GiB the uint32 cast below would silently wrap; the
	// 1 GiB ceiling is reached first and rejects both cases.
	if len(f.Payload) > maxFrameSize {
		metrics.IncCounter("store.wal.Encode.errors", 1)
		return 0, ErrFrameTooLarge
	}
	switch f.Version {
	case 0, LegacyVersion:
		return encodeLegacy(w, f.Payload)
	case CurrentVersion:
		return encodeV2(w, f.Payload, f.Pos, f.PrevLen, f.StoreID)
	default:
		metrics.IncCounter("store.wal.Encode.errors", 1)
		return 0, ErrUnsupportedVersion
	}
}

// encodeLegacy writes one [LegacyVersion] frame. The caller has bounded the
// payload by maxFrameSize.
func encodeLegacy(w io.Writer, payload []byte) (int, error) {
	//nolint:gosec // G115: bounded by the len(payload)>maxFrameSize (1<<30) rejection in Encode (rmp #2742)
	plen := uint32(len(payload))
	// Build the 14-byte header on the stack — no per-frame heap allocation —
	// and write header then payload as two Writes (#1509).
	var header [HeaderSize]byte
	copy(header[0:4], Magic[:])
	binary.LittleEndian.PutUint16(header[4:6], LegacyVersion)
	binary.LittleEndian.PutUint32(header[6:10], plen)
	// CRC is over magic+version+length+payload; the 4 crc bytes at
	// header[10:14] are not part of the input.
	crc := crc32.Update(0, castagnoli, header[0:10])
	crc = crc32.Update(crc, castagnoli, payload)
	binary.LittleEndian.PutUint32(header[10:14], crc)
	return writeFrame(w, header[:], payload)
}

// encodeV2 writes one [CurrentVersion] frame with the given position, prevLen
// and store id. The caller has bounded the payload by maxFrameSize.
//
// The CRC covers the payload FIRST and then the header bytes [0,32): the
// payload's CRC does not depend on the position, so a caller can compute it
// before it holds the lock that assigns the position — the arrangement
// PostgreSQL uses (XLogRecordAssemble computes the record CRC over the data;
// XLogInsertRecord completes it over the header once xl_prev is known inside
// the insertion lock: src/backend/access/transam/xloginsert.c:979-1007 and
// src/backend/access/transam/xlog.c:1001-1007 at commit 10cc5aa9).
func encodeV2(w io.Writer, payload []byte, pos uint64, prevLen uint32, storeID uint64) (int, error) {
	//nolint:gosec // G115: bounded by the len(payload)>maxFrameSize (1<<30) rejection in Encode and Writer.appendLocked
	plen := uint32(len(payload))
	var header [HeaderSizeV2]byte
	putHeaderV2(&header, plen, pos, prevLen, storeID, crc32.Update(0, castagnoli, payload))
	return writeFrame(w, header[:], payload)
}

// putHeaderV2 fills a [CurrentVersion] header; payloadCRC is the CRC32C of the
// payload alone, which the header CRC continues over header[0:32].
func putHeaderV2(header *[HeaderSizeV2]byte, plen uint32, pos uint64, prevLen uint32, storeID uint64, payloadCRC uint32) {
	copy(header[0:4], Magic[:])
	binary.LittleEndian.PutUint16(header[4:6], CurrentVersion)
	binary.LittleEndian.PutUint16(header[6:8], 0) // flags: reserved
	binary.LittleEndian.PutUint32(header[8:12], plen)
	binary.LittleEndian.PutUint64(header[12:20], pos)
	binary.LittleEndian.PutUint32(header[20:24], prevLen)
	binary.LittleEndian.PutUint64(header[24:32], storeID)
	binary.LittleEndian.PutUint32(header[32:36], crc32.Update(payloadCRC, castagnoli, header[0:32]))
}

// writeFrame writes header then payload. bufio.Writer (the production sink)
// copies each Write into its internal buffer synchronously before returning,
// so the caller's payload slice is fully consumed when it returns — which is
// what makes the pooled txn-layer scratch buffer safe to reuse (#1509).
func writeFrame(w io.Writer, header, payload []byte) (int, error) {
	nh, err := w.Write(header)
	if err != nil {
		metrics.IncCounter("store.wal.Encode.errors", 1)
		return nh, err
	}
	np, err := w.Write(payload)
	if err != nil {
		metrics.IncCounter("store.wal.Encode.errors", 1)
	}
	return nh + np, err
}

// FrameSize returns the number of bytes f occupies on disk: its header (by
// version) plus its payload. A Version of 0 counts as [LegacyVersion].
func FrameSize(f Frame) int {
	if f.Version == CurrentVersion {
		return HeaderSizeV2 + len(f.Payload)
	}
	return HeaderSize + len(f.Payload)
}

// Decode reads the next frame from r, of either version. It returns
// ErrTornFrame when the reader ends mid-frame (clean tail truncation),
// ErrBadMagic on a missing magic, ErrUnsupportedVersion on a version
// this build does not know, and ErrCRCMismatch on integrity failure.
// Any other error is propagated from the underlying reader.
func Decode(r io.Reader) (Frame, error) {
	defer metrics.Time("store.wal.Decode").Stop()
	var head [HeaderSizeV2]byte
	// The shortest header is the legacy one: fewer bytes than that is a torn
	// tail whatever they contain, exactly as before the 36-byte header existed.
	if _, err := io.ReadFull(r, head[:HeaderSize]); err != nil {
		return Frame{}, decodeReadErr(err)
	}
	if head[0] != Magic[0] || head[1] != Magic[1] || head[2] != Magic[2] || head[3] != Magic[3] {
		metrics.IncCounter("store.wal.Decode.errors", 1)
		return Frame{}, ErrBadMagic
	}
	version := binary.LittleEndian.Uint16(head[4:6])
	hsz := headerSizeFor(version)
	if hsz == 0 {
		metrics.IncCounter("store.wal.Decode.errors", 1)
		return Frame{}, ErrUnsupportedVersion
	}
	if _, err := io.ReadFull(r, head[HeaderSize:hsz]); err != nil {
		return Frame{}, decodeReadErr(err)
	}
	var (
		plen, expectCRC, prevLen uint32
		pos, storeID             uint64
	)
	if version == LegacyVersion {
		plen = binary.LittleEndian.Uint32(head[6:10])
		expectCRC = binary.LittleEndian.Uint32(head[10:14])
	} else {
		plen = binary.LittleEndian.Uint32(head[8:12])
		pos = binary.LittleEndian.Uint64(head[12:20])
		prevLen = binary.LittleEndian.Uint32(head[20:24])
		storeID = binary.LittleEndian.Uint64(head[24:32])
		expectCRC = binary.LittleEndian.Uint32(head[32:36])
	}

	// Reject an implausibly large length before allocating (see maxFrameSize).
	if plen > maxFrameSize {
		metrics.IncCounter("store.wal.Decode.errors", 1)
		return Frame{}, ErrFrameTooLarge
	}

	// Read the payload without eagerly reserving the untrusted plen; on a
	// short read the returned slice holds exactly the bytes consumed.
	payload, err := readFramePayload(r, plen)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			metrics.IncCounter("store.wal.Decode.errors", 1)
			// A short payload is normally a benign torn tail. But a corrupted
			// length that OVER-declares past EOF produces the same EOF, and
			// then the consumed bytes are the durable frames that follow. If a
			// CRC-valid frame begins inside them, this is mid-stream
			// corruption: promote it to a hard error so recovery fail-stops.
			if embedsValidFrame(payload) {
				metrics.IncCounter("store.wal.Decode.tornMasksData", 1)
				return Frame{}, ErrTornFrameMasksData
			}
			return Frame{}, ErrTornFrame
		}
		metrics.IncCounter("store.wal.Decode.errors", 1)
		return Frame{}, err
	}
	if frameCRC(version, head[:hsz], payload) != expectCRC {
		metrics.IncCounter("store.wal.Decode.errors", 1)
		return Frame{}, ErrCRCMismatch
	}
	return Frame{Version: version, Payload: payload, Pos: pos, PrevLen: prevLen, StoreID: storeID}, nil
}

// decodeReadErr maps a short header read to [ErrTornFrame] and passes any
// other reader error through.
func decodeReadErr(err error) error {
	metrics.IncCounter("store.wal.Decode.errors", 1)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrTornFrame
	}
	return err
}

// frameCRC recomputes the CRC32C a frame of the given version carries, from its
// full header bytes and its payload: magic+version+length then payload for
// [LegacyVersion], payload then header[0:32] for [CurrentVersion].
func frameCRC(version uint16, header, payload []byte) uint32 {
	if version == LegacyVersion {
		c := crc32.Update(0, castagnoli, header[0:10])
		return crc32.Update(c, castagnoli, payload)
	}
	c := crc32.Update(0, castagnoli, payload)
	return crc32.Update(c, castagnoli, header[0:32])
}

// embedsValidFrame reports whether buf contains, at any byte offset, the start
// of a structurally complete and CRC-valid WAL frame of either version. It is
// the discriminator [Decode] uses to tell a benign torn tail from a corrupt
// length that swallowed durable frames.
//
// A candidate must have the magic, a supported version, a length that fits in
// buf, AND a matching CRC32C — the CRC is what makes a false positive a ~2^-32
// per-offset event. The scan advances one byte at a time because the swallowed
// frame's start is unknown. A cumulative CRC budget of 2·len(buf) keeps the
// scan linear on adversarial input; on exhaustion it conservatively reports an
// embedded frame (→ [ErrTornFrameMasksData] → fail-stop), the safe direction.
func embedsValidFrame(buf []byte) bool {
	const crcBudgetFactor = 2
	crcBudget := crcBudgetFactor * len(buf)
	crcSpent := 0
	for off := 0; off+HeaderSize <= len(buf); off++ {
		if !bytes.Equal(buf[off:off+4], Magic[:]) {
			continue
		}
		version := binary.LittleEndian.Uint16(buf[off+4 : off+6])
		hsz := headerSizeFor(version)
		if hsz == 0 || off+hsz > len(buf) {
			continue
		}
		var plen, expectCRC uint32
		if version == LegacyVersion {
			plen = binary.LittleEndian.Uint32(buf[off+6 : off+10])
			expectCRC = binary.LittleEndian.Uint32(buf[off+10 : off+14])
		} else {
			plen = binary.LittleEndian.Uint32(buf[off+8 : off+12])
			expectCRC = binary.LittleEndian.Uint32(buf[off+32 : off+36])
		}
		if plen > maxFrameSize {
			continue
		}
		end := off + hsz + int(plen)
		if end > len(buf) || end < off {
			// Not fully present, so not a swallowed durable frame; keep scanning.
			continue
		}
		crcSpent += hsz + int(plen)
		if crcSpent > crcBudget {
			metrics.IncCounter("store.wal.Decode.embedScanBudgetExceeded", 1)
			return true
		}
		if frameCRC(version, buf[off:off+hsz], buf[off+hsz:end]) == expectCRC {
			return true
		}
	}
	return false
}
