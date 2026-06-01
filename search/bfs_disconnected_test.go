package search

// Task 574: BFS on a disconnected forest.
//
// Three separate components are built in a single directed adjlist:
//
//   - Component A: directed path 0→1→2→3  (4 nodes)
//   - Component B: star with centre 10, leaves 11,12,13  (4 nodes)
//   - Component C: complete graph on {20,21,22}  (3 nodes)
//
// BFS from node 0 must visit exactly the 4 nodes of component A.
// Nodes in B and C must not appear in the distance map.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

func TestBFS_DisconnectedForest_OnlySourceComponent(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})

	// Component A: directed path 0→1→2→3.
	compA := []int{0, 1, 2, 3}
	for _, v := range compA {
		if err := a.AddNode(v); err != nil {
			t.Fatalf("AddNode(%d): %v", v, err)
		}
	}
	pathEdges := [][2]int{{0, 1}, {1, 2}, {2, 3}}
	for _, e := range pathEdges {
		if err := a.AddEdge(e[0], e[1], 0); err != nil {
			t.Fatalf("AddEdge(%d→%d): %v", e[0], e[1], err)
		}
	}

	// Component B: directed star, centre=10 outgoing to 11,12,13.
	compB := []int{10, 11, 12, 13}
	for _, v := range compB {
		if err := a.AddNode(v); err != nil {
			t.Fatalf("AddNode(%d): %v", v, err)
		}
	}
	for _, leaf := range []int{11, 12, 13} {
		if err := a.AddEdge(10, leaf, 0); err != nil {
			t.Fatalf("AddEdge(10→%d): %v", leaf, err)
		}
	}

	// Component C: complete directed graph on {20, 21, 22}.
	compC := []int{20, 21, 22}
	for _, v := range compC {
		if err := a.AddNode(v); err != nil {
			t.Fatalf("AddNode(%d): %v", v, err)
		}
	}
	for _, u := range compC {
		for _, v := range compC {
			if u == v {
				continue
			}
			if err := a.AddEdge(u, v, 0); err != nil {
				t.Fatalf("AddEdge(%d→%d): %v", u, v, err)
			}
		}
	}

	c := csr.BuildFromAdjList(a)
	srcID, ok := a.Mapper().Lookup(0)
	if !ok {
		t.Fatalf("key 0 not found in mapper")
	}
	dist := make(map[int]int, 4)
	BFS(c, srcID, func(node graph.NodeID, d int) bool {
		v, vok := a.Mapper().Resolve(node)
		if !vok {
			t.Errorf("resolve failed for NodeID %d", node)
			return false
		}
		dist[v] = d
		return true
	})

	const wantVisited = 4
	if len(dist) != wantVisited {
		t.Fatalf("visited %d nodes, want %d (component A only)", len(dist), wantVisited)
	}

	// Verify distances within component A.
	wantDist := map[int]int{0: 0, 1: 1, 2: 2, 3: 3}
	for v, want := range wantDist {
		if got, found := dist[v]; !found {
			t.Errorf("node %d not visited", v)
		} else if got != want {
			t.Errorf("dist[%d] = %d, want %d", v, got, want)
		}
	}

	// Components B and C must not appear.
	for _, v := range compB {
		if _, found := dist[v]; found {
			t.Errorf("node %d should not be reachable from 0 (different component)", v)
		}
	}
	for _, v := range compC {
		if _, found := dist[v]; found {
			t.Errorf("node %d should not be reachable from 0 (different component)", v)
		}
	}
}
