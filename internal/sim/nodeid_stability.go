package sim

// nodeid_stability.go — the DST "NodeID stability" oracle (WAL v2 step 3, rmp
// #3021 A, docs/design-wal-v2.md §9 step 3).
//
// Every commit marker names the exact node ids its transaction created, and
// recovery places each key at that id. So every node that is alive both before a
// crash and after its recovery must have the SAME id on both sides. A node alive
// before the crash and absent after it was not durable — the durability oracle
// owns that question — and is not compared here.

import (
	"fmt"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// liveNodeIDs returns key -> id for every node g holds alive now.
func liveNodeIDs(g *lpg.Graph[string, float64]) map[string]graph.NodeID {
	if g == nil {
		return nil
	}
	var pairs []struct {
		k  string
		id graph.NodeID
	}
	// Collected first and tested after the walk: the walk must not re-enter the
	// mapper (see graph.Mapper.Walk).
	g.AdjList().Mapper().Walk(func(id graph.NodeID, k string) bool {
		pairs = append(pairs, struct {
			k  string
			id graph.NodeID
		}{k, id})
		return true
	})
	out := make(map[string]graph.NodeID, len(pairs))
	for _, p := range pairs {
		if g.NodeExistsAsOf(p.id, nil) {
			out[p.k] = p.id
		}
	}
	return out
}

// checkNodeIDStability compares the ids of the nodes alive on both sides of a
// recovery and reports every one that moved. It returns the number compared.
func checkNodeIDStability(tick int64, before map[string]graph.NodeID, after *lpg.Graph[string, float64]) (int, []Violation) {
	now := liveNodeIDs(after)
	var moved []string
	compared := 0
	for k, id := range before {
		got, ok := now[k]
		if !ok {
			continue
		}
		compared++
		if got != id {
			moved = append(moved, fmt.Sprintf("%s: %d before the crash, %d after", k, uint64(id), uint64(got)))
		}
	}
	if len(moved) == 0 {
		return compared, nil
	}
	sort.Strings(moved)
	if len(moved) > 4 {
		moved = append(moved[:4], fmt.Sprintf("… %d more", len(moved)-4))
	}
	return compared, []Violation{{
		Kind:    ViolationACIDDurability,
		Tick:    tick,
		Op:      "<crash recovery: NodeID stability>",
		Message: fmt.Sprintf("NodeID stability: %d of %d surviving nodes changed id across recovery: %v", len(moved), compared, moved),
	}}
}
