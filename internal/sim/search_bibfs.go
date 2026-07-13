package sim

import (
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// bibfsViolations exercises bidirectional BFS point-to-point on the unweighted
// directed CSR of the live graph and cross-checks the result against the
// independent hop-distance reference [nameGraph.naiveBFSDistance].
//
// For every ordered (src, dst) pair drawn from the deterministic
// [nameGraph.checkSources] set it asserts:
//
//   - when dst is unreachable from src, both [search.BiBFS] and [search.BiBFSOn]
//     return [search.ErrNoPath];
//   - when reachable, the returned path is a real src..dst walk whose hop length
//     (len(path)-1) equals the plain-BFS shortest hop distance — bidirectional
//     BFS is documented to return the true unweighted shortest distance, so a
//     longer or shorter path is a divergence.
//
// [search.BiBFS] auto-builds the reverse adjacency internally for the directed
// CSR; [search.BiBFSOn] is additionally exercised with an explicitly-built
// reverse CSR (hoisted once via [csr.CSR.BuildReverse]) so the caller-provided
// reverse path is covered too. Both must agree with the reference. The path
// witness is not unique, so only its length is compared — never its identity.
func bibfsViolations(tick int64, g *nameGraph, c *csr.CSR[float64]) []Violation {
	n := len(g.names)
	if n == 0 {
		return nil
	}
	rev := c.BuildReverse()
	var vs []Violation
	for _, src := range g.checkSources() {
		dist := g.naiveBFSDistance(src)
		for _, dst := range g.checkSources() {
			want := dist[dst] // -1 when unreachable, else the true hop distance

			path, err := search.BiBFS(c, graph.NodeID(src), graph.NodeID(dst))
			vs = append(vs, bibfsCompare(tick, "BiBFS", g, src, dst, want, path, err)...)

			pathOn, errOn := search.BiBFSOn(c, rev, graph.NodeID(src), graph.NodeID(dst))
			vs = append(vs, bibfsCompare(tick, "BiBFSOn", g, src, dst, want, pathOn, errOn)...)
		}
	}
	return vs
}

// bibfsCompare folds one bidirectional-BFS (path, err) outcome against the
// hop-distance reference for the (src, dst) pair. want is -1 for an unreachable
// target (which must surface as ErrNoPath) or the exact shortest hop count.
func bibfsCompare(tick int64, algo string, g *nameGraph, src, dst, want int, path []graph.NodeID, err error) []Violation {
	if want < 0 {
		if !errors.Is(err, search.ErrNoPath) {
			return bibfsDiverge(tick, algo, fmt.Sprintf(
				"%q->%q unreachable but got path=%v err=%v", g.names[src], g.names[dst], path, err))
		}
		return nil
	}
	if err != nil {
		return bibfsDiverge(tick, algo, fmt.Sprintf(
			"%q->%q reachable (hop dist %d) but returned err=%v", g.names[src], g.names[dst], want, err))
	}
	if len(path) == 0 {
		return bibfsDiverge(tick, algo, fmt.Sprintf(
			"%q->%q reachable but returned empty path", g.names[src], g.names[dst]))
	}
	if int(path[0]) != src || int(path[len(path)-1]) != dst {
		return bibfsDiverge(tick, algo, fmt.Sprintf(
			"%q->%q path endpoints (%d..%d) are not src=%d dst=%d",
			g.names[src], g.names[dst], path[0], path[len(path)-1], src, dst))
	}
	if got := len(path) - 1; got != want {
		return bibfsDiverge(tick, algo, fmt.Sprintf(
			"%q->%q path hop length %d != BFS shortest distance %d", g.names[src], g.names[dst], got, want))
	}
	return nil
}

// bibfsDiverge builds a single bidirectional-BFS divergence violation.
func bibfsDiverge(tick int64, algo, msg string) []Violation {
	return []Violation{{Kind: ViolationSearchDivergence, Tick: tick, Op: "search:" + algo, Message: msg}}
}
