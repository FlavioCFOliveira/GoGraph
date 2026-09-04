package snapshot

import (
	"errors"
	"fmt"
)

// ErrFieldTooLong is the sentinel every snapshot length-prefix refusal wraps.
// A capture that would have to emit a field the matching reader is required to
// reject fails with it instead, so the caller sees a typed, testable error
// rather than a durable file nothing can load (rmp #2743).
//
// It is the snapshot-format counterpart of [github.com/FlavioCFOliveira/GoGraph/store/txn.ErrFieldTooLong],
// deliberately sharing that name: the two durable formats — the WAL and the
// snapshot — now refuse an over-long field in the same shape, with the same
// sentinel name and the same message layout, so an operator who has learned one
// has learned the other. Consistency across the two formats was judged worth
// more than a locally nicer name.
//
// It reaches the caller from every snapshot component writer, and from there
// out through the checkpointer, whose phase-2 writeSnapshot failure returns
// BEFORE the phase-3 WAL prefix truncation. That ordering is the whole point of
// the guard: the refusal happens at the one choke point where the WAL still
// holds the data, so an unwritable value costs a failed checkpoint and an
// un-reclaimed WAL, never a committed write.
var ErrFieldTooLong = errors.New("snapshot: field too long for the reader's cap")

// maxStringTableLen caps a single string-table entry — a property key, a label
// name, an edge-handle label or key — at 1 MiB.
//
// It is the WRITER's half of a bound the readers already enforced alone. Every
// string-table reader in this package rejects an entry longer than this
// (ReadProperties at properties.go, ReadLabels at labels.go,
// readEdgeHandleStrTable at edgehandles.go), while the writers checked only
// that the length fitted the uint32 prefix — 4096 times looser. A 2 MiB key
// therefore produced a byte-perfect, CRC-valid snapshot that its own reader was
// required to refuse. The constant exists so both halves name the same number
// and cannot drift apart again.
const maxStringTableLen = 1 << 20

// maxPerRecordCount caps the number of labels, or of properties, carried by a
// SINGLE edge-handle record at 1 Mi.
//
// It replaces two checks that could not fail: a uint32 count tested against
// edgeHandlesMaxCount (1<<40) is true for no value a uint32 can hold, so the
// comparison read as protection while providing none. This ceiling can fail —
// a crafted four-byte header declaring 2_000_000 labels on one edge handle is
// rejected — and it bounds the reader's per-record append loop, which
// previously grew unclamped for any count a sufficiently large body could
// satisfy. It is enforced on the write side too, so the pair stays symmetric.
//
// 1 Mi labels or properties on one edge handle is far beyond anything the
// engine can produce; the ceiling is slack by many orders of magnitude and
// exists to bound a hostile file, not to constrain a legitimate graph.
const maxPerRecordCount = 1 << 20

// checkSnapshotValueLen rejects an encoded property value whose byte length
// exceeds what the matching reader accepts ([maxValueLen], 1 GiB).
//
// The bound is the READER's cap, not the uint32 prefix's capacity. Aligning on
// the prefix — the pre-#2743 behaviour — let the writer emit values in
// [1 GiB, 4 GiB) that no reader in this package would ever load.
func checkSnapshotValueLen(what string, n int) error {
	if n > maxValueLen {
		return errFieldTooLong(what, n, maxValueLen)
	}
	return nil
}

// checkSnapshotStringLen is [checkSnapshotValueLen] for a string-table entry,
// bounded by [maxStringTableLen] (1 MiB).
func checkSnapshotStringLen(what string, n int) error {
	if n > maxStringTableLen {
		return errFieldTooLong(what, n, maxStringTableLen)
	}
	return nil
}

// checkSnapshotPerRecordCount rejects a per-edge-handle label or property count
// above [maxPerRecordCount], keeping the writer symmetric with the reader check
// that replaced the vacuous 1<<40 comparison.
func checkSnapshotPerRecordCount(what string, n int) error {
	if n > maxPerRecordCount {
		return errFieldTooLong(what, n, maxPerRecordCount)
	}
	return nil
}

// errFieldTooLong builds the refusal.
//
// It is a separate function, and not the body of the three checks above, so
// those stay within the inliner's budget: they sit on the snapshot capture path
// once per key, per label and per value, and must cost a compare and a
// well-predicted branch rather than a call. Mirrors the same split in
// store/txn, and verified the same way with `go build -gcflags='-m'`.
func errFieldTooLong(what string, n, maxLen int) error {
	return fmt.Errorf("%w: %s is %d bytes, maximum %d", ErrFieldTooLong, what, n, maxLen)
}

// checkMapperKeyLen rejects a natural-key entry longer than [maxMapperKeyLen]
// (1 GiB), the cap ReadMapper / ReadMapperBytes already enforced alone. The
// mapper keeps its own constant rather than sharing [maxValueLen] because the
// two bound different things — a key in the NodeID mapping versus a property
// value — and are free to diverge.
func checkMapperKeyLen(n int) error {
	if n > maxMapperKeyLen {
		return errFieldTooLong("mapper key", n, maxMapperKeyLen)
	}
	return nil
}

// checkSnapshotIndexDefString rejects an index-definition identifier longer
// than [indexDefsMaxStringLen] (64 KiB), the cap readIndexDefString already
// enforced alone.
func checkSnapshotIndexDefString(n int) error {
	if n > indexDefsMaxStringLen {
		return errFieldTooLong("index definition identifier", n, indexDefsMaxStringLen)
	}
	return nil
}
