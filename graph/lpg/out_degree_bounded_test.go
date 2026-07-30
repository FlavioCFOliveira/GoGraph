package lpg

// out_degree_bounded_test.go — rmp #2265: [Graph.OutDegreeBoundedByID] stops at
// the caller's limit, and the early stop counts LIVE edges only.
//
// # Why the liveness half is the dangerous half
//
// Threading the limit into the tombstone-aware walk is a cost fix. Charging the
// limit against the wrong thing would be a correctness bug, and a silent one: a
// node whose first adjacency slots all point at tombstoned neighbours has to be
// walked PAST those slots before its first live edge is reached, so a limit
// charged per SLOT would stop inside the dead run and report a degree of zero for
// a node that has edges.
//
// Every fixture here therefore lays the tombstoned slots down FIRST, which is the
// layout under which the two rules disagree, and every expectation is a literal
// hand-counted number rather than a comparison against another call — a
// capped-vs-uncapped differential would agree with itself if both were charged
// wrongly.

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// boundedDegreeFixture builds three hubs over one graph:
//
//	"h"       dead tombstoned out-edges FIRST, then live ones
//	"allDead" dead out-edges only
//	"clean"   live out-edges only
//
// All three are read under the same graph-wide tombstone state, which is the
// state the defect turns on: once ANY node is deleted, every untyped degree in
// the graph takes the walking path, including "clean"'s.
func boundedDegreeFixture(t *testing.T, dead, live int) *Graph[string, float64] {
	t.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})

	add := func(key string) {
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode(%s): %v", key, err)
		}
	}
	edge := func(src, dst string) {
		if err := g.AddEdge(src, dst, 1); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", src, dst, err)
		}
	}

	add("h")
	add("allDead")
	add("clean")

	// DEAD FIRST. The order of these calls is the order of the adjacency column,
	// and that order is the whole point of the fixture.
	for i := 0; i < dead; i++ {
		d := fmt.Sprintf("dead%d", i)
		add(d)
		edge("h", d)
		edge("allDead", d)
	}
	for i := 0; i < live; i++ {
		l := fmt.Sprintf("live%d", i)
		add(l)
		edge("h", l)
		edge("clean", l)
	}
	for i := 0; i < dead; i++ {
		g.RemoveNode(fmt.Sprintf("dead%d", i))
	}
	return g
}

// nodeID resolves key to its interned id, failing the test when it is absent.
func nodeID(t *testing.T, g *Graph[string, float64], key string) graph.NodeID {
	t.Helper()
	id, ok := g.AdjList().Mapper().Lookup(key)
	if !ok {
		t.Fatalf("node %q is not interned", key)
	}
	return id
}

// TestOutDegreeBoundedByID_CapCountsLiveEdgesOnly asserts literal degrees under
// every interesting cap, over a hub whose first five slots are tombstoned.
func TestOutDegreeBoundedByID_CapCountsLiveEdgesOnly(t *testing.T) {
	const (
		dead = 5
		live = 3
	)
	g := boundedDegreeFixture(t, dead, live)

	cases := []struct {
		name  string
		node  string
		limit int
		want  int
	}{
		// A cap of 1 must walk past all five dead slots and report 1. A cap charged
		// per slot would stop inside the dead run and report 0 — the correctness
		// defect this fix must not trade the cost defect for.
		{"hub, cap 1, must see past five dead slots", "h", 1, 1},
		{"hub, cap 2", "h", 2, 2},
		{"hub, cap 3 equals the live degree", "h", 3, 3},
		{"hub, cap above the live degree", "h", 99, 3},
		{"hub, uncapped", "h", maxInt, 3},
		{"hub, cap 0 inspects nothing", "h", 0, 0},
		{"hub, negative cap inspects nothing", "h", -1, 0},

		// Every out-edge dead: no cap can make this anything but zero.
		{"all-dead, cap 1", "allDead", 1, 0},
		{"all-dead, cap 99", "allDead", 99, 0},
		{"all-dead, uncapped", "allDead", maxInt, 0},

		// No dead neighbours of its own, but the GRAPH holds tombstones, so this
		// still takes the walking path rather than the O(1) column length.
		{"clean node, cap 1", "clean", 1, 1},
		{"clean node, cap 2", "clean", 2, 2},
		{"clean node, uncapped", "clean", maxInt, 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := g.OutDegreeBoundedByID(nodeID(t, g, tc.node), tc.limit)
			if !found {
				t.Fatalf("OutDegreeBoundedByID(%s, %d) reported the node as absent", tc.node, tc.limit)
			}
			if got != tc.want {
				t.Fatalf("OutDegreeBoundedByID(%s, %d) = %d, the hand-counted answer is %d.\n"+
					"The fixture lays %d TOMBSTONED slots before %d live ones, so a cap "+
					"charged per slot rather than per live edge stops inside the dead run.",
					tc.node, tc.limit, got, tc.want, dead, live)
			}
		})
	}
}

// TestOutDegreeBoundedByID_MatchesUncappedAtEveryCap sweeps every cap from 0 past
// the true degree and requires min(trueLiveDegree, cap) at each, across fixtures
// with and without leading tombstones — including the tombstone-free graph, whose
// O(1) path must apply the cap too.
func TestOutDegreeBoundedByID_MatchesUncappedAtEveryCap(t *testing.T) {
	for _, dead := range []int{0, 1, 7} {
		for _, live := range []int{0, 1, 4} {
			t.Run(fmt.Sprintf("dead=%d/live=%d", dead, live), func(t *testing.T) {
				g := boundedDegreeFixture(t, dead, live)
				hub := nodeID(t, g, "h")

				uncapped, found := g.OutDegreeByID(hub)
				if !found {
					t.Fatal("uncapped degree reported the hub as absent")
				}
				if uncapped != live {
					t.Fatalf("uncapped live degree = %d, the fixture built %d live edges "+
						"behind %d tombstoned ones", uncapped, live, dead)
				}
				for c := 0; c <= live+2; c++ {
					got, ok := g.OutDegreeBoundedByID(hub, c)
					if !ok {
						t.Fatalf("cap %d reported the hub as absent", c)
					}
					if want := min(live, c); got != want {
						t.Fatalf("cap %d gave %d, want min(%d, %d) = %d", c, got, live, c, want)
					}
				}
			})
		}
	}
}

// TestOutDegreeBoundedByID_AbsentNode pins the answer for an id this graph never
// interned: zero, reported as found, exactly as [Graph.OutDegreeByID] reports it.
// The two must agree, because one is now implemented in terms of the other.
func TestOutDegreeBoundedByID_AbsentNode(t *testing.T) {
	g := boundedDegreeFixture(t, 1, 1)
	const absent = graph.NodeID(9_999_999)

	unbounded, uok := g.OutDegreeByID(absent)
	bounded, bok := g.OutDegreeBoundedByID(absent, 1)
	if unbounded != bounded || uok != bok {
		t.Fatalf("absent node: OutDegreeByID = (%d, %v), OutDegreeBoundedByID = (%d, %v); "+
			"the bounded form is the implementation of the unbounded one and they cannot disagree",
			unbounded, uok, bounded, bok)
	}
	if unbounded != 0 {
		t.Fatalf("absent node reported degree %d, want 0", unbounded)
	}
}
