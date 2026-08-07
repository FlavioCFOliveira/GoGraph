package adjlist

import "github.com/FlavioCFOliveira/GoGraph/graph/mvcc"

// WriteStampForTest exposes the shared write stamp so a test can drive a
// transaction's brackets directly, without the lpg layer that normally owns
// them.
func (a *AdjList[N, W]) WriteStampForTest() *mvcc.WriteStamp { return a.stamp }

// beginTx opens a write window on ws for a fresh transaction and returns its id.
//
// The per-transaction state is allocated here and kept alive by the stamp's slot
// until [mvcc.WriteStamp.End] retracts it — which is exactly the ownership
// rmp #2301 established: the state belongs to the transaction, and the stamp only
// names the one currently writing. lpg recycles it from a pool; a test has no
// reason to.
func beginTx(ws *mvcc.WriteStamp) uint64 {
	var id uint64
	if c := ws.Clock(); c != nil {
		id = c.NextTxID()
	}
	st := &mvcc.TxState{}
	st.Arm(id)
	ws.Publish(st)
	return id
}
