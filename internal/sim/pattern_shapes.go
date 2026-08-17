package sim

import (
	"context"
	"fmt"
)

// tmplCreateFollows links two existing Person nodes by name with a FOLLOWS
// edge — the second relationship type of the pattern-shapes scenario (rmp
// #2451). It mirrors [tmplCreateKnows] exactly, so the oracle's typed edge
// model (edgeKey.label) covers two labels symmetrically and the multi-type
// union probe `[:KNOWS|FOLLOWS]` has a non-trivial reference.
const tmplCreateFollows = "MATCH (a:Person {name:$a}),(b:Person {name:$b}) CREATE (a)-[:FOLLOWS]->(b)"

// patternShapesCheckEvery is the periodic cadence (in ticks) of the
// pattern-shape battery inside [runPatternShapes].
const patternShapesCheckEvery = 70

// deleteContractEvery is the tick cadence of the non-detach DELETE probe
// ([probeDeleteContract], rmp #2462) inside [runPatternShapes]. It is coprime
// with [patternShapesCheckEvery] so the mutating probe and the read battery do
// not land on the same tick, and frequent enough that both contract arms fire
// many times within the scenario's tick budget.
const deleteContractEvery = 31

// patternMotifEvery is the writer-op cadence at which [PatternShapesWriter]
// plants one deterministic motif (a directed triangle, a mutual KNOWS pair, a
// KNOWS+FOLLOWS both-types pair, and a KNOWS self-loop), so the terminal
// non-vacuity assertion always has something to see.
const patternMotifEvery = 45

// createFollows models [tmplCreateFollows]: a FOLLOWS edge between the Person
// nodes named $a and $b. Like createKnows it is a committed no-effect result
// when either endpoint is missing (the MATCH yields no rows). The writer never
// re-CREATEs an existing (src,dst) FOLLOWS edge; the guard below keeps the
// model single-instance even if a caller did.
func (o *GraphOracle) createFollows(params map[string]any) OracleResult {
	a, okA := paramString(params, "a")
	b, okB := paramString(params, "b")
	if !okA || !okB {
		return OracleResult{ErrorMsg: "oracle: createFollows missing endpoint"}
	}
	srcID, srcOK := o.byName[a]
	dstID, dstOK := o.byName[b]
	if !srcOK || !dstOK {
		return OracleResult{Committed: true} // MATCH found nothing; no edge created.
	}
	k := edgeKey{src: srcID, dst: dstID, label: "FOLLOWS"}
	if _, exists := o.edges[k]; exists {
		return OracleResult{Committed: true} // simple-graph re-CREATE is a no-op.
	}
	o.edges[k] = &EdgeState{SrcID: srcID, DstID: dstID, Label: "FOLLOWS", Properties: map[string]any{}}
	return OracleResult{Committed: true, EdgesCreated: 1}
}

// PatternShapesWriter builds the two-relationship-type Person graph the
// pattern-shapes battery reads: fresh Person nodes, seed-drawn KNOWS and
// FOLLOWS edges (endpoint draws are independent, so self-loops occur), and a
// deterministic motif planted every [patternMotifEvery] ops — a directed
// KNOWS triangle a→b→c→a, a mutual KNOWS pair a⇄b, a FOLLOWS a→b on top of
// the KNOWS a→b (a both-types pair), and a KNOWS self-loop c→c. The motifs
// guarantee every reference shape (triangle, mutual pair, both-types union,
// relationship-uniqueness exclusion) is exercised non-vacuously.
//
// The scenario opens the engine as a directed MULTIGRAPH, because a simple
// graph admits at most one edge per ordered node pair REGARDLESS of type —
// a KNOWS+FOLLOWS both-types pair is impossible there. The writer, however,
// never re-CREATEs an existing (src,dst,label) edge (it consults the oracle
// first, the edge-properties guard precedent), so the multigraph never holds
// parallel instances and edge identity remains exactly (src,dst,label): every
// adjacency-derived reference count below is exact without per-instance
// modelling (parallel-instance multiplicity is the edge-properties scenario's
// concern). Self-loops are deliberately ALLOWED and planted: openCypher
// matches a self-loop once for an undirected relationship pattern, and a
// self-loop is the only single-instance shape where the
// relationship-isomorphism exclusion (r1<>r2) actually removes a binding —
// planting them is what makes the uniqueness probe sensitive to a
// Cyphermorphism regression.
//
// # Concurrency contract
//
// PatternShapesWriter is NOT safe for concurrent use; it is invoked from the
// single simulation goroutine.
type PatternShapesWriter struct {
	pending []Op
	counter int64 // fresh random Person names (p<N>)
	motifs  int64 // planted motifs so far (names m<K>a / m<K>b / m<K>c)
	ops     int64 // NextOp invocations, drives the motif cadence
}

// Name returns the actor's identifier.
func (*PatternShapesWriter) Name() string { return "PatternShapesWriter" }

// NextOp drains the pending motif queue first; otherwise it returns a fresh
// Person CREATE or a seed-drawn KNOWS/FOLLOWS edge between existing Persons
// (endpoints drawn independently, so occasional random self-loops occur). A
// drawn pair whose (src,dst,label) edge the oracle already models falls back
// to a Person CREATE instead, so the multigraph never holds a parallel
// instance and edge identity stays (src,dst,label)-exact.
func (w *PatternShapesWriter) NextOp(seed *Seed, oracle *GraphOracle) Op {
	if len(w.pending) == 0 && w.ops%patternMotifEvery == 0 {
		w.enqueueMotif()
	}
	w.ops++
	if len(w.pending) > 0 {
		op := w.pending[0]
		w.pending = w.pending[1:]
		return op
	}
	names := oracle.NodeNames()
	if len(names) >= 2 {
		r := seed.Float64()
		if r < 0.45 {
			label, tmpl := "KNOWS", tmplCreateKnows
			if r >= 0.25 {
				label, tmpl = "FOLLOWS", tmplCreateFollows
			}
			a := names[seed.IntN(len(names))]
			b := names[seed.IntN(len(names))]
			if !oracle.hasEdgeByName(a, b, label) {
				return Op{Kind: OpCreate, Cypher: tmpl, Params: map[string]any{"a": a, "b": b}}
			}
		}
	}
	name := fmt.Sprintf("p%d", w.counter)
	w.counter++
	return Op{Kind: OpCreate, Cypher: tmplCreatePerson, Params: map[string]any{"name": name, "age": int64(seed.IntN(100))}}
}

// hasEdgeByName reports whether the oracle models an edge of the given label
// between the Person nodes named a and b. The pattern-shapes writer uses it to
// never re-CREATE an existing (src,dst,label) edge, which is what keeps the
// multigraph free of parallel instances.
func (o *GraphOracle) hasEdgeByName(a, b, label string) bool {
	srcID, okA := o.byName[a]
	dstID, okB := o.byName[b]
	if !okA || !okB {
		return false
	}
	return o.HasEdge(srcID, dstID, label)
}

// enqueueMotif appends the nine ops of one planted motif over three fresh
// Persons a, b, c: the directed KNOWS triangle a→b→c→a, the mutual pair
// (extra b→a), the both-types pair (FOLLOWS a→b over the existing KNOWS a→b),
// and the self-loop c→c. Ages are derived from the motif index, so planting
// draws no randomness and cannot shift the seed stream.
func (w *PatternShapesWriter) enqueueMotif() {
	k := w.motifs
	w.motifs++
	a := fmt.Sprintf("m%da", k)
	b := fmt.Sprintf("m%db", k)
	c := fmt.Sprintf("m%dc", k)
	age := func(i int64) int64 { return (k*7 + i) % 100 }
	w.pending = append(w.pending,
		Op{Kind: OpCreate, Cypher: tmplCreatePerson, Params: map[string]any{"name": a, "age": age(0)}},
		Op{Kind: OpCreate, Cypher: tmplCreatePerson, Params: map[string]any{"name": b, "age": age(1)}},
		Op{Kind: OpCreate, Cypher: tmplCreatePerson, Params: map[string]any{"name": c, "age": age(2)}},
		Op{Kind: OpCreate, Cypher: tmplCreateKnows, Params: map[string]any{"a": a, "b": b}},   // triangle 1/3 + both-types base
		Op{Kind: OpCreate, Cypher: tmplCreateKnows, Params: map[string]any{"a": b, "b": c}},   // triangle 2/3
		Op{Kind: OpCreate, Cypher: tmplCreateKnows, Params: map[string]any{"a": c, "b": a}},   // triangle 3/3
		Op{Kind: OpCreate, Cypher: tmplCreateKnows, Params: map[string]any{"a": b, "b": a}},   // mutual pair a<->b
		Op{Kind: OpCreate, Cypher: tmplCreateFollows, Params: map[string]any{"a": a, "b": b}}, // both-types pair on (a,b)
		Op{Kind: OpCreate, Cypher: tmplCreateKnows, Params: map[string]any{"a": c, "b": c}},   // deliberate self-loop
	)
}

// patternShapesWorkload is a 100% PatternShapesWriter mix.
func patternShapesWorkload(_ *Seed) *Workload {
	return &Workload{Actors: []Actor{&PatternShapesWriter{}}, Weights: []float64{1.0}}
}

// dirEdge identifies one simple-graph edge of a single label by its endpoint
// oracle ids; it is the edge-identity unit of every pattern-shape reference
// (relationship isomorphism compares these keys).
type dirEdge struct{ src, dst uint64 }

// patternShapeRefs holds the independent, adjacency-derived reference counts
// for the pattern-shape battery, computed purely from the oracle's edge sets
// (never through the engine).
type patternShapeRefs struct {
	knows         int64 // |KNOWS|
	follows       int64 // |FOLLOWS|
	twoHop        int64 // ordered KNOWS edge pairs (e1,e2), e1.dst=e2.src, e1<>e2
	triangles     int64 // ordered KNOWS edge triples closing a 3-walk, pairwise distinct
	undirected    int64 // 2*|KNOWS| - selfLoops (undirected pattern, self-loop matched once)
	uniqueness    int64 // ordered mutual pairs: e1 with reverse present, e1 <> reverse
	selfLoops     int64 // KNOWS edges with src == dst
	bothTypePairs int64 // (src,dst) pairs carrying BOTH a KNOWS and a FOLLOWS edge
}

// computePatternShapeRefs derives every reference count from the oracle's
// adjacency, by direct composition of the KNOWS/FOLLOWS edge sets.
//
// Semantics (openCypher): MATCH rows are pattern bindings; nodes may repeat
// within a binding (homomorphism) but relationships may not (relationship
// isomorphism), and count(*) counts one per binding row. On a simple graph a
// relationship is identified by (src,dst,label), so:
//
//   - two-hop (a)-[:K]->()-[:K]->(c): ordered pairs (e1,e2) with
//     e1.dst = e2.src and e1 <> e2. Only a self-loop can pair with itself, so
//     the exclusion removes exactly one pair per self-loop.
//   - triangle (a)-[:K]->(b)-[:K]->(c)-[:K]->(a): ordered triples (e1,e2,e3)
//     closing a length-3 directed walk with pairwise-distinct edges. A
//     node-distinct directed 3-cycle contributes 3 rows (one per rotation);
//     degenerate walks (e.g. a self-loop plus a mutual pair) are valid
//     bindings whenever their three edges are distinct, and the direct
//     enumeration counts them exactly.
//   - undirected (a)-[:K]-(b): each non-self-loop relationship binds twice
//     (once per endpoint assignment); a self-loop binds ONCE (openCypher
//     requires each matched relationship to appear exactly once for an
//     undirected pattern), hence 2*|K| - selfLoops.
//   - uniqueness (a)-[r1:K]->(b)-[r2:K]->(a): ordered pairs whose reverse
//     edge exists, with r1 <> r2. r1 = r2 forces src = dst, so the exclusion
//     removes exactly the self-loop bindings — a mutual pair contributes 2
//     rows, a self-loop contributes 0 (an engine that skips the
//     relationship-isomorphism check would count each self-loop once).
//   - multi-type (a)-[:K|F]->(b): every relationship of either type binds
//     once, so the union count is |K| + |F| even where a pair carries both.
func computePatternShapeRefs(oracle *GraphOracle) patternShapeRefs {
	var refs patternShapeRefs
	knowsSet := make(map[dirEdge]bool)
	followsSet := make(map[dirEdge]bool)
	out := make(map[uint64][]uint64)
	for _, e := range oracle.edgeStates() {
		switch e.Label {
		case "KNOWS":
			knowsSet[dirEdge{e.SrcID, e.DstID}] = true
			out[e.SrcID] = append(out[e.SrcID], e.DstID)
			if e.SrcID == e.DstID {
				refs.selfLoops++
			}
			refs.knows++
		case "FOLLOWS":
			followsSet[dirEdge{e.SrcID, e.DstID}] = true
			refs.follows++
		}
	}
	for e1 := range knowsSet {
		// Two-hop composition: e2 ranges over out[e1.dst]; skip e2 == e1 (only
		// possible when e1 is a self-loop and the composed target is e1.dst).
		for _, c := range out[e1.dst] {
			if e1.src == e1.dst && c == e1.dst {
				continue
			}
			refs.twoHop++
		}
		// Triangles: close the walk with e3 = (c, e1.src) and require the three
		// edge keys pairwise distinct.
		for _, c := range out[e1.dst] {
			e2 := dirEdge{e1.dst, c}
			e3 := dirEdge{c, e1.src}
			if !knowsSet[e3] {
				continue
			}
			if e1 == e2 || e2 == e3 || e1 == e3 {
				continue
			}
			refs.triangles++
		}
		// Relationship-uniqueness pairs: reverse present and distinct from e1.
		rev := dirEdge{e1.dst, e1.src}
		if e1 != rev && knowsSet[rev] {
			refs.uniqueness++
		}
	}
	for k := range knowsSet {
		if followsSet[k] {
			refs.bothTypePairs++
		}
	}
	refs.undirected = 2*refs.knows - refs.selfLoops
	return refs
}

// CheckPatternShapes runs the multi-hop / undirected / reverse / multi-type /
// cyclic pattern battery — the join-and-intersect planner family (ExpandInto,
// ExpandIntersect, hash join, reverse expansion, relationship uniqueness) —
// and asserts each count(*) equals a reference computed independently from
// the oracle's adjacency by [computePatternShapeRefs]. The ExpandInto shape
// (both endpoints bound by property) is probed for a bounded, deterministic
// selection of existing KNOWS pairs, in BOTH literal and $param form.
func CheckPatternShapes(tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	ctx := context.Background()
	var vs []Violation

	scalar := func(label, query string, params map[string]any, want int64) {
		res, err := engine.Run(ctx, query, params)
		if err != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: label,
				Message: fmt.Sprintf("%s query error: %v", label, err)})
			return
		}
		var got int64
		if res.Next() {
			got, _ = res.IntAt(0)
		}
		derr := res.Err()
		_ = res.Close()
		if derr != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: label,
				Message: fmt.Sprintf("%s drain error: %v", label, derr)})
			return
		}
		if got != want {
			vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: label,
				Message: fmt.Sprintf("%s: engine=%d, oracle reference=%d\nquery: %s", label, got, want, query)})
		}
	}

	refs := computePatternShapeRefs(oracle)

	scalar("2-hop KNOWS", "MATCH (a:Person)-[:KNOWS]->()-[:KNOWS]->(c) RETURN count(*)", nil, refs.twoHop)
	scalar("directed triangle", "MATCH (a)-[:KNOWS]->(b)-[:KNOWS]->(c)-[:KNOWS]->(a) RETURN count(*)", nil, refs.triangles)
	scalar("undirected KNOWS", "MATCH (a:Person)-[:KNOWS]-(b) RETURN count(*)", nil, refs.undirected)
	scalar("reverse KNOWS", "MATCH (b)<-[:KNOWS]-(a) RETURN count(*)", nil, refs.knows)
	scalar("multi-type KNOWS|FOLLOWS", "MATCH (a)-[:KNOWS|FOLLOWS]->(b) RETURN count(*)", nil, refs.knows+refs.follows)
	scalar("rel-uniqueness mutual", "MATCH (a)-[r1:KNOWS]->(b)-[r2:KNOWS]->(a) RETURN count(*)", nil, refs.uniqueness)

	// ExpandInto: both endpoints bound by an equality predicate, probed for a
	// bounded deterministic selection of existing KNOWS pairs (first, middle,
	// last of the oracle's deterministic edge order). On the simple graph each
	// existing pair holds exactly one KNOWS edge, so both spellings must count 1.
	// Writer-generated names ([a-z0-9] only) are safe to inline as literals.
	var pairs [][2]string
	for _, e := range oracle.edgeStates() {
		if e.Label != "KNOWS" {
			continue
		}
		src, dst := oracle.nameOf(e.SrcID), oracle.nameOf(e.DstID)
		if src == "" || dst == "" {
			continue
		}
		pairs = append(pairs, [2]string{src, dst})
	}
	if n := len(pairs); n > 0 {
		seen := map[int]bool{}
		for _, i := range []int{0, n / 2, n - 1} {
			if seen[i] {
				continue
			}
			seen[i] = true
			x, y := pairs[i][0], pairs[i][1]
			scalar(fmt.Sprintf("ExpandInto literal (%s,%s)", x, y),
				fmt.Sprintf("MATCH (a:Person {name:'%s'})-[:KNOWS]->(b:Person {name:'%s'}) RETURN count(*)", x, y),
				nil, 1)
			scalar(fmt.Sprintf("ExpandInto param (%s,%s)", x, y),
				"MATCH (a:Person {name:$x})-[:KNOWS]->(b:Person {name:$y}) RETURN count(*)",
				map[string]any{"x": x, "y": y}, 1)
		}
	}
	return vs
}

// patternShapesVacuity asserts the run actually exercised every reference
// shape: at least one directed triangle, one mutual KNOWS pair, one
// KNOWS+FOLLOWS both-types pair, and one KNOWS self-loop (the shape that
// makes the relationship-uniqueness exclusion and the undirected
// matched-once rule observable) must exist in the final oracle model. The
// writer only creates, so the terminal state contains every planted motif; a
// run without them compared trivial counts and proved nothing.
func patternShapesVacuity(tick int64, oracle *GraphOracle) []Violation {
	refs := computePatternShapeRefs(oracle)
	var missing []string
	if refs.triangles == 0 {
		missing = append(missing, "directed triangle")
	}
	if refs.uniqueness == 0 {
		missing = append(missing, "mutual KNOWS pair")
	}
	if refs.bothTypePairs == 0 {
		missing = append(missing, "KNOWS+FOLLOWS both-types pair")
	}
	if refs.selfLoops == 0 {
		missing = append(missing, "KNOWS self-loop")
	}
	if len(missing) == 0 {
		return nil
	}
	return []Violation{{
		Kind: ViolationOracleDeviation,
		Tick: tick,
		Op:   "pattern-shapes non-vacuity",
		Message: fmt.Sprintf("vacuous run: the final model never contained: %v — the corresponding "+
			"reference checks compared trivial counts and proved nothing", missing),
	}}
}

// patternShapesScenario exercises the join/intersect planner family the
// single-hop DST workloads leave dead: multi-hop composition, the cyclic
// triangle shape (ExpandIntersect), undirected and reverse expansion,
// multi-type union, relationship uniqueness (Cyphermorphism), and the
// bound-both-endpoints ExpandInto shape — each count(*) verified against an
// independent adjacency-composition reference over the oracle's KNOWS/FOLLOWS
// edge sets, periodically, after each crash/recovery, and at the end, with a
// terminal non-vacuity assertion. It opens the engine as a directed
// multigraph — a simple graph admits only one edge per ordered pair regardless
// of type, making both-types pairs impossible — but the writer never issues a
// duplicate (src,dst,label) CREATE, so edge identity stays (src,dst,label)-
// exact (parallel instances are the edge-properties scenario's concern). It is
// bit-reproducible.
func patternShapesScenario() Scenario {
	return Scenario{
		Name:        ScenarioPatternShapes,
		Description: "multi-hop/undirected/reverse/multi-type/cyclic pattern shapes vs adjacency-composition references (join/intersect planner family)",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x9A77E121,
		MaxTicks:    500,
		Workload:    patternShapesWorkload,
		Multigraph:  true,
		Crash:       CrashConfig{Enabled: true, CrashProb: 1.0 / 90.0, StabilityWindow: 25},
		run:         runPatternShapes,
	}
}

// runPatternShapes drives the pattern-shapes safety loop: the standard
// per-cadence parity check, the pattern battery periodically, after each
// crash/recovery, and at the end, plus the terminal non-vacuity assertion.
// Deterministic.
func runPatternShapes(ctx context.Context, seed uint64) (*SimReport, error) {
	sc := patternShapesScenario()
	cfg := sc.DeterministicConfig(seed)
	sm, err := New(cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: pattern-shapes new: %w", err)
	}
	defer func() { _ = sm.Close() }()

	var delStats deleteContractStats
	var lastTick int64
	var lastOp Op
	for i := 0; i < cfg.MaxTicks; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tick := sm.clock.Tick()

		crashesBefore := sm.crashCount
		if report, err := sm.maybeCrash(ctx, tick); err != nil {
			return nil, err
		} else if report != nil {
			return report, nil
		}
		if sm.crashCount > crashesBefore {
			if v := CheckPatternShapes(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery pattern shapes>"}, v), nil
			}
			// The advisory is a pure function of the query, so it must survive
			// recovery exactly as it stood before the crash.
			if v := CheckCartesianNotification(tick, sm.engine); len(v) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery cartesian notification>"}, v), nil
			}
		}

		actor := sm.workload.SelectActor(sm.seed)
		op := actor.NextOp(sm.seed, sm.oracle)
		committed := sm.execute(ctx, op)
		sm.applyToOracle(op, committed)
		lastTick, lastOp = tick, op

		if tick%int64(sm.cfg.CheckEvery) == 0 {
			if v := sm.checker.Check(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, op, v), nil
			}
		}
		if tick%patternShapesCheckEvery == 0 {
			if v := CheckPatternShapes(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, op, v), nil
			}
			if v := CheckCartesianNotification(tick, sm.engine); len(v) > 0 {
				return sm.report(tick, op, v), nil
			}
		}
		// The non-detach DELETE contract (rmp #2462) rides this scenario because
		// its oracle knows adjacency exactly, which is what lets both arms be
		// PREDICTED: a degree-0 Person must delete, a connected one must be
		// refused. It runs on its own cadence, offset from the read battery so
		// the two do not stack on one tick, and it MUTATES (the accepted delete
		// is applied to the oracle too). Only degree-0 nodes are ever removed,
		// so every planted motif survives for the terminal shape assertions.
		if tick%deleteContractEvery == 0 {
			if v := probeDeleteContract(ctx, tick, sm.oracle, sm.engine, &delStats); len(v) > 0 {
				return sm.report(tick, op, v), nil
			}
		}
	}
	if v := CheckPatternShapes(lastTick, sm.oracle, sm.engine); len(v) > 0 {
		return sm.report(lastTick, lastOp, v), nil
	}
	if v := CheckCartesianNotification(lastTick, sm.engine); len(v) > 0 {
		return sm.report(lastTick, lastOp, v), nil
	}
	if v := patternShapesVacuity(lastTick, sm.oracle); len(v) > 0 {
		return sm.report(lastTick, lastOp, v), nil
	}
	if v := deleteContractVacuity(lastTick, &delStats); len(v) > 0 {
		return sm.report(lastTick, lastOp, v), nil
	}
	return nil, nil
}
