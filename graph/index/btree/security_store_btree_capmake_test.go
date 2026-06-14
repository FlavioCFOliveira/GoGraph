package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// SECURITY-GAP #1480 — btree Index.Deserialize eagerly reserves
// make([]entry[V], 0, entryCount) with entryCount taken straight from the
// untrusted header (rejected only when strictly > 1<<40). A crafted
// indexes/<name>.bin reaches this decoder through
// store/recovery.applySnapshotIndexes when a hostile snapshot directory is
// restored. A 20-byte CRC-valid payload with entryCount == 2^40 drives a
// single ~16 TiB reservation before the per-entry loop reads a single byte
// and EOFs. CWE-789 / CWE-20: memory-exhaustion DoS on open.
//
// The reference-safe pattern is store/snapshot/tombstones.go, which clamps
// the eager reservation to min(count, capHintMax) and then appends, so a
// truncated body fails fail-stop on the first ReadFull without first
// reserving an attacker-chosen capacity.

// craftBtreeHeaderOnly builds a CRC-valid btree payload that declares
// entryCount entries but carries none, so a clamped decoder allocates only a
// bounded reservation before it EOFs on the (absent) first entry.
func craftBtreeHeaderOnly(entryCount uint64) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, btreeMagic)
	_ = binary.Write(&b, binary.LittleEndian, btreeFormatVersion)
	_ = binary.Write(&b, binary.LittleEndian, entryCount)
	body := b.Bytes()
	crc := crc32.Checksum(body, castagnoli)
	var tr [4]byte
	binary.LittleEndian.PutUint32(tr[:], crc)
	return append(append([]byte{}, body...), tr[:]...)
}

// TestSec_BtreeDeserialize_RejectsTruncatedEntries is the cheap, always-on
// guard: an under-filled body (declares entries it does not carry) must be
// rejected fail-stop with index.ErrIndexCorrupted. It uses a modest count so
// it never itself triggers the unbounded reservation under audit.
func TestSec_BtreeDeserialize_RejectsTruncatedEntries(t *testing.T) {
	t.Parallel()
	payload := craftBtreeHeaderOnly(1024)
	if err := New[float64]().Deserialize(bytes.NewReader(payload)); !errors.Is(err, index.ErrIndexCorrupted) {
		t.Fatalf("header-only payload: err = %v, want wrapped index.ErrIndexCorrupted", err)
	}
}

// TestSec_BtreeDeserialize_HugeCountRejectedFast is the deterministic,
// always-on regression assertion for #1480. It feeds the worst-case in-bounds
// entryCount (1<<40, the largest value the guard accepts) but carries no entry
// body, so the decoder must reject it fail-stop with index.ErrIndexCorrupted
// on the very first absent-entry read. Before the clamp this same payload
// reserved ~16 TiB; after the clamp the reservation is bounded to
// btreeCapHintMax and the test returns immediately. This proves the secure
// contract — the untrusted count is no longer honoured as an unbounded make
// cap — without depending on a process-global MemStats delta (left to the
// soak gate below, which is intentionally heavier).
func TestSec_BtreeDeserialize_HugeCountRejectedFast(t *testing.T) {
	t.Parallel()
	// Guard the clamp ceiling itself so a future edit that widens it is caught.
	if btreeCapHintMax > 1<<20 {
		t.Fatalf("btreeCapHintMax = %d, want <= 1<<20 (matches tombstones/constraints ceiling)", btreeCapHintMax)
	}
	payload := craftBtreeHeaderOnly(1 << 40)
	if len(payload) != 20 {
		t.Fatalf("crafted payload size = %d, want 20", len(payload))
	}
	if err := New[float64]().Deserialize(bytes.NewReader(payload)); !errors.Is(err, index.ErrIndexCorrupted) {
		t.Fatalf("hostile entryCount=2^40 payload: err = %v, want wrapped index.ErrIndexCorrupted", err)
	}
}

// TestSec_BtreeDeserialize_CapMakeBounded is the security regression gate for
// #1480. It runs only under the soak layer because, until the cap is clamped,
// executing the crafted 2^40 payload reserves ~16 TiB (proven via a
// GOMEMLIMIT subprocess during the audit) — too costly to run on every PR.
// Once #1480 clamps entryCount to min(entryCount, capHintMax), this test
// passes in the short window the decoder needs to EOF, and the soak gate
// keeps it from regressing.
func TestSec_BtreeDeserialize_CapMakeBounded(t *testing.T) {
	testlayers.RequireSoak(t)

	// entryCount == 1<<40 is the largest value the guard accepts (the check
	// is strictly `> 1<<40`): the worst-case in-bounds reservation.
	payload := craftBtreeHeaderOnly(1 << 40)
	if len(payload) != 20 {
		t.Fatalf("crafted payload size = %d, want 20", len(payload))
	}

	var m0 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)

	err := New[float64]().Deserialize(bytes.NewReader(payload))

	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	delta := m1.TotalAlloc - m0.TotalAlloc

	if !errors.Is(err, index.ErrIndexCorrupted) {
		t.Fatalf("Deserialize err = %v, want wrapped index.ErrIndexCorrupted", err)
	}

	const allocCeiling = 64 << 20 // 64 MiB: generous headroom over a clamped reservation.
	t.Logf("entryCount=2^40 payload=%d bytes TotalAlloc delta=%d bytes (%.2f MiB)",
		len(payload), delta, float64(delta)/(1<<20))
	if delta > allocCeiling {
		t.Fatalf("Deserialize reserved %d bytes for a 20-byte hostile payload (ceiling %d): "+
			"entryCount is honoured unbounded as a make cap (#1480)", delta, allocCeiling)
	}
}
