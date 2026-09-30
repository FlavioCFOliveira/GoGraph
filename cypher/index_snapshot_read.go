package cypher

// index_snapshot_read.go — what a MATCH access path that reads a property index
// supplies so the operator can prove the index described its reader's snapshot,
// and answer from the snapshot when it cannot (rmp #2937).
//
// The operator-side contract, and the measured defect, are in
// cypher/exec/index_snapshot.go. This file builds the two things the operators
// take:
//
//   - [seekSnapshot], the proof and the reader's start instant, resolved once per
//     plan build from the view the build reads through ([indexSeekSnapshotOf]);
//   - [snapshotSeekResidual], the label check and the snapshot's own answer for
//     the equality and key-set seeks, which subsume the Selection they replace.
//
// Neither allocates when the proof holds beyond what the seek allocated before:
// the proof is the graph's index manager behind a pointer conversion, and the
// residual replaces the label-check closure the seek used to allocate.

import (
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// indexSnapshotDeclineCount counts the index reads a MATCH access path discarded
// because the index could not be proved to describe the reader's snapshot. It is
// a process-global, monotonic diagnostic seam read only by in-package tests,
// which snapshot it around a statement.
var indexSnapshotDeclineCount atomic.Uint64

// countedSnapshotProof is the graph's index manager as an [exec.SnapshotProof]
// that counts its refusals. A pointer conversion, so obtaining it allocates
// nothing.
type countedSnapshotProof index.Manager

// DescribesSnapshot implements [exec.SnapshotProof] through
// [index.Manager.DescribesSnapshot].
func (p *countedSnapshotProof) DescribesSnapshot(startTS uint64) bool {
	if (*index.Manager)(p).DescribesSnapshot(startTS) {
		return true
	}
	indexSnapshotDeclineCount.Add(1)
	return false
}

// trustedIndexProof is the proof a build uses when the engine's test seam
// disables the check: it vouches for every read, which is the behaviour before
// rmp #2937. It exists only so the regression tests can show that they detect the
// defect when the fix is removed.
type trustedIndexProof struct{}

// DescribesSnapshot implements [exec.SnapshotProof]; it always reports true.
func (trustedIndexProof) DescribesSnapshot(uint64) bool { return true }

// seekSnapshot is the proof a plan build hands to every index-driven access path,
// and the start instant of the snapshot the build reads at. The zero value
// carries no proof; an access path given it reads the index unguarded, which is
// the behaviour of a build with no snapshot-bound view (the public
// [BuildPlanWithMutator] path with a caller-supplied resolver).
type seekSnapshot struct {
	proof   exec.SnapshotProof
	startTS uint64
	// view is the reader's view, which the equality and key-set seeks read their
	// label check and their fallback through.
	view *lpg.ReadView[string, float64]
}

// guarded reports whether the access paths must ask for the proof.
func (s seekSnapshot) guarded() bool { return s.proof != nil }

// indexSeekSnapshotOf resolves the proof for a build that reads through g. A view
// with no snapshot reads the present, whose instant cannot be compared with the
// indexes', so it gets a proof that never holds. trustIndex is the engine's test
// seam that restores the unguarded behaviour; see [trustedIndexProof].
func indexSeekSnapshotOf(g *lpg.ReadView[string, float64], trustIndex bool) seekSnapshot {
	if g == nil {
		return seekSnapshot{}
	}
	if trustIndex {
		return seekSnapshot{proof: trustedIndexProof{}, view: g}
	}
	snap := g.Snapshot()
	if snap == nil {
		return seekSnapshot{proof: exec.UnprovableSnapshot{}, view: g}
	}
	return seekSnapshot{
		proof:   (*countedSnapshotProof)(g.IndexManager()),
		startTS: snap.StartTS(),
		view:    g,
	}
}

// residualFor returns the equality or key-set seek residual for the (label, key)
// pair on this build's view, or nil when the build carries no proof or the seek
// replaces an unlabelled scan, which has no label scan to fall back to.
func (s seekSnapshot) residualFor(label, key string) *snapshotSeekResidual {
	if !s.guarded() || s.view == nil || label == "" || key == "" {
		return nil
	}
	return &snapshotSeekResidual{view: s.view, label: label, key: key}
}

// snapshotSeekResidual implements [exec.SeekResidual] for an equality or key-set
// seek over (label, key), reading through the reader's view. It belongs to one
// operator and is not safe for concurrent use.
type snapshotSeekResidual struct {
	view  *lpg.ReadView[string, float64]
	label string
	key   string
}

// Admit implements [exec.SeekResidual]: the node carries the label at the
// reader's snapshot (rmp #2423).
func (r *snapshotSeekResidual) Admit(nodeID uint64) bool {
	return r.view.HasNodeLabelByID(graph.NodeID(nodeID), r.label)
}

// AppendMatching implements [exec.SeekResidual]: it walks the label's bitmap at
// the reader's snapshot — what the replaced NodeByLabelScan emits — and keeps the
// nodes whose property equals a key under openCypher `=`, which is the filter the
// seek subsumed. The bitmap iterates in ascending node-id order, the order the
// index's posting lists have.
func (r *snapshotSeekResidual) AppendMatching(keys []expr.Value, dst []uint64) []uint64 {
	src := lpgLabelResolver{g: r.view}
	it := src.ResolveLabelBitmap(r.label).Iterator()
	for it.HasNext() {
		id := it.Next()
		pv, ok := r.view.NodePropertyByID(graph.NodeID(id), r.key)
		if !ok {
			continue
		}
		for _, k := range keys {
			if snapshotSeekKeyEquals(pv, k) {
				dst = append(dst, id)
				break
			}
		}
	}
	return dst
}

// snapshotSeekKeyEquals reports whether a stored property equals a seek key under
// openCypher `=`: a NULL key equals nothing, and a string key is compared without
// converting the stored value, since only a non-temporal string can equal it.
func snapshotSeekKeyEquals(pv lpg.PropertyValue, k expr.Value) bool {
	if k == nil || k.Kind() == expr.KindNull {
		return false
	}
	if ks, isStr := k.(expr.StringValue); isStr {
		if pv.Kind() != lpg.PropString {
			return false
		}
		s, ok := pv.String()
		if !ok || s != string(ks) {
			return false
		}
		_, isTemporal := decodeTemporalString(s)
		return !isTemporal
	}
	return expr.IsTruthy(lpgPropToExpr(pv).Equal(k))
}
