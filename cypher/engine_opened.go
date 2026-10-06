package cypher

import "github.com/FlavioCFOliveira/GoGraph/store"

// NewEngineWithOpened creates a WAL-backed Engine over a store directory opened
// by [store.Open] / [store.OpenCtx]. It is the recommended constructor for a
// persisted store:
//
//	o, err := store.Open(dir, store.Options[string, float64]{
//		Codec:       txn.NewStringCodec(),
//		WeightCodec: txn.NewFloat64WeightCodec(),
//	})
//	if err != nil { return err }
//	defer o.Close()
//	eng := cypher.NewEngineWithOpened(o)
//
// It is [NewEngineWithStoreAndRecovery] applied to o.Store() and o.Recovery(),
// so the recovered schema constraints and index definitions are re-registered
// and each index is hydrated from its snapshot payload wherever recovery
// certified that safe. Taking the opened store whole, rather than its parts,
// keeps the recovery result from being dropped between the open and the
// engine.
//
// o must not be nil. The Engine does not own o: closing the store remains the
// caller's job, through o.Close.
func NewEngineWithOpened(o *store.Opened[string, float64]) *Engine {
	return NewEngineWithStoreAndRecovery(o.Store(), o.Recovery())
}
