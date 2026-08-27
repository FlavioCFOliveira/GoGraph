package audit352_test

import (
	"context"
	"fmt"
	"log"
	"math"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildRelGraphN builds an n-node fixture with exactly one outgoing :R
// relationship per node. Out-degree is held at 1 for EVERY n, so the work an
// honest implementation has to do per outer row is constant and total work is
// O(n). Any growth faster than linear is the implementation's, not the
// workload's — this is what makes the sweep able to distinguish "a large
// constant factor" from "a worse complexity class".
func buildRelGraphN(n int) *lpg.Graph[string, float64] {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if _, err := eng.RunAny(ctx, fmt.Sprintf(`CREATE (:P {sid:%d})`, 100000+i), nil); err != nil {
			log.Fatalf("scaling fixture node: %v", err)
		}
	}
	for i := 0; i < n; i++ {
		q := fmt.Sprintf(`MATCH (a:P {sid:%d}), (b:P {sid:%d}) CREATE (a)-[:R {w:%d}]->(b)`,
			100000+i, 100000+((i+7)%n), 500000+i)
		if _, err := eng.RunAny(ctx, q, nil); err != nil {
			log.Fatalf("scaling fixture rel: %v", err)
		}
	}
	return g
}

// TestScaling_SubqueryComplexity fits log(time) against log(n) for each shape.
// The fitted exponent is the complexity class: ~1.0 is linear in the number of
// outer rows (correct for a fixed out-degree of 1), ~2.0 means every outer row
// is doing work proportional to the whole graph.
//
//	go test -run '^TestScaling_SubqueryComplexity$' -v -timeout 40m ./bench/audit352/
func TestScaling_SubqueryComplexity(t *testing.T) {
	if testing.Short() {
		t.Skip("scaling sweep is not a short-layer test")
	}
	shapes := []struct{ name, query string }{
		{"optional_match", `MATCH (a:P) OPTIONAL MATCH (a)-[:R]->(b:P) RETURN a.sid, b.sid`},
		{"exists_subquery", `MATCH (a:P) WHERE EXISTS { MATCH (a)-[:R]->(:P) } RETURN a.sid`},
		{"count_subquery", `MATCH (a:P) RETURN a.sid AS sid, COUNT { MATCH (a)-[:R]->(:P) } AS c`},
		{"pattern_predicate", `MATCH (a:P) WHERE (a)-[:R]->(:P) RETURN a.sid`},
		{"plain_match", `MATCH (a:P) RETURN a.sid`},
	}
	// Small sizes: count_subquery is already seconds per query at n=4000.
	sizes := []int{250, 500, 1000, 2000}

	type key struct {
		shape string
		n     int
	}
	timings := map[key]float64{}

	for _, n := range sizes {
		g := buildRelGraphN(n)
		engine := cypher.NewEngine(g)
		ctx := context.Background()
		for _, s := range shapes {
			// One untimed warm-up, then the median of three timed runs, so a
			// single scheduling hiccup cannot set the point.
			var samples []float64
			for rep := 0; rep < 4; rep++ {
				start := time.Now()
				res, err := engine.Run(ctx, s.query, nil)
				if err != nil {
					t.Fatalf("%s n=%d: %v", s.name, n, err)
				}
				rows := 0
				for res.Next() {
					rows++
				}
				if e := res.Err(); e != nil {
					t.Fatalf("%s n=%d: %v", s.name, n, e)
				}
				if err := res.Close(); err != nil {
					t.Fatalf("%s n=%d close: %v", s.name, n, err)
				}
				el := time.Since(start).Seconds()
				if rows != n {
					t.Fatalf("%s n=%d shipped %d rows, want %d", s.name, n, rows, n)
				}
				if rep > 0 {
					samples = append(samples, el)
				}
			}
			timings[key{s.name, n}] = medianOf(samples)
		}
		t.Logf("n=%d fixture done", n)
	}

	t.Logf("%-20s %12s %12s %12s %12s", "shape", "n=250", "n=500", "n=1000", "n=2000")
	for _, s := range shapes {
		t.Logf("%-20s %11.3fms %11.3fms %11.3fms %11.3fms", s.name,
			timings[key{s.name, 250}]*1e3, timings[key{s.name, 500}]*1e3,
			timings[key{s.name, 1000}]*1e3, timings[key{s.name, 2000}]*1e3)
	}
	t.Logf("")
	t.Logf("%-20s %10s %10s   %s", "shape", "exponent", "r2", "verdict")
	for _, s := range shapes {
		var lx, ly []float64
		for _, n := range sizes {
			lx = append(lx, math.Log(float64(n)))
			ly = append(ly, math.Log(timings[key{s.name, n}]))
		}
		a, b, r2 := olsFitXY(lx, ly)
		_ = a
		verdict := "sub-linear / linear"
		switch {
		case b > 1.7:
			verdict = "QUADRATIC — per-row work grows with graph size"
		case b > 1.3:
			verdict = "super-linear"
		}
		t.Logf("%-20s %10.3f %10.5f   %s", s.name, b, r2, verdict)
	}
}

// olsFitXY is olsFit over explicit x/y slices (the log-log fit needs floats
// for x, which fitPoint's integer k cannot carry).
func olsFitXY(xs, ys []float64) (a, b, r2 float64) {
	n := float64(len(xs))
	var sx, sy, sxx, sxy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}
	b = (n*sxy - sx*sy) / (n*sxx - sx*sx)
	a = (sy - b*sx) / n
	mean := sy / n
	var ssTot, ssRes float64
	for i := range xs {
		pred := a + b*xs[i]
		ssRes += (ys[i] - pred) * (ys[i] - pred)
		ssTot += (ys[i] - mean) * (ys[i] - mean)
	}
	if ssTot == 0 {
		return a, b, math.NaN()
	}
	return a, b, 1 - ssRes/ssTot
}

// TestRelPropertyMaterialisationCount probes whether each additional
// relationship property READ triggers another full materialisation of the
// relationship's property map. If it does, cost grows linearly in the number
// of DISTINCT properties projected, and projecting all five costs about five
// times projecting one — while `RETURN r`, which materialises once, stays flat.
func TestRelPropertyMaterialisationCount(t *testing.T) {
	if testing.Short() {
		t.Skip("not a short-layer test")
	}
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
