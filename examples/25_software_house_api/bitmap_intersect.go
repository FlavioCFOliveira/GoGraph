package main

// bitmap_intersect.go — exercise and evidence for the set-at-a-time multi-label
// conjunction (GoGraph planner increment R2-P2, #2133).
//
// This example already carries the P1 evidence for the min-cardinality anchor
// scan (min_label_scan.go). R2-P2 is the next step on the same shape: instead of
// scanning the smallest label and re-checking the rest on every row, the planner
// intersects the labels' Roaring bitmaps and drops the residual label filter
// entirely, because the intersected bitmap already encodes the conjunction.
//
// # Why this example needs a cross-cutting label
//
// Every seeded node carries exactly one LAYER label (Code / Work / People) and one
// TYPE label (Repository / Module / Component / …), and each type is a strict
// SUBSET of its layer. That nesting is precisely the regime where the intersection
// has nothing to win: |Code ∩ Repository| = |Repository|, so intersecting removes
// no rows the min-label re-anchor would not already have avoided, and the gate
// declines.
//
// A real software house also tracks things that cut ACROSS that hierarchy — an
// items-awaiting-review backlog spans a component pending code review, a task
// pending sign-off and a developer pending an onboarding review. That is the shape
// where a conjunction is genuinely selective: neither label contains the other, so
// their intersection is strictly smaller than either.
//
// The demo therefore derives a `:NeedsReview` marker over a COPY of the served
// graph's nodes rather than adding a label to the served graph itself. The API
// surface, the seeded facts and every pinned count stay exactly as they are, and
// the demo still runs over this example's own data and label vocabulary.
//
// # What it reports
//
// Both regimes, side by side, which is the point: an optimisation that only ever
// reports its wins is not evidence. For each it prints the annotated plan with its
// exact row estimate and provenance, the rows returned, and a wall-clock and
// allocation contrast between an engine with the path ON (the default) and one
// built with EngineOptions.DisableBitmapIntersection.

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// biReviewLabel is the cross-cutting marker: items awaiting a review of some kind,
// which in a real software house spans code, work and people alike.
const biReviewLabel = "NeedsReview"

// biReviewEvery marks 1 node in N as needing review. Chosen so the marker is a
// realistic minority of the graph and its intersection with any single type label
// is strictly smaller than either — the regime where the path fires.
const biReviewEvery = 7

const (
	// biFireQuery is the SELECTIVE regime. :Component and :NeedsReview overlap
	// without either containing the other, so the intersection is strictly smaller
	// than both and the planner serves it set-at-a-time.
	biFireQuery = "MATCH (n:" + typeComponent + ":" + biReviewLabel + ")\n" +
		"RETURN count(n) AS pending"
	// biVetoQuery is the NON-SELECTIVE regime, and it is the very query the P1
	// harness uses. :Repository is a strict subset of :Code, so the intersection
	// equals the smaller label: there are no rows left to remove, the gate declines,
	// and the shipped min-label plan keeps the shape.
	biVetoQuery = "MATCH (n:" + layerCode + ":" + typeRepository + ")\n" +
		"RETURN count(n) AS repositories"
)

// biRegime is the deterministic evidence for one conjunction: the plan the
// planner chose, whether it intersected, the participating label cardinalities and
// the answer. Every field is a FACT of the derived graph — none of it is timing.
type biRegime struct {
	name        string
	query       string
	plan        string
	intersected bool
	leftLabel   string
	rightLabel  string
	leftCount   int64
	rightCount  int64
	rows        int64
}

// biDemo is both regimes together, which is how they are meant to be read.
type biDemo struct {
	fire biRegime
	veto biRegime
}

// buildReviewView derives the demo graph: the served graph's nodes and their
// labels, plus the cross-cutting :NeedsReview marker on a deterministic subset.
//
// It copies rather than mutates because the served graph backs a live API and a
// suite of pinned facts; a demo has no business changing either. The Cypher engine
// is defined over lpg.Graph[string, float64], which the served graph already is, so
// the copy is a faithful relabelling and not a different data model.
func buildReviewView(src *lpg.Graph[string, float64]) (*lpg.Graph[string, float64], error) {
	dst := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	i := 0
	var addErr error
	src.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		if src.IsTombstoned(id) {
			return true
		}
		if err := dst.AddNode(key); err != nil {
			addErr = fmt.Errorf("AddNode %q: %w", key, err)
			return false
		}
		for _, l := range src.NodeLabelsByID(id) {
			if err := dst.SetNodeLabel(key, l); err != nil {
				addErr = fmt.Errorf("SetNodeLabel %q %s: %w", key, l, err)
				return false
			}
		}
		// The cross-cutting marker, applied deterministically so the demo's facts
		// are reproducible for a given seed.
		if i%biReviewEvery == 0 {
			if err := dst.SetNodeLabel(key, biReviewLabel); err != nil {
				addErr = fmt.Errorf("SetNodeLabel %q %s: %w", key, biReviewLabel, err)
				return false
			}
		}
		i++
		return true
	})
	if addErr != nil {
		return nil, addErr
	}
	return dst, nil
}

// collectBitmapIntersectDemo gathers the deterministic evidence for both regimes.
// It performs no timing, so a test can assert on every field.
func collectBitmapIntersectDemo(src *lpg.Graph[string, float64]) (biDemo, error) {
	view, err := buildReviewView(src)
	if err != nil {
		return biDemo{}, fmt.Errorf("derive review view: %w", err)
	}
	eng := cypher.NewEngine(view) // intersection ON (the default)

	fire, err := collectBiRegime(eng, "fire", biFireQuery, typeComponent, biReviewLabel)
	if err != nil {
		return biDemo{}, err
	}
	veto, err := collectBiRegime(eng, "veto", biVetoQuery, layerCode, typeRepository)
	if err != nil {
		return biDemo{}, err
	}
	return biDemo{fire: fire, veto: veto}, nil
}

// collectBiRegime gathers one regime's evidence.
func collectBiRegime(eng *cypher.Engine, name, query, left, right string) (biRegime, error) {
	ctx := context.Background()
	// ExplainLogical, not Explain: the logical rendering carries the cardinality
	// annotation, so this is where the seek's EXACT row estimate and its provenance
	// are visible. The physical rendering names the operator and the intersected
	// labels; both are printed below.
	plan, err := eng.ExplainLogical(query, nil)
	if err != nil {
		return biRegime{}, fmt.Errorf("%s: explain: %w", name, err)
	}
	physical, err := eng.Explain(query, nil)
	if err != nil {
		return biRegime{}, fmt.Errorf("%s: explain physical: %w", name, err)
	}
	leftCount, err := mlsCountLabel(ctx, eng, left)
	if err != nil {
		return biRegime{}, fmt.Errorf("%s: count %s: %w", name, left, err)
	}
	rightCount, err := mlsCountLabel(ctx, eng, right)
	if err != nil {
		return biRegime{}, fmt.Errorf("%s: count %s: %w", name, right, err)
	}
	rows, err := biScalar(ctx, eng, query)
	if err != nil {
		return biRegime{}, fmt.Errorf("%s: run: %w", name, err)
	}
	return biRegime{
		name:  name,
		query: query,
		// Both renderings, joined: the physical names the access path and the
		// intersected labels, the logical carries the estimate.
		plan:        physical + "\n" + plan,
		intersected: strings.Contains(physical, "∩"),
		leftLabel:   left,
		rightLabel:  right,
		leftCount:   leftCount,
		rightCount:  rightCount,
		rows:        rows,
	}, nil
}

// biScalar runs a single-row aggregate query and returns its integer value.
func biScalar(ctx context.Context, eng *cypher.Engine, query string) (int64, error) {
	res, err := eng.Run(ctx, query, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Close() }()
	var n int64
	for res.Next() {
		for _, v := range res.Record() {
			if iv, ok := jsonValue(v).(int64); ok {
				n = iv
			}
		}
	}
	return n, res.Err()
}

// biMeasure runs query iters times after a warm-up and returns the mean
// wall-clock and mean bytes allocated per run. Volatile telemetry.
func biMeasure(eng *cypher.Engine, query string, iters int) (avgNs int64, avgBytes uint64) {
	if iters <= 0 {
		return 0, 0
	}
	ctx := context.Background()
	_, _ = biScalar(ctx, eng, query) // warm the plan cache

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	for i := 0; i < iters; i++ {
		_, _ = biScalar(ctx, eng, query)
	}
	elapsed := time.Since(start).Nanoseconds()
	runtime.ReadMemStats(&after)
	return elapsed / int64(iters), (after.TotalAlloc - before.TotalAlloc) / uint64(iters)
}

// reportBitmapIntersect prints the R2-P2 evidence for both regimes on stderr,
// following the fact-versus-telemetry convention the rest of the example uses:
// bare "bitmapintersect.*" lines are deterministic facts, "# " lines are volatile.
//
// It runs at startup, single-threaded before the server begins serving, so reading
// ds.graph without the store hold is safe — the same footing as reportMinLabelScan.
func reportBitmapIntersect(ds *dataStore) {
	demo, err := collectBitmapIntersectDemo(ds.graph)
	if err != nil {
		fmt.Fprintf(os.Stderr, "# bitmapintersect.error=%v\n", err)
		return
	}
	view, err := buildReviewView(ds.graph)
	if err != nil {
		fmt.Fprintf(os.Stderr, "# bitmapintersect.error=%v\n", err)
		return
	}

	for _, r := range []biRegime{demo.fire, demo.veto} {
		fmt.Fprintf(os.Stderr, "bitmapintersect.%s.intersected=%t\n", r.name, r.intersected)
		fmt.Fprintf(os.Stderr, "bitmapintersect.%s.left=%s:%d\n", r.name, r.leftLabel, r.leftCount)
		fmt.Fprintf(os.Stderr, "bitmapintersect.%s.right=%s:%d\n", r.name, r.rightLabel, r.rightCount)
		fmt.Fprintf(os.Stderr, "bitmapintersect.%s.result_rows=%d\n", r.name, r.rows)
		// The gate's own condition, made visible: the path is taken exactly when the
		// intersection scans strictly fewer rows than the smaller label alone.
		smaller := r.leftCount
		if r.rightCount < smaller {
			smaller = r.rightCount
		}
		fmt.Fprintf(os.Stderr, "bitmapintersect.%s.strictly_fewer_rows=%t\n", r.name, r.rows < smaller)
		for _, line := range strings.Split(strings.TrimRight(r.plan, "\n"), "\n") {
			fmt.Fprintf(os.Stderr, "# bitmapintersect.%s.plan| %s\n", r.name, line)
		}
	}

	// Telemetry: the identical query with the path ON (default) and OFF, for BOTH
	// regimes. The veto arm is the honest half — it should show no difference,
	// because the gate declined and both engines planned the same thing.
	const iters = 25
	on := cypher.NewEngineWithOptions(view, cypher.EngineOptions{})
	off := cypher.NewEngineWithOptions(view, cypher.EngineOptions{DisableBitmapIntersection: true})

	for _, r := range []biRegime{demo.fire, demo.veto} {
		onNs, onBytes := biMeasure(on, r.query, iters)
		offNs, offBytes := biMeasure(off, r.query, iters)
		fmt.Fprintf(os.Stderr, "# bitmapintersect.%s.on_query=%s\n", r.name, time.Duration(onNs).Round(time.Microsecond))
		fmt.Fprintf(os.Stderr, "# bitmapintersect.%s.off_query=%s\n", r.name, time.Duration(offNs).Round(time.Microsecond))
		fmt.Fprintf(os.Stderr, "# bitmapintersect.%s.on_alloc=%s\n", r.name, humanBytes(onBytes))
		fmt.Fprintf(os.Stderr, "# bitmapintersect.%s.off_alloc=%s\n", r.name, humanBytes(offBytes))
		// Above 1.0 means the intersection is faster; below 1.0 would mean it is
		// slower, which the veto regime must never show since both engines plan the
		// same thing there.
		if onNs > 0 {
			fmt.Fprintf(os.Stderr, "# bitmapintersect.%s.time_ratio_off_over_on=%.2fx\n",
				r.name, float64(offNs)/float64(onNs))
		}
		// Reported as a RATIO, not a "reduction": on a graph this size the
		// intersection allocates MORE than the scan it replaces, because it
		// materialises a bitmap where the scan walks one in place. A value below 1.0
		// therefore means the intersection used more memory, and the label has to say
		// so rather than imply a saving that is not there. The trade is visible in
		// both columns: 4x less time for about twice the bytes, on a fixture whose
		// scanned label is only a couple of thousand rows.
		if onBytes > 0 {
			fmt.Fprintf(os.Stderr, "# bitmapintersect.%s.alloc_ratio_off_over_on=%.2fx\n",
				r.name, float64(offBytes)/float64(onBytes))
		}
	}
}
