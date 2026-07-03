//go:build !race

package snapshot

// readvalue_bound_alloc_test.go — security regression gate for #1886: a forged
// length far above snapshotEagerReadCap, backed by only a few real bytes, must
// fail fast WITHOUT a speculative make of the declared size. Gated !race
// because the race detector inflates heap accounting and would make the
// TotalAlloc delta meaningless.

import (
	"bufio"
	"bytes"
	"runtime"
	"testing"
)

func TestReadLenPrefixedValue_ForgedLengthBoundedAlloc(t *testing.T) {
	// 256 MiB declared, ~10 bytes present. The pre-fix inline make([]byte, n)
	// reserved 256 MiB before the read EOF'd; the bounded reader grows from the
	// snapshotEagerReadCap (1 MiB) hint only as bytes arrive, so it allocates
	// ~1 MiB and returns an EOF error.
	const forged = 256 << 20

	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)

	got, err := readLenPrefixedValue(bufio.NewReader(bytes.NewReader([]byte("few-bytes."))), forged)

	runtime.ReadMemStats(&m1)
	delta := m1.TotalAlloc - m0.TotalAlloc

	if err == nil {
		t.Fatal("forged length exceeding available bytes: want error, got nil")
	}
	if got != nil {
		t.Fatalf("forged length: want nil slice on error, got %d bytes", len(got))
	}
	const ceiling = 32 << 20 // 32 MiB: generous over the ~1 MiB grow; catches a 256 MiB eager make.
	t.Logf("forged=%d bytes present=10 TotalAlloc delta=%d (%.2f MiB)", forged, delta, float64(delta)/(1<<20))
	if delta > ceiling {
		t.Fatalf("readLenPrefixedValue reserved %d bytes for a 256 MiB forged length backed by 10 bytes "+
			"(ceiling %d): the untrusted length is honoured as an eager make", delta, ceiling)
	}
}
