//go:build soak || nightly

package audit352_test

// relprops_soak_test.go — every test that drives the relGraph fixture (rmp #2667).
//
// # Why this file is soak-layer
//
// The three tests here share one fixture, buildRelGraph in relprops_test.go, and
// that fixture is the cost. It creates 4 000 nodes and 4 000 five-property
// relationships one Cypher statement at a time, and each relationship statement
// is a `MATCH (a:P {sid:...}), (b:P {sid:...}) CREATE ...` — two label scans over
// a graph that is still growing. That shape is deliberate (a Cypher-created
// relationship lands in the by-handle property store, which is the read path
// under test; the plain Go SetEdgeProperty API writes the columnar store
// instead), so the cost cannot be traded away without measuring something else.
//
// Measured on the reference host (Apple M4, 10 cores, darwin/arm64, go1.27.0),
// in-package under -race, load average 1.79 before / 2.55 after:
//
//	TestRelationshipPropsPlans          82.75 s   <- pays the fixture build
//	TestRelPropertyMaterialisationCount  3.69 s
//	TestSubqueryShapeRowCounts           0.21 s
//
// The split is an artefact of ordering, not of the tests: buildRelGraph caches
// into a package-level variable, so whichever consumer runs first pays ~82.5 s
// and the rest pay almost nothing. That is why ALL THREE are gated together.
// Gating only the largest would have moved the 82.5 s onto TestSubqueryShapeRowCounts
// and saved the short layer nothing measurable.
//
// Left in the short layer they were 86.65 s of a package that measured 399.77 s
// under -race, against the 240 s hard ceiling that scripts/pkg_time_budget.sh
// fails `make ci` on. See docs/test-layers.md.
//
// # What each one asserts — stated, because moving a gate is not the same as
// # moving a measurement
//
//   - TestRelationshipPropsPlans: NOTHING about the module's results. It calls
//     Explain and countRows for each shape and t.Logf's both. It fails only if
//     Explain, Run, Err or Close returns an error. A measurement.
//   - TestSubqueryShapeRowCounts: likewise NOTHING, despite the name. It logs
//     countRows' answer and never compares it. A measurement.
//   - TestRelPropertyMaterialisationCount: this one DOES carry a value
//     assertion — `rows != relNodes` for each of its seven projection shapes.
//     It is not a threshold on the quantity it measures (the medians and the
//     ratio against 1_prop are logged, never asserted); it is the
//     measurement-validity guard that fixture_test.go's countRows documents,
//     which fails an arm that silently measured a different workload. It moves
//     here only because it cannot be separated from the fixture, and the
//     cardinality it pins — `MATCH ()-[r:R]->() RETURN <props>` shipping one row
//     per relationship — is core openCypher semantics covered by the TCK
//     execution suite that runs in the short layer.
//
// Run them with:
//
//	go test -tags=soak -race ./bench/audit352/ -run 'TestRelationshipPropsPlans|TestSubqueryShapeRowCounts|TestRelPropertyMaterialisationCount' -v

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

func TestRelationshipPropsPlans(t *testing.T) {
	g := buildRelGraph()
	engine := cypher.NewEngine(g)
	for _, s := range relShapes {
		p, err := engine.Explain(s.query, nil)
		if err != nil {
			t.Fatalf("Explain(%q): %v", s.query, err)
		}
		t.Logf("--- %s ships %d rows\n%s\n%s", s.name, countRows(t, engine, s.query), s.query, p)
	}
}

func TestSubqueryShapeRowCounts(t *testing.T) {
	g := buildRelGraph()
	engine := cypher.NewEngine(g)
	for _, s := range subqueryShapes {
		p, err := engine.Explain(s.query, nil)
		if err != nil {
			t.Fatalf("Explain(%q): %v", s.query, err)
		}
		t.Logf("--- %s ships %d rows\n%s\n%s", s.name, countRows(t, engine, s.query), s.query, p)
	}
}

// TestRelPropertyMaterialisationCount probes whether each additional
// relationship property READ triggers another full materialisation of the
// relationship's property map. If it does, cost grows linearly in the number
// of DISTINCT properties projected, and projecting all five costs about five
// times projecting one — while `RETURN r`, which materialises once, stays flat.
func TestRelPropertyMaterialisationCount(t *testing.T) {
	g := buildRelGraph()
	engine := cypher.NewEngine(g)
	ctx := context.Background()
	shapes := []struct{ name, query string }{
		{"whole_r", `MATCH ()-[r:R]->() RETURN r`},
		{"1_prop", `MATCH ()-[r:R]->() RETURN r.w`},
		{"2_props", `MATCH ()-[r:R]->() RETURN r.w, r.k3`},
		{"3_props", `MATCH ()-[r:R]->() RETURN r.w, r.k3, r.k4`},
		{"4_props", `MATCH ()-[r:R]->() RETURN r.w, r.k3, r.k4, r.k1`},
		{"5_props", `MATCH ()-[r:R]->() RETURN r.w, r.k3, r.k4, r.k1, r.k2`},
		{"same_prop_x5", `MATCH ()-[r:R]->() RETURN r.w, r.w, r.w, r.w, r.w`},
	}
	t.Logf("%-14s %12s %12s", "shape", "median ms", "vs 1_prop")
	base := 0.0
	for _, s := range shapes {
		var samples []float64
		for rep := 0; rep < 4; rep++ {
			start := time.Now()
			res, err := engine.Run(ctx, s.query, nil)
			if err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			rows := 0
			for res.Next() {
				rows++
			}
			if e := res.Err(); e != nil {
				t.Fatalf("%s: %v", s.name, e)
			}
			if err := res.Close(); err != nil {
				t.Fatalf("%s close: %v", s.name, err)
			}
			if rows != relNodes {
				t.Fatalf("%s shipped %d rows, want %d", s.name, rows, relNodes)
			}
			if rep > 0 {
				samples = append(samples, time.Since(start).Seconds())
			}
		}
		m := medianOf(samples) * 1e3
		if s.name == "1_prop" {
			base = m
		}
		ratio := ""
		if base > 0 {
			ratio = fmt.Sprintf("%.2fx", m/base)
		}
		t.Logf("%-14s %12.3f %12s", s.name, m, ratio)
	}
}
