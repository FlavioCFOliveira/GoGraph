package snapshot

// Security test battery — snapshot component decoders.
//
// DEFENSE LOCK-INS for the persistence-layer audit (findings #1467 / #1468 /
// #1469, now FIXED). The snapshot component readers (ReadLabels,
// ReadProperties, ReadMapperString / ReadMapperBytes, ReadEdgeHandles, and the
// bare ReadCSR) used to reserve make([]T, count) — or make([]string, count)
// for a string table — for a count read straight from an untrusted header,
// guarded only by an absolute backstop ceiling. The eager reservation happened
// BEFORE the truncated body was read and BEFORE the manifest CRC was verified
// (the CRC is checked only after a full, successful parse + drain), so a
// hostile count was an out-of-memory DoS on the recovery.Open -> LoadSnapshotFull
// path.
//
// The fix mirrors the reference-SAFE patterns that already lived in this
// package:
//   - tombstones.go / constraints.go / edgehandles.go: a capHintMax (1<<20)
//     clamp on the eager reservation plus an append-grown read loop, so a
//     header declaring a vast count with a short body hits EOF on the first
//     per-record read long before any large allocation.
//   - writer.go readCSRLimited: rejects nV/nE against the precise byte budget
//     (recordCap = byteBudget/8) BEFORE make(), with the bare ReadCSR backstop
//     ceiling lowered (maxCSRCount = 1<<34) so even the size-blind entry point
//     cannot drive a multi-TiB make.
//
// Each decoder now applies the shared capHint(count, max) clamp (writer.go):
// the count is validated against its implausibility ceiling first, then the
// slice is grown via append. These tests pin that fixed invariant — at a count
// ONE PAST the ceiling (so the count guard rejects it before the make is even
// reached) with a truncated body, each decoder returns a typed corruption
// error and allocates nothing proportional to the count (a TotalAlloc delta
// inside boundedAllocBudget). They mirror the shape of the sibling lock-ins
// TestSec_Store_ConstraintsHostileCountBoundedReject and
// TestSec_Store_TombstonesHostileCountBoundedReject in security_fs_posture_test.go.
//
// The end-to-end OOM guard at a TRULY hostile count (1<<40, exercising the
// clamp on a count UNDER the ceiling through the full LoadSnapshotFull path)
// runs in a GOMEMLIMIT subprocess (see security_oom_subprocess_test.go).
//
// assertBoundedAlloc reads process-global runtime.MemStats, so every subtest
// here runs serially (no t.Parallel) and is skipped under -race
// (secStoreRaceEnabled) — the same caveat the sibling lock-ins carry. The
// error-classification half (a typed corruption error, no panic) is also
// covered, race-safe, by the per-component error tests
// (labels_error_test.go / properties_error_test.go / writer_error_ext_test.go /
// edgehandles_test.go).

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// secStorePutU64 / secStorePutU32 append a little-endian integer to b. They
// keep the synthetic-header builders below terse and match the on-disk
// encoding every Write* helper uses (binary.LittleEndian).
func secStorePutU64(b []byte, v uint64) []byte { return binary.LittleEndian.AppendUint64(b, v) }
func secStorePutU32(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }

// secStoreAssertBoundedCorruptReject decodes payload via dec under
// assertBoundedAlloc and asserts the post-fix contract: the decoder rejects
// the hostile count with a typed error that wraps wantSentinel, AND it grows
// the heap by nothing proportional to the count (delta inside
// boundedAllocBudget). It is the shared body of every decoder lock-in. It must
// run serially (assertBoundedAlloc forces a process-wide runtime.GC and reads
// global MemStats) and is therefore guarded by the secStoreRaceEnabled skip in
// every caller.
//
// It also asserts no panic: a decoder that reached an overflow-driven make()
// with a garbage size would panic rather than allocate, and that must never
// happen either.
func secStoreAssertBoundedCorruptReject(t *testing.T, name string, payload []byte, wantSentinel error, dec func([]byte) error) {
	t.Helper()
	assertBoundedAlloc(t, func() {
		var err error
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Fatalf("%s: decoder panicked on a hostile header: %v", name, rec)
				}
			}()
			err = dec(payload)
		}()
		if err == nil {
			t.Fatalf("%s: decoder accepted a header declaring a count past the implausibility ceiling; want a typed error", name)
		}
		if wantSentinel != nil && !errors.Is(err, wantSentinel) {
			t.Fatalf("%s: decoder error = %v; want errors.Is(%v)", name, err, wantSentinel)
		}
	})
}

// TestSec_Store_LabelsDecoderEagerCount feeds ReadLabels three separate hostile
// headers — one per eager allocation site (the string table, the node-record
// array, the edge-record array) — each declaring a count ONE PAST its
// implausibility ceiling with a truncated body.
//
// FIXED #1468 (string table) / #1467 (node + edge records): ReadLabels now
// clamps each eager reservation via capHint(count, labelsCapHintMax) and grows
// via append. A count past the ceiling (1<<30 for the string table, 1<<40 for
// the record arrays) is rejected with ErrLabelsCorrupted BEFORE the make is
// reached, and a count under the ceiling would allocate only the clamp. This
// pins the bounded-allocation property: a TotalAlloc delta inside
// boundedAllocBudget, never anything proportional to the hostile count.
func TestSec_Store_LabelsDecoderEagerCount(t *testing.T) {
	if secStoreRaceEnabled {
		t.Skip("assertBoundedAlloc reads process-global MemStats; unreliable and race-flagged under -race")
	}

	dec := func(b []byte) error {
		_, err := ReadLabels(bytes.NewReader(b))
		return err
	}

	// string table: count one past the 1<<30 string-table ceiling.
	t.Run("string-table", func(t *testing.T) {
		var b []byte
		b = secStorePutU32(b, labelsMagic)
		b = secStorePutU32(b, labelsFormatVersion)
		b = secStorePutU64(b, uint64(1<<30)+1) // stringCount > ceiling, no string bytes follow
		secStoreAssertBoundedCorruptReject(t, "labels/string-table", b, ErrLabelsCorrupted, dec)
	})

	// node records: count one past the 1<<40 record ceiling.
	t.Run("node-records", func(t *testing.T) {
		var b []byte
		b = secStorePutU32(b, labelsMagic)
		b = secStorePutU32(b, labelsFormatVersion)
		b = secStorePutU64(b, 0)               // empty string table
		b = secStorePutU64(b, uint64(1<<40)+1) // nodeCount > ceiling, no records follow
		secStoreAssertBoundedCorruptReject(t, "labels/node-records", b, ErrLabelsCorrupted, dec)
	})

	// edge records: count one past the 1<<40 record ceiling.
	t.Run("edge-records", func(t *testing.T) {
		var b []byte
		b = secStorePutU32(b, labelsMagic)
		b = secStorePutU32(b, labelsFormatVersion)
		b = secStorePutU64(b, 0)               // empty string table
		b = secStorePutU64(b, 0)               // zero node records
		b = secStorePutU64(b, uint64(1<<40)+1) // edgeCount > ceiling, no records follow
		secStoreAssertBoundedCorruptReject(t, "labels/edge-records", b, ErrLabelsCorrupted, dec)
	})
}

// TestSec_Store_PropertiesDecoderEagerCount feeds ReadProperties hostile
// headers for each of its eager allocation sites, each declaring a count one
// past its implausibility ceiling.
//
// FIXED #1468 (key table) / #1467 (node + edge records): ReadProperties now
// clamps each eager reservation via capHint(count, propertiesCapHintMax) and
// grows via append. A count past the ceiling (1<<30 for the key table, 1<<40
// for the record arrays) is rejected with ErrPropertiesCorrupted before the
// make. Bounded-allocation is asserted via assertBoundedAlloc.
func TestSec_Store_PropertiesDecoderEagerCount(t *testing.T) {
	if secStoreRaceEnabled {
		t.Skip("assertBoundedAlloc reads process-global MemStats; unreliable and race-flagged under -race")
	}

	dec := func(b []byte) error {
		_, err := ReadProperties(bytes.NewReader(b))
		return err
	}

	// key table: count one past the 1<<30 key-table ceiling.
	t.Run("key-table", func(t *testing.T) {
		var b []byte
		b = secStorePutU32(b, propertiesMagic)
		b = secStorePutU32(b, propertiesFormatVersion)
		b = secStorePutU64(b, uint64(1<<30)+1) // keyCount > ceiling, no key bytes follow
		secStoreAssertBoundedCorruptReject(t, "properties/key-table", b, ErrPropertiesCorrupted, dec)
	})

	// node records: count one past the 1<<40 record ceiling.
	t.Run("node-records", func(t *testing.T) {
		var b []byte
		b = secStorePutU32(b, propertiesMagic)
		b = secStorePutU32(b, propertiesFormatVersion)
		b = secStorePutU64(b, 0)               // empty key table
		b = secStorePutU64(b, uint64(1<<40)+1) // nodeCount > ceiling, no records follow
		secStoreAssertBoundedCorruptReject(t, "properties/node-records", b, ErrPropertiesCorrupted, dec)
	})

	// edge records: count one past the 1<<40 record ceiling.
	t.Run("edge-records", func(t *testing.T) {
		var b []byte
		b = secStorePutU32(b, propertiesMagic)
		b = secStorePutU32(b, propertiesFormatVersion)
		b = secStorePutU64(b, 0)               // empty key table
		b = secStorePutU64(b, 0)               // zero node records
		b = secStorePutU64(b, uint64(1<<40)+1) // edgeCount > ceiling, no records follow
		secStoreAssertBoundedCorruptReject(t, "properties/edge-records", b, ErrPropertiesCorrupted, dec)
	})
}

// TestSec_Store_MapperDecoderEagerCount feeds the two mapper readers a
// pairCount one past the 1<<40 ceiling with a truncated body.
//
// FIXED #1467: ReadMapperString clamps make([]MapperPair, ...) and
// ReadMapperBytes clamps make([]MapperRawPair, ...) via
// capHint(pairCount, mapperCapHintMax), growing via append. A count past the
// 1<<40 ceiling is rejected with ErrMapperCorrupted before the make.
// Bounded-allocation is asserted via assertBoundedAlloc.
func TestSec_Store_MapperDecoderEagerCount(t *testing.T) {
	if secStoreRaceEnabled {
		t.Skip("assertBoundedAlloc reads process-global MemStats; unreliable and race-flagged under -race")
	}

	// ReadMapperString — version 1 (string) layout.
	t.Run("string", func(t *testing.T) {
		var b []byte
		b = secStorePutU32(b, mapperMagic)
		b = binary.LittleEndian.AppendUint16(b, mapperFormatVersionString)
		b = secStorePutU64(b, uint64(1<<40)+1) // pairCount > ceiling, no pairs follow
		secStoreAssertBoundedCorruptReject(t, "mapper/string", b, ErrMapperCorrupted, func(p []byte) error {
			_, err := ReadMapperString(bytes.NewReader(p))
			return err
		})
	})

	// ReadMapperBytes — version 2 (codec) layout.
	t.Run("bytes", func(t *testing.T) {
		var b []byte
		b = secStorePutU32(b, mapperMagic)
		b = binary.LittleEndian.AppendUint16(b, mapperFormatVersionCodec)
		b = secStorePutU64(b, uint64(1<<40)+1) // pairCount > ceiling, no pairs follow
		secStoreAssertBoundedCorruptReject(t, "mapper/bytes", b, ErrMapperCorrupted, func(p []byte) error {
			_, err := ReadMapperBytes(bytes.NewReader(p))
			return err
		})
	})
}

// TestSec_Store_EdgeHandlesStringTableEagerCount feeds ReadEdgeHandles a
// label-string-table length one past the 1<<30 ceiling with a truncated body.
//
// FIXED #1468: readEdgeHandleStrTable now clamps make([]string, ...) via
// capHint(n, edgeHandlesCapHintMax), growing via append (the record loop was
// already clamped via edgeHandlesCapHintMax). A length past the 1<<30 ceiling
// is rejected with ErrEdgeHandlesCorrupted before the make. Bounded-allocation
// is asserted via assertBoundedAlloc.
func TestSec_Store_EdgeHandlesStringTableEagerCount(t *testing.T) {
	if secStoreRaceEnabled {
		t.Skip("assertBoundedAlloc reads process-global MemStats; unreliable and race-flagged under -race")
	}
	var b []byte
	b = secStorePutU32(b, edgeHandlesMagic)
	b = secStorePutU32(b, edgeHandlesFormatVersion)
	// First string table (labels): length one past the 1<<30 ceiling.
	b = secStorePutU64(b, uint64(1<<30)+1) // labelTableLen > ceiling, no string bytes follow
	secStoreAssertBoundedCorruptReject(t, "edgehandles/string-table", b, ErrEdgeHandlesCorrupted, func(p []byte) error {
		_, err := ReadEdgeHandles(bytes.NewReader(p))
		return err
	})
}

// TestSec_Store_BareReadCSREagerCount feeds the bare ReadCSR a vertex count
// one past the lowered backstop ceiling with a truncated body.
//
// FIXED #1469: the bare ReadCSR entry point cannot know the true
// remaining-bytes bound, so it falls back to the absolute backstop maxCSRCount.
// That backstop is now lowered to 1<<34 (≈ 128 GiB array) so the size-blind
// entry point cannot drive a multi-TiB make; a count past the backstop is
// rejected with ErrCSRCorrupted before the make([]uint64, nV). The
// precise-bound path (readCSRLimited with FileEntry.Size, via Open /
// LoadSnapshotFull) remains the primary defence and is covered by
// csr_manifest_bound_test.go. Bounded-allocation is asserted via
// assertBoundedAlloc.
func TestSec_Store_BareReadCSREagerCount(t *testing.T) {
	if secStoreRaceEnabled {
		t.Skip("assertBoundedAlloc reads process-global MemStats; unreliable and race-flagged under -race")
	}
	var b []byte
	b = secStorePutU64(b, uint64(maxCSRCount)+1) // nV > backstop ceiling
	b = secStorePutU64(b, 0)                     // nE
	b = append(b, 0, 0)                          // hasWeights = 0, weightSize = 0; no vertex bytes follow
	secStoreAssertBoundedCorruptReject(t, "csr/bare-readcsr", b, ErrCSRCorrupted, func(p []byte) error {
		_, err := ReadCSR(bytes.NewReader(p))
		return err
	})
}
