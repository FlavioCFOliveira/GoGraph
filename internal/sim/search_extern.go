package sim

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
	"github.com/FlavioCFOliveira/GoGraph/search/centrality"
	"github.com/FlavioCFOliveira/GoGraph/search/extern"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
)

// externSalt keeps the external-memory check's draw stream disjoint from the
// other per-tick search checks.
const externSalt uint64 = 0xe47e_2b91_0c5d_a6f3

// externFixtures is how many graphs the external-memory check serialises and
// probes per tick. Kept to 2 so the per-check disk I/O (a small csrfile written
// and mmap-opened) stays bounded.
const externFixtures = 2

// externViolations exercises the semi-external algorithms [extern.BFS] and
// [extern.PageRank], which operate directly on an mmap-backed
// [csrfile.Reader] and which the rest of the DST never drives. For each fixture
// it serialises the in-memory CSR to a temporary csrfile, opens a Reader over
// it, and cross-checks:
//
//   - extern.BFS's reachable set against the in-memory [search.BFS] reachable
//     set from the same source (both stream the identical adjacency, so the
//     reachable sets must be equal);
//   - extern.PageRank's rank vector against the in-memory [centrality.PageRank]
//     within [pagerankEpsilon] (both implement the same damped model with
//     dangling-mass redistribution, one streaming from the mmap and one in RAM).
//
// The fixture is a directed graph with a cycle and a dangling sink (reusing the
// PageRank generator), so the dangling and teleport paths are exercised. A
// setup/I/O failure is surfaced as a ViolationOracleDeviation so a broken
// harness environment is not silently ignored. The Reader is always closed and
// the temporary file removed, so the check leaks no file descriptor or mmap.
func externViolations(tick int64) []Violation {
	seed := NewSeed(uint64(tick) ^ externSalt)

	dir, err := os.MkdirTemp("", "sim-extern-*")
	if err != nil {
		return []Violation{externDeviate(tick, fmt.Sprintf("tempdir: %v", err))}
	}
	defer func() { _ = os.RemoveAll(dir) }()

	var vs []Violation
	for i := 0; i < externFixtures; i++ {
		n, edges := pagerankGenGraph(seed)
		c := pagerankBuildCSR(n, edges)
		path := filepath.Join(dir, fmt.Sprintf("g%d.csr", i))
		vs = append(vs, externCheckOne(tick, path, n, c)...)
	}
	return vs
}

// externCheckOne serialises c to path, opens a Reader, and runs the two
// external-vs-in-memory cross-checks. It closes the Reader before returning.
func externCheckOne(tick int64, path string, n int, c *csr.CSR[float64]) []Violation {
	if _, err := csrfile.WriteToFile(path, c); err != nil {
		return []Violation{externDeviate(tick, fmt.Sprintf("WriteToFile: %v", err))}
	}
	r, err := csrfile.Open(path)
	if err != nil {
		return []Violation{externDeviate(tick, fmt.Sprintf("Open: %v", err))}
	}
	defer func() { _ = r.Close() }()

	const src = 0
	var vs []Violation

	// --- extern.BFS reachable set vs in-memory search.BFS ---
	externSeen := make([]bool, n)
	if berr := extern.BFS(r, graph.NodeID(src), func(node graph.NodeID, _ int) bool {
		if int(node) < n {
			externSeen[int(node)] = true
		}
		return true
	}); berr != nil {
		vs = append(vs, externDeviate(tick, fmt.Sprintf("extern.BFS: %v", berr)))
	} else {
		memSeen := make([]bool, n)
		search.BFS(c, graph.NodeID(src), func(node graph.NodeID, _ int) bool {
			if int(node) < n {
				memSeen[int(node)] = true
			}
			return true
		})
		if got, want := boolsToSortedIDs(externSeen), boolsToSortedIDs(memSeen); !slices.Equal(got, want) {
			vs = append(vs, Violation{
				Kind: ViolationSearchDivergence, Tick: tick, Op: "search:extern.BFS",
				Message: fmt.Sprintf("extern.BFS reachable set (%d) disagrees with in-memory search.BFS (%d) from src=%d (n=%d)",
					len(got), len(want), src, n),
			})
		}
	}

	// --- extern.PageRank vs in-memory centrality.PageRank ---
	extRanks, _, perr := extern.PageRank(r, extern.DefaultPageRankOptions())
	if perr != nil {
		vs = append(vs, externDeviate(tick, fmt.Sprintf("extern.PageRank: %v", perr)))
		return vs
	}
	memRanks, _, merr := centrality.PageRank(c, centrality.DefaultPageRankOptions())
	if merr != nil {
		vs = append(vs, externDeviate(tick, fmt.Sprintf("centrality.PageRank: %v", merr)))
		return vs
	}
	if len(extRanks) != len(memRanks) {
		vs = append(vs, Violation{
			Kind: ViolationSearchDivergence, Tick: tick, Op: "search:extern.PageRank",
			Message: fmt.Sprintf("extern.PageRank length %d != centrality.PageRank length %d (n=%d)", len(extRanks), len(memRanks), n),
		})
		return vs
	}
	for v := range extRanks {
		if diff := extRanks[v] - memRanks[v]; diff > pagerankEpsilon || diff < -pagerankEpsilon {
			vs = append(vs, Violation{
				Kind: ViolationSearchDivergence, Tick: tick, Op: "search:extern.PageRank",
				Message: fmt.Sprintf("rank[%d] extern %.9f vs in-memory %.9f (n=%d, |diff|>%g)", v, extRanks[v], memRanks[v], n, pagerankEpsilon),
			})
			break
		}
	}
	return vs
}

// externDeviate builds an oracle-deviation violation for an external-memory
// setup or I/O failure (distinct from an algorithm divergence).
func externDeviate(tick int64, msg string) Violation {
	return Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "search:extern", Message: msg}
}
