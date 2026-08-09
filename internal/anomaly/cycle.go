package anomaly

// cycle.go — finding the cycles a phenomenon is defined over.
//
// # Why not "any cycle"
//
// Classifying a history needs more than "the graph has a cycle": the phenomena
// are distinguished by WHICH edges a cycle is made of, and the same graph can
// hold a permitted write-skew cycle and a forbidden lost-update cycle at once.
// So the search enumerates distinct simple cycles and hands each to
// [classifyCycle], rather than answering a yes/no reachability question.
//
// # Why it is bounded, and why the bound is loud
//
// The number of simple cycles in a graph is exponential in the worst case. A
// checker that hangs on a large history is a checker nobody attaches, so the
// enumeration is bounded — and hitting the bound sets [Report.Truncated], which
// renders as "VERDICT: INCONCLUSIVE". A bound that quietly turned a large
// history into a clean verdict would be the exact failure mode this whole
// package exists to remove: evidence that says nothing while looking like it
// says everything.
//
// The common case is cheap. A correct system's DSG is acyclic, Tarjan settles
// that in linear time, and the enumeration never runs at all.

import "sort"

// findCycles returns the distinct simple cycles of the DSG, and whether the
// search was truncated.
//
// Distinct means distinct as a SET of edges: the same cycle discovered from a
// different starting node is one cycle, not several.
func findCycles(g *DSG) ([][]Edge, bool) {
	var (
		out       [][]Edge
		seen      = make(map[string]struct{}, 16)
		truncated bool
	)
	for _, comp := range sccs(g) {
		if len(comp) == 1 && !hasSelfPath(g, comp[0]) {
			continue
		}
		in := make(map[TxID]struct{}, len(comp))
		for _, id := range comp {
			in[id] = struct{}{}
		}
		for _, start := range comp {
			if len(out) >= maxCyclesExamined {
				truncated = true
				return out, truncated
			}
			enumerateFrom(g, in, start, &out, seen, &truncated)
			if truncated {
				return out, truncated
			}
		}
	}
	return out, truncated
}

// enumerateFrom walks simple cycles that begin and end at start, restricted to
// the strongly connected component `in`.
//
// Only nodes greater than or equal to start are visited, which is the standard
// device for emitting each cycle from exactly one starting node rather than once
// per member.
func enumerateFrom(g *DSG, in map[TxID]struct{}, start TxID, out *[][]Edge, seen map[string]struct{}, truncated *bool) {
	path := make([]Edge, 0, maxCycleLength)
	onPath := map[TxID]bool{start: true}

	var walk func(cur TxID)
	walk = func(cur TxID) {
		if *truncated || len(*out) >= maxCyclesExamined {
			*truncated = len(*out) >= maxCyclesExamined
			return
		}
		if len(path) >= maxCycleLength {
			return
		}
		for _, e := range g.Out[cur] {
			if _, ok := in[e.To]; !ok || e.To < start {
				continue
			}
			if e.To == start {
				path = append(path, e)
				if key := cycleKey(path); key != "" {
					if _, dup := seen[key]; !dup {
						seen[key] = struct{}{}
						c := make([]Edge, len(path))
						copy(c, path)
						*out = append(*out, c)
					}
				}
				path = path[:len(path)-1]
				if len(*out) >= maxCyclesExamined {
					*truncated = true
					return
				}
				continue
			}
			if onPath[e.To] {
				continue
			}
			onPath[e.To] = true
			path = append(path, e)
			walk(e.To)
			path = path[:len(path)-1]
			onPath[e.To] = false
			if *truncated {
				return
			}
		}
	}
	walk(start)
}

// cycleKey canonicalises a cycle so the same edge set is emitted once. The
// rotation is normalised to start at the lowest transaction id, and the edges
// carry their dependency kind and key, so two cycles over the same transactions
// but different dependency kinds stay distinct — which matters, because the
// dependency kinds are exactly what the classification turns on.
func cycleKey(path []Edge) string {
	if len(path) == 0 {
		return ""
	}
	lo := 0
	for i := 1; i < len(path); i++ {
		if path[i].From < path[lo].From {
			lo = i
		}
	}
	b := make([]byte, 0, len(path)*24)
	for i := range path {
		e := path[(lo+i)%len(path)]
		b = appendUint(b, uint64(e.From))
		b = append(b, '-')
		b = append(b, e.Dep.String()...)
		b = append(b, '(')
		b = append(b, e.Key...)
		b = append(b, ')', '>')
		b = appendUint(b, uint64(e.To))
		b = append(b, ';')
	}
	return string(b)
}

func appendUint(b []byte, v uint64) []byte {
	if v == 0 {
		return append(b, '0')
	}
	var tmp [20]byte
	i := len(tmp)
	for v > 0 {
		i--
		tmp[i] = byte('0' + v%10)
		v /= 10
	}
	return append(b, tmp[i:]...)
}

// hasSelfPath reports whether a single-node component nonetheless carries a
// cycle, i.e. an edge from the node to itself. [BuildDSG] refuses to add one, so
// this is always false today; it is here because a future edge kind that DID
// allow self-dependence would otherwise be silently unreachable.
func hasSelfPath(g *DSG, id TxID) bool {
	for _, e := range g.Out[id] {
		if e.To == id {
			return true
		}
	}
	return false
}

// sccs returns the strongly connected components of the DSG, using Tarjan's
// algorithm, iteratively.
//
// Iteratively rather than recursively because a history from a long soak run has
// tens of thousands of transactions in one chain, and a recursive Tarjan would
// exhaust the goroutine stack on exactly the histories the checker exists for.
//
// Components come back with their members sorted, and in a deterministic order,
// so the whole classification is reproducible.
func sccs(g *DSG) [][]TxID {
	index := make(map[TxID]int, len(g.Nodes))
	low := make(map[TxID]int, len(g.Nodes))
	onStack := make(map[TxID]bool, len(g.Nodes))
	var stack []TxID
	var out [][]TxID
	next := 0

	type frame struct {
		node TxID
		edge int
	}

	for _, root := range g.Nodes {
		if _, done := index[root]; done {
			continue
		}
		work := []frame{{node: root}}
		index[root], low[root] = next, next
		next++
		stack = append(stack, root)
		onStack[root] = true

		for len(work) > 0 {
			f := &work[len(work)-1]
			edges := g.Out[f.node]
			if f.edge < len(edges) {
				w := edges[f.edge].To
				f.edge++
				if _, seen := index[w]; !seen {
					index[w], low[w] = next, next
					next++
					stack = append(stack, w)
					onStack[w] = true
					work = append(work, frame{node: w})
				} else if onStack[w] {
					if index[w] < low[f.node] {
						low[f.node] = index[w]
					}
				}
				continue
			}
			// Finished this node: pop it, propagate its lowlink, and close a
			// component when it is a root.
			v := f.node
			work = work[:len(work)-1]
			if len(work) > 0 {
				p := work[len(work)-1].node
				if low[v] < low[p] {
					low[p] = low[v]
				}
			}
			if low[v] == index[v] {
				var comp []TxID
				for {
					w := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					onStack[w] = false
					comp = append(comp, w)
					if w == v {
						break
					}
				}
				sort.Slice(comp, func(i, j int) bool { return comp[i] < comp[j] })
				out = append(out, comp)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}
