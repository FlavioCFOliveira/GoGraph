package sim

import (
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// sccViolations cross-checks the search package's Tarjan SCC against the
// double-reachability reference, comparing the induced partition up to component
// relabelling. fwd is the forward-reachability matrix (forwardReachAll).
func sccViolations(tick int64, g *nameGraph, c *csr.CSR[float64], fwd [][]bool) []Violation {
	got := componentsToSig(tarjanComponents(search.TarjanSCC(c)))
	want := componentsToSig(g.naiveSCC(fwd))
	if got != want {
		return []Violation{{
			Kind: ViolationSearchDivergence, Tick: tick, Op: "search:SCC",
			Message: "Tarjan SCC partition disagrees with the double-reachability reference",
		}}
	}
	return nil
}

// topoViolations checks TopologicalSort: on an acyclic graph it must return a
// valid order (validated, since the order is not unique); on a cyclic graph it
// must return search.ErrCycle. The acyclicity decision uses an independent
// three-colour DFS (g.isAcyclic), so the check does not trust the algorithm it
// is testing.
func topoViolations(tick int64, g *nameGraph, c *csr.CSR[float64]) []Violation {
	order, err := search.TopologicalSort(c)
	if !g.isAcyclic() {
		if !errors.Is(err, search.ErrCycle) {
			return []Violation{{
				Kind: ViolationSearchDivergence, Tick: tick, Op: "search:topo",
				Message: fmt.Sprintf("graph has a cycle but TopologicalSort did not return ErrCycle (err=%v)", err),
			}}
		}
		return nil
	}
	if err != nil {
		return []Violation{{
			Kind: ViolationSearchDivergence, Tick: tick, Op: "search:topo",
			Message: fmt.Sprintf("acyclic graph but TopologicalSort returned an error: %v", err),
		}}
	}
	return validateTopoOrder(tick, g, order)
}

// validateTopoOrder asserts the returned order is a valid topological order: a
// permutation of exactly the edge-incident nodes in which every directed edge
// goes forward.
func validateTopoOrder(tick int64, g *nameGraph, order []graph.NodeID) []Violation {
	n := len(g.names)
	pos := make([]int, n)
	for i := range pos {
		pos[i] = -1
	}
	for i, nid := range order {
		id := int(nid)
		if id < 0 || id >= n {
			return topoViolation(tick, fmt.Sprintf("order contains out-of-range NodeID %d", id))
		}
		if pos[id] != -1 {
			return topoViolation(tick, fmt.Sprintf("order contains duplicate NodeID %d (%q)", id, g.names[id]))
		}
		pos[id] = i
	}
	incident := g.incidentMask()
	want := 0
	for u := 0; u < n; u++ {
		switch {
		case incident[u]:
			want++
			if pos[u] == -1 {
				return topoViolation(tick, fmt.Sprintf("edge-incident node %q missing from the order", g.names[u]))
			}
		case pos[u] != -1:
			return topoViolation(tick, fmt.Sprintf("isolated node %q must not appear in the order", g.names[u]))
		}
	}
	if len(order) != want {
		return topoViolation(tick, fmt.Sprintf("order covers %d nodes, want %d edge-incident", len(order), want))
	}
	for u := 0; u < n; u++ {
		for _, v := range g.out[u] {
			if pos[u] >= pos[v] {
				return topoViolation(tick, fmt.Sprintf("edge %q->%q is not forward in the order (pos %d >= %d)",
					g.names[u], g.names[v], pos[u], pos[v]))
			}
		}
	}
	return nil
}

// topoViolation builds a single topological-sort divergence violation.
func topoViolation(tick int64, msg string) []Violation {
	return []Violation{{Kind: ViolationSearchDivergence, Tick: tick, Op: "search:topo", Message: msg}}
}

// tcViolations cross-checks the search package's TransitiveClosure against the
// forward-reachability reference, over edge-incident nodes only (the closure
// excludes non-live slots by contract). fwd is forwardReachAll.
func tcViolations(tick int64, g *nameGraph, c *csr.CSR[float64], fwd [][]bool) []Violation {
	tc := search.TransitiveClosure(c)
	n := len(g.names)
	incident := g.incidentMask()
	for u := 0; u < n; u++ {
		if !incident[u] {
			continue
		}
		for v := 0; v < n; v++ {
			if !incident[v] {
				continue
			}
			if got, want := tc.Reachable(graph.NodeID(u), graph.NodeID(v)), fwd[u][v]; got != want {
				return []Violation{{
					Kind: ViolationSearchDivergence, Tick: tick, Op: "search:TC",
					Message: fmt.Sprintf("TransitiveClosure.Reachable(%q,%q)=%v but the reference says %v",
						g.names[u], g.names[v], got, want),
				}}
			}
		}
	}
	return nil
}

// tarjanComponents converts the search package's [][]graph.NodeID SCC output to
// the [][]int form componentsToSig consumes.
func tarjanComponents(comps [][]graph.NodeID) [][]int {
	out := make([][]int, len(comps))
	for i, c := range comps {
		ids := make([]int, len(c))
		for j, nid := range c {
			ids[j] = int(nid)
		}
		out[i] = ids
	}
	return out
}
