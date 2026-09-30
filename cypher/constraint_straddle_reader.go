package cypher

// constraint_straddle_reader.go — the transaction state commit-time constraint
// validation reads (rmp #2936; see exec.ConstraintRegistry.ValidateStraddler).

import (
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// straddleReader implements [exec.StraddleReader] for an explicit transaction:
// the touched nodes are those its index buffer names plus those whose existence
// it changed (a deletion made before any index existed records no index
// change), and both states are read at the LATEST committed state, without and
// with its own writes. It is built once per validation and is not safe for
// concurrent use.
type straddleReader struct {
	nodes  []graph.NodeID
	before *lpg.ReadView[string, float64]
	after  *lpg.ReadView[string, float64]
}

func newStraddleReader(tx *ExplicitTx) *straddleReader {
	pend := tx.buf.Pending()
	seen := make(map[graph.NodeID]struct{}, len(pend))
	nodes := make([]graph.NodeID, 0, len(pend))
	for _, c := range pend {
		if c.IsEdgeChange() {
			continue
		}
		if _, ok := seen[c.Node]; ok {
			continue
		}
		seen[c.Node] = struct{}{}
		nodes = append(nodes, c.Node)
	}
	for _, id := range tx.eng.g.NodesLifeWrittenBy(tx.wtx) {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			nodes = append(nodes, id)
		}
	}
	return &straddleReader{
		nodes:  nodes,
		before: tx.eng.g.LatestViewOf(tx.wtx, false),
		after:  tx.eng.g.LatestViewOf(tx.wtx, true),
	}
}

// Touched implements [exec.StraddleReader].
func (r *straddleReader) Touched() []graph.NodeID { return r.nodes }

// Before implements [exec.StraddleReader].
func (r *straddleReader) Before(id graph.NodeID, label, prop string) (bool, lpg.PropertyValue) {
	return straddleState(r.before, id, label, prop)
}

// After implements [exec.StraddleReader].
func (r *straddleReader) After(id graph.NodeID, label, prop string) (bool, lpg.PropertyValue) {
	return straddleState(r.after, id, label, prop)
}

func straddleState(v *lpg.ReadView[string, float64], id graph.NodeID, label, prop string) (bool, lpg.PropertyValue) {
	if !v.Exists(id) || !v.HasNodeLabelByID(id, label) {
		return false, lpg.PropertyValue{}
	}
	pv, ok := v.NodePropertyByID(id, prop)
	if !ok {
		return true, lpg.PropertyValue{}
	}
	return true, pv
}
