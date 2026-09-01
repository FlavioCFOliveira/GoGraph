package cypher

// lazy_relationship_value_test.go — rmp #2388. The relationship counterpart of
// the lazy node value: a bound relationship whose every dereference is a scalar
// property read is materialised as an [expr.LazyRelationshipValue] carrying no
// property map, and each touched property is read from storage on demand.
//
// Three properties make that sound, and each has a test here:
//
//  1. REACHABILITY. The lazy value is actually produced for the shapes it
//     targets. Without this the other two tests could pass over a code path that
//     never runs — the vacuous-gate failure mode this project has been bitten by
//     before. TestLazyRelationshipIsProducedForScalarUse drives the production
//     materialiser with the nodeScalarUse a REAL query execution memoised, so
//     the reachability claim is about the engine and not about a hand-built
//     fixture.
//
//  2. NON-ESCAPE. A lazy relationship must never reach a result row: the result
//     encoders type-switch on the concrete [expr.RelationshipValue], so a lazy
//     value arriving there would serialise as an unrecognised value — the
//     relationship analogue of the truncated property map analyseNodeScalarUse
//     exists to prevent. TestLazyRelationshipNeverReachesAResultRow scans every
//     cell of every row — recursing through lists, maps and paths — of a matrix
//     of shapes that return a relationship, each paired with the scalar read
//     that would otherwise trigger the lazy path.
//
//  3. BYTE-IDENTITY. The value the lazy resolver returns must be the value the
//     eager map would have held under that key, for every property kind
//     including the tagged temporal encodings, on BOTH storage routes (per-pair
//     and per-instance by-handle).
//     TestLazyRelationshipPropertyIsByteIdenticalToTheEagerMap compares them
//     directly against buildEdgeProps's own output, and
//     TestLazyRelationshipEndToEndMatchesTheEagerProjection compares two
//     queries that differ only in which path they take.

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// Fixtures
// ─────────────────────────────────────────────────────────────────────────────

// lazyRelTempProps are the SOH-tagged temporal encodings a Cypher write lays
// down (cypher/exec/temporal_literal.go). They are written here through the raw
// storage API so the PER-PAIR route carries them too — the Go public API has no
// temporal property kind of its own, and a decode regression on this route would
// otherwise be invisible.
var lazyRelTempProps = map[string]string{
	"d_date":      "\x01" + "2020-01-02",
	"d_localdt":   "\x02" + "2020-01-02T03:04:05",
	"d_datetime":  "\x03" + "2020-01-02T03:04:05Z",
	"d_localtime": "\x04" + "03:04:05",
	"d_time":      "\x05" + "03:04:05Z",
	"d_duration":  "\x06" + "P1DT2H",
}

// buildLazyRelPerPairGraph seeds a graph the way examples/26_social_scale_bench
// does — through the public Go API, which stamps an edge handle but records
// properties in the PER-PAIR store only, leaving the by-handle store empty. That
// is the route relLazyRoute reaches through its latch-is-false arm.
func buildLazyRelPerPairGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, k := range []string{"a", "b"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%q): %v", k, err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel(%q): %v", k, err)
		}
		if err := g.SetNodeProperty(k, "nm", lpg.StringValue(k)); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.SetEdgeLabel("a", "b", "R")
	set := func(k string, v lpg.PropertyValue) {
		t.Helper()
		if err := g.SetEdgeProperty("a", "b", k, v); err != nil {
			t.Fatalf("SetEdgeProperty(%q): %v", k, err)
		}
	}
	set("p_str", lpg.StringValue("hello"))
	set("p_int", lpg.Int64Value(-7))
	set("p_bigint", lpg.Int64Value(1<<40))
	set("p_float", lpg.Float64Value(1.5))
	set("p_bool", lpg.BoolValue(true))
	set("p_list", lpg.ListValue([]lpg.PropertyValue{
		lpg.Int64Value(1), lpg.StringValue("x"), lpg.BoolValue(false),
	}))
	// Kinds with NO Cypher mapping: lpgPropToExpr yields expr.Null for both, so
	// `r.k` must read null through the lazy path exactly as it does through the
	// map. Without these the null-mapping half of the conversion is untested.
	set("p_bytes", lpg.BytesValue([]byte{1, 2, 3}))
	for k, v := range lazyRelTempProps {
		set(k, lpg.StringValue(v))
	}
	return g
}

// buildLazyRelByHandleGraph seeds the same shape through CYPHER, which records
// the relationship's mandatory type by-handle and so routes every property read
// to the PER-INSTANCE store. That is the route relLazyRoute reaches through its
// hasByHandleEntry arm — and, measured at HEAD, the route 217 of the TCK's 218
// value-key relationship materialisations take.
func buildLazyRelByHandleGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	e := NewEngine(g)
	runWrite(t, e, `CREATE (a:P {nm:'a'}), (b:P {nm:'b'})`)
	runWrite(t, e, `MATCH (a:P {nm:'a'}), (b:P {nm:'b'}) CREATE (a)-[:R {
		p_str:'hello', p_int:-7, p_bigint:1099511627776, p_float:1.5, p_bool:true,
		p_list:[1,'x',false],
		d_date: date('2020-01-02'),
		d_localdt: localdatetime('2020-01-02T03:04:05'),
		d_datetime: datetime('2020-01-02T03:04:05Z'),
		d_localtime: localtime('03:04:05'),
		d_time: time('03:04:05Z'),
		d_duration: duration('P1DT2H')
	}]->(b)`)
	return g
}

// lazyRelCoords are the storage coordinates of the fixture's single
// relationship, discovered from the graph rather than assumed: a Cypher CREATE
// interns its own node keys (__cx_1, __cx_2), so the two fixtures do not share a
// naming scheme and a hard-coded key would silently test only one of them.
type lazyRelCoords struct {
	view        *lpg.ReadView[string, float64]
	srcKey      string
	dstKey      string
	srcID       uint64
	dstID       uint64
	handle      uint64
	hasByHandle bool
}

// lazyRelEdgeCoords resolves the fixture's one edge the way
// buildRelationshipValueFromRow does, so a test can drive the production
// materialiser and the production resolver against the same instance.
func lazyRelEdgeCoords(t *testing.T, g *lpg.Graph[string, float64]) lazyRelCoords {
	t.Helper()
	view := g.ReadAt(nil)
	var out lazyRelCoords
	found := 0
	g.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		for dst := range g.AdjList().Neighbours(key) {
			did, ok := g.AdjList().Mapper().Lookup(dst)
			if !ok {
				continue
			}
			found++
			out = lazyRelCoords{view: view, srcKey: key, dstKey: dst, srcID: uint64(id), dstID: uint64(did)}
		}
		return true
	})
	if found != 1 {
		t.Fatalf("the fixture holds %d edges, this helper assumes exactly 1", found)
	}
	out.handle, _ = view.FirstEdgeHandle(out.srcKey, out.dstKey)
	out.hasByHandle = len(view.EdgeLabelsByHandle(out.srcKey, out.dstKey, out.handle)) > 0
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. Reachability
// ─────────────────────────────────────────────────────────────────────────────

// lazyRelReachQueries are shapes whose relationship variable is dereferenced
// ONLY through scalar property reads, so the gate must admit them. Each is run
// through the engine so the analysis under test is the one the engine memoised,
// not one this test synthesised.
var lazyRelReachQueries = []string{
	`MATCH (a:P)-[r:R]->(b:P) WHERE r.p_int < 0 RETURN count(*) AS c`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN r.p_str AS s`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN r["p_str"] AS s`,
	`MATCH (a:P)-[r:R]->(b:P) WHERE r:R AND r.p_bool RETURN count(*) AS c`,
	`MATCH (a:P)-[r:R]->(b:P) WITH r.p_int AS i RETURN sum(i) AS s`,
	`MATCH (a:P)-[r:R]->(b:P) WHERE r.p_str IS NOT NULL AND r.p_int < 0 RETURN count(*) AS c`,
}

// lazyRelEagerQueries are shapes the gate must REJECT: the relationship (or its
// whole property map, or a field the lazy value cannot serve) is needed.
var lazyRelEagerQueries = []string{
	`MATCH (a:P)-[r:R]->(b:P) RETURN r`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN properties(r) AS p`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN keys(r) AS k`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN type(r) AS t`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN id(r) AS i`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN startNode(r) AS s`,
	// One EXPRESSION that both extracts a field and reads a property: the
	// extractor type-switches on a concrete RelationshipValue, so the whole
	// binding must stay eager. (Two SEPARATE projection items are a different
	// case — see TestLazyRelationshipGateIsPerExpressionNotPerQuery.)
	`MATCH (a:P)-[r:R]->(b:P) RETURN type(r) + r.p_str AS x`,
	`MATCH (a:P)-[r:R]->(b:P) WHERE r.p_str IS NOT NULL RETURN count(*) AS c`,
}

// relUsesFromMemo runs q and returns EVERY memoised nodeScalarUse the engine
// derived for variable v — one per analysed expression, because the gate is
// applied per expression and a query may analyse several. Returning them all,
// rather than an arbitrary first, is what keeps the assertions below
// deterministic: map iteration order would otherwise decide which one a test
// saw.
//
// An analysis that BAILED is reported as a nil element: a bailout nulls
// scalarUse at the call site, so the materialiser receives a nil use, which is
// the eager path.
func relUsesFromMemo(t *testing.T, g *lpg.Graph[string, float64], q, v string) []*nodeScalarUse {
	t.Helper()
	e := NewEngine(g)
	drainRows(t, e, q)
	entry, ok := e.cache.get(planCacheKeyFor(q))
	if !ok {
		t.Fatalf("query is not in the plan cache after one execution: %s", q)
	}
	var out []*nodeScalarUse
	entry.scalarUse.m.Range(func(_, val any) bool {
		a, isAnalysis := val.(*nodeScalarAnalysis)
		if !isAnalysis {
			return true
		}
		if a.bailout {
			out = append(out, nil)
			return true
		}
		if u, has := a.uses[v]; has {
			out = append(out, u)
		}
		return true
	})
	return out
}

// buildRelValueWithUse drives the PRODUCTION materialiser with a hand-built row
// that carries exactly what Expand emits — (srcID, edgeHandle, dstID) — and the
// supplied use, returning the value the engine would have bound to the variable.
func buildRelValueWithUse(t *testing.T, c lazyRelCoords, use *nodeScalarUse) expr.Value {
	t.Helper()
	row := exec.Row{expr.IntegerValue(c.srcID), expr.IntegerValue(c.handle), expr.IntegerValue(c.dstID)}
	meta := edgeVarInfo{edgeType: "R", acceptedTypes: []string{"R"}, srcCol: 0, edgeCol: 1, dstCol: 2}
	v, ok := buildRelationshipValueFromRow(row, meta, c.view, nil, use)
	if !ok {
		t.Fatal("buildRelationshipValueFromRow refused the synthesised row")
	}
	return v
}

// TestLazyRelationshipIsProducedForScalarUse is the reachability oracle for
// every other test in this file. It closes the loop between the ANALYSIS a real
// query execution produced and the MATERIALISER that consumes it: the use is
// taken from the engine's own memo, and the value is built by the engine's own
// function.
//
// Both storage routes are covered, because relLazyRoute admits them through
// different arms and only one of them is reachable from either fixture.
func TestLazyRelationshipIsProducedForScalarUse(t *testing.T) {
	for _, route := range []struct {
		name  string
		build func(*testing.T) *lpg.Graph[string, float64]
	}{
		{"per_pair", buildLazyRelPerPairGraph},
		{"by_handle", buildLazyRelByHandleGraph},
	} {
		t.Run(route.name, func(t *testing.T) {
			g := route.build(t)
			c := lazyRelEdgeCoords(t, g)
			if route.name == "by_handle" && !c.hasByHandle {
				t.Fatalf("the by-handle fixture produced no by-handle entry, so this route is untested")
			}
			if route.name == "per_pair" && c.view.AnyEdgeHandlePropertyEverWritten() {
				t.Fatalf("the per-pair fixture wrote a by-handle property, so this route is untested")
			}

			for _, q := range lazyRelReachQueries {
				t.Run("lazy/"+q, func(t *testing.T) {
					uses := relUsesFromMemo(t, g, q, "r")
					if len(uses) == 0 {
						t.Fatalf("no memoised analysis binds r, so this shape proves nothing")
					}
					for _, use := range uses {
						got := buildRelValueWithUse(t, c, use)
						if _, isLazy := got.(*expr.LazyRelationshipValue); !isLazy {
							t.Fatalf("expected a lazy relationship for a scalar-only use, got %T (use=%+v)", got, use)
						}
					}
				})
			}
			for _, q := range lazyRelEagerQueries {
				t.Run("eager/"+q, func(t *testing.T) {
					uses := relUsesFromMemo(t, g, q, "r")
					if len(uses) == 0 {
						// No analysis reached the materialiser at all, so it runs
						// with a nil use — itself the eager path. Assert that
						// explicitly rather than passing on an empty loop.
						uses = []*nodeScalarUse{nil}
					}
					for _, use := range uses {
						got := buildRelValueWithUse(t, c, use)
						if _, isLazy := got.(*expr.LazyRelationshipValue); isLazy {
							t.Fatalf("a whole-relationship / field-extractor use produced a LAZY value; "+
								"it could escape into a result row (use=%+v)", use)
						}
						if _, isEager := got.(expr.RelationshipValue); !isEager {
							t.Fatalf("expected an eager RelationshipValue, got %T", got)
						}
					}
				})
			}
		})
	}
}

// TestLazyRelationshipGateIsPerExpressionNotPerQuery documents — and pins — the
// granularity of the gate. `RETURN type(r) AS t, r.p_str AS s` analyses TWO
// independent expressions, each with its own RowContext, so the extractor item
// takes the eager path while the property item takes the lazy one. A future
// change that made the gate per QUERY would either lose the win on the second
// item or hand a lazy value to fnType; this test fails on both.
func TestLazyRelationshipGateIsPerExpressionNotPerQuery(t *testing.T) {
	g := buildLazyRelPerPairGraph(t)
	c := lazyRelEdgeCoords(t, g)
	const q = `MATCH (a:P)-[r:R]->(b:P) RETURN type(r) AS t, r.p_str AS s`

	uses := relUsesFromMemo(t, g, q, "r")
	if len(uses) < 2 {
		t.Fatalf("expected one analysis per projection item, got %d", len(uses))
	}
	lazy, eager := 0, 0
	for _, use := range uses {
		switch buildRelValueWithUse(t, c, use).(type) {
		case *expr.LazyRelationshipValue:
			lazy++
		case expr.RelationshipValue:
			eager++
		}
	}
	if lazy == 0 || eager == 0 {
		t.Fatalf("expected both paths across the two items, got lazy=%d eager=%d", lazy, eager)
	}
	// And the query itself must still answer correctly.
	got := drainRows(t, NewEngine(g), q)
	want := []string{`t="R"|s="hello"`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("query returned %v, want %v", got, want)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Non-escape
// ─────────────────────────────────────────────────────────────────────────────

// findLazyRel reports the first path at which v contains a lazy relationship,
// recursing through every container a result cell can nest one in. An empty
// string means none was found.
func findLazyRel(path string, v expr.Value) string {
	switch x := v.(type) {
	case *expr.LazyRelationshipValue:
		return path
	case *expr.LazyNodeValue:
		return path + "(lazy-node)"
	case expr.ListValue:
		for i, e := range x {
			if p := findLazyRel(fmt.Sprintf("%s[%d]", path, i), e); p != "" {
				return p
			}
		}
	case expr.MapValue:
		for k, e := range x {
			if p := findLazyRel(fmt.Sprintf("%s.%s", path, k), e); p != "" {
				return p
			}
		}
	case expr.NodeValue:
		for k, e := range x.Properties {
			if p := findLazyRel(fmt.Sprintf("%s.props.%s", path, k), e); p != "" {
				return p
			}
		}
	case expr.RelationshipValue:
		for k, e := range x.Properties {
			if p := findLazyRel(fmt.Sprintf("%s.props.%s", path, k), e); p != "" {
				return p
			}
		}
	case expr.PathValue:
		for i := range x.Nodes {
			if p := findLazyRel(fmt.Sprintf("%s.n[%d]", path, i), x.Nodes[i]); p != "" {
				return p
			}
		}
		for i := range x.Relationships {
			if p := findLazyRel(fmt.Sprintf("%s.r[%d]", path, i), x.Relationships[i]); p != "" {
				return p
			}
		}
	}
	return ""
}

// lazyRelEscapeQueries pair a scalar relationship read — the trigger for the
// lazy path — with every shape that puts a relationship, or something built from
// one, into the result. If the gate ever admitted one of these, a lazy value
// would be handed to the result encoders.
var lazyRelEscapeQueries = []string{
	`MATCH (a:P)-[r:R]->(b:P) RETURN r, r.p_int AS i`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN [r] AS l, r.p_int AS i`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN {rel: r} AS m, r.p_int AS i`,
	`MATCH p = (a:P)-[r:R]->(b:P) RETURN p, r.p_int AS i`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN collect(r) AS rs`,
	`MATCH (a:P)-[r:R]->(b:P) WITH r AS rr, r.p_int AS i RETURN rr, i`,
	`MATCH (a:P)-[r:R]->(b:P) WHERE r.p_int < 0 RETURN r`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN properties(r) AS p, r.p_int AS i`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN DISTINCT r`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN r ORDER BY r.p_int`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN CASE WHEN r.p_bool THEN r ELSE null END AS c`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN coalesce(r) AS c, r.p_int AS i`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN r{.p_str} AS mp`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN r AS one UNION ALL MATCH (a:P)-[q:R]->(b:P) RETURN q AS one`,
	// The shapes the win actually targets: no relationship in the result, so a
	// lazy value IS produced here and must still not leak through any cell.
	`MATCH (a:P)-[r:R]->(b:P) RETURN r.p_str AS s, r.p_int AS i, r.p_list AS l`,
	`MATCH (a:P)-[r:R]->(b:P) WHERE r.p_int < 0 RETURN a, b`,
	`MATCH (a:P)-[r:R]->(b:P) WITH r.p_int AS i, r.p_str AS s RETURN i, s`,
}

// TestLazyRelationshipNeverReachesAResultRow is acceptance criterion 2. It scans
// every cell of every row — recursing into lists, maps, nodes, relationships and
// paths — and fails on the first lazy value found anywhere in the result.
func TestLazyRelationshipNeverReachesAResultRow(t *testing.T) {
	for _, route := range []struct {
		name  string
		build func(*testing.T) *lpg.Graph[string, float64]
	}{
		{"per_pair", buildLazyRelPerPairGraph},
		{"by_handle", buildLazyRelByHandleGraph},
	} {
		t.Run(route.name, func(t *testing.T) {
			g := route.build(t)
			e := NewEngine(g)
			for _, q := range lazyRelEscapeQueries {
				t.Run(q, func(t *testing.T) {
					res, err := e.Run(context.Background(), q, nil)
					if err != nil {
						t.Fatalf("Run: %v", err)
					}
					defer func() {
						if cerr := res.Close(); cerr != nil {
							t.Errorf("Close: %v", cerr)
						}
					}()
					cols := res.Columns()
					rows := 0
					for res.Next() {
						rows++
						for i, c := range cols {
							if p := findLazyRel(c, res.ValueAt(i)); p != "" {
								t.Fatalf("a lazily materialised value reached a result row at %s: "+
									"the result encoders type-switch on the concrete value and would "+
									"mis-serialise it", p)
							}
						}
					}
					if err := res.Err(); err != nil {
						t.Fatalf("Err: %v", err)
					}
					if rows == 0 {
						t.Fatalf("the query returned no rows, so the scan asserted nothing")
					}
				})
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Byte-identity
// ─────────────────────────────────────────────────────────────────────────────

// TestLazyRelationshipPropertyIsByteIdenticalToTheEagerMap is acceptance
// criterion 3. For every property the fixture carries — every storage kind
// including all six tagged temporal encodings, and the two kinds that map to
// Cypher null — it compares what the lazy resolver returns against the entry the
// EAGER buildEdgeProps map holds under the same key, on both storage routes.
//
// The oracle is buildEdgeProps's own output rather than a hand-written
// expectation, so a change to the eager conversion cannot silently make the two
// agree on something wrong.
func TestLazyRelationshipPropertyIsByteIdenticalToTheEagerMap(t *testing.T) {
	for _, route := range []struct {
		name  string
		build func(*testing.T) *lpg.Graph[string, float64]
	}{
		{"per_pair", buildLazyRelPerPairGraph},
		{"by_handle", buildLazyRelByHandleGraph},
	} {
		t.Run(route.name, func(t *testing.T) {
			g := route.build(t)
			c := lazyRelEdgeCoords(t, g)

			eager := buildEdgeProps(c.view, c.srcKey, c.dstKey, c.handle, c.hasByHandle, nil)
			if len(eager) == 0 {
				t.Fatal("the eager map is empty, so there is nothing to compare")
			}
			relSrc, ok := relLazyRoute(c.view, c.srcKey, c.dstKey, c.handle, c.hasByHandle,
				&nodeScalarUse{keys: map[string]struct{}{"p_str": {}}})
			if !ok {
				t.Fatal("relLazyRoute rejected this fixture, so the lazy resolver is untested here")
			}
			if relSrc.ByHandle != (route.name == "by_handle") {
				t.Fatalf("route mismatch: relLazyRoute chose ByHandle=%v for the %s fixture",
					relSrc.ByHandle, route.name)
			}
			res := lazyRelResolver{g: c.view}

			// Distinct KINDS, not a count. A count of 6 is also satisfied by
			// five kinds plus a duplicate, which would let one tagged encoding
			// silently drop out of the comparison while the gate still read 6.
			seenTemporalKinds := map[reflect.Type]struct{}{}
			for key, want := range eager {
				got := expr.Null
				if v, found := res.RelProperty(relSrc, key); found {
					got = v
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("r.%s: lazy resolver returned %#v (%T), the eager map holds %#v (%T)",
						key, got, got, want, want)
				}
				switch want.(type) {
				case expr.DateValue, expr.LocalDateTimeValue, expr.DateTimeValue,
					expr.LocalTimeValue, expr.TimeValue, expr.DurationValue:
					seenTemporalKinds[reflect.TypeOf(want)] = struct{}{}
				}
			}
			if len(seenTemporalKinds) != len(lazyRelTempProps) {
				t.Errorf("the fixture decoded %d DISTINCT temporal kinds (%v), expected all %d tagged "+
					"encodings; the temporal half of the comparison is incomplete",
					len(seenTemporalKinds), seenTemporalKinds, len(lazyRelTempProps))
			}

			// An absent key must read null through the lazy path exactly as it
			// does through the map, and must NOT fall back to the other store.
			if v, found := res.RelProperty(relSrc, "no_such_key"); found {
				t.Errorf("an absent key resolved to %#v instead of reading as missing", v)
			}
		})
	}
}

// TestLazyRelationshipEndToEndMatchesTheEagerProjection compares two queries
// that differ ONLY in which materialisation path they take: `r.k` is the
// value-key scalar use the gate admits, while `properties(r).k` marks r as a
// whole-relationship use and so takes the eager path. Their projected values
// must be identical for every property kind.
func TestLazyRelationshipEndToEndMatchesTheEagerProjection(t *testing.T) {
	base := []string{"p_str", "p_int", "p_bigint", "p_float", "p_bool", "p_list"}
	keys := make([]string, 0, len(base)+len(lazyRelTempProps))
	keys = append(keys, base...)
	for k := range lazyRelTempProps {
		keys = append(keys, k)
	}
	for _, route := range []struct {
		name  string
		build func(*testing.T) *lpg.Graph[string, float64]
	}{
		{"per_pair", buildLazyRelPerPairGraph},
		{"by_handle", buildLazyRelByHandleGraph},
	} {
		t.Run(route.name, func(t *testing.T) {
			g := route.build(t)
			e := NewEngine(g)
			for _, k := range keys {
				t.Run(k, func(t *testing.T) {
					lazy := drainRows(t, e, fmt.Sprintf(
						`MATCH (a:P)-[r:R]->(b:P) RETURN r.%s AS v`, k))
					eager := drainRows(t, e, fmt.Sprintf(
						`MATCH (a:P)-[r:R]->(b:P) RETURN properties(r).%s AS v`, k))
					if !reflect.DeepEqual(lazy, eager) {
						t.Fatalf("r.%s projected %v through the lazy path and %v through the eager one",
							k, lazy, eager)
					}
					if len(lazy) == 0 {
						t.Fatal("no rows, so nothing was compared")
					}
				})
			}
		})
	}
}

// TestLazyRelationshipRouteIsExclusive pins the one property that makes a
// per-instance read safe: a parallel edge's own store is the ONLY store its
// values may come from. Two parallel relationships carrying DIFFERENT values
// under the same key must each report their own — if the resolver ever fell back
// from the by-handle store to the per-pair coalesced union, the later sibling's
// value would leak into the earlier one's answer.
func TestLazyRelationshipRouteIsExclusive(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	e := NewEngine(g)
	runWrite(t, e, `CREATE (a:P {nm:'a'}), (b:P {nm:'b'})`)
	runWrite(t, e, `MATCH (a:P {nm:'a'}), (b:P {nm:'b'}) CREATE (a)-[:R {w:10}]->(b)`)
	runWrite(t, e, `MATCH (a:P {nm:'a'}), (b:P {nm:'b'}) CREATE (a)-[:R {w:20}]->(b)`)

	got := drainRows(t, e, `MATCH (a:P)-[r:R]->(b:P) RETURN r.w AS w ORDER BY w`)
	want := []string{"w=10", "w=20"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parallel edges reported %v, want %v: the per-instance route is not exclusive", got, want)
	}

	// The sharper case: a sibling that carries NO value for the key. Its own
	// store has no record, and the PER-PAIR coalesced union does — so a resolver
	// that fell back on a by-handle miss would report the sibling's 10 instead of
	// null. Without this arm the exclusivity claim is untested for absence, which
	// is exactly where a fallback hides.
	g2 := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	e2 := NewEngine(g2)
	runWrite(t, e2, `CREATE (a:P {nm:'a'}), (b:P {nm:'b'})`)
	runWrite(t, e2, `MATCH (a:P {nm:'a'}), (b:P {nm:'b'}) CREATE (a)-[:R {w:10}]->(b)`)
	runWrite(t, e2, `MATCH (a:P {nm:'a'}), (b:P {nm:'b'}) CREATE (a)-[:R {q:1}]->(b)`)

	gotAbs := drainRows(t, e2, `MATCH (a:P)-[r:R]->(b:P) RETURN r.w AS w ORDER BY w`)
	wantAbs := []string{"w=10", "w=null"}
	if !reflect.DeepEqual(gotAbs, wantAbs) {
		t.Fatalf("a sibling with no value for the key reported %v, want %v: "+
			"the resolver fell back to the coalesced union on a per-instance miss", gotAbs, wantAbs)
	}
	// The same answer through the EAGER path, so the absolute expectation above
	// is anchored to the behaviour this change had to preserve rather than to a
	// literal someone could have copied from the new implementation's output.
	eagerAbs := drainRows(t, e2, `MATCH (a:P)-[r:R]->(b:P) RETURN properties(r).w AS w ORDER BY w`)
	if !reflect.DeepEqual(gotAbs, eagerAbs) {
		t.Fatalf("the lazy path reported %v where the eager path reports %v", gotAbs, eagerAbs)
	}
}

// TestLazyRelationshipReversedHopReadsTheStorageEndpoints guards the coordinate
// the lazy value carries. An undirected reverse hop binds the edge with the
// TRAVERSAL endpoints inverted relative to storage, and buildRelationshipValueFromRow
// swaps them before the property lookup. The lazy value must capture the SWAPPED
// pair, or the reverse row reads an edge that does not exist and reports null.
func TestLazyRelationshipReversedHopReadsTheStorageEndpoints(t *testing.T) {
	for _, route := range []struct {
		name  string
		build func(*testing.T) *lpg.Graph[string, float64]
	}{
		{"per_pair", buildLazyRelPerPairGraph},
		{"by_handle", buildLazyRelByHandleGraph},
	} {
		t.Run(route.name, func(t *testing.T) {
			g := route.build(t)
			e := NewEngine(g)
			// The undirected pattern emits both the forward and the reverse row
			// for the one stored edge, so both orientations read the property.
			got := drainRows(t, e, `MATCH (a:P)-[r:R]-(b:P) RETURN r.p_str AS s ORDER BY s`)
			want := []string{`s="hello"`, `s="hello"`}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("undirected match read %v, want %v: a reverse hop lost its property", got, want)
			}
		})
	}
}

// TestLazyRelationshipEachBindingIsItsOwnInstance pins the consequence of
// allocating fresh instead of reusing a pooled struct: two relationship
// variables bound in one row are independent values. An arena variant was
// measured and rejected (see buildRelationshipValueFromRow); if one is ever
// reintroduced, this is the property it must not break.
func TestLazyRelationshipEachBindingIsItsOwnInstance(t *testing.T) {
	src1 := expr.RelSource{StartKey: "a", EndKey: "b"}
	src2 := expr.RelSource{StartKey: "b", EndKey: "c"}
	v1 := expr.NewLazyRelationshipValue(1, 10, 11, "R1", src1, nil)
	v2 := expr.NewLazyRelationshipValue(2, 20, 21, "R2", src2, nil)
	if v1 == v2 {
		t.Fatal("two bindings share one instance")
	}
	if v1.ID() != 1 || v1.RelType() != "R1" || v1.StartID() != 10 || v1.EndID() != 11 {
		t.Fatalf("the first binding is wrong: %+v", v1)
	}
	if v2.ID() != 2 || v2.RelType() != "R2" || v2.StartID() != 20 || v2.EndID() != 21 {
		t.Fatalf("the second binding is wrong: %+v", v2)
	}
	// A nil resolver must read as null rather than panic: it is the state a
	// zero-valued instance is in, and Property is called on whatever the engine
	// bound.
	if got := v1.Property("anything"); !expr.IsNull(got) {
		t.Fatalf("a resolver-less lazy relationship returned %#v, want null", got)
	}
}

// TestForWorkerClearsTheRelResolver pins the concurrency contract of the new
// buildOpts field. relResolver is assigned by lazyRelResolverFor on first use
// WITHOUT synchronisation, exactly like nodeResolver, and the morsel-parallel
// factory rebuilds its sub-plan on the WORKER goroutine — so a shared copy would
// be two goroutines writing one field. forWorker must hand each worker a nil.
//
// It asserts both directions: that the copy is cleared (the contract) and that
// the SOURCE keeps its resolver (the non-vacuity oracle — a nil copy must mean
// "deliberately cleared", not "never populated"). It mirrors
// TestForWorker_ClearsTheProfiler_2664, which exists because that field was
// missed once already.
func TestForWorkerClearsTheRelResolver(t *testing.T) {
	t.Parallel()

	res := lazyRelResolver{}
	bopts := &buildOpts{relResolver: res}

	worker := bopts.forWorker()
	if worker.relResolver != nil {
		t.Errorf("forWorker kept relResolver on the per-worker copy. Each morsel worker " +
			"calls the sub-plan factory on its OWN goroutine and lazyRelResolverFor " +
			"assigns this field unsynchronised, so sharing it is a data race — the same " +
			"one nodeResolver is cleared to avoid.")
	}
	if bopts.relResolver != expr.RelationshipResolver(res) {
		t.Fatalf("the SOURCE buildOpts lost its relResolver; forWorker must clear the copy, " +
			"never the original. This assertion is also what stops the check above from " +
			"passing vacuously on a build where the field was never set.")
	}
	if got := (&buildOpts{}).forWorker(); got.relResolver != nil {
		t.Errorf("forWorker on a resolver-less buildOpts produced a non-nil relResolver")
	}
}

// TestLazyRelationshipDeletedRelationshipTakesTheEagerPath pins the
// DeletedEntityAccess contract the lazy value preserves BY CONSTRUCTION: DELETE
// stamps the row column with a Deleted RelationshipValue carrying a frozen
// property snapshot, and populateRowCtx forwards that value before the lazy path
// is reached. Reading a property off it must still raise the openCypher error
// rather than silently resolving from storage.
func TestLazyRelationshipDeletedRelationshipTakesTheEagerPath(t *testing.T) {
	for _, route := range []struct {
		name  string
		build func(*testing.T) *lpg.Graph[string, float64]
	}{
		{"per_pair", buildLazyRelPerPairGraph},
		{"by_handle", buildLazyRelByHandleGraph},
	} {
		t.Run(route.name, func(t *testing.T) {
			g := route.build(t)
			e := NewEngine(g)
			res, err := e.RunInTx(context.Background(),
				`MATCH (a:P)-[r:R]->(b:P) DELETE r RETURN r.p_str AS s`, nil)
			if err != nil {
				// A build-time rejection is also an acceptable refusal.
				if !containsAll(err.Error(), "DeletedEntityAccess") {
					t.Fatalf("expected a DeletedEntityAccess error, got: %v", err)
				}
				return
			}
			for res.Next() { //nolint:revive // intentional full drain
			}
			drainErr := res.Err()
			_ = res.Close()
			if drainErr == nil {
				t.Fatal("reading a property of a deleted relationship did not fail: " +
					"the lazy path resolved it from storage instead of seeing the DELETE stamp")
			}
			if !containsAll(drainErr.Error(), "DeletedEntityAccess") {
				t.Fatalf("expected a DeletedEntityAccess error, got: %v", drainErr)
			}
		})
	}
}

// containsAll reports whether s contains every substring.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
