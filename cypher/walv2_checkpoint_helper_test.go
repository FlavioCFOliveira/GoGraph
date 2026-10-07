package cypher_test

import (
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// checkpointTestStore publishes a self-sufficient snapshot of g under dir and
// records it in the WAL control file as the start of recovery — a real
// checkpoint, with the store quiescent. Recovery then rebuilds g from the
// snapshot and replays only the frames written after it, which is what these
// tests used to model with a hand-written snapshot followed by a whole-WAL
// truncate. constraints, when non-nil, supplies the constraint set the snapshot
// must carry.
func checkpointTestStore(dir string, g *lpg.Graph[string, float64], w *wal.Writer, constraints func() []snapshot.ConstraintSpec) error {
	var mu sync.Mutex
	opts := []checkpoint.Option[string, float64]{
		checkpoint.WithMapperCodec[string, float64](txn.NewStringCodec()),
	}
	if constraints != nil {
		opts = append(opts, checkpoint.WithConstraintSpecs[string, float64](constraints))
	}
	return checkpoint.New[string, float64](checkpoint.Config{Dir: dir}, g, w, &mu, opts...).RunCheckpoint()
}
