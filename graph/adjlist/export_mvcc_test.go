package adjlist

import "github.com/FlavioCFOliveira/GoGraph/graph/mvcc"

// WriteStampForTest exposes the shared write stamp so a test can drive a
// transaction's brackets directly, without the lpg layer that normally owns
// them.
func (a *AdjList[N, W]) WriteStampForTest() *mvcc.WriteStamp { return a.stamp }
