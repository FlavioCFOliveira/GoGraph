package sim

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
)

// Relationship-mutation templates for the edge-properties scenario (rmp #2449).
// Every KNOWS edge this scenario creates carries a unique integer `eid`
// property, so an op over a pair joined by PARALLEL edges can pin exactly one
// instance with `WHERE r.eid = $eid` — which is what lets the oracle predict,
// per instance, the effect of a standalone SET / REMOVE / DELETE r. The
// parallel-edge by-handle instance-identity code is exactly the surface these
// templates guard (see cypher/byhandle_edge_prop_mutation_test.go and
// cypher/delete_parallel_edge_instance_test.go for the pinned engine
// semantics).
const (
	// tmplCreateKnowsInst links two existing Persons with a KNOWS edge carrying
	// a unique instance id (`eid`), an ISO-8601 `since` string, and a float
	// `weight`. Issued between an already-linked pair it creates a PARALLEL
	// instance (the scenario runs on a multigraph).
	tmplCreateKnowsInst = "MATCH (a:Person {name:$a}),(b:Person {name:$b}) CREATE (a)-[:KNOWS {eid:$eid, since:$since, weight:$weight}]->(b)"
	// tmplSetKnowsWeight is the standalone relationship SET: it re-assigns the
	// weight of exactly the instance identified by $eid, leaving any parallel
	// twin untouched.
	tmplSetKnowsWeight = "MATCH (a:Person {name:$a})-[r:KNOWS]->(b:Person {name:$b}) WHERE r.eid = $eid SET r.weight = $weight"
	// tmplRemoveKnowsSince is the standalone relationship REMOVE: it removes the
	// `since` property of exactly the instance identified by $eid.
	tmplRemoveKnowsSince = "MATCH (a:Person {name:$a})-[r:KNOWS]->(b:Person {name:$b}) WHERE r.eid = $eid REMOVE r.since"
	// tmplSetKnowsSinceNull is the SET-to-null removal: openCypher defines
	// `SET r.since = null` as removing the property, exactly like REMOVE, but it
	// executes through the SetProperty operator's delRelProp path rather than
	// RemoveProperty — the residual per-pair counter gate fixed by rmp #2501
	// (the SET-path sibling of the #2500 REMOVE defect this scenario found).
	tmplSetKnowsSinceNull = "MATCH (a:Person {name:$a})-[r:KNOWS]->(b:Person {name:$b}) WHERE r.eid = $eid SET r.since = null"
	// tmplDeleteKnowsInst is the standalone edge deletion: DELETE r removes
	// exactly the instance identified by $eid and must leave both endpoint
	// nodes (and any parallel twin) alive.
	tmplDeleteKnowsInst = "MATCH (a:Person {name:$a})-[r:KNOWS]->(b:Person {name:$b}) WHERE r.eid = $eid DELETE r"
	// tmplSetKnowsNote plants `note` on exactly the instance identified by
	// $eid. On a parallel pair this is the instance-only-key seed for the
	// replace teardown (rmp #2502): a later whole-entity replace on ANY
	// instance of the pair must tear `note` down from that instance's own bag
	// even after the per-pair aggregate entry has been cleared by an op pinned
	// to a sibling.
	tmplSetKnowsNote = "MATCH (a:Person {name:$a})-[r:KNOWS]->(b:Person {name:$b}) WHERE r.eid = $eid SET r.note = $note"
	// tmplReplaceKnowsInst is the whole-entity replace on one instance: the
	// openCypher contract is "the resulting property map equals exactly the
	// given map", so any key the instance carried outside the map — `note`
	// included, however the aggregate/bag stores diverged — must be gone
	// afterwards (rmp #2502, suspicion 1).
	tmplReplaceKnowsInst = "MATCH (a:Person {name:$a})-[r:KNOWS]->(b:Person {name:$b}) WHERE r.eid = $eid SET r = {eid: $eid, since: $since, weight: $weight}"
	// tmplReplaceKnowsInstWith is [tmplReplaceKnowsInst] issued AFTER a WITH
	// projection boundary: the bound RelationshipValue must keep carrying its
	// stable handle so the replace reaches the instance's own bag instead of
	// degrading to the pairwise path (rmp #2502, suspicion 2 — class #2334).
	tmplReplaceKnowsInstWith = "MATCH (a:Person {name:$a})-[r:KNOWS]->(b:Person {name:$b}) WHERE r.eid = $eid WITH r SET r = {eid: $eid, since: $since, weight: $weight}"
	// tmplCopyKnowsInst is the copy-from-instance whole-entity assignment (rmp
	// #2503): `SET r2 = r1` copies the SOURCE instance's own property map —
	// exactly what `RETURN properties(r1)` shows — onto the target instance,
	// never the per-pair aggregate, which on a parallel pair can carry keys
	// only a twin ever had. The trailing `SET r2.eid = $dstEid` re-pins the
	// target's unique eid (the copied map carries the source's), keeping every
	// instance addressable by eid for the oracle and the checker.
	tmplCopyKnowsInst = "MATCH (a:Person {name:$a})-[r1:KNOWS]->(b:Person {name:$b}) WHERE r1.eid = $srcEid MATCH (:Person {name:$a})-[r2:KNOWS]->(:Person {name:$b}) WHERE r2.eid = $dstEid SET r2 = r1 SET r2.eid = $dstEid"
)

// KnowsInstance identifies one modelled KNOWS edge instance by its unique eid
// and its endpoint Person names — the coordinates the edge-properties writer
// targets ops with and the checker probes the engine with.
type KnowsInstance struct {
	Src, Dst string
	EID      int64
}

// EdgePropsWriter is the edge-property coverage actor: it grows a Person
// population and links pairs with KNOWS edges carrying a unique `eid`, an
// ISO-8601 `since` string, and a float `weight`, then mutates individual edge
// INSTANCES with the full relationship write surface — standalone SET
// (r.weight), REMOVE (r.since), the SET-to-null removal (SET r.since = null),
// the instance-only-key SET (r.note), the whole-entity replace in its plain
// and WITH-projected forms (SET r = {…}, rmp #2502), the copy-from-instance
// assignment (SET r2 = r1, rmp #2503), and DELETE r — including
// over parallel-edge pairs, where one instance is touched and its twin must
// survive with its own property map. Every op pins its target instance by
// eid, so the oracle predicts exactly which instance changes.
//
// # Concurrency contract
//
// EdgePropsWriter is NOT safe for concurrent use; it is invoked from the single
// simulation goroutine.
type EdgePropsWriter struct {
	// counter derives unique Person names.
	counter int64
	// nextEID allocates unique edge-instance ids. It only ever grows (a rejected
	// CREATE burns its eid), so an eid can never be reused across the run.
	nextEID int64
}

// Name returns the actor's identifier.
func (*EdgePropsWriter) Name() string { return "EdgePropsWriter" }

// NextOp returns the next seed-chosen operation: a fresh Person, a KNOWS
// instance between a random pair, a PARALLEL twin of an existing instance, or
// a standalone SET / REMOVE / SET-to-null / DELETE r pinned to one existing
// instance. Every family that lacks a viable target falls back to a Person
// CREATE, so the op stream stays a pure function of (seed state, oracle
// state).
func (w *EdgePropsWriter) NextOp(seed *Seed, oracle *GraphOracle) Op {
	names := oracle.NodeNames()
	if len(names) >= 2 {
		switch pick := seed.IntN(15); {
		case pick < 3:
			// ~20%: grow the Person population (fall through to the create below).
		case pick < 6:
			// ~20%: a KNOWS instance between a seed-chosen pair. A pair that is
			// already linked gains a parallel instance (multigraph); a self-pair
			// draw falls back to a Person create (self-loops are out of scope).
			a := names[seed.IntN(len(names))]
			b := names[seed.IntN(len(names))]
			if a != b {
				return w.opCreateInstance(seed, a, b)
			}
		case pick < 7:
			// ~7%: a deliberate PARALLEL twin of an existing instance — the
			// defect-magnet shape: two edges between the same endpoints, each with
			// its own eid and property map.
			if insts := oracle.KnowsInstancesByEID(); len(insts) > 0 {
				t := insts[seed.IntN(len(insts))]
				return w.opCreateInstance(seed, t.Src, t.Dst)
			}
		case pick < 8:
			// ~7%: standalone relationship SET on one instance.
			if insts := oracle.KnowsInstancesByEID(); len(insts) > 0 {
				t := insts[seed.IntN(len(insts))]
				return Op{Kind: OpUpdate, Cypher: tmplSetKnowsWeight, Params: map[string]any{
					"a": t.Src, "b": t.Dst, "eid": t.EID, "weight": float64(seed.IntN(100000)) / 100.0,
				}}
			}
		case pick < 9:
			// ~7%: standalone relationship REMOVE on one instance. Removing an
			// already-removed `since` is a modelled no-op the engine must agree on.
			if insts := oracle.KnowsInstancesByEID(); len(insts) > 0 {
				t := insts[seed.IntN(len(insts))]
				return Op{Kind: OpUpdate, Cypher: tmplRemoveKnowsSince, Params: map[string]any{
					"a": t.Src, "b": t.Dst, "eid": t.EID,
				}}
			}
		case pick < 10:
			// ~7%: SET-to-null removal on one instance — same modelled removal
			// semantics as REMOVE, but through the SET operator path (rmp #2501).
			// Nulling an already-absent `since` is a modelled no-op the engine
			// must agree on.
			if insts := oracle.KnowsInstancesByEID(); len(insts) > 0 {
				t := insts[seed.IntN(len(insts))]
				return Op{Kind: OpUpdate, Cypher: tmplSetKnowsSinceNull, Params: map[string]any{
					"a": t.Src, "b": t.Dst, "eid": t.EID,
				}}
			}
		case pick < 11:
			// ~7%: plant the instance-only `note` key on one instance (rmp
			// #2502): the seed a later whole-entity replace must tear down.
			if insts := oracle.KnowsInstancesByEID(); len(insts) > 0 {
				t := insts[seed.IntN(len(insts))]
				return Op{Kind: OpUpdate, Cypher: tmplSetKnowsNote, Params: map[string]any{
					"a": t.Src, "b": t.Dst, "eid": t.EID, "note": fmt.Sprintf("n%d", seed.IntN(1000)),
				}}
			}
		case pick < 12:
			// ~7%: whole-entity replace on one instance — the resulting map must
			// equal exactly {eid, since, weight}, dropping any planted `note`
			// however the aggregate/bag stores diverged (rmp #2502).
			if insts := oracle.KnowsInstancesByEID(); len(insts) > 0 {
				t := insts[seed.IntN(len(insts))]
				return w.opReplaceInstance(seed, t, tmplReplaceKnowsInst)
			}
		case pick < 13:
			// ~7%: the same whole-entity replace issued across a WITH projection
			// boundary — the stable handle must survive the projection (rmp
			// #2502, class #2334).
			if insts := oracle.KnowsInstancesByEID(); len(insts) > 0 {
				t := insts[seed.IntN(len(insts))]
				return w.opReplaceInstance(seed, t, tmplReplaceKnowsInstWith)
			}
		case pick < 14:
			// ~7%: copy-from-instance assignment (rmp #2503): SET twin = pinned
			// instance. The copied map must be the SOURCE instance's own bag,
			// never the per-pair aggregate; a parallel twin is preferred as the
			// target, with the degenerate self-copy as the no-twin fallback.
			if insts := oracle.KnowsInstancesByEID(); len(insts) > 0 {
				src := insts[seed.IntN(len(insts))]
				var twins []KnowsInstance
				for _, s := range insts {
					if s.Src == src.Src && s.Dst == src.Dst && s.EID != src.EID {
						twins = append(twins, s)
					}
				}
				dst := src
				if len(twins) > 0 {
					dst = twins[seed.IntN(len(twins))]
				}
				return Op{Kind: OpUpdate, Cypher: tmplCopyKnowsInst, Params: map[string]any{
					"a": src.Src, "b": src.Dst, "srcEid": src.EID, "dstEid": dst.EID,
				}}
			}
		default:
			// ~7%: standalone DELETE r of one instance — endpoints and any parallel
			// twin must survive.
			if insts := oracle.KnowsInstancesByEID(); len(insts) > 0 {
				t := insts[seed.IntN(len(insts))]
				return Op{Kind: OpDelete, Cypher: tmplDeleteKnowsInst, Params: map[string]any{
					"a": t.Src, "b": t.Dst, "eid": t.EID,
				}}
			}
		}
	}
	name := fmt.Sprintf("ep%d", w.counter)
	w.counter++
	return Op{Kind: OpCreate, Cypher: tmplCreatePerson,
		Params: map[string]any{"name": name, "age": int64(seed.IntN(100))}}
}

// opCreateInstance builds a KNOWS-instance CREATE between a and b with a fresh
// unique eid and seed-derived since/weight.
func (w *EdgePropsWriter) opCreateInstance(seed *Seed, a, b string) Op {
	w.nextEID++
	since := fmt.Sprintf("2026-%02d-%02d", 1+int(seed.IntN(12)), 1+int(seed.IntN(28)))
	weight := float64(seed.IntN(100000)) / 100.0
	return Op{Kind: OpCreate, Cypher: tmplCreateKnowsInst,
		Params: map[string]any{"a": a, "b": b, "eid": w.nextEID, "since": since, "weight": weight}}
}

// opReplaceInstance builds a whole-entity replace (plain or WITH-projected)
// pinned to instance t, with fresh seed-derived since/weight and t's own eid
// (the eid is re-written by the map so the instance stays pinnable).
func (*EdgePropsWriter) opReplaceInstance(seed *Seed, t KnowsInstance, tmpl string) Op {
	since := fmt.Sprintf("2026-%02d-%02d", 1+int(seed.IntN(12)), 1+int(seed.IntN(28)))
	weight := float64(seed.IntN(100000)) / 100.0
	return Op{Kind: OpUpdate, Cypher: tmpl,
		Params: map[string]any{"a": t.Src, "b": t.Dst, "eid": t.EID, "since": since, "weight": weight}}
}

// KnowsInstancesByEID returns every modelled KNOWS edge instance that carries a
// non-zero eid, ascending by eid. The deterministic order is load-bearing: the
// edge-properties writer indexes into this slice with seed-derived integers, so
// a map-range order would break reproducibility. The returned slice is freshly
// allocated and owned by the caller.
func (o *GraphOracle) KnowsInstancesByEID() []KnowsInstance {
	var out []KnowsInstance
	for _, e := range o.edgeStates() { // already src/dst/label/eid-sorted
		if e.Label != "KNOWS" || e.EID == 0 {
			continue
		}
		out = append(out, KnowsInstance{Src: o.nameOf(e.SrcID), Dst: o.nameOf(e.DstID), EID: e.EID})
	}
	// Re-sort by eid alone: unique, so the order is total and deterministic.
	slices.SortFunc(out, func(a, b KnowsInstance) int { return cmp.Compare(a.EID, b.EID) })
	return out
}

// DeletedKnowsInstances returns every KNOWS instance the model deleted through
// the standalone DELETE r template, in deletion order. The returned slice
// aliases the oracle's backing store and must not be mutated.
func (o *GraphOracle) DeletedKnowsInstances() []KnowsInstance { return o.deletedKnows }

// knowsInstParams reads the (a, b, eid) target coordinates of an
// instance-pinned op and resolves the endpoint node ids. ok is false when a
// parameter is missing or ill-typed (a workload bug the oracle surfaces as an
// unmodelled result); found is false when either endpoint name is not
// modelled, i.e. the MATCH finds no row and the op is a committed no-op.
func (o *GraphOracle) knowsInstParams(params map[string]any) (k edgeKey, ok, found bool) {
	a, okA := paramString(params, "a")
	b, okB := paramString(params, "b")
	eid, okE := params["eid"].(int64)
	if !okA || !okB || !okE {
		return edgeKey{}, false, false
	}
	srcID, srcOK := o.byName[a]
	dstID, dstOK := o.byName[b]
	if !srcOK || !dstOK {
		return edgeKey{}, true, false
	}
	return edgeKey{src: srcID, dst: dstID, label: "KNOWS", eid: eid}, true, true
}

// createKnowsInst models [tmplCreateKnowsInst]: it adds a KNOWS edge instance
// keyed by its unique eid, carrying {eid, since, weight}. On the scenario's
// multigraph a second instance between an already-linked pair is a genuine
// parallel edge with its own property map, never a collapse onto the first. A
// missing endpoint is a committed no-effect result (the MATCH found nothing).
func (o *GraphOracle) createKnowsInst(params map[string]any) OracleResult {
	k, ok, found := o.knowsInstParams(params)
	if !ok {
		return OracleResult{ErrorMsg: "oracle: createKnowsInst missing/ill-typed param"}
	}
	if !found {
		return OracleResult{Committed: true} // MATCH found nothing; no edge created.
	}
	o.edges[k] = &EdgeState{
		SrcID: k.src, DstID: k.dst, Label: "KNOWS", EID: k.eid,
		Properties: map[string]any{"eid": k.eid, "since": params["since"], "weight": params["weight"]},
	}
	return OracleResult{Committed: true, EdgesCreated: 1}
}

// setKnowsWeight models [tmplSetKnowsWeight]: it re-assigns the weight of
// exactly the instance pinned by eid, leaving any parallel twin untouched. A
// miss (endpoint or instance absent) is a committed no-effect result.
func (o *GraphOracle) setKnowsWeight(params map[string]any) OracleResult {
	k, ok, found := o.knowsInstParams(params)
	if !ok {
		return OracleResult{ErrorMsg: "oracle: setKnowsWeight missing/ill-typed param"}
	}
	if found {
		if e, exists := o.edges[k]; exists {
			e.Properties["weight"] = params["weight"]
		}
	}
	return OracleResult{Committed: true}
}

// removeKnowsSince models [tmplRemoveKnowsSince] and [tmplSetKnowsSinceNull]
// (openCypher gives `SET r.since = null` the same removal semantics as
// `REMOVE r.since`): it removes the `since` property of exactly the instance
// pinned by eid. Removing an absent property (or missing the instance
// entirely) is a committed no-effect result, exactly as the engine counts it.
func (o *GraphOracle) removeKnowsSince(params map[string]any) OracleResult {
	k, ok, found := o.knowsInstParams(params)
	if !ok {
		return OracleResult{ErrorMsg: "oracle: removeKnowsSince missing/ill-typed param"}
	}
	if found {
		if e, exists := o.edges[k]; exists {
			delete(e.Properties, "since")
		}
	}
	return OracleResult{Committed: true}
}

// setKnowsNote models [tmplSetKnowsNote]: it plants the `note` property on
// exactly the instance pinned by eid — the instance-only-key seed for the
// replace teardown (rmp #2502). A miss is a committed no-effect result.
func (o *GraphOracle) setKnowsNote(params map[string]any) OracleResult {
	k, ok, found := o.knowsInstParams(params)
	if !ok {
		return OracleResult{ErrorMsg: "oracle: setKnowsNote missing/ill-typed param"}
	}
	if found {
		if e, exists := o.edges[k]; exists {
			e.Properties["note"] = params["note"]
		}
	}
	return OracleResult{Committed: true}
}

// replaceKnowsInst models [tmplReplaceKnowsInst] and
// [tmplReplaceKnowsInstWith]: after the whole-entity replace, the instance's
// property map equals EXACTLY {eid, since, weight} — any other key it carried
// (a planted `note` included) is gone, and the parallel twin is untouched. The
// read-back check ([CheckEdgeProperties]) then enforces the exact-map contract
// continuously, across crash/recovery too. A miss is a committed no-effect
// result.
func (o *GraphOracle) replaceKnowsInst(params map[string]any) OracleResult {
	k, ok, found := o.knowsInstParams(params)
	if !ok {
		return OracleResult{ErrorMsg: "oracle: replaceKnowsInst missing/ill-typed param"}
	}
	if found {
		if e, exists := o.edges[k]; exists {
			e.Properties = map[string]any{"eid": k.eid, "since": params["since"], "weight": params["weight"]}
		}
	}
	return OracleResult{Committed: true}
}

// copyKnowsInstParams reads the (a, b, srcEid, dstEid) coordinates of a
// copy-from-instance op and resolves both instance keys. ok is false when a
// parameter is missing or ill-typed; found is false when either endpoint name
// is not modelled (the MATCH finds no row and the op is a committed no-op).
func (o *GraphOracle) copyKnowsInstParams(params map[string]any) (src, dst edgeKey, ok, found bool) {
	a, okA := paramString(params, "a")
	b, okB := paramString(params, "b")
	srcEid, okS := params["srcEid"].(int64)
	dstEid, okD := params["dstEid"].(int64)
	if !okA || !okB || !okS || !okD {
		return edgeKey{}, edgeKey{}, false, false
	}
	srcID, srcOK := o.byName[a]
	dstID, dstOK := o.byName[b]
	if !srcOK || !dstOK {
		return edgeKey{}, edgeKey{}, true, false
	}
	return edgeKey{src: srcID, dst: dstID, label: "KNOWS", eid: srcEid},
		edgeKey{src: srcID, dst: dstID, label: "KNOWS", eid: dstEid}, true, true
}

// copyKnowsInst models [tmplCopyKnowsInst]: the target instance's property map
// becomes EXACTLY the SOURCE instance's map — what `RETURN properties(r1)`
// shows, never the per-pair aggregate (rmp #2503) — with the target's own eid
// re-pinned by the trailing single-property SET. The source map is cloned
// before the target is touched, so the degenerate self-copy (srcEid == dstEid)
// is modelled soundly. A miss on either instance is a committed no-effect
// result (the MATCH found no row).
func (o *GraphOracle) copyKnowsInst(params map[string]any) OracleResult {
	kSrc, kDst, ok, found := o.copyKnowsInstParams(params)
	if !ok {
		return OracleResult{ErrorMsg: "oracle: copyKnowsInst missing/ill-typed param"}
	}
	if found {
		e1, ok1 := o.edges[kSrc]
		e2, ok2 := o.edges[kDst]
		if ok1 && ok2 {
			next := make(map[string]any, len(e1.Properties)+1)
			for k, v := range e1.Properties {
				next[k] = v
			}
			next["eid"] = kDst.eid
			e2.Properties = next
		}
	}
	return OracleResult{Committed: true}
}

// deleteKnowsInst models [tmplDeleteKnowsInst]: it deletes exactly the
// instance pinned by eid — endpoints and any parallel twin survive — and
// records the tombstone so [CheckEdgeProperties] keeps asserting the deleted
// instance stays absent (across crash/recovery too) while both endpoints
// remain alive. A miss is a committed no-effect result.
func (o *GraphOracle) deleteKnowsInst(params map[string]any) OracleResult {
	k, ok, found := o.knowsInstParams(params)
	if !ok {
		return OracleResult{ErrorMsg: "oracle: deleteKnowsInst missing/ill-typed param"}
	}
	if found {
		if _, exists := o.edges[k]; exists {
			delete(o.edges, k)
			o.deletedKnows = append(o.deletedKnows, KnowsInstance{
				Src: o.nameOf(k.src), Dst: o.nameOf(k.dst), EID: k.eid,
			})
		}
	}
	return OracleResult{Committed: true}
}

// edgePropertiesWorkload is a 100% EdgePropsWriter mix.
func edgePropertiesWorkload(_ *Seed) *Workload {
	return &Workload{Actors: []Actor{&EdgePropsWriter{}}, Weights: []float64{1.0}}
}

// CheckEdgeProperties reads every modelled KNOWS edge's properties back through
// the real engine read path and asserts each round-trips to its modelled value
// (canonical type-aware compare) and that the edge still exists — pinning the
// probe to one instance with `WHERE r.eid` when the edge carries a non-zero
// eid, so a parallel twin can never answer for its sibling. It then walks the
// deleted-instance tombstones ([GraphOracle.DeletedKnowsInstances]) asserting
// each deleted instance is ABSENT while BOTH its endpoint nodes are still
// alive (DELETE r must never cascade to a node). Run on a quiescent graph
// including immediately after crash/recovery, so edge properties, per-instance
// deletions, and surviving twins are all validated against a WAL-recovered
// (and columnar-tier-rebuilt) graph.
func CheckEdgeProperties(tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	var vs []Violation
	cols := make([]string, len(knowsEdgePropKeys))
	for i, k := range knowsEdgePropKeys {
		cols[i] = "r." + k
	}
	proj := strings.Join(cols, ", ")
	ctx := context.Background()

	for _, e := range oracle.KnowsEdgesByName() {
		if e.Src == "" || e.Dst == "" {
			continue
		}
		match := fmt.Sprintf("MATCH (:Person {name:'%s'})-[r:KNOWS]->(:Person {name:'%s'})", e.Src, e.Dst)
		if e.EID != 0 {
			// Pin the probe to this instance so a parallel twin cannot mask a
			// wrong-instance mutation or a lost sibling.
			match += fmt.Sprintf(" WHERE r.eid = %d", e.EID)
		}
		got, err := engine.projectRowStrings(ctx, match+" RETURN "+proj, len(knowsEdgePropKeys))
		if err != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: "edge property read",
				Message: fmt.Sprintf("KNOWS(%q->%q eid=%d): read failed: %v", e.Src, e.Dst, e.EID, err)})
			continue
		}
		if got == nil {
			vs = append(vs, Violation{Kind: ViolationACIDDurability, Tick: tick, Op: "edge existence",
				Message: fmt.Sprintf("committed KNOWS(%q->%q eid=%d) absent in engine (did not survive recovery)", e.Src, e.Dst, e.EID)})
			continue
		}
		for i, k := range knowsEdgePropKeys {
			want := canonicalValueString(e.Props[k])
			if got[i] != want {
				vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "edge property value",
					Message: fmt.Sprintf("KNOWS(%q->%q eid=%d).%s = %s, want %s", e.Src, e.Dst, e.EID, k, got[i], want)})
			}
		}
	}

	// Deleted-instance tombstones: each DELETE r'd instance must stay absent
	// while both endpoint nodes stay alive.
	for _, d := range oracle.DeletedKnowsInstances() {
		q := fmt.Sprintf(
			"MATCH (:Person {name:'%s'})-[r:KNOWS]->(:Person {name:'%s'}) WHERE r.eid = %d RETURN r.eid",
			d.Src, d.Dst, d.EID)
		got, err := engine.projectRowStrings(ctx, q, 1)
		if err != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: "deleted edge probe",
				Message: fmt.Sprintf("KNOWS(%q->%q eid=%d): absence probe failed: %v", d.Src, d.Dst, d.EID, err)})
			continue
		}
		if got != nil {
			vs = append(vs, Violation{Kind: ViolationACIDDurability, Tick: tick, Op: "deleted edge resurrection",
				Message: fmt.Sprintf("DELETE r'd KNOWS(%q->%q eid=%d) is present in the engine", d.Src, d.Dst, d.EID)})
		}
		ep := fmt.Sprintf("MATCH (a:Person {name:'%s'}), (b:Person {name:'%s'}) RETURN a.name, b.name", d.Src, d.Dst)
		gotEP, err := engine.projectRowStrings(ctx, ep, 2)
		if err != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: "delete-r endpoint probe",
				Message: fmt.Sprintf("KNOWS(%q->%q eid=%d): endpoint probe failed: %v", d.Src, d.Dst, d.EID, err)})
			continue
		}
		if gotEP == nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: "delete-r endpoint survival",
				Message: fmt.Sprintf("an endpoint of DELETE r'd KNOWS(%q->%q eid=%d) no longer exists; DELETE r must not cascade to nodes", d.Src, d.Dst, d.EID)})
		}
	}
	return vs
}

// edgePropertiesScenario verifies the relationship write surface under the
// DST, on a directed MULTIGRAPH: the workload creates KNOWS edge instances
// carrying {eid, since, weight} — including parallel twins between the same
// endpoints — and mutates individual instances with standalone SET r.weight,
// REMOVE r.since, SET r.since = null, and DELETE r, each pinned by eid.
// [CheckEdgeProperties]
// confirms every surviving instance's properties round-trip (a mutated
// instance's parallel twin keeps its own map), every deleted instance stays
// absent with both endpoints alive, and all of it survives crash/recovery.
// The per-op counters oracle ([CheckOpCounters]) additionally pins each op's
// reported effect set. It is bit-reproducible.
func edgePropertiesScenario() Scenario {
	return Scenario{
		Name:        ScenarioEdgeProperties,
		Description: "relationship writes on parallel KNOWS instances (SET r.weight / REMOVE r.since / SET r.since = null / SET r.note / SET r = map plain+WITH / SET r2 = r1 copy-from-instance / DELETE r by eid): per-instance round-trip, exact-map replace, bag-authoritative copy source, twin survival, endpoints alive, crash/recovery",
		Mode:        ModeDeterministic,
		DefaultSeed: 0xED9E9405,
		MaxTicks:    500,
		Workload:    edgePropertiesWorkload,
		Crash:       CrashConfig{Enabled: true, CrashProb: 1.0 / 90.0, StabilityWindow: 25},
		Multigraph:  true,
		run:         runEdgeProperties,
	}
}

// edgePropertiesCheckEvery is the periodic edge-property check cadence.
const edgePropertiesCheckEvery = 80

// runEdgeProperties drives the edge-property safety loop: it builds and
// mutates KNOWS edge instances (SET / REMOVE / DELETE r over parallel pairs),
// verifies each op's reported counters against the oracle's expectation
// ([CheckOpCounters], rmp #2448), and runs [CheckEdgeProperties] periodically,
// after every crash/recovery, and once at the end. It is deterministic.
func runEdgeProperties(ctx context.Context, seed uint64) (*SimReport, error) {
	sc := edgePropertiesScenario()
	cfg := sc.DeterministicConfig(seed)
	sm, err := New(cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: edge-properties new: %w", err)
	}
	defer func() { _ = sm.Close() }()

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
			if v := CheckEdgeProperties(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery edge props>"}, v), nil
			}
		}

		actor := sm.workload.SelectActor(sm.seed)
		op := actor.NextOp(sm.seed, sm.oracle)
		committed, counters := sm.executeCounted(ctx, op)
		// Per-op counters oracle (#2448): a relationship SET must report exactly
		// one property assignment, REMOVE exactly one -properties when the pinned
		// instance still carries `since` (per-instance attribution, rmp #2500),
		// and DELETE r exactly one deleted relationship with zero deleted nodes —
		// all adjudicated on the pre-apply model.
		if v := CheckOpCounters(tick, op, committed, counters, sm.oracle); len(v) > 0 {
			return sm.report(tick, op, v), nil
		}
		sm.applyToOracle(op, committed)
		lastTick, lastOp = tick, op

		if tick%int64(sm.cfg.CheckEvery) == 0 {
			if v := sm.checker.Check(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, op, v), nil
			}
		}
		if tick%edgePropertiesCheckEvery == 0 {
			if v := CheckEdgeProperties(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, op, v), nil
			}
		}
	}
	if v := CheckEdgeProperties(lastTick, sm.oracle, sm.engine); len(v) > 0 {
		return sm.report(lastTick, lastOp, v), nil
	}
	return nil, nil
}
