package exec

// index_snapshot.go — the proof an index-driven access path asks for before it
// treats what the index returned as its reader's answer (rmp #2937).
//
// # The defect
//
// A property index is written when a transaction commits and is read at the
// present. A reader runs at a snapshot. When a peer commits a change to the
// indexed property after the reader's snapshot was taken, the index already
// describes that commit and the reader's snapshot does not: an equality seek for
// the old value misses a node the snapshot still holds under it, and a seek for
// the new value returns a node the snapshot holds under the old one. Measured at
// HEAD before this change, on both an explicit read transaction and an explicit
// write transaction: after a peer's committed `SET n.s = 'w'`, the reader's
// `MATCH (n:L {s: 'v'})` returned 0 rows and `MATCH (n:L {s: 'w'})` returned 1,
// while the label scan returned 1 and 0.
//
// # The proof, and what is done without it
//
// [SnapshotProof] is asked AFTER the lookup, with the reader's start instant. It
// is satisfied by the index manager's DescribesSnapshot, whose derivation is on
// that method: true proves the indexes held exactly the commits the snapshot
// sees while they were read. The operator then uses the lookup as it always has,
// allocation-free.
//
// Without the proof the operator answers from the snapshot instead:
//
//   - The equality seek and the key-set seek SUBSUME the Selection they replace,
//     so they carry a [SeekResidual] that enumerates the replaced label scan at
//     the snapshot and keeps the nodes whose property equals a key under
//     openCypher equality — exactly what the scan+filter would have returned.
//   - The range, prefix and intersection scans keep the original predicate as a
//     residual Filter above them, so they emit the replaced label scan's bitmap
//     (their label restriction) and let that Filter decide.
//   - The index nested-loop join takes its existing per-row fallback, which drives
//     the inner label scan and applies the join equality to every row.
//
// Declining is always correct and never cheaper: it costs the label scan the
// index was chosen to avoid. The proof is process-wide, so a commit to any index
// after the reader began makes it decline.

import (
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// SnapshotProof proves, when asked after an index read, that the indexes held
// exactly the commits a snapshot started at startTS sees while they were read.
// The graph's index manager satisfies it. Implementations must be safe for
// concurrent use.
type SnapshotProof interface {
	DescribesSnapshot(startTS uint64) bool
}

// UnprovableSnapshot is a [SnapshotProof] that never proves anything. It is the
// proof of a reader with no snapshot, whose instant cannot be compared with the
// indexes'. It is a stateless value and safe for concurrent use.
type UnprovableSnapshot struct{}

// DescribesSnapshot implements [SnapshotProof]; it always reports false.
func (UnprovableSnapshot) DescribesSnapshot(uint64) bool { return false }

// SeekResidual is what an equality or key-set seek that subsumes a labelled scan
// and its equality filter needs from the reader's view.
//
// Admit is the label residual every index candidate must pass (rmp #2423).
// AppendMatching is the snapshot's own answer, used when [SnapshotProof] fails:
// it appends to dst, in ascending node-id order, every node the replaced label
// scan emits at the reader's snapshot whose property equals one of keys under
// openCypher equality, and returns the extended slice.
//
// A SeekResidual belongs to one operator tree and is not required to be safe for
// concurrent use.
type SeekResidual interface {
	Admit(nodeID uint64) bool
	AppendMatching(keys []expr.Value, dst []uint64) []uint64
}

// snapshotGuard is the proof an index-driven operator asks for after its lookup.
// The zero value asks nothing, which keeps an operator built without one exactly
// as it was.
type snapshotGuard struct {
	proof   SnapshotProof
	startTS uint64
}

// declines reports whether a proof was requested and is not given. It must be
// called after the index read it vouches for.
func (g *snapshotGuard) declines() bool {
	return g.proof != nil && !g.proof.DescribesSnapshot(g.startTS)
}
