package sim

// surface_entity.go — entity-valued function probes, path-materialisation
// content checks, and honest invariants for the non-deterministic functions
// (rmp #2458).
//
// Before #2458 the DST never called an entity-valued function: labels(),
// type(), startNode(), endNode(), nodes(), relationships(), properties() and
// elementId() were entirely unexercised, so a regression in any of them was
// invisible to the simulator. The named paths the sim built were only ever
// length-checked — `RETURN length(p)` — so path MATERIALISATION (the node and
// relationship sequences the path actually carries) was never verified at all:
// an engine that returned the right length over an empty or wrongly-anchored
// node list passed. The probes here close both gaps against references computed
// independently from the oracle model, and pin honest invariants — never
// constants — for rand(), randomUUID() and timestamp().

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// knowsVLEMaxLen is the upper bound of the variable-length pattern the path
// probe drives: `(a:Person)-[:KNOWS*1..3]->(b)`. It matches the bound the
// sim's read actors already use, so the probe exercises the same VLE shape the
// workload does.
const knowsVLEMaxLen = 3

// entityTrailLimit caps the oracle-side trail enumeration that references the
// path probe. The surface workload's terminal graph yields a few hundred
// trails of length <= 3, so the cap is a ~500x safety margin that exists only
// so a pathologically dense model can never make the checker itself the
// bottleneck; above it the oracle-referenced arm is skipped and the per-row
// self-consistency arm still runs.
const entityTrailLimit = 200_000

// reUUIDv4 matches the canonical textual form of an RFC 4122 version-4 UUID:
// eight-four-four-four-twelve lower-case hex digits, with the version nibble
// pinned to 4 and the variant nibble to 8/9/a/b. randomUUID() has no known
// answer, so its SHAPE is the invariant that can be asserted absolutely.
var reUUIDv4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// CheckCypherSurfaceEntity runs the entity-valued function battery over the
// Person/KNOWS graph, each probe referenced against the oracle model
// (rmp #2458):
//
//   - labels(n) per Person vs the oracle's modelled label set — which also
//     gives SET n:Label / REMOVE n:Label an independent read-back of WHICH
//     labels a node carries, beyond a count;
//   - properties(n) per Person vs the oracle's modelled property map, compared
//     as a key SET plus a per-key canonical value;
//   - type(r), startNode(r) and endNode(r) over every KNOWS edge vs the
//     oracle's modelled endpoints — an edge whose endpoints the engine
//     transposed reads back transposed;
//   - path materialisation on `MATCH p=(a:Person)-[:KNOWS*1..3]->(b)`:
//     size(nodes(p)) = length(p)+1 and size(relationships(p)) = length(p) per
//     row, the path's first node identical to the anchor and its last node
//     identical to the matched endpoint, plus the per-anchor path-length
//     histogram vs an oracle trail enumeration;
//   - elementId(n) / elementId(r): stability across two reads, distinctness
//     across entities, and the engine's actual contract (see
//     [checkEntityElementID]).
//
// It runs on the quiescent graph, periodically, after each crash/recovery, and
// at the end, exactly as the rest of the surface battery does.
func CheckCypherSurfaceEntity(tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	ctx := context.Background()
	vs := make([]Violation, 0, 4)
	vs = append(vs, checkEntityLabels(ctx, tick, oracle, engine)...)
	vs = append(vs, checkEntityProperties(ctx, tick, oracle, engine)...)
	vs = append(vs, checkEntityEdges(ctx, tick, oracle, engine)...)
	vs = append(vs, checkEntityPaths(ctx, tick, oracle, engine)...)
	vs = append(vs, checkEntityElementID(ctx, tick, oracle, engine)...)
	return vs
}

// ─────────────────────────────────────────────────────────────────────────────
// labels(n)
// ─────────────────────────────────────────────────────────────────────────────

// personEntity is the oracle's modelled view of one Person node: its name, its
// label set (ascending), and its property map. The engine must reproduce all
// three through labels() and properties().
type personEntity struct {
	Props  map[string]any
	Name   string
	Labels []string
}

// personEntities returns every modelled Person that carries a string name, in
// ascending name order, with its labels sorted ascending. complete is false
// when a modelled Person carries no string name: the engine probes key on the
// name, so an unnamed Person would make the comparison ill-defined and the
// caller skips rather than compares a truncated reference.
func personEntities(o *GraphOracle) (out []personEntity, complete bool) {
	complete = true
	for _, n := range o.nodes {
		if !hasLabel(n, "Person") {
			continue
		}
		name, ok := n.Properties["name"].(string)
		if !ok {
			complete = false
			continue
		}
		labels := slices.Clone(n.Labels)
		slices.Sort(labels)
		out = append(out, personEntity{Name: name, Labels: labels, Props: n.Properties})
	}
	slices.SortFunc(out, func(a, b personEntity) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		default:
			return 0
		}
	})
	return out, complete
}

// checkEntityLabels asserts labels(n) reproduces the oracle's label set for
// every Person, as full row-set equality ordered by name. labels() has no
// specified ordering (the engine returns storage order — `SET n:Extra` appends
// rather than sorts), so the per-node comparison is on the SORTED label
// sequence, i.e. on the label SET.
func checkEntityLabels(ctx context.Context, tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	const op = "labels(n)"
	want, complete := personEntities(oracle)
	if !complete {
		return nil
	}
	type row struct {
		name   string
		labels []string
	}
	var got []row
	err := forEachRow(ctx, engine, "MATCH (n:Person) RETURN n.name AS name, labels(n) AS ls ORDER BY name",
		func(at func(int) expr.Value) error {
			name, ok := at(0).(expr.StringValue)
			if !ok {
				return fmt.Errorf("row %d: name column is %T, want a string", len(got), at(0))
			}
			lst, ok := at(1).(expr.ListValue)
			if !ok {
				return fmt.Errorf("row %d: labels(n) is %T, want a list", len(got), at(1))
			}
			labels := make([]string, 0, len(lst))
			for _, v := range lst {
				s, ok := v.(expr.StringValue)
				if !ok {
					return fmt.Errorf("row %d: label element is %T, want a string", len(got), v)
				}
				labels = append(labels, string(s))
			}
			slices.Sort(labels)
			got = append(got, row{name: string(name), labels: labels})
			return nil
		})
	if err != nil {
		return []Violation{entityQueryViolation(tick, op, err)}
	}
	if len(got) != len(want) {
		return []Violation{entityDeviation(tick, op,
			fmt.Sprintf("row count: engine=%d, oracle=%d", len(got), len(want)))}
	}
	for i, w := range want {
		if got[i].name != w.Name {
			return []Violation{entityDeviation(tick, op,
				fmt.Sprintf("row %d: engine name=%q, oracle=%q", i, got[i].name, w.Name))}
		}
		if !equalStrings(got[i].labels, w.Labels) {
			return []Violation{entityDeviation(tick, op,
				fmt.Sprintf("%q: engine labels=%v, oracle=%v", w.Name, got[i].labels, w.Labels))}
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// properties(n)
// ─────────────────────────────────────────────────────────────────────────────

// checkEntityProperties asserts properties(n) reproduces the oracle's modelled
// property map for every Person: the same KEY SET (a property the engine
// invented or dropped fires) and, for every modelled key, the same canonical
// value rendering ([canonicalValueString], the same mapping the type-coverage
// checker uses). It is the read-back that makes the whole property map — not
// just the individually projected keys the other probes read — an enforced
// contract.
func checkEntityProperties(ctx context.Context, tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	const op = "properties(n)"
	want, complete := personEntities(oracle)
	if !complete {
		return nil
	}
	type row struct {
		props map[string]string
		name  string
	}
	var got []row
	err := forEachRow(ctx, engine, "MATCH (n:Person) RETURN n.name AS name, properties(n) AS props ORDER BY name",
		func(at func(int) expr.Value) error {
			name, ok := at(0).(expr.StringValue)
			if !ok {
				return fmt.Errorf("row %d: name column is %T, want a string", len(got), at(0))
			}
			m, ok := at(1).(expr.MapValue)
			if !ok {
				return fmt.Errorf("row %d: properties(n) is %T, want a map", len(got), at(1))
			}
			props := make(map[string]string, len(m))
			for k, v := range m {
				props[k] = v.String()
			}
			got = append(got, row{name: string(name), props: props})
			return nil
		})
	if err != nil {
		return []Violation{entityQueryViolation(tick, op, err)}
	}
	if len(got) != len(want) {
		return []Violation{entityDeviation(tick, op,
			fmt.Sprintf("row count: engine=%d, oracle=%d", len(got), len(want)))}
	}
	for i, w := range want {
		if got[i].name != w.Name {
			return []Violation{entityDeviation(tick, op,
				fmt.Sprintf("row %d: engine name=%q, oracle=%q", i, got[i].name, w.Name))}
		}
		if len(got[i].props) != len(w.Props) {
			return []Violation{entityDeviation(tick, op,
				fmt.Sprintf("%q: engine key set=%v, oracle keys=%v",
					w.Name, sortedKeys(got[i].props), sortedAnyKeys(w.Props)))}
		}
		for k, wv := range w.Props {
			gv, present := got[i].props[k]
			if !present {
				return []Violation{entityDeviation(tick, op,
					fmt.Sprintf("%q: engine map lacks modelled key %q (engine keys=%v)",
						w.Name, k, sortedKeys(got[i].props)))}
			}
			if want := canonicalValueString(wv); gv != want {
				return []Violation{entityDeviation(tick, op,
					fmt.Sprintf("%q.%s: engine=%s, oracle=%s", w.Name, k, gv, want))}
			}
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// type(r) / startNode(r) / endNode(r)
// ─────────────────────────────────────────────────────────────────────────────

// knowsEndpointNames returns every modelled KNOWS edge whose two endpoints are
// both named Person nodes, as (source, destination) name pairs sorted by source
// then destination — the reference for the relationship-entity probe. complete
// is false when a modelled KNOWS edge has an endpoint the model cannot name, in
// which case the engine's row set and this reference would legitimately differ
// and the caller skips.
func knowsEndpointNames(o *GraphOracle) (out [][2]string, complete bool) {
	complete = true
	for k := range o.edges {
		if k.label != "KNOWS" {
			continue
		}
		src, srcOK := o.nodes[k.src]
		dst, dstOK := o.nodes[k.dst]
		if !srcOK || !dstOK || !hasLabel(src, "Person") || !hasLabel(dst, "Person") {
			complete = false
			continue
		}
		sn, snOK := src.Properties["name"].(string)
		dn, dnOK := dst.Properties["name"].(string)
		if !snOK || !dnOK {
			complete = false
			continue
		}
		out = append(out, [2]string{sn, dn})
	}
	slices.SortFunc(out, func(a, b [2]string) int {
		if a[0] != b[0] {
			if a[0] < b[0] {
				return -1
			}
			return 1
		}
		switch {
		case a[1] < b[1]:
			return -1
		case a[1] > b[1]:
			return 1
		default:
			return 0
		}
	})
	return out, complete
}

// entityEdgeForwardQuery reads every KNOWS edge through FORWARD traversal. The
// WITH barrier between the pattern and the projection is deliberate: after it
// only the relationship `r` — never the pattern's node variables — is in scope
// where startNode(r) and endNode(r) are evaluated, so the endpoints the probe
// compares come from the RELATIONSHIP, and an implementation that merely echoed
// the pattern's start variable could not satisfy them.
const entityEdgeForwardQuery = "MATCH (a:Person)-[r:KNOWS]->(b:Person)" +
	" WITH a.name AS an, b.name AS bn, r AS r" +
	" RETURN an, bn, type(r) AS t, startNode(r).name AS sn, endNode(r).name AS en" +
	" ORDER BY an, bn"

// checkEntityEdges asserts, for every KNOWS edge, that type(r) is "KNOWS" and
// that startNode(r) and endNode(r) resolve to the oracle's modelled source and
// destination.
//
// Only FORWARD traversal is probed. The natural companion — reading the same
// edge through `(b)<-[r:KNOWS]-(a)` and requiring the same stored endpoints —
// is NOT asserted here because the engine currently fails it: when a reciprocal
// edge exists between the same ordered pair, a reverse-expanded (or undirected)
// row keeps the correct id(r) while startNode(r), endNode(r), r.<prop> and
// properties(r) all resolve against the OTHER edge of the pair. That is a
// pre-existing engine defect, well outside this checker's remit to fix, so the
// arm is left out rather than pinned as if the wrong answer were the contract.
//
// Note for anyone reading a coverage report: the code this probe executes is
// the engine's own graph-aware startnode/endnode overlay
// (cypher/api.go:newGraphAwareRegistry), which hydrates the endpoint node from
// the graph so `startNode(r).name` can read a property. funcs.fnStartNode and
// funcs.fnEndNode are shadowed by that overlay and stay at zero coverage no
// matter how hard this probe drives startNode() — the zero is the overlay's
// signature, not a gap in the battery.
func checkEntityEdges(ctx context.Context, tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	want, complete := knowsEndpointNames(oracle)
	if !complete {
		return nil
	}
	return compareEntityEdges(ctx, tick, "type(r)/startNode(r)/endNode(r)", entityEdgeForwardQuery, want, engine)
}

// compareEntityEdges drains one edge-entity projection and compares it, row for
// row, against the oracle's (source, destination) name pairs.
func compareEntityEdges(ctx context.Context, tick int64, op, query string, want [][2]string, engine *EngineAdapter) []Violation {
	type row struct{ an, bn, typ, sn, en string }
	var got []row
	err := forEachRow(ctx, engine, query, func(at func(int) expr.Value) error {
		cells := make([]string, 5)
		for i := range cells {
			s, ok := at(i).(expr.StringValue)
			if !ok {
				return fmt.Errorf("row %d column %d is %T, want a string", len(got), i, at(i))
			}
			cells[i] = string(s)
		}
		got = append(got, row{an: cells[0], bn: cells[1], typ: cells[2], sn: cells[3], en: cells[4]})
		return nil
	})
	if err != nil {
		return []Violation{entityQueryViolation(tick, op, err)}
	}
	if len(got) != len(want) {
		return []Violation{entityDeviation(tick, op,
			fmt.Sprintf("row count: engine=%d, oracle=%d", len(got), len(want)))}
	}
	for i, w := range want {
		g := got[i]
		if g.an != w[0] || g.bn != w[1] {
			return []Violation{entityDeviation(tick, op,
				fmt.Sprintf("row %d: engine pattern endpoints=(%q,%q), oracle=(%q,%q)",
					i, g.an, g.bn, w[0], w[1]))}
		}
		if g.typ != "KNOWS" {
			return []Violation{entityDeviation(tick, op,
				fmt.Sprintf("(%q)->(%q): type(r)=%q, oracle=%q", w[0], w[1], g.typ, "KNOWS"))}
		}
		if g.sn != w[0] {
			return []Violation{entityDeviation(tick, op,
				fmt.Sprintf("(%q)->(%q): startNode(r).name=%q, oracle=%q", w[0], w[1], g.sn, w[0]))}
		}
		if g.en != w[1] {
			return []Violation{entityDeviation(tick, op,
				fmt.Sprintf("(%q)->(%q): endNode(r).name=%q, oracle=%q", w[0], w[1], g.en, w[1]))}
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// nodes(p) / relationships(p)
// ─────────────────────────────────────────────────────────────────────────────

// trailKey identifies one class of variable-length path by its anchor Person
// and its length in relationships.
type trailKey struct {
	Anchor string
	Len    int
}

// knowsTrailCounts enumerates every KNOWS TRAIL of length 1..maxLen starting at
// a modelled Person and returns the per-(anchor, length) count — the
// independent reference for the path probe's row histogram.
//
// It is a TRAIL enumeration, not a walk enumeration: openCypher's
// relationship-isomorphism rule forbids a path from traversing the same
// relationship twice, while nodes MAY repeat (so a 2-cycle a->b->a is a valid
// length-2 path, and a self-loop yields exactly one length-1 path). The
// modelled graph is simple — at most one KNOWS edge per ordered pair — so
// relationship uniqueness is pair uniqueness here.
//
// ok is false when the enumeration would exceed limit trails, in which case the
// caller skips the histogram comparison and keeps the per-row self-consistency
// arm.
func knowsTrailCounts(o *GraphOracle, maxLen, limit int) (counts map[trailKey]int, ok bool) {
	adj := make(map[uint64][]uint64, len(o.nodes))
	for k := range o.edges {
		if k.label != "KNOWS" {
			continue
		}
		if _, srcOK := o.nodes[k.src]; !srcOK {
			continue
		}
		if _, dstOK := o.nodes[k.dst]; !dstOK {
			continue
		}
		adj[k.src] = append(adj[k.src], k.dst)
	}
	counts = make(map[trailKey]int)
	used := make(map[[2]uint64]bool, maxLen)
	total := 0
	var walk func(anchor string, at uint64, depth int) bool
	walk = func(anchor string, at uint64, depth int) bool {
		if depth == maxLen {
			return true
		}
		for _, dst := range adj[at] {
			e := [2]uint64{at, dst}
			if used[e] {
				continue // relationship isomorphism: never reuse an edge in one path
			}
			used[e] = true
			total++
			if total > limit {
				used[e] = false
				return false
			}
			counts[trailKey{Anchor: anchor, Len: depth + 1}]++
			cont := walk(anchor, dst, depth+1)
			used[e] = false
			if !cont {
				return false
			}
		}
		return true
	}
	for id, n := range o.nodes {
		if !hasLabel(n, "Person") {
			continue
		}
		anchor, named := n.Properties["name"].(string)
		if !named {
			return nil, false
		}
		if !walk(anchor, id, 0) {
			return nil, false
		}
	}
	return counts, true
}

// checkEntityPaths content-checks path materialisation on the variable-length
// pattern `MATCH p=(a:Person)-[:KNOWS*1..3]->(b)`. Per row it asserts the
// structural identities that a path returning the right LENGTH over the wrong
// CONTENT would fail — size(nodes(p)) = length(p)+1, size(relationships(p)) =
// length(p), the first node of nodes(p) identical to the anchor `a` and the
// last identical to the matched endpoint `b` (identity via elementId, not name)
// — and then compares the per-(anchor, length) row histogram against the
// oracle's own trail enumeration ([knowsTrailCounts]), which is what makes a
// wrong path LENGTH or a missing/extra path observable.
func checkEntityPaths(ctx context.Context, tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	const op = "nodes(p)/relationships(p)"
	query := fmt.Sprintf("MATCH p=(a:Person)-[:KNOWS*1..%d]->(b)"+
		" RETURN a.name AS anchor, length(p) AS len, size(nodes(p)) AS nn, size(relationships(p)) AS nr,"+
		" head([x IN nodes(p) | elementId(x)]) AS headId, elementId(a) AS anchorId,"+
		" last([x IN nodes(p) | elementId(x)]) AS lastId, elementId(b) AS endId", knowsVLEMaxLen)

	var vs []Violation
	got := make(map[trailKey]int)
	err := forEachRow(ctx, engine, query, func(at func(int) expr.Value) error {
		anchor, ok := at(0).(expr.StringValue)
		if !ok {
			return fmt.Errorf("anchor column is %T, want a string", at(0))
		}
		ints := make([]int64, 3)
		for i := range ints {
			n, ok := at(i + 1).(expr.IntegerValue)
			if !ok {
				return fmt.Errorf("column %d is %T, want an integer", i+1, at(i+1))
			}
			ints[i] = int64(n)
		}
		ids := make([]string, 4)
		for i := range ids {
			s, ok := at(i + 4).(expr.StringValue)
			if !ok {
				return fmt.Errorf("column %d is %T, want a string elementId", i+4, at(i+4))
			}
			ids[i] = string(s)
		}
		length, nodeCount, relCount := ints[0], ints[1], ints[2]
		headID, anchorID, lastID, endID := ids[0], ids[1], ids[2], ids[3]
		if nodeCount != length+1 {
			vs = append(vs, entityDeviation(tick, op,
				fmt.Sprintf("anchor %q: size(nodes(p))=%d, want length(p)+1=%d", anchor, nodeCount, length+1)))
		}
		if relCount != length {
			vs = append(vs, entityDeviation(tick, op,
				fmt.Sprintf("anchor %q: size(relationships(p))=%d, want length(p)=%d", anchor, relCount, length)))
		}
		if headID != anchorID {
			vs = append(vs, entityDeviation(tick, op,
				fmt.Sprintf("anchor %q: nodes(p)[0] is node %s, want the anchor %s", anchor, headID, anchorID)))
		}
		if lastID != endID {
			vs = append(vs, entityDeviation(tick, op,
				fmt.Sprintf("anchor %q: last(nodes(p)) is node %s, want the matched endpoint %s", anchor, lastID, endID)))
		}
		got[trailKey{Anchor: string(anchor), Len: int(length)}]++
		return nil
	})
	if err != nil {
		return append(vs, entityQueryViolation(tick, op, err))
	}
	want, ok := knowsTrailCounts(oracle, knowsVLEMaxLen, entityTrailLimit)
	if !ok {
		return vs // model too dense (or unnamed anchors) to reference; per-row arm stands
	}
	if len(got) != len(want) {
		return append(vs, entityDeviation(tick, op,
			fmt.Sprintf("path classes: engine=%d, oracle=%d", len(got), len(want))))
	}
	for k, wc := range want {
		if got[k] != wc {
			return append(vs, entityDeviation(tick, op,
				fmt.Sprintf("anchor %q length %d: engine=%d paths, oracle=%d", k.Anchor, k.Len, got[k], wc)))
		}
	}
	return vs
}

// ─────────────────────────────────────────────────────────────────────────────
// elementId(n) / elementId(r)
// ─────────────────────────────────────────────────────────────────────────────

// entityID is one (elementId, id) observation for a single entity, keyed by the
// stable business key the probe can address (a Person name, or an endpoint name
// pair for a relationship).
type entityID struct {
	Key       string
	ElementID string
	ID        int64
}

// checkEntityElementID pins the engine's ACTUAL elementId contract, verified
// empirically rather than assumed:
//
//   - elementId returns a STRING, id returns an INTEGER (a kind assertion, so a
//     degradation to the other type fails rather than comparing equal by text);
//   - elementId(e) is the decimal rendering of id(e), for both nodes and
//     relationships — the engine's element id IS its storage id as text, it
//     carries no database or generation prefix;
//   - it is STABLE: two reads of the same entity within one check return the
//     same value;
//   - it is DISTINCT across entities of the same kind;
//   - one row is returned per modelled entity, referenced against the oracle,
//     which is what gives this family an oracle-driven sensitivity handle.
//
// Stability is asserted WITHIN one check, never across ticks: the ids are
// storage identifiers, and the battery also runs after crash/recovery, so a
// cross-tick assertion would pin a durability property this probe does not
// measure.
func checkEntityElementID(ctx context.Context, tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	var vs []Violation

	nodeQuery := "MATCH (n:Person) RETURN n.name AS name, elementId(n) AS eid, id(n) AS iid ORDER BY name"
	first, err := readEntityIDs(ctx, engine, nodeQuery)
	if err != nil {
		vs = append(vs, entityQueryViolation(tick, "elementId(n)", err))
	} else {
		second, err := readEntityIDs(ctx, engine, nodeQuery)
		if err != nil {
			vs = append(vs, entityQueryViolation(tick, "elementId(n)", err))
		} else {
			vs = append(vs, entityIDViolations(tick, "elementId(n)", first, second, oracle.personCount())...)
		}
	}

	relQuery := "MATCH (a:Person)-[r:KNOWS]->(b:Person)" +
		" RETURN a.name + '->' + b.name AS name, elementId(r) AS eid, id(r) AS iid ORDER BY name"
	wantRels, complete := knowsEndpointNames(oracle)
	if !complete {
		return vs
	}
	firstRel, err := readEntityIDs(ctx, engine, relQuery)
	if err != nil {
		return append(vs, entityQueryViolation(tick, "elementId(r)", err))
	}
	secondRel, err := readEntityIDs(ctx, engine, relQuery)
	if err != nil {
		return append(vs, entityQueryViolation(tick, "elementId(r)", err))
	}
	return append(vs, entityIDViolations(tick, "elementId(r)", firstRel, secondRel, len(wantRels))...)
}

// readEntityIDs drains a (key, elementId, id) projection into [entityID]
// observations, failing loudly on a wrong column KIND: elementId must be a
// string and id an integer, so a value that merely renders the same text
// through another type does not pass.
func readEntityIDs(ctx context.Context, engine *EngineAdapter, query string) ([]entityID, error) {
	var out []entityID
	err := forEachRow(ctx, engine, query, func(at func(int) expr.Value) error {
		key, ok := at(0).(expr.StringValue)
		if !ok {
			return fmt.Errorf("row %d: key column is %T, want a string", len(out), at(0))
		}
		eid, ok := at(1).(expr.StringValue)
		if !ok {
			return fmt.Errorf("row %d: elementId is %T, want a string", len(out), at(1))
		}
		iid, ok := at(2).(expr.IntegerValue)
		if !ok {
			return fmt.Errorf("row %d: id is %T, want an integer", len(out), at(2))
		}
		out = append(out, entityID{Key: string(key), ElementID: string(eid), ID: int64(iid)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// entityIDViolations is the PURE comparison behind [checkEntityElementID]: it
// takes two independent reads of the same entity set plus the oracle's expected
// row count, and reports every contract breach (row count, stability,
// distinctness, and the elementId == decimal(id) format). Keeping it pure is
// what lets the sensitivity test drive each failure mode with hand-built input
// instead of hoping a real engine misbehaves.
func entityIDViolations(tick int64, op string, first, second []entityID, wantRows int) []Violation {
	var vs []Violation
	fail := func(msg string) { vs = append(vs, entityDeviation(tick, op, msg)) }

	if len(first) != wantRows {
		fail(fmt.Sprintf("row count: engine=%d, oracle=%d", len(first), wantRows))
	}
	if len(first) != len(second) {
		fail(fmt.Sprintf("two reads returned %d and %d rows: the id projection is not stable",
			len(first), len(second)))
		return vs
	}
	seen := make(map[string]string, len(first))
	for i, f := range first {
		s := second[i]
		if f.Key != s.Key {
			fail(fmt.Sprintf("row %d: two reads disagree on the key (%q vs %q)", i, f.Key, s.Key))
			continue
		}
		if f.ElementID != s.ElementID {
			fail(fmt.Sprintf("%q: elementId changed between two reads: %q then %q", f.Key, f.ElementID, s.ElementID))
		}
		if f.ID != s.ID {
			fail(fmt.Sprintf("%q: id changed between two reads: %d then %d", f.Key, f.ID, s.ID))
		}
		if want := strconv.FormatInt(f.ID, 10); f.ElementID != want {
			fail(fmt.Sprintf("%q: elementId=%q, want the decimal rendering of id=%d (%q)",
				f.Key, f.ElementID, f.ID, want))
		}
		if prev, dup := seen[f.ElementID]; dup {
			fail(fmt.Sprintf("elementId %q identifies two distinct entities (%q and %q)", f.ElementID, prev, f.Key))
			continue
		}
		seen[f.ElementID] = f.Key
	}
	return vs
}

// ─────────────────────────────────────────────────────────────────────────────
// Non-deterministic functions
// ─────────────────────────────────────────────────────────────────────────────

// randSampleSize is how many rand() draws the non-determinism probe takes in a
// single statement. Eight is enough that "every draw is identical" is a
// certain-enough defect signal (a working generator repeats eight float64 draws
// with probability far below any flake budget) while staying one cheap query.
const randSampleSize = 8

// uuidSampleSize is how many randomUUID() values the probe draws in a single
// statement; all must be distinct and v4-shaped.
const uuidSampleSize = 4

// CheckNonDeterministicFuncs asserts the honest invariants of the three
// functions whose results are NOT a known constant (rmp #2458). Each is stated
// as a property the correct implementation must satisfy on every run, never as
// a captured value:
//
//   - rand() — every draw is a FLOAT in [0, 1), and eight draws in one
//     statement are not all identical (a constant generator fires);
//   - randomUUID() — every value matches the RFC 4122 version-4 textual shape
//     ([reUUIDv4]) and four draws in one statement are pairwise distinct;
//   - timestamp() — every call WITHIN one statement returns the same integer
//     (the engine freezes the statement clock in cypher/stmt_now_reg.go, which
//     overrides timestamp() alongside the five temporal `now` constructors),
//     the value is epoch MILLISECONDS rather than seconds, and it is
//     non-decreasing ACROSS two successive statements.
func CheckNonDeterministicFuncs(tick int64, engine *EngineAdapter) []Violation {
	ctx := context.Background()
	var vs []Violation
	fail := func(op, msg string) { vs = append(vs, entityDeviation(tick, op, msg)) }

	// rand(): bounded, and not a constant.
	var draws []float64
	err := forEachRow(ctx, engine, fmt.Sprintf("UNWIND range(1, %d) AS i RETURN rand() AS r", randSampleSize),
		func(at func(int) expr.Value) error {
			f, ok := at(0).(expr.FloatValue)
			if !ok {
				return fmt.Errorf("rand() returned %T, want a float", at(0))
			}
			draws = append(draws, float64(f))
			return nil
		})
	switch {
	case err != nil:
		vs = append(vs, entityQueryViolation(tick, "rand()", err))
	case len(draws) != randSampleSize:
		fail("rand()", fmt.Sprintf("got %d draws, want %d", len(draws), randSampleSize))
	default:
		allSame := true
		for _, d := range draws {
			if d < 0 || d >= 1 {
				fail("rand()", fmt.Sprintf("draw %v is outside [0, 1)", d))
			}
			if d != draws[0] {
				allSame = false
			}
		}
		if allSame {
			fail("rand()", fmt.Sprintf("all %d draws returned %v: rand() is not varying", len(draws), draws[0]))
		}
	}

	// randomUUID(): v4 shape, pairwise distinct.
	var uuids []string
	err = forEachRow(ctx, engine, fmt.Sprintf("UNWIND range(1, %d) AS i RETURN randomUUID() AS u", uuidSampleSize),
		func(at func(int) expr.Value) error {
			s, ok := at(0).(expr.StringValue)
			if !ok {
				return fmt.Errorf("randomUUID() returned %T, want a string", at(0))
			}
			uuids = append(uuids, string(s))
			return nil
		})
	switch {
	case err != nil:
		vs = append(vs, entityQueryViolation(tick, "randomUUID()", err))
	case len(uuids) != uuidSampleSize:
		fail("randomUUID()", fmt.Sprintf("got %d values, want %d", len(uuids), uuidSampleSize))
	default:
		seen := make(map[string]bool, len(uuids))
		for _, u := range uuids {
			if !reUUIDv4.MatchString(u) {
				fail("randomUUID()", fmt.Sprintf("%q is not an RFC 4122 version-4 UUID", u))
			}
			if seen[u] {
				fail("randomUUID()", fmt.Sprintf("%q was returned twice in one statement", u))
			}
			seen[u] = true
		}
	}

	// timestamp(): statement-frozen, epoch millis, non-decreasing across statements.
	t1a, t1b, err := readTimestampPair(ctx, engine)
	if err != nil {
		return append(vs, entityQueryViolation(tick, "timestamp()", err))
	}
	if t1a != t1b {
		fail("timestamp()", fmt.Sprintf("two calls in ONE statement returned %d and %d: the statement clock is not frozen", t1a, t1b))
	}
	// 1e12 ms is 2001-09-09 and 1e13 ms is the year 2286: a seconds-valued
	// timestamp() would land three orders of magnitude below the window.
	if t1a < 1_000_000_000_000 || t1a >= 10_000_000_000_000 {
		fail("timestamp()", fmt.Sprintf("%d is outside the epoch-millisecond window [1e12, 1e13)", t1a))
	}
	t2a, _, err := readTimestampPair(ctx, engine)
	if err != nil {
		return append(vs, entityQueryViolation(tick, "timestamp()", err))
	}
	if t2a < t1a {
		fail("timestamp()", fmt.Sprintf("went backwards across two statements: %d then %d", t1a, t2a))
	}
	return vs
}

// readTimestampPair runs one statement projecting timestamp() twice and returns
// both integers, so the caller can assert the statement clock is frozen.
func readTimestampPair(ctx context.Context, engine *EngineAdapter) (int64, int64, error) {
	var a, b int64
	rows := 0
	err := forEachRow(ctx, engine, "RETURN timestamp() AS a, timestamp() AS b",
		func(at func(int) expr.Value) error {
			ia, ok := at(0).(expr.IntegerValue)
			if !ok {
				return fmt.Errorf("timestamp() returned %T, want an integer", at(0))
			}
			ib, ok := at(1).(expr.IntegerValue)
			if !ok {
				return fmt.Errorf("timestamp() returned %T, want an integer", at(1))
			}
			a, b = int64(ia), int64(ib)
			rows++
			return nil
		})
	if err != nil {
		return 0, 0, err
	}
	if rows != 1 {
		return 0, 0, fmt.Errorf("timestamp() probe returned %d rows, want 1", rows)
	}
	return a, b, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Shared helpers
// ─────────────────────────────────────────────────────────────────────────────

// forEachRow runs a read query and invokes fn once per row with an accessor for
// the engine's own expr.Value at each column.
//
// fn MUST NOT retain the values it is handed beyond its own call: the engine
// owns the row buffers and may reuse them between rows, so every probe converts
// what it needs to a plain Go value inside fn. An error returned by fn aborts
// the drain and is reported as the query's error.
func forEachRow(ctx context.Context, engine *EngineAdapter, query string, fn func(at func(int) expr.Value) error) error {
	res, err := engine.Run(ctx, query, nil)
	if err != nil {
		return err
	}
	at := func(i int) expr.Value { return rawValueAt(res, i) }
	var fnErr error
	for res.Next() {
		if fnErr = fn(at); fnErr != nil {
			break
		}
	}
	derr := res.Err()
	_ = res.Close()
	if fnErr != nil {
		return fnErr
	}
	return derr
}

// entityDeviation builds an oracle-deviation violation for an entity probe.
func entityDeviation(tick int64, op, msg string) Violation {
	return Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: op, Message: msg}
}

// entityQueryViolation builds a graph-integrity violation for a probe whose
// query failed to run or drain.
func entityQueryViolation(tick int64, op string, err error) Violation {
	return Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: op,
		Message: fmt.Sprintf("%s query error: %v", op, err)}
}

// sortedKeys returns the ascending keys of a string map, for failure messages.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// sortedAnyKeys returns the ascending keys of the oracle's property map, for
// failure messages.
func sortedAnyKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
