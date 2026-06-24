package sim

import (
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// ssspParallelWorkers is the worker count pinned for the parallel APSP variants.
// It is fixed so the serial-vs-parallel comparison is exact and reproducible: a
// varying worker count would change reduction order and (for floating weights)
// could make bit-identity a false-positive generator. The weights here are
// integer-valued, so the min-plus reductions are exact regardless, but pinning
// keeps the contract explicit.
const ssspParallelWorkers = 4

// ssspViolations runs the weighted shortest-path battery on the graph. SSSP
// (Dijkstra, Bellman-Ford, bidirectional Dijkstra, A*) from a few deterministic
// sources is compared on the DISTANCE MAP — never path identity, which is not
// unique — against an independent naive Bellman-Ford reference. APSP
// (Floyd-Warshall and Johnson, serial and parallel; plus Dijkstra-APSP) is
// compared to the same reference and serial-vs-parallel for exact agreement,
// gated by the node cap because it materialises an O(n^2) result.
func ssspViolations(tick int64, g *nameGraph) []Violation {
	n := len(g.names)
	if n == 0 {
		return nil
	}
	c := g.toWeightedCSR()
	var vs []Violation

	for _, src := range g.checkSources() {
		dist, reach := g.naiveSSSP(src)
		if d, err := search.Dijkstra(c, graph.NodeID(src)); err != nil {
			vs = append(vs, searchDeviation(tick, "Dijkstra", err))
		} else {
			vs = append(vs, compareDistances(tick, g, "Dijkstra", src, d, dist, reach)...)
		}
		if d, err := search.BellmanFord(c, graph.NodeID(src)); err != nil {
			vs = append(vs, searchDeviation(tick, "BellmanFord", err))
		} else {
			vs = append(vs, compareDistances(tick, g, "BellmanFord", src, d, dist, reach)...)
		}
		vs = append(vs, pointToPointViolations(tick, g, c, src, dist, reach)...)
	}

	if n <= maxDenseCheckNodes {
		vs = append(vs, apspViolations(tick, g, c)...)
	}
	return vs
}

// compareDistances asserts a Distances result agrees with the reference on
// reachability and (when reachable) the exact distance, for every node.
func compareDistances(tick int64, g *nameGraph, algo string, src int, d *search.Distances[float64], dist []float64, reach []bool) []Violation {
	for v := 0; v < len(g.names); v++ {
		got, ok := d.Distance(graph.NodeID(v))
		if ok != reach[v] {
			return ssspDiverge(tick, algo, fmt.Sprintf("reachability %q->%q: got %v want %v",
				g.names[src], g.names[v], ok, reach[v]))
		}
		if ok && got != dist[v] {
			return ssspDiverge(tick, algo, fmt.Sprintf("distance %q->%q: got %v want %v",
				g.names[src], g.names[v], got, dist[v]))
		}
	}
	return nil
}

// pointToPointViolations checks the point-to-point algorithms (bidirectional
// Dijkstra and A* with an admissible zero heuristic) against the reference: an
// unreachable target must yield ErrNoPath, a reachable one the exact cost.
func pointToPointViolations(tick int64, g *nameGraph, c *csr.CSR[float64], src int, dist []float64, reach []bool) []Violation {
	var vs []Violation
	zeroH := func(graph.NodeID) float64 { return 0 }
	for _, dst := range g.checkSources() {
		if dst == src {
			continue
		}
		_, cost, err := search.BidirectionalDijkstra(c, graph.NodeID(src), graph.NodeID(dst))
		vs = append(vs, comparePointToPoint(tick, g, "BiDijkstra", src, dst, cost, err, dist, reach)...)
		_, costA, errA := search.AStar(c, graph.NodeID(src), graph.NodeID(dst), zeroH)
		vs = append(vs, comparePointToPoint(tick, g, "AStar", src, dst, costA, errA, dist, reach)...)
	}
	return vs
}

// comparePointToPoint folds a point-to-point (cost, err) outcome against the
// reachability/distance reference for the (src,dst) pair.
func comparePointToPoint(tick int64, g *nameGraph, algo string, src, dst int, cost float64, err error, dist []float64, reach []bool) []Violation {
	if !reach[dst] {
		if !errors.Is(err, search.ErrNoPath) {
			return ssspDiverge(tick, algo, fmt.Sprintf("%q->%q unreachable but err=%v", g.names[src], g.names[dst], err))
		}
		return nil
	}
	if err != nil {
		return ssspDiverge(tick, algo, fmt.Sprintf("%q->%q reachable but returned err=%v", g.names[src], g.names[dst], err))
	}
	if cost != dist[dst] {
		return ssspDiverge(tick, algo, fmt.Sprintf("%q->%q cost: got %v want %v", g.names[src], g.names[dst], cost, dist[dst]))
	}
	return nil
}

// apspViolations cross-checks the all-pairs algorithms against the naive
// reference and serial-vs-parallel for exact agreement, over edge-incident nodes
// only (APSP excludes non-live slots by contract).
func apspViolations(tick int64, g *nameGraph, c *csr.CSR[float64]) []Violation {
	n := len(g.names)
	ref := make([][]float64, n)
	refReach := make([][]bool, n)
	for u := 0; u < n; u++ {
		ref[u], refReach[u] = g.naiveSSSP(u)
	}
	fw := search.FloydWarshall(c)
	fwp := search.FloydWarshallParallel(c, ssspParallelWorkers)
	jo, err := search.JohnsonAPSP(c)
	if err != nil {
		return []Violation{searchDeviation(tick, "JohnsonAPSP", err)}
	}
	jop, err := search.JohnsonAPSPParallel(c, ssspParallelWorkers)
	if err != nil {
		return []Violation{searchDeviation(tick, "JohnsonAPSPParallel", err)}
	}
	dap, err := search.DijkstraAPSP(c)
	if err != nil {
		return []Violation{searchDeviation(tick, "DijkstraAPSP", err)}
	}

	incident := g.incidentMask()
	for u := 0; u < n; u++ {
		if !incident[u] {
			continue
		}
		for v := 0; v < n; v++ {
			if !incident[v] {
				continue
			}
			if vio := apspAt(tick, g, "FloydWarshall", fw, u, v, ref[u][v], refReach[u][v]); vio != nil {
				return vio
			}
			if vio := apspAt(tick, g, "Johnson", jo, u, v, ref[u][v], refReach[u][v]); vio != nil {
				return vio
			}
			if vio := apspAt(tick, g, "DijkstraAPSP", dap, u, v, ref[u][v], refReach[u][v]); vio != nil {
				return vio
			}
			if vio := apspExact(tick, g, "FloydWarshall", fw, fwp, u, v); vio != nil {
				return vio
			}
			if vio := apspExact(tick, g, "Johnson", jo, jop, u, v); vio != nil {
				return vio
			}
		}
	}
	return nil
}

// apspAt compares one APSP cell against the reference.
func apspAt(tick int64, g *nameGraph, algo string, a *search.APSP[float64], u, v int, want float64, wantReach bool) []Violation {
	got, ok := a.At(graph.NodeID(u), graph.NodeID(v))
	if ok != wantReach {
		return ssspDiverge(tick, algo, fmt.Sprintf("APSP reach %q->%q: got %v want %v", g.names[u], g.names[v], ok, wantReach))
	}
	if ok && got != want {
		return ssspDiverge(tick, algo, fmt.Sprintf("APSP dist %q->%q: got %v want %v", g.names[u], g.names[v], got, want))
	}
	return nil
}

// apspExact asserts the serial and parallel variants of an APSP algorithm agree
// exactly on one cell.
func apspExact(tick int64, g *nameGraph, algo string, a, b *search.APSP[float64], u, v int) []Violation {
	ga, oka := a.At(graph.NodeID(u), graph.NodeID(v))
	gb, okb := b.At(graph.NodeID(u), graph.NodeID(v))
	if oka != okb || (oka && ga != gb) {
		return ssspDiverge(tick, algo+"Parallel",
			fmt.Sprintf("serial vs parallel disagree %q->%q: serial=(%v,%v) parallel=(%v,%v)",
				g.names[u], g.names[v], ga, oka, gb, okb))
	}
	return nil
}

// ssspDiverge builds a single shortest-path divergence violation.
func ssspDiverge(tick int64, algo, msg string) []Violation {
	return []Violation{{Kind: ViolationSearchDivergence, Tick: tick, Op: "search:" + algo, Message: msg}}
}
