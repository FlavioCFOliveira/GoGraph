package wal

// embed_scan_budget_test.go — regression gate for #1883: embedsValidFrame's
// torn-vs-corruption scan must be strictly linear in len(buf). A crafted WAL
// tail could otherwise drive O(len(buf)^2) crc32 work (magic + valid-version +
// in-range-length candidates with wrong CRCs at many offsets), hanging recovery
// for minutes-to-weeks. The scan now caps cumulative crc32 input and fail-stops
// (returns true → ErrTornFrameMasksData) on budget exhaustion.

import (
	"encoding/binary"
	"hash/crc32"
	"testing"
	"time"
)

// putCandidateHeader writes a frame header (magic + version + plen + crc) at
// off. It writes no payload; callers overlap payloads with later headers.
func putCandidateHeader(buf []byte, off int, version uint16, plen uint32, crc uint32) {
	copy(buf[off:off+4], Magic[:])
	binary.LittleEndian.PutUint16(buf[off+4:off+6], version)
	binary.LittleEndian.PutUint32(buf[off+6:off+10], plen)
	binary.LittleEndian.PutUint32(buf[off+10:off+14], crc)
}

// TestEmbedsValidFrame_AdversarialTailIsBounded builds the quadratic worst case
// — a supported-version, in-range-length, wrong-CRC candidate every HeaderSize
// bytes across the first half of the buffer, each declaring a large payload so
// the pre-fix scan CRCs ~len(buf) bytes per candidate. With the cumulative-work
// budget the scan reports the tail as corruption (true) after only a few
// candidates and returns promptly. Without the budget it CRCs every in-range
// candidate (O(n^2)) and returns false; this test asserts true, so it fails on
// the unbounded implementation.
func TestEmbedsValidFrame_AdversarialTailIsBounded(t *testing.T) {
	t.Parallel()
	const n = 1 << 19 // 512 KiB — big enough to be seconds-slow unbounded
	buf := make([]byte, n)
	plen := uint32(n / 2) // each candidate's declared payload spans half the buf
	const wrongCRC = 0x0BADF00D
	// Lay candidates every HeaderSize bytes only where end (= off+HeaderSize+plen)
	// still fits in buf, so each is an in-range candidate the scan must CRC.
	for off := 0; off+HeaderSize+int(plen) <= n; off += HeaderSize {
		putCandidateHeader(buf, off, CurrentVersion, plen, wrongCRC)
	}

	start := time.Now()
	got := embedsValidFrame(buf)
	elapsed := time.Since(start)

	if !got {
		t.Fatalf("embedsValidFrame(adversarial) = false; want true (budget must fail-stop-safe)")
	}
	// Linear scan of 512 KiB with a 2*len crc budget is sub-millisecond in
	// practice; a very generous ceiling still catches an O(n^2) regression,
	// which would take seconds even at this modest size.
	if elapsed > 2*time.Second {
		t.Fatalf("embedsValidFrame(adversarial) took %v; budget must keep it linear", elapsed)
	}
}

// TestEmbedsValidFrame_BenignTailNotCorruption confirms the budget does not
// misclassify a genuine torn tail: opaque payload bytes that contain no
// magic-aligned candidate must scan cheaply and return false (a benign torn
// write, not corruption).
func TestEmbedsValidFrame_BenignTailNotCorruption(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 4096)
	// Deterministic non-magic fill: 'G' would risk a magic prefix, so use a
	// byte that can never begin the "GGWA" magic.
	for i := range buf {
		buf[i] = 0x5A
	}
	if embedsValidFrame(buf) {
		t.Fatal("embedsValidFrame(opaque non-frame tail) = true; want false (benign torn tail)")
	}
}

// TestEmbedsValidFrame_GenuineEmbeddedFrameDetected confirms the anti-silent-
// data-loss signal is preserved: a real, CRC-valid frame embedded in the buffer
// is still detected (true), so a length field that over-declared past EOF and
// swallowed a durable committed frame is caught.
func TestEmbedsValidFrame_GenuineEmbeddedFrameDetected(t *testing.T) {
	t.Parallel()
	const off = 32
	payload := []byte("committed-txn-bytes")
	buf := make([]byte, off+HeaderSize+len(payload)+16)
	for i := range buf {
		buf[i] = 0x5A
	}
	crc := crc32Header(CurrentVersion, uint32(len(payload)), payload)
	putCandidateHeader(buf, off, CurrentVersion, uint32(len(payload)), crc)
	copy(buf[off+HeaderSize:], payload)

	if !embedsValidFrame(buf) {
		t.Fatal("embedsValidFrame(buffer with a genuine CRC-valid frame) = false; want true")
	}
}

// crc32Header computes the frame CRC exactly as Decode/embedsValidFrame do:
// over head[0:10] (magic+version+plen) then the payload.
func crc32Header(version uint16, plen uint32, payload []byte) uint32 {
	var head [10]byte
	copy(head[0:4], Magic[:])
	binary.LittleEndian.PutUint16(head[4:6], version)
	binary.LittleEndian.PutUint32(head[6:10], plen)
	c := crc32.Update(0, castagnoli, head[:])
	c = crc32.Update(c, castagnoli, payload)
	return c
}
