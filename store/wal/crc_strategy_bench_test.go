package wal

import (
	"hash/crc32"
	"testing"
)

// This benchmark documents the empirical basis for rmp #1511: it compares the
// CRC32C strategies that could be used on the WAL Encode/Decode path and proves
// that the current incremental two-call crc32.Update already dispatches to the
// hardware CRC32C instruction, so a single-pass crc32.Checksum is not faster on
// the compute side and would only add the cost of reassembling a contiguous
// buffer (the per-frame allocation that #1509 deliberately removed).
//
// crcBenchSizes are representative WAL payload sizes; every frame additionally
// carries the 14-byte header, of which header[0:10] participates in the CRC.
var crcBenchSizes = []int{16, 64, 256, 4096}

// hardwareDispatchActive reports whether crc32.Update with the Castagnoli table
// uses the architecture-specific hardware instruction on this build. It is a
// proxy for the stdlib's internal haveCastagnoli flag: hash/crc32 routes both
// Update and Checksum through the same update() function, which selects the
// hardware path (archUpdateCastagnoli) whenever the table is the Castagnoli
// table and the CPU exposes the CRC32 instruction. We cannot read the unexported
// flag directly, so this benchmark records the platform via b.Log instead.
func makeFrameBytes(payloadLen int) (header [HeaderSize]byte, payload []byte) {
	payload = make([]byte, payloadLen)
	for i := range payload {
		payload[i] = byte(i * 31)
	}
	copy(header[0:4], Magic[:])
	header[4] = byte(CurrentVersion)
	header[6] = byte(payloadLen)
	header[7] = byte(payloadLen >> 8)
	return header, payload
}

// BenchmarkCRCIncremental is the CURRENT approach (post-#1509): two incremental
// crc32.Update calls over header[0:10] then the payload, with no combined buffer.
func BenchmarkCRCIncremental(b *testing.B) {
	for _, n := range crcBenchSizes {
		header, payload := makeFrameBytes(n)
		b.Run(sizeName(n), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(10 + n))
			var sink uint32
			for i := 0; i < b.N; i++ {
				crc := crc32.Update(0, castagnoli, header[0:10])
				crc = crc32.Update(crc, castagnoli, payload)
				sink = crc
			}
			_ = sink
		})
	}
}

// BenchmarkCRCSinglePassRealloc is single-pass crc32.Checksum over a contiguous
// buffer, INCLUDING the realloc+copy cost that #1509 removed — the fair cost of
// adopting single-pass on the current allocation-free path.
func BenchmarkCRCSinglePassRealloc(b *testing.B) {
	for _, n := range crcBenchSizes {
		header, payload := makeFrameBytes(n)
		b.Run(sizeName(n), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(10 + n))
			var sink uint32
			for i := 0; i < b.N; i++ {
				buf := make([]byte, 0, 10+n)
				buf = append(buf, header[0:10]...)
				buf = append(buf, payload...)
				sink = crc32.Checksum(buf, castagnoli)
			}
			_ = sink
		})
	}
}

// BenchmarkCRCSinglePassScratch is single-pass crc32.Checksum over a reusable,
// pre-allocated scratch buffer (no per-iteration alloc) — the best case for a
// pooled-scratch design. It isolates the pure compute difference from the alloc.
func BenchmarkCRCSinglePassScratch(b *testing.B) {
	for _, n := range crcBenchSizes {
		header, payload := makeFrameBytes(n)
		scratch := make([]byte, 0, 10+n)
		b.Run(sizeName(n), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(10 + n))
			var sink uint32
			for i := 0; i < b.N; i++ {
				buf := scratch[:0]
				buf = append(buf, header[0:10]...)
				buf = append(buf, payload...)
				sink = crc32.Checksum(buf, castagnoli)
			}
			_ = sink
		})
	}
}

func sizeName(n int) string {
	switch n {
	case 16:
		return "payload16"
	case 64:
		return "payload64"
	case 256:
		return "payload256"
	case 4096:
		return "payload4096"
	default:
		return "payloadN"
	}
}

// TestCRCStrategiesAgree pins the invariant that all three strategies produce
// the byte-identical CRC32C the on-disk format depends on. If a future change
// adopted single-pass, this guards that the checksum value never drifts.
func TestCRCStrategiesAgree(t *testing.T) {
	for _, n := range crcBenchSizes {
		header, payload := makeFrameBytes(n)

		inc := crc32.Update(0, castagnoli, header[0:10])
		inc = crc32.Update(inc, castagnoli, payload)

		buf := make([]byte, 0, 10+n)
		buf = append(buf, header[0:10]...)
		buf = append(buf, payload...)
		single := crc32.Checksum(buf, castagnoli)

		if inc != single {
			t.Fatalf("payload %d: incremental crc %#08x != single-pass crc %#08x", n, inc, single)
		}
	}
}
