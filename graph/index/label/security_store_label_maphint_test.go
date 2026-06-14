package label

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"runtime"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// SECURITY-GAP #1481 — label Index.Deserialize sizes its destination map
// with make(map[uint32]*roaring64.Bitmap, int(count)) where count is the
// untrusted uint32 header field, never clamped. A crafted indexes/<name>.bin
// reaches this decoder through store/recovery.applySnapshotIndexes when a
// hostile snapshot directory is restored. A 16-byte CRC-valid payload with
// count == 2^32-1 drives Go to eagerly allocate AND zero ~144 GiB of map
// buckets (and ~tens of seconds of CPU) before the per-record loop reads a
// single labelID and EOFs. CWE-789 / CWE-400 / CWE-20: memory + CPU
// exhaustion DoS on open. This is worse than the slice cap-make case
// (#1480) because map buckets are actively allocated, not lazily faulted
// zero pages.
//
// The reference-safe pattern is store/snapshot/tombstones.go /
// constraints.go, which clamp the eager reservation to a capHintMax ceiling
// and rely on the per-record loop to fail fail-stop on a truncated body.

// craftLabelHeaderOnly builds a CRC-valid label-index payload that declares
// count records but carries none. It reuses the magic + version bytes that
// Serialize emits for an empty index so the test never hard-codes the
// on-disk constants.
func craftLabelHeaderOnly(count uint32) []byte {
	empty := NewIndex()
	var s bytes.Buffer
	_ = empty.Serialize(&s)
	hdr := s.Bytes() // magic(4) | version(4) | count(4=0) | crc(4)

	var b bytes.Buffer
	b.Write(hdr[:8]) // magic + version, verbatim
	_ = binary.Write(&b, binary.LittleEndian, count)
	body := b.Bytes()
	crc := crc32.Checksum(body, castagnoli)
	var tr [4]byte
	binary.LittleEndian.PutUint32(tr[:], crc)
	return append(append([]byte{}, body...), tr[:]...)
}

// TestSec_LabelDeserialize_RejectsTruncatedRecords is the cheap, always-on
// guard: an under-filled body (declares records it does not carry) must be
// rejected fail-stop with index.ErrIndexCorrupted. It uses a modest count so
// it never itself triggers the unbounded map allocation under audit.
func TestSec_LabelDeserialize_RejectsTruncatedRecords(t *testing.T) {
	t.Parallel()
	payload := craftLabelHeaderOnly(1024)
	if err := NewIndex().Deserialize(bytes.NewReader(payload)); !errors.Is(err, index.ErrIndexCorrupted) {
		t.Fatalf("header-only payload: err = %v, want wrapped index.ErrIndexCorrupted", err)
	}
}

// TestSec_LabelDeserialize_HugeCountRejectedFast is the deterministic,
// always-on regression assertion for #1481. It feeds the largest uint32 count
// the header can carry but carries no record body, so the decoder must reject
// it fail-stop with index.ErrIndexCorrupted on the very first absent-record
// read. Before the clamp this same 16-byte payload allocated ~144 GiB of map
// buckets and spun tens of seconds; after the clamp the map hint is bounded to
// labelCapHintMax and the test returns immediately. This proves the secure
// contract without depending on a process-global MemStats delta (left to the
// soak gate below).
func TestSec_LabelDeserialize_HugeCountRejectedFast(t *testing.T) {
	t.Parallel()
	// Guard the clamp ceiling itself so a future edit that widens it is caught.
	if labelCapHintMax > 1<<20 {
		t.Fatalf("labelCapHintMax = %d, want <= 1<<20 (matches tombstones/constraints ceiling)", labelCapHintMax)
	}
	payload := craftLabelHeaderOnly(^uint32(0))
	if len(payload) != 16 {
		t.Fatalf("crafted payload size = %d, want 16", len(payload))
	}
	if err := NewIndex().Deserialize(bytes.NewReader(payload)); !errors.Is(err, index.ErrIndexCorrupted) {
		t.Fatalf("hostile count=2^32-1 payload: err = %v, want wrapped index.ErrIndexCorrupted", err)
	}
}

// TestSec_LabelDeserialize_MapHintBounded is the security regression gate for
// #1481. It runs only under the soak layer because, until the map hint is
// clamped, executing the crafted 2^32-1 payload allocates ~144 GiB and spins
// ~26 s of CPU (proven via a GOMEMLIMIT subprocess during the audit) — far
// too costly to run on every PR. Once #1481 clamps count to a capHintMax
// ceiling, this test passes in the short window the decoder needs to EOF.
func TestSec_LabelDeserialize_MapHintBounded(t *testing.T) {
	testlayers.RequireSoak(t)

	// count == 2^32-1 is the largest uint32 the header can carry, passed
	// straight to make(map, hint) today.
	payload := craftLabelHeaderOnly(^uint32(0))
	if len(payload) != 16 {
		t.Fatalf("crafted payload size = %d, want 16", len(payload))
	}

	var m0 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)

	start := time.Now()
	err := NewIndex().Deserialize(bytes.NewReader(payload))
	elapsed := time.Since(start)

	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	delta := m1.TotalAlloc - m0.TotalAlloc

	if !errors.Is(err, index.ErrIndexCorrupted) {
		t.Fatalf("Deserialize err = %v, want wrapped index.ErrIndexCorrupted", err)
	}

	const allocCeiling = 64 << 20 // 64 MiB.
	const timeCeiling = 2 * time.Second
	t.Logf("count=2^32-1 payload=%d bytes TotalAlloc delta=%d bytes (%.2f MiB) elapsed=%s",
		len(payload), delta, float64(delta)/(1<<20), elapsed)
	if delta > allocCeiling {
		t.Fatalf("Deserialize reserved %d bytes for a 16-byte hostile payload (ceiling %d): "+
			"count is honoured unbounded as a map size hint (#1481)", delta, allocCeiling)
	}
	if elapsed > timeCeiling {
		t.Fatalf("Deserialize took %s for a 16-byte hostile payload (ceiling %s): "+
			"unbounded map-bucket allocation (#1481)", elapsed, timeCeiling)
	}
}
