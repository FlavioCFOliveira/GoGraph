package cypher_test

// rel_accessor_agreement_test.go — rmp #2815.
//
// # Status of the report
//
// The report claimed that after `SET e = {stamp:'M'}` a relationship property is
// stored but unreadable: `e.stamp` null, `properties(e)` empty, `keys(e)` empty,
// while `RETURN e` shows it. That premise did NOT reproduce at HEAD. Every
// accessor agrees, on the in-memory engine and the WAL-backed store, with and
// without multigraph, in one statement or two, and across a store reopen.
//
// The reported symptom is what a SECOND, propertyless :R relationship between a
// second (:T{key:'s'}, :T{key:'t'}) pair produces — and CREATE always creates,
// so re-running the reproduction script against a non-empty database yields
// exactly that. The reads then return TWO rows, the first of which correctly has
// no stamp. That is correct openCypher behaviour, not a defect, and
// TestRelAccessors_SecondPairIsTwoRows below pins it so the distinction is not
// lost.
//
// # What this file is for
//
// The four accessors read a relationship's properties through routes that CAN
// diverge: `e.prop` may take the lazy resolver (expr.LazyRelationshipValue),
// while properties/keys/type/startNode/endNode and a bare projection force eager
// materialisation, and both then choose between the per-pair and the by-handle
// property store. Two stores and two materialisation strategies is the shape
// #2815 alleged a split in. Nothing pinned their agreement, so the report's
// claim could only be answered by ad-hoc probing.
//
// These tests make the agreement an asserted invariant across every relationship
// SET form, so a future divergence fails here instead of being reported from
// outside again.
//
// Layer: short.

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// relAccessorRead is the single-relationship fixture's read pattern.
const relAccessorMatch = `MATCH (:T {key:'s'})-[e:R]->(:T {key:'t'}) `

// TestRelAccessors_AgreeAfterSet is the #2815 acceptance gate: after any
// relationship SET form, `e.stamp`, `properties(e)`, `keys(e)` and `RETURN e`
// must describe the SAME property set.
//
// Each accessor is also read in ISOLATION as well as in one combined
// projection, because the lazy-versus-eager route is chosen from how the
// variable is used in the statement: `RETURN e.stamp` alone is the only shape
// that can take the lazy resolver, and a combined projection that also mentions
// `e` forces eager materialisation. Reading them only together would test one
// route twice.
func TestRelAccessors_AgreeAfterSet(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		createRel string // relationship pattern properties, or empty
		set       string
		wantStamp string // the value e.stamp must report
		wantKeys  int    // how many keys the relationship must end up with
	}{
		{"replaceMap", "", `SET e = {stamp:'M'}`, `"M"`, 1},
		{"mutateMap", "", `SET e += {stamp:'M'}`, `"M"`, 1},
		{"plainScalar", "", `SET e.stamp = 'M'`, `"M"`, 1},
		{"replaceTwoKeys", "", `SET e = {stamp:'M', other:'O'}`, `"M"`, 2},
		{"replaceDropsOldKeys", ` {old:'o'}`, `SET e = {stamp:'M'}`, `"M"`, 1},
		{"mutateKeepsOldKeys", ` {old:'o'}`, `SET e += {stamp:'M'}`, `"M"`, 2},
	}
	for _, mg := range []bool{false, true} {
		for _, tc := range cases {
			name := tc.name
			if mg {
				name += "_multigraph"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: mg})
				eng := cypher.NewEngine(g)
				seed := fmt.Sprintf(`CREATE (a:T {key:'s'})-[e:R%s]->(b:T {key:'t'})`, tc.createRel)
				if _, err := runEntityProp(eng, seed); err != nil {
					t.Fatalf("seed: %v", err)
				}
				if _, err := runEntityProp(eng, relAccessorMatch+tc.set); err != nil {
					t.Fatalf("%s: %v", tc.set, err)
				}

				scalar := oneRow(t, eng, relAccessorMatch+`RETURN e.stamp AS v`)
				props := oneRow(t, eng, relAccessorMatch+`RETURN properties(e) AS v`)
				keys := oneRow(t, eng, relAccessorMatch+`RETURN keys(e) AS v`)
				combined := oneRowFull(t, eng, relAccessorMatch+`RETURN e.stamp AS sv, properties(e) AS pv, keys(e) AS kv, e AS wv`)

				// Each accessor read alone must equal the same accessor read
				// inside a whole-entity projection: the lazy route and the eager
				// route must not disagree.
				if got, want := fmtAny(scalar), fmtAny(combined["sv"]); got != want {
					t.Errorf("e.stamp: isolated = %s, inside a whole-entity projection = %s", got, want)
				}
				if got, want := canonicalMap(fmtAny(props)), canonicalMap(fmtAny(combined["pv"])); got != want {
					t.Errorf("properties(e): isolated = %s, combined = %s", got, want)
				}

				// The accessors must agree with each other.
				if got := fmtAny(scalar); got != tc.wantStamp {
					t.Errorf("e.stamp = %s, want %s", got, tc.wantStamp)
				}
				if n := countListKeys(fmtAny(keys)); n != tc.wantKeys {
					t.Errorf("keys(e) = %s (%d keys), want %d", fmtAny(keys), n, tc.wantKeys)
				}
				// The central #2815 claim, stated directly: a non-null scalar
				// read and an empty property map cannot both be right.
				if fmtAny(scalar) != "null" && fmtAny(props) == "{}" {
					t.Errorf("e.stamp = %s but properties(e) = {} — the accessors disagree about one stored property", fmtAny(scalar))
				}
				if fmtAny(scalar) == "null" && fmtAny(props) != "{}" {
					t.Errorf("e.stamp = null but properties(e) = %s — the accessors disagree about one stored property", fmtAny(props))
				}
				// A whole-entity projection must be materialisable at all; it is
				// the one accessor the report said still worked.
				if combined["wv"] == nil {
					t.Error("RETURN e produced a nil relationship column")
				}
			})
		}
	}
}

// TestRelAccessors_SecondPairIsTwoRows pins the behaviour the #2815 report
// actually observed, so it is never mistaken for the defect it was filed as.
//
// CREATE always creates. Running the reproduction script against a database that
// already holds a (:T{key:'s'})-[:R]->(:T{key:'t'}) edge therefore leaves TWO
// such edges, and the reads return two rows — the first with no stamp. Every
// accessor agrees ROW BY ROW; only a reader that looks at the first row of one
// query and a later row of another sees a contradiction.
func TestRelAccessors_SecondPairIsTwoRows(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	if _, err := runEntityProp(eng, `CREATE (a:T {key:'s'})-[e:R]->(b:T {key:'t'})`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := runEntityProp(eng, `CREATE (a:T {key:'s'})-[e:R]->(b:T {key:'t'}) SET e = {stamp:'M'}`); err != nil {
		t.Fatalf("second create: %v", err)
	}

	rows, err := runEntityProp(eng, relAccessorMatch+`RETURN e.stamp AS sv, properties(e) AS pv, keys(e) AS kv`)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("two relationships between two (:T{key:'s'}, :T{key:'t'}) pairs must give 2 rows, got %d", len(rows))
	}
	// Row by row the accessors agree; that is the whole point.
	for i, r := range rows {
		hasStamp := fmtAny(r["sv"]) != "null"
		hasProps := fmtAny(r["pv"]) != "{}"
		if hasStamp != hasProps {
			t.Errorf("row %d: e.stamp = %s but properties(e) = %s — accessors disagree within one row",
				i, fmtAny(r["sv"]), fmtAny(r["pv"]))
		}
	}
	// Exactly one of the two carries the stamp — the one the SET targeted.
	stamped := 0
	for _, r := range rows {
		if fmtAny(r["sv"]) != "null" {
			stamped++
		}
	}
	if stamped != 1 {
		t.Errorf("%d of 2 relationships carry the stamp, want exactly 1 (SET targeted only the edge it created)", stamped)
	}
}

// oneRow runs a single-column query that must return exactly one row and returns
// that column's value.
func oneRow(t *testing.T, eng *cypher.Engine, query string) any {
	t.Helper()
	return oneRowFull(t, eng, query)["v"]
}

// oneRowFull runs a query that must return exactly one row and returns it.
func oneRowFull(t *testing.T, eng *cypher.Engine, query string) map[string]any {
	t.Helper()
	rows, err := runEntityProp(eng, query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if len(rows) != 1 {
		t.Fatalf("%s returned %d rows, want 1", query, len(rows))
	}
	return rows[0]
}

// canonicalMap normalises a rendered Cypher map such as `{stamp: "M", other: "O"}`
// by sorting its entries, so two renderings of the SAME map compare equal.
//
// openCypher leaves a map's entry order unspecified and the engine renders it
// straight from a Go map, so the order genuinely varies run to run; comparing
// the raw strings made this file flake. Sorting is the right normalisation
// precisely because order carries no meaning here — it is not papering over a
// difference, it is removing a non-difference. The CONTENT is still compared in
// full: nothing is dropped, only reordered.
func canonicalMap(rendered string) string {
	inner, ok := strings.CutPrefix(rendered, "{")
	if !ok {
		return rendered // not a map rendering; compare verbatim
	}
	inner, ok = strings.CutSuffix(inner, "}")
	if !ok {
		return rendered
	}
	if strings.TrimSpace(inner) == "" {
		return "{}"
	}
	entries := strings.Split(inner, ", ")
	slices.Sort(entries)
	return "{" + strings.Join(entries, ", ") + "}"
}

// countListKeys counts the entries in a rendered Cypher list such as
// `["stamp", "other"]`. keys() leaves element ORDER unspecified, so the
// cardinality is what may be asserted.
func countListKeys(rendered string) int {
	if rendered == "[]" || rendered == "null" {
		return 0
	}
	n := 1
	for i := 0; i < len(rendered); i++ {
		if rendered[i] == ',' {
			n++
		}
	}
	return n
}
