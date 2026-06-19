package main

// residual is a small Dinic max-flow solver kept inside the example so it
// can expose the settled residual graph — which is what lets the example
// derive the minimum cut (the library's flow.Network does not expose its
// residual). It is deterministic: BFS and the blocking-flow DFS visit
// arcs in stable insertion order, so the same input always yields the same
// flow assignment and the same residual reachability.
//
// The graph is stored sparsely (an arc list plus per-node head indices),
// so memory is O(V + E) rather than the O(V^2) of a dense capacity matrix.
// This keeps the example usable at the scales its flags reach.
type residual struct {
	n    int
	head [][]int // head[u] = indices into arcs of the arcs leaving u
	arcs []arc
}

// arc is one directed residual arc. to is the destination node, cap the
// remaining capacity, and rev the index in arcs of the paired reverse arc
// (so pushing flow on one arc credits its reverse in O(1)).
type arc struct {
	to  int
	cap int
	rev int
}

func newResidual(n int) *residual {
	return &residual{n: n, head: make([][]int, n)}
}

// addUndirected adds an undirected capacitated link as two opposing
// directed arcs of equal capacity, each able to carry the link's full
// capacity in its own direction. The two arcs are mutual reverses.
func (r *residual) addUndirected(u, v, c int) {
	i := len(r.arcs)
	r.arcs = append(r.arcs,
		arc{to: v, cap: c, rev: i + 1},
		arc{to: u, cap: c, rev: i},
	)
	r.head[u] = append(r.head[u], i)
	r.head[v] = append(r.head[v], i+1)
}

// maxFlow runs Dinic from src to snk, mutating the residual capacities in
// place, and returns the total flow. After it returns, the residual graph
// is settled and minCut can read the source side of the cut out of it.
func (r *residual) maxFlow(src, snk int) int {
	if src == snk {
		return 0
	}
	total := 0
	level := make([]int, r.n)
	iter := make([]int, r.n)
	for r.bfsLevels(src, snk, level) {
		for i := range iter {
			iter[i] = 0
		}
		for {
			pushed := r.dfsAugment(src, snk, 1<<30, level, iter)
			if pushed == 0 {
				break
			}
			total += pushed
		}
	}
	return total
}

// bfsLevels labels each node with its BFS distance from src over arcs with
// spare capacity, returning whether snk is reachable. A fresh level graph
// is built per Dinic phase.
func (r *residual) bfsLevels(src, snk int, level []int) bool {
	for i := range level {
		level[i] = -1
	}
	level[src] = 0
	queue := []int{src}
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		for _, ai := range r.head[u] {
			a := r.arcs[ai]
			if a.cap > 0 && level[a.to] < 0 {
				level[a.to] = level[u] + 1
				queue = append(queue, a.to)
			}
		}
	}
	return level[snk] >= 0
}

// dfsAugment pushes flow along one level-respecting path from u to snk,
// bounded by f, returning the amount pushed (0 if no augmenting path
// remains from u in the current level graph). iter advances the per-node
// arc cursor so each arc is considered at most once per phase.
func (r *residual) dfsAugment(u, snk, f int, level, iter []int) int {
	if u == snk {
		return f
	}
	for ; iter[u] < len(r.head[u]); iter[u]++ {
		ai := r.head[u][iter[u]]
		a := r.arcs[ai]
		if a.cap <= 0 || level[a.to] != level[u]+1 {
			continue
		}
		d := min(f, a.cap)
		pushed := r.dfsAugment(a.to, snk, d, level, iter)
		if pushed > 0 {
			r.arcs[ai].cap -= pushed
			r.arcs[a.rev].cap += pushed
			return pushed
		}
	}
	return 0
}

// minCut derives the minimum cut from the settled residual graph. The set
// R of nodes still reachable from src over arcs with spare capacity is the
// source side of the cut; every original link with exactly one endpoint in
// R is saturated and forms the bottleneck. It returns those links (in
// link-list order, so the output is stable) and their total capacity,
// which equals the max flow.
func (r *residual) minCut(src int, links []link) (cut []link, cutCap int) {
	reachable := make([]bool, r.n)
	reachable[src] = true
	queue := []int{src}
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		for _, ai := range r.head[u] {
			a := r.arcs[ai]
			if a.cap > 0 && !reachable[a.to] {
				reachable[a.to] = true
				queue = append(queue, a.to)
			}
		}
	}
	for _, l := range links {
		if reachable[l.a] != reachable[l.b] {
			cut = append(cut, l)
			cutCap += l.cap
		}
	}
	return cut, cutCap
}
