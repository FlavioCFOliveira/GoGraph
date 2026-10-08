package sim

import "github.com/FlavioCFOliveira/GoGraph/store/wal"

// simTxnWALCounters returns the transaction frames and bytes w has appended:
// every frame minus the WAL control records ([wal.Stats.ControlFrames]).
//
// Control records are not transaction data. A node id reservation (ReserveIDs)
// is logged when the mapper first issues an id beyond its shard's reserved limit,
// whatever becomes of the transaction that asked for the id — a refused or
// rolled-back transaction may leave one — and the clean-close marks
// (NextIDsExact) belong to no transaction. This is PostgreSQL's NEXTOID
// semantics, which logs OID reservations regardless of transaction outcome (WAL
// v2 step 4, docs/design-wal-v2.md §5). The scenario contracts that a refused,
// doomed or abandoned transaction "appends nothing" are about transaction data,
// so they count transaction frames only.
func simTxnWALCounters(w *wal.Writer) (frames, bytes uint64) {
	s := w.Stats()
	return s.Frames - s.ControlFrames, s.Bytes - s.ControlBytes
}
