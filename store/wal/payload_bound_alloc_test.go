//go:build !race

package wal

// payload_bound_alloc_test.go — security regression gate: a forged frame plen
// far above framePayloadEagerCap, backed by only a few real bytes, must fail
// fast WITHOUT a speculative make of the declared plen. Gated !race because the
// race detector inflates heap accounting and would void the TotalAlloc delta.

import (
	"bytes"
	"runtime"
	"testing"
)

func TestDecode_ForgedLargePlen_BoundedAlloc(t *testing.T) {
	const plen = 256 << 20 // 256 MiB declared, 3 bytes present
	payload := []byte("few")
	buf := make([]byte, HeaderSize+len(payload))
	putCandidateHeader(buf, 0, CurrentVersion, plen, 0)
	copy(buf[HeaderSize:], payload)

	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)

	_, err := Decode(bytes.NewReader(buf))

	runtime.ReadMemStats(&m1)
	delta := m1.TotalAlloc - m0.TotalAlloc

	if err == nil {
		t.Fatal("forged large plen backed by 3 bytes: want a torn error, got nil")
	}
	const ceiling = 32 << 20 // 32 MiB: generous over the ~1 MiB grow; catches a 256 MiB eager make.
	t.Logf("plen=%d present=3 TotalAlloc delta=%d (%.2f MiB)", plen, delta, float64(delta)/(1<<20))
	if delta > ceiling {
		t.Fatalf("Decode reserved %d bytes for a 256 MiB forged plen backed by 3 bytes (ceiling %d): "+
			"the untrusted plen is honoured as an eager make", delta, ceiling)
	}
}
