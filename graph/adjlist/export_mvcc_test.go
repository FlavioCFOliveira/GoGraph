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
	return beginTxW(ws).ID()
}

// beginTxW is [beginTx] returning the transaction itself, for the writes that
// belong to it: since rmp #2967 a write carrying no transaction is its own
// transaction and never joins the one the slot names, so a test transaction's
// writes go through [AdjList.Writer] over the returned handle.
func beginTxW(ws *mvcc.WriteStamp) mvcc.Tx {
	var id uint64
	if c := ws.Clock(); c != nil {
		id = c.NextTxID()
	}
	st := &mvcc.TxState{}
	st.Arm(id)
	ws.Publish(st)
	return mvcc.NewTx(st)
}
