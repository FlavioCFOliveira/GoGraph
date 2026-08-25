package sim

// varlen_paths.go — DST adjudication of the VARIABLE-LENGTH PATH surface
// (rmp #2463).
//
// Before this probe the simulator issued variable-length expansions only as
// `[:KNOWS*1..3]`, `[:KNOWS*1..4]`, `[*1..6]` and the unbounded form inside
// `shortestPath`. The two workload spellings project `length(p)` without ever
// checking it, so they are load, not adjudication: nothing in the DST held a
// plain VLE row count to an independent reference. Absent entirely were the
// exact form `*2`, the zero-length form `*0..n`, the lower-bound-only form
// `*2..`, a predicate over the intermediate nodes, multi-type VLE, and
// undirected VLE.
//
// This probe rides the pattern-shapes scenario because that scenario's oracle
// knows adjacency EXACTLY — the writer never re-CREATEs an existing
// (src,dst,label) edge, so a relationship's identity is its (src,dst,label)
// key — which is precisely what a variable-length reference has to compose.
// Every reference below is computed by trail enumeration over the oracle's edge
// sets; the engine is never asked what it thinks the answer is.
//
// # The contracts this probe pins (verified against the engine, and by hand)
//
// RELATIONSHIP UNIQUENESS. openCypher requires that a variable-length pattern
// never repeat a relationship within one path (nodes may repeat; relationships
// may not). The engine implements exactly that, as
// cypher/varlen_expand_cycle_test.go and cypher/varlen_expand_cycle_trap_test.go
// already pin on C4, diamond and lollipop fixtures. So the reference enumerates
// TRAILS (edge-distinct walks), not walks: a self-loop cannot be traversed
// twice, and `*2` over a lone self-loop is empty. Enumerating plain walks
// instead would overcount, which is what makes this reference sensitive to a
// Cyphermorphism regression — see TestVarlenPaths_ReferenceIsUniquenessSensitive.
//
// ZERO LENGTH. `*0..n` admits the length-0 binding, which binds the two
// endpoints to the SAME node and traverses no relationship. Verified on the
// hand fixture: `*0..0` returns one row per node matching the left-hand
// pattern; the far-side pattern still applies to that node (a `:Nope` label
// yields nothing); the row survives even when the relationship type does not
// exist at all (`[:NOSUCHTYPE*0..2]` returns exactly the node count); and its
// path functions are length 0, one node, zero relationships. `*..n` is `*1..n`,
// NOT `*0..n`.
//
// UNDIRECTED. An undirected variable-length pattern traverses each relationship
// in either direction and STILL applies relationship uniqueness per path. A
// self-loop contributes exactly ONE incidence (openCypher matches an undirected
// relationship once), which is the same rule the single-hop undirected
// reference in [computePatternShapeRefs] already encodes.
//
// MULTI-TYPE. `[:KNOWS|FOLLOWS*1..2]` treats each typed edge as a distinct
// relationship, so a (src,dst) pair carrying both types offers two distinct
// steps and uniqueness is applied over the typed key.
//
// No divergence from openCypher was found on any of these forms.
//
// # Enumeration bound
//
// Trail enumeration is exponential in the worst case, so every reference here
// is bounded twice over:
//
//   - the whole-graph probes never look deeper than [vleMaxDepth] hops, which
//     is the QUERY's own upper bound, so the depth limit costs no exactness;
//   - the anchored lower-bound-only probe (`*2..`, which has no upper bound)
//     enumerates from a SINGLE start node under both a depth cap
//     ([vleAnchoredDepthCap]) and a walk budget ([vleWalkBudget]), and REFUSES
//     to assert when either limit is reached. A reference that might be
//     truncated is skipped, never compared; [vleStats] records the skip and the
//     terminal gate fails a run in which the probe never once fired.
//
// Because the reference enumerates the identical trail set before the engine is
// asked, the guard bounds the engine's work too: a start whose closure the
// reference cannot afford is never put to the engine.

import (
	"context"
	"fmt"
	"slices"
)

// Query constants of the variable-length battery. Each is paired with a
// reference computed by [buildVLEModel] plus trail enumeration; the pairing is
// spelled out in [CheckVarlenPaths].
const (
	// tmplVLEExactDepth2 is the exact-depth form the simulator never issued:
	// exactly two hops, no shorter and no longer.
	tmplVLEExactDepth2 = "MATCH (a:Person)-[:KNOWS*2]->(b) RETURN count(*)"
	// tmplVLEExactDepth3 is the same shape one hop deeper, so an off-by-one in
	// the depth bookkeeping cannot hide behind a single depth.
	tmplVLEExactDepth3 = "MATCH (a:Person)-[:KNOWS*3]->(b) RETURN count(*)"
	// tmplVLEZeroOnly isolates the length-0 binding: one row per Person, bound
	// to itself, traversing nothing.
	tmplVLEZeroOnly = "MATCH (a:Person)-[:KNOWS*0..0]->(b) RETURN count(*)"
	// tmplVLEZeroIdentity proves the length-0 binding really is the IDENTITY —
	// a and b are the same node — rather than merely a row per node.
	tmplVLEZeroIdentity = "MATCH (a:Person)-[:KNOWS*0..0]->(b) WHERE a = b RETURN count(*)"
	// tmplVLEZeroAbsentType proves the length-0 binding does not depend on the
	// relationship type existing: over a type no edge carries, `*0..2` must
	// still return exactly the length-0 rows.
	tmplVLEZeroAbsentType = "MATCH (a:Person)-[:NOSUCHTYPE*0..2]->(b) RETURN count(*)"
	// tmplVLEZeroToTwo mixes the length-0 binding with real hops.
	tmplVLEZeroToTwo = "MATCH (a:Person)-[:KNOWS*0..2]->(b) RETURN count(*)"
	// tmplVLEBounded1to3 is the bounded form, oracle-computed rather than merely
	// projected as the workload queries do.
	tmplVLEBounded1to3 = "MATCH (a:Person)-[:KNOWS*1..3]->(b) RETURN count(*)"
	// tmplVLELowerBoundOnly is the lower-bound-only form: at least two hops,
	// with NO upper bound. It is anchored at a single Person so the trail
	// closure the reference must enumerate stays bounded and skippable.
	tmplVLELowerBoundOnly = "MATCH (a:Person {name:$s})-[:KNOWS*2..]->(b) RETURN count(*)"
	// tmplVLEMultiType is the multi-type union over a variable length, where a
	// pair carrying both types offers two distinct relationships.
	tmplVLEMultiType = "MATCH (a:Person)-[:KNOWS|FOLLOWS*1..2]->(b) RETURN count(*)"
	// tmplVLEUndirected traverses each relationship in either direction, still
	// under relationship uniqueness, with a self-loop incident once.
	tmplVLEUndirected = "MATCH (a:Person)-[:KNOWS*1..2]-(b) RETURN count(*)"
	// tmplVLEPathFunctions projects the three path functions as aggregates over
	// the SAME rows, so their mutual consistency and their absolute values are
	// both adjudicated.
	tmplVLEPathFunctions = "MATCH p=(a:Person)-[:KNOWS*1..3]->(b) " +
		"RETURN count(*), sum(length(p)), sum(size(nodes(p))), sum(size(relationships(p)))"
	// tmplVLEPathFunctionsZero is the same projection over a range that includes
	// the length-0 binding, which is where a path implementation that
	// materialises a phantom relationship for the zero-length case shows up.
	tmplVLEPathFunctionsZero = "MATCH p=(a:Person)-[:KNOWS*0..2]->(b) " +
		"RETURN count(*), sum(length(p)), sum(size(nodes(p))), sum(size(relationships(p)))"
	// tmplVLEPathConsistency counts the rows that BREAK the per-row identity
	// size(nodes(p)) = length(p)+1 = size(relationships(p))+1. It must be 0, and
	// unlike the aggregate sums it cannot be satisfied by compensating errors.
	tmplVLEPathConsistency = "MATCH p=(a:Person)-[:KNOWS*0..3]->(b) " +
		"WHERE size(nodes(p)) <> length(p) + 1 OR size(relationships(p)) <> length(p) RETURN count(*)"
	// tmplVLEMiddlePredParam constrains only the INTERMEDIATE nodes of a
	// two-hop path, by slicing the endpoints off nodes(p).
	tmplVLEMiddlePredParam = "MATCH p=(a:Person)-[:KNOWS*2]->(b) " +
		"WHERE all(x IN nodes(p)[1..-1] WHERE x.age >= $t) RETURN count(*)"
	// tmplVLEAllNodesPredParam constrains EVERY node of the path, endpoints
	// included. Its reference differs from the intermediate-only one, so an
	// engine that conflated the two would be caught.
	tmplVLEAllNodesPredParam = "MATCH p=(a:Person)-[:KNOWS*2]->(b) " +
		"WHERE all(x IN nodes(p) WHERE x.age >= $t) RETURN count(*)"
)

const (
	// vleMaxDepth is the deepest hop count any WHOLE-GRAPH probe asks for. It is
	// the queries' own upper bound (`*1..3`), so enumerating no deeper costs the
	// references nothing in exactness while keeping the enumeration polynomial:
	// the trail count at depth d is bounded by |E| * maxOutDegree^(d-1).
	vleMaxDepth = 3
	// vleAnchoredDepthCap bounds the lower-bound-only probe's enumeration, which
	// has no query-side upper bound and is therefore limited only by the number
	// of relationships in the start node's reachable component. A trail cannot
	// revisit a relationship, so the true depth is bounded by |E|; the cap is set
	// well above the depth the pattern-shapes population actually reaches (30 at
	// the scenario's default seed) and, when a trail WOULD have extended past it,
	// the reference declares itself inexact and the probe is skipped rather than
	// compared.
	vleAnchoredDepthCap = 64
	// vleWalkBudget caps how many trails a single enumeration may visit before
	// declaring itself inexact. It bounds both the reference and — because the
	// reference runs first and the engine is only asked once the reference is
	// exact — the engine's work.
	vleWalkBudget = 1 << 18
	// vleAnchorAttempts bounds how many candidate start nodes the
	// lower-bound-only probe will try before giving up for this tick.
	vleAnchorAttempts = 4
	// vleAgeThreshold is the age bound of the two path-predicate probes. Person
	// ages are drawn across [0,100), so the threshold splits the population and
	// the predicates genuinely filter; [varlenPathsVacuity] fails a run in which
	// they never did.
	vleAgeThreshold = 50
	// vleMinDepth2Walks and vleMinDepth3Walks are the terminal non-vacuity
	// floors. The planted motifs alone contribute seven depth-2 trails each, so
	// a run whose final model falls below these never built a graph big enough
	// for the multi-hop references to mean anything.
	vleMinDepth2Walks = 20
	vleMinDepth3Walks = 20
)

// vleRelKey identifies one relationship exactly as the oracle does — by
// (src, dst, label). The pattern-shapes writer never creates a parallel
// instance, so this key IS the relationship's identity, and it is what the
// openCypher relationship-uniqueness rule forbids repeating within one
// variable-length path.
type vleRelKey struct {
	label    string
	src, dst uint64
}

// vleStep is one traversable incidence: a relationship and the node reached by
// traversing it away from the node whose adjacency list holds the step. In a
// directed view the reached node is always the relationship's destination; in
// the undirected view it is whichever endpoint is not the one being left.
type vleStep struct {
	rel vleRelKey
	to  uint64
}

// vleModel is the adjacency-composition model the variable-length references
// are enumerated over, derived purely from the oracle's edge and node state.
//
// # Concurrency contract
//
// vleModel is NOT safe for concurrent use; it is built and consumed on the
// single simulation goroutine.
type vleModel struct {
	knowsOut map[uint64][]vleStep // directed KNOWS out-adjacency
	unionOut map[uint64][]vleStep // directed KNOWS|FOLLOWS out-adjacency
	knowsInc map[uint64][]vleStep // undirected KNOWS incidence, self-loop once
	ages     map[uint64]int64     // Person ages, for the path-predicate references
	persons  []uint64             // Person ids, in oracle name order
	names    []string             // persons[i]'s name, parallel to persons
	allAged  bool                 // every Person carries an integer age
}

// buildVLEModel derives the three adjacency views and the Person set from the
// oracle. Edges are read in [GraphOracle.edgeStates] order and Persons in
// [GraphOracle.NodeNames] order, so every list is deterministic and the probe
// stays bit-reproducible.
func buildVLEModel(oracle *GraphOracle) *vleModel {
	m := &vleModel{
		knowsOut: make(map[uint64][]vleStep),
		unionOut: make(map[uint64][]vleStep),
		knowsInc: make(map[uint64][]vleStep),
		ages:     make(map[uint64]int64),
		allAged:  true,
	}
	for _, e := range oracle.edgeStates() {
		if e.Label != "KNOWS" && e.Label != "FOLLOWS" {
			continue
		}
		rel := vleRelKey{label: e.Label, src: e.SrcID, dst: e.DstID}
		m.unionOut[e.SrcID] = append(m.unionOut[e.SrcID], vleStep{rel: rel, to: e.DstID})
		if e.Label != "KNOWS" {
			continue
		}
		m.knowsOut[e.SrcID] = append(m.knowsOut[e.SrcID], vleStep{rel: rel, to: e.DstID})
		// Undirected incidence: a non-loop relationship is incident to BOTH
		// endpoints (two bindings), a self-loop to its single endpoint ONCE —
		// openCypher matches an undirected relationship exactly once per path.
		m.knowsInc[e.SrcID] = append(m.knowsInc[e.SrcID], vleStep{rel: rel, to: e.DstID})
		if e.SrcID != e.DstID {
			m.knowsInc[e.DstID] = append(m.knowsInc[e.DstID], vleStep{rel: rel, to: e.SrcID})
		}
	}
	for _, name := range oracle.NodeNames() {
		id, ok := oracle.byName[name]
		if !ok {
			continue
		}
		n, ok := oracle.nodes[id]
		if !ok || !slices.Contains(n.Labels, "Person") {
			continue
		}
		m.persons = append(m.persons, id)
		m.names = append(m.names, name)
		if age, ok := n.Properties["age"].(int64); ok {
			m.ages[id] = age
		} else {
			// A Person with no integer age would make `x.age >= $t` NULL, whose
			// interaction with all() the reference does not model; the predicate
			// arms stand down rather than guess.
			m.allAged = false
		}
	}
	return m
}

// vleWalks is the outcome of one trail enumeration: how many edge-distinct
// walks exist at each exact depth, and whether the enumeration was exhaustive.
// When exact is false the counts are a LOWER bound and must not be compared
// with the engine.
type vleWalks struct {
	perDepth []int64 // index d holds the number of trails of length exactly d
	exact    bool
}

// total sums the trail counts over the inclusive depth range [lo, hi], clamping
// hi to the deepest depth enumerated.
func (w vleWalks) total(lo, hi int) int64 {
	var n int64
	for d := lo; d <= hi && d < len(w.perDepth); d++ {
		n += w.perDepth[d]
	}
	return n
}

// weighted sums d*perDepth[d] over [lo, hi] — the reference for
// `sum(length(p))` over the same rows.
func (w vleWalks) weighted(lo, hi int) int64 {
	var n int64
	for d := lo; d <= hi && d < len(w.perDepth); d++ {
		n += int64(d) * w.perDepth[d]
	}
	return n
}

// enumerateTrails walks every edge-distinct path of length at most maxDepth
// that starts at one of starts, calling visit once per path with the ordered
// node sequence (length depth+1) and its depth — including the length-0 path at
// each start. The nodes slice is REUSED between calls and must not be retained.
//
// It reports whether the enumeration was exhaustive. Exhaustiveness fails when
// the walk budget is spent, or — only when requireExhaustive is set — when a
// path stopped at maxDepth while an unused relationship was still available to
// extend it. requireExhaustive is for references to queries with NO upper bound,
// where the depth cap is a budget rather than the query's own limit; for a
// bounded query the cap IS the limit and truncation is the correct answer.
//
// Enumeration is depth-first with a used-relationship stack, so it allocates
// nothing per path.
func enumerateTrails(adj map[uint64][]vleStep, starts []uint64, maxDepth int, budget int64,
	requireExhaustive bool, visit func(nodes []uint64, depth int)) bool {
	nodes := make([]uint64, 1, maxDepth+1)
	used := make([]vleRelKey, 0, maxDepth)
	spent := int64(0)
	exact := true

	// dfs extends the current path; it reports false to abort the whole
	// enumeration (budget spent, or a truncation that matters).
	var dfs func(depth int) bool
	dfs = func(depth int) bool {
		cur := nodes[depth]
		if depth == maxDepth {
			if !requireExhaustive {
				return true
			}
			for _, s := range adj[cur] {
				if !slices.Contains(used, s.rel) {
					exact = false // this path would have gone deeper
					return false
				}
			}
			return true
		}
		for _, s := range adj[cur] {
			if slices.Contains(used, s.rel) {
				continue // relationship uniqueness: never repeat one within a path
			}
			if spent >= budget {
				exact = false
				return false
			}
			spent++
			used = append(used, s.rel)
			nodes = append(nodes, s.to)
			visit(nodes, depth+1)
			ok := dfs(depth + 1)
			nodes = nodes[:len(nodes)-1]
			used = used[:len(used)-1]
			if !ok {
				return false
			}
		}
		return true
	}

	for _, start := range starts {
		nodes, used = nodes[:1], used[:0]
		nodes[0] = start
		visit(nodes, 0) // the length-0 binding: endpoints identical, nothing traversed
		if !dfs(0) {
			break
		}
	}
	return exact
}

// countTrails enumerates trails and returns only the per-depth counts.
func countTrails(adj map[uint64][]vleStep, starts []uint64, maxDepth int, requireExhaustive bool) vleWalks {
	w := vleWalks{perDepth: make([]int64, maxDepth+1)}
	w.exact = enumerateTrails(adj, starts, maxDepth, vleWalkBudget, requireExhaustive,
		func(_ []uint64, depth int) { w.perDepth[depth]++ })
	return w
}

// countDepth2ByPredicate returns how many depth-2 trails have every node in the
// chosen slice satisfying pred: the intermediate node alone when intermediates
// is set, otherwise all three nodes. It mirrors the two path-predicate queries.
//
// Its enumeration cannot outrun the walk budget where [CheckVarlenPaths] calls
// it, because that caller has already enumerated the SAME adjacency exhaustively
// to [vleMaxDepth] — a strictly larger trail set — and refused to go on
// otherwise. The exactness flag is therefore not re-checked here.
func (m *vleModel) countDepth2ByPredicate(intermediates bool, pred func(uint64) bool) int64 {
	var n int64
	enumerateTrails(m.knowsOut, m.persons, 2, vleWalkBudget, false, func(nodes []uint64, depth int) {
		if depth != 2 {
			return
		}
		scope := nodes
		if intermediates {
			scope = nodes[1 : len(nodes)-1] // nodes(p)[1..-1]
		}
		for _, id := range scope {
			if !pred(id) {
				return
			}
		}
		n++
	})
	return n
}

// vleStats records which arms of the variable-length battery actually did
// something over a run, so [varlenPathsVacuity] can refuse a run that compared
// only trivia.
//
// # Concurrency contract
//
// vleStats is NOT safe for concurrent use; it is updated from the single
// simulation goroutine.
type vleStats struct {
	depth2Seen      int64 // ticks whose reference held at least one depth-2 trail
	zeroLengthSeen  int64 // ticks whose reference held at least one length-0 binding
	multiTypeBeyond int64 // ticks where the union count exceeded the KNOWS-only count
	undirectedAbove int64 // ticks where the undirected count exceeded the directed one
	unboundedDeep   int64 // ticks where the anchored `*2..` probe ran and saw depth > 2
	predSelective   int64 // ticks where the intermediate predicate strictly filtered
	anchorSkipped   int64 // ticks where every anchor candidate exceeded the budget
}

// CheckVarlenPaths runs the variable-length path battery — exact depth,
// zero length, lower-bound-only, bounded, intermediate-node predicates,
// multi-type and undirected expansion, and the path functions over VLE rows —
// and asserts every result equals a reference enumerated independently from the
// oracle's adjacency by [buildVLEModel] and [enumerateTrails]. It only reads
// from the engine.
//
// st accumulates what actually fired; pass the same value across a run and hand
// it to [varlenPathsVacuity] at the end.
func CheckVarlenPaths(tick int64, oracle *GraphOracle, engine *EngineAdapter, st *vleStats) []Violation {
	ctx := context.Background()
	var vs []Violation

	scalar := func(label, query string, params map[string]any, want int64) {
		got, err := scalarCountWithParams(ctx, engine, query, params)
		if err != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: label,
				Message: fmt.Sprintf("%s failed: %v\nquery: %s", label, err, query)})
			return
		}
		if got != want {
			vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: label,
				Message: fmt.Sprintf("%s: engine=%d, oracle reference=%d\nquery: %s", label, got, want, query)})
		}
	}

	m := buildVLEModel(oracle)
	knows := countTrails(m.knowsOut, m.persons, vleMaxDepth, false)
	union := countTrails(m.unionOut, m.persons, vleMaxDepth, false)
	undir := countTrails(m.knowsInc, m.persons, vleMaxDepth, false)
	if !knows.exact || !union.exact || !undir.exact {
		// The bounded enumerations blew the walk budget: the references are lower
		// bounds, so nothing may be compared this tick. This is reported rather
		// than silently skipped because at the pattern-shapes population it must
		// never happen.
		return []Violation{{
			Kind: ViolationGraphIntegrity, Tick: tick, Op: "variable-length reference budget",
			Message: fmt.Sprintf(
				"the bounded trail enumeration exceeded the %d-walk budget at depth %d "+
					"(knows exact=%v union exact=%v undirected exact=%v) — the references are lower "+
					"bounds and nothing could be adjudicated", vleWalkBudget, vleMaxDepth,
				knows.exact, union.exact, undir.exact),
		}}
	}

	nPersons := int64(len(m.persons))

	// Exact depth: `*2` and `*3` bind paths of that length and no other.
	scalar("VLE exact depth 2", tmplVLEExactDepth2, nil, knows.perDepth[2])
	scalar("VLE exact depth 3", tmplVLEExactDepth3, nil, knows.perDepth[3])

	// Zero length: the length-0 binding is the identity, applies the far-side
	// pattern to that same node, and does not need the type to exist.
	scalar("VLE zero-length only", tmplVLEZeroOnly, nil, nPersons)
	scalar("VLE zero-length identity", tmplVLEZeroIdentity, nil, nPersons)
	scalar("VLE zero-length over an absent type", tmplVLEZeroAbsentType, nil, nPersons)
	scalar("VLE zero-length plus two hops", tmplVLEZeroToTwo, nil, nPersons+knows.total(1, 2))

	// Bounded range.
	scalar("VLE bounded 1..3", tmplVLEBounded1to3, nil, knows.total(1, 3))

	// Multi-type union and undirected traversal.
	scalar("VLE multi-type 1..2", tmplVLEMultiType, nil, union.total(1, 2))
	scalar("VLE undirected 1..2", tmplVLEUndirected, nil, undir.total(1, 2))

	// Path functions over the SAME rows: the three absolute sums plus the
	// per-row identity, which compensating errors cannot satisfy.
	vs = append(vs, checkVLEPathFunctions(ctx, tick, engine, "VLE path functions 1..3",
		tmplVLEPathFunctions, knows.total(1, 3), knows.weighted(1, 3))...)
	vs = append(vs, checkVLEPathFunctions(ctx, tick, engine, "VLE path functions 0..2",
		tmplVLEPathFunctionsZero, nPersons+knows.total(1, 2), knows.weighted(1, 2))...)
	scalar("VLE path-function per-row consistency", tmplVLEPathConsistency, nil, 0)

	// Predicates over the path's nodes: intermediates only versus all of them.
	if m.allAged {
		params := map[string]any{"t": int64(vleAgeThreshold)}
		aged := func(id uint64) bool { return m.ages[id] >= vleAgeThreshold }
		mid := m.countDepth2ByPredicate(true, aged)
		all := m.countDepth2ByPredicate(false, aged)
		scalar("VLE intermediate-node predicate", tmplVLEMiddlePredParam, params, mid)
		scalar("VLE all-path-node predicate", tmplVLEAllNodesPredParam, params, all)
		if mid > 0 && mid < knows.perDepth[2] {
			st.predSelective++
		}
	}

	// Lower-bound-only `*2..`: anchored, budget-guarded, skipped when inexact.
	vs = append(vs, checkVLELowerBoundOnly(ctx, tick, m, engine, st)...)

	if knows.perDepth[2] > 0 {
		st.depth2Seen++
	}
	if nPersons > 0 {
		st.zeroLengthSeen++
	}
	if union.total(1, 2) > knows.total(1, 2) {
		st.multiTypeBeyond++
	}
	if undir.total(1, 2) > knows.total(1, 2) {
		st.undirectedAbove++
	}
	return vs
}

// checkVLEPathFunctions adjudicates one four-column path-function projection:
// the row count, sum(length(p)), sum(size(nodes(p))) and
// sum(size(relationships(p))). The two size sums are derived from the same
// reference as the length sum — size(nodes) is one per row more than the length
// and size(relationships) equals it — so the projection is held to BOTH the
// absolute oracle value and the internal identity at once.
func checkVLEPathFunctions(ctx context.Context, tick int64, engine *EngineAdapter,
	label, query string, wantRows, wantLen int64) []Violation {
	res, err := engine.Run(ctx, query, nil)
	if err != nil {
		return []Violation{{Kind: ViolationGraphIntegrity, Tick: tick, Op: label,
			Message: fmt.Sprintf("%s query error: %v\nquery: %s", label, err, query)}}
	}
	var got [4]int64
	var ok [4]bool
	if res.Next() {
		for i := range got {
			got[i], ok[i] = res.IntAt(i)
		}
	}
	for res.Next() { //nolint:revive // draining is the point
	}
	derr := res.Err()
	_ = res.Close()
	if derr != nil {
		return []Violation{{Kind: ViolationGraphIntegrity, Tick: tick, Op: label,
			Message: fmt.Sprintf("%s drain error: %v\nquery: %s", label, derr, query)}}
	}
	for i := range ok {
		if !ok[i] {
			return []Violation{{Kind: ViolationOracleDeviation, Tick: tick, Op: label,
				Message: fmt.Sprintf("%s: column %d is not an integer — the path functions must "+
					"aggregate to integers (0 over an empty result)\nquery: %s", label, i, query)}}
		}
	}
	want := [4]int64{wantRows, wantLen, wantLen + wantRows, wantLen}
	if got != want {
		return []Violation{{Kind: ViolationOracleDeviation, Tick: tick, Op: label,
			Message: fmt.Sprintf(
				"%s: engine=[rows=%d sum(length)=%d sum(size(nodes))=%d sum(size(relationships))=%d], "+
					"oracle reference=[rows=%d sum(length)=%d sum(size(nodes))=%d sum(size(relationships))=%d]\nquery: %s",
				label, got[0], got[1], got[2], got[3], want[0], want[1], want[2], want[3], query)}}
	}
	return nil
}

// checkVLELowerBoundOnly adjudicates the lower-bound-only form `*2..`, which has
// no upper bound and so cannot be enumerated over the whole graph. It anchors
// the pattern at a single Person and enumerates that node's trail closure under
// both the depth cap and the walk budget; only an EXHAUSTIVE enumeration is put
// to the engine. Candidates are taken in the oracle's name order — the first
// Persons that can reach two hops — so the choice is deterministic; at most
// [vleAnchorAttempts] are tried before the probe stands down for this tick.
func checkVLELowerBoundOnly(ctx context.Context, tick int64, m *vleModel,
	engine *EngineAdapter, st *vleStats) []Violation {
	attempts := 0
	for i, id := range m.persons {
		if attempts >= vleAnchorAttempts {
			break
		}
		if !m.reachesTwoHops(id) {
			continue
		}
		attempts++
		w := countTrails(m.knowsOut, []uint64{id}, vleAnchoredDepthCap, true)
		if !w.exact {
			continue // the closure outran the budget or the cap; try the next anchor
		}
		want := w.total(2, vleAnchoredDepthCap)
		params := map[string]any{"s": m.names[i]}
		got, err := scalarCountWithParams(ctx, engine, tmplVLELowerBoundOnly, params)
		if err != nil {
			return []Violation{{Kind: ViolationGraphIntegrity, Tick: tick, Op: "VLE lower-bound-only 2..",
				Message: fmt.Sprintf("anchored at %q: %v\nquery: %s", m.names[i], err, tmplVLELowerBoundOnly)}}
		}
		if got != want {
			return []Violation{{Kind: ViolationOracleDeviation, Tick: tick, Op: "VLE lower-bound-only 2..",
				Message: fmt.Sprintf(
					"anchored at %q: engine=%d, oracle reference=%d (trails of length >= 2 from that node, "+
						"enumerated exhaustively to depth %d)\nquery: %s",
					m.names[i], got, want, vleAnchoredDepthCap, tmplVLELowerBoundOnly)}}
		}
		if w.total(3, vleAnchoredDepthCap) > 0 {
			// The probe saw trails BEYOND the exact-depth-2 case, so the missing
			// upper bound genuinely mattered.
			st.unboundedDeep++
		}
		return nil
	}
	st.anchorSkipped++
	return nil
}

// reachesTwoHops reports whether a trail of length 2 can leave id, which is the
// precondition for the lower-bound-only probe to have anything to count.
func (m *vleModel) reachesTwoHops(id uint64) bool {
	for _, s := range m.knowsOut[id] {
		for _, t := range m.knowsOut[s.to] {
			if t.rel != s.rel {
				return true
			}
		}
	}
	return false
}

// varlenPathsVacuity is the terminal assert-something-was-seen gate for the
// variable-length battery. It fails a run in which the battery compared only
// trivia, on two independent grounds:
//
//   - the FINAL model must be big enough for the multi-hop references to mean
//     something — at least [vleMinDepth2Walks] two-hop and [vleMinDepth3Walks]
//     three-hop trails, which the planted motifs alone guarantee;
//   - each arm must have DONE something over the run: a depth-2 trail and a
//     length-0 binding existed; the multi-type union genuinely exceeded the
//     KNOWS-only count (so FOLLOWS edges really took part); the undirected count
//     genuinely exceeded the directed one (so reverse traversal really happened);
//     the anchored `*2..` probe ran at least once and saw trails deeper than two
//     hops (so the absent upper bound mattered); and the intermediate-node
//     predicate strictly filtered at least once (neither everything nor nothing).
func varlenPathsVacuity(tick int64, oracle *GraphOracle, st *vleStats) []Violation {
	m := buildVLEModel(oracle)
	knows := countTrails(m.knowsOut, m.persons, vleMaxDepth, false)

	var missing []string
	if knows.perDepth[2] < vleMinDepth2Walks {
		missing = append(missing, fmt.Sprintf("only %d two-hop trail(s) in the final model, want >= %d",
			knows.perDepth[2], vleMinDepth2Walks))
	}
	if knows.perDepth[3] < vleMinDepth3Walks {
		missing = append(missing, fmt.Sprintf("only %d three-hop trail(s) in the final model, want >= %d",
			knows.perDepth[3], vleMinDepth3Walks))
	}
	if st.depth2Seen == 0 {
		missing = append(missing, "no check ever saw a depth-2 trail")
	}
	if st.zeroLengthSeen == 0 {
		missing = append(missing, "no check ever saw a zero-length binding")
	}
	if st.multiTypeBeyond == 0 {
		missing = append(missing, "the multi-type union never exceeded the KNOWS-only count, so FOLLOWS never took part")
	}
	if st.undirectedAbove == 0 {
		missing = append(missing, "the undirected count never exceeded the directed one, so reverse traversal was never observed")
	}
	if st.unboundedDeep == 0 {
		missing = append(missing, fmt.Sprintf(
			"the lower-bound-only probe never ran on trails deeper than two hops (skipped %d time(s) on the enumeration budget)",
			st.anchorSkipped))
	}
	if st.predSelective == 0 {
		missing = append(missing, "the intermediate-node predicate never strictly filtered (it matched everything or nothing)")
	}
	if len(missing) == 0 {
		return nil
	}
	return []Violation{{
		Kind: ViolationVacuousRun, Tick: tick, Op: "variable-length paths non-vacuity",
		Message: fmt.Sprintf("vacuous run: the variable-length battery proved nothing — %v", missing),
	}}
}
