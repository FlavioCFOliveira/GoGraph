package main

// min_label_scan.go — exercise and evidence for the min-cardinality
// multi-label anchor scan (GoGraph planner increment F1b, #2077).
//
// Every node in this example carries two labels: a layer label (Code / Work /
// People) and a type label (Repository / Module / Component / …). A layer is
// therefore a superset of each of its types. The Code layer, in particular, is
// dominated by :Component nodes (source files), while :Repository — the handful
// of roots — is a tiny fraction of it.
//
// A pattern that lists the broad layer label FIRST, e.g.
//
//	MATCH (r:Code:Repository) …
//
// is anchored by the IR translator on :Code (the first label) with :Repository
// re-checked as a residual filter — a full scan of the whole code layer. Since
// a label conjunction is commutative, the engine's build-time peephole (#2077)
// re-anchors the scan on the smallest-cardinality label, :Repository, and
// re-checks :Code as the residual filter: an identical result multiset visiting
// |Repository| candidate rows instead of |Code|.
//
// This file surfaces that behaviour as explicit startup telemetry: the physical
// plan (showing the scan re-anchored on the smaller label), the two candidate
// cardinalities (the rows each plan's scan visits — the db-hit skew), and a
// wall-clock and allocation contrast between an engine with the optimisation ON
// (the default) and one built with EngineOptions.DisableMinLabelScan.

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// mlsLayerLabel is the broad, low-selectivity layer label the demo pattern
// lists first. The Code layer holds every repository, module and component.
const mlsLayerLabel = layerCode

// mlsTypeLabel is the small, high-selectivity type label — a strict subset of
// the layer. Repositories are the few roots of a code layer dominated by
// components, so |Repository| ≪ |Code|.
const mlsTypeLabel = typeRepository

// minLabelDemoQuery lists the broad layer label FIRST, so the default IR
// anchors the scan on :Code and re-checks :Repository — exactly the shape the
// #2077 peephole re-anchors onto the smaller :Repository label. It is also
// catalogue query Q9, so every POST /query of Q9 exercises the optimisation.
const minLabelDemoQuery = "MATCH (r:" + mlsLayerLabel + ":" + mlsTypeLabel + ")\n" +
	"RETURN r.key AS repository, r.name AS name\n" +
	"ORDER BY repository"

// mlsScanDemo is the deterministic evidence of the anchor-selection: the
// chosen scan label, the two candidate cardinalities (the rows each plan's scan
// visits), and the result-row count (identical under both plans). Every field
// is a FACT of the seeded graph — none of it is volatile timing.
type mlsScanDemo struct {
	plan        string
	anchorLabel string
	layerCount  int64
	typeCount   int64
	rows        int
}

// anchoredOnSmallerLabel reports whether the planner re-anchored the scan on
// the smaller type label rather than the broad layer label written first.
func (d mlsScanDemo) anchoredOnSmallerLabel() bool {
	return d.anchorLabel == mlsTypeLabel && d.typeCount < d.layerCount
}

// collectMinLabelScanDemo gathers the deterministic anchor-selection evidence
// for minLabelDemoQuery over g, using a default (optimisation-ON) engine. It
// returns the EXPLAIN plan, the label the scan is anchored on, the two
// candidate cardinalities, and the result-row count. It performs no timing, so
// it is safe to assert on from a test.
func collectMinLabelScanDemo(g *lpg.Graph[string, float64]) (mlsScanDemo, error) {
	ctx := context.Background()
	eng := cypher.NewEngine(g) // min-label anchor scan ON (the default)

	// ExplainLogical, not Explain: this demo is about the PLANNER's anchor choice
	// and parses the variable-qualified "[var:label]" form the logical rendering
	// uses. The physical plan names the operator that runs but not the variable it
	// was bound to, because a built NodeByLabelScan holds only its label.
	plan, err := eng.ExplainLogical(minLabelDemoQuery, nil)
	if err != nil {
		return mlsScanDemo{}, fmt.Errorf("explain: %w", err)
	}
	layerCount, err := mlsCountLabel(ctx, eng, mlsLayerLabel)
	if err != nil {
		return mlsScanDemo{}, fmt.Errorf("count %s: %w", mlsLayerLabel, err)
	}
	typeCount, err := mlsCountLabel(ctx, eng, mlsTypeLabel)
	if err != nil {
		return mlsScanDemo{}, fmt.Errorf("count %s: %w", mlsTypeLabel, err)
	}
	rows, err := mlsDrainCount(ctx, eng, minLabelDemoQuery)
	if err != nil {
		return mlsScanDemo{}, fmt.Errorf("run demo: %w", err)
	}
	return mlsScanDemo{
		plan:        plan,
		anchorLabel: scanAnchorLabel(plan),
		layerCount:  layerCount,
		typeCount:   typeCount,
		rows:        rows,
	}, nil
}

// scanAnchorLabel extracts the label of the first "NodeByLabelScan [var:label]"
// leaf in an EXPLAIN plan. It returns "" when the plan contains no such leaf
// (e.g. the pattern lowered to an index seek instead).
func scanAnchorLabel(plan string) string {
	const marker = "NodeByLabelScan ["
	i := strings.Index(plan, marker)
	if i < 0 {
		return ""
	}
	rest := plan[i+len(marker):]
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return ""
	}
	inner := rest[:end] // "r:Repository"
	if c := strings.IndexByte(inner, ':'); c >= 0 {
		return inner[c+1:]
	}
	return ""
}

// mlsCountLabel returns the live-node count of a single label via
// MATCH (n:label) RETURN count(n).
func mlsCountLabel(ctx context.Context, eng *cypher.Engine, label string) (int64, error) {
	res, err := eng.Run(ctx, "MATCH (n:"+label+") RETURN count(n) AS n", nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Close() }()
	var n int64
	for res.Next() {
		if v, ok := res.Record()["n"]; ok {
			if iv, ok := jsonValue(v).(int64); ok {
				n = iv
			}
		}
	}
	return n, res.Err()
}

// mlsDrainCount runs query, drains every row, and returns the row count.
func mlsDrainCount(ctx context.Context, eng *cypher.Engine, query string) (int, error) {
	res, err := eng.Run(ctx, query, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Close() }()
	rows := 0
	for res.Next() {
		rows++
	}
	return rows, res.Err()
}

// mlsMeasure runs minLabelDemoQuery on eng iters times after a warm-up pass and
// returns the mean wall-clock per run and the mean bytes allocated per run. The
// allocation figure is a TotalAlloc delta across a forced-GC boundary. Both are
// volatile telemetry — they vary per run and per machine and are never pinned.
func mlsMeasure(eng *cypher.Engine, iters int) (avgNs int64, avgBytes uint64) {
	ctx := context.Background()
	_, _ = mlsDrainCount(ctx, eng, minLabelDemoQuery) // warm the plan cache

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	for i := 0; i < iters; i++ {
		_, _ = mlsDrainCount(ctx, eng, minLabelDemoQuery)
	}
	elapsed := time.Since(start).Nanoseconds()
	runtime.ReadMemStats(&after)

	if iters <= 0 {
		return 0, 0
	}
	return elapsed / int64(iters), (after.TotalAlloc - before.TotalAlloc) / uint64(iters)
}

// reportMinLabelScan prints, on stderr, the F1b anchor-selection evidence for
// the seeded graph. Deterministic FACTS (the chosen scan label and the two
// candidate cardinalities) are printed as bare "minlabelscan.*" lines; the
// verbose plan text and the volatile ON-vs-OFF timing/allocation contrast are
// printed on "# " telemetry lines, matching the fact-vs-telemetry convention
// the example uses elsewhere (see reportSchemaPlan).
//
// The ON/OFF contrast builds two throwaway engines over the same live graph —
// one default (optimisation ON), one with EngineOptions.DisableMinLabelScan —
// and runs the identical query on each. It runs at startup after a scaled seed,
// single-threaded before the server begins serving, so reading ds.graph without
// the store hold is safe.
func reportMinLabelScan(ds *dataStore) {
	demo, err := collectMinLabelScanDemo(ds.graph)
	if err != nil {
		fmt.Fprintf(os.Stderr, "# minlabelscan.error=%v\n", err)
		return
	}

	// Facts: the anchor and the scan-cardinality skew. The default plan's scan
	// visits layer_count rows; the re-anchored plan visits type_count rows — the
	// db-hit reduction the optimisation buys.
	fmt.Fprintf(os.Stderr, "minlabelscan.anchor=%s\n", demo.anchorLabel)
	fmt.Fprintf(os.Stderr, "minlabelscan.anchored_on_smaller_label=%t\n", demo.anchoredOnSmallerLabel())
	fmt.Fprintf(os.Stderr, "minlabelscan.layer_label=%s\n", mlsLayerLabel)
	fmt.Fprintf(os.Stderr, "minlabelscan.layer_scan_rows=%d\n", demo.layerCount)
	fmt.Fprintf(os.Stderr, "minlabelscan.type_label=%s\n", mlsTypeLabel)
	fmt.Fprintf(os.Stderr, "minlabelscan.type_scan_rows=%d\n", demo.typeCount)
	fmt.Fprintf(os.Stderr, "minlabelscan.result_rows=%d\n", demo.rows)

	// The physical plan, verbatim (verbose diagnostic → "# " lines). The scan
	// leaf shows the re-anchored (smaller) label.
	for _, line := range strings.Split(strings.TrimRight(demo.plan, "\n"), "\n") {
		fmt.Fprintf(os.Stderr, "# minlabelscan.plan| %s\n", line)
	}

	// Telemetry: run the identical query with the optimisation ON (default) and
	// OFF (DisableMinLabelScan) and contrast wall-clock and allocations.
	const iters = 25
	on := cypher.NewEngineWithOptions(ds.graph, cypher.EngineOptions{})
	off := cypher.NewEngineWithOptions(ds.graph, cypher.EngineOptions{DisableMinLabelScan: true})
	onNs, onBytes := mlsMeasure(on, iters)
	offNs, offBytes := mlsMeasure(off, iters)

	fmt.Fprintf(os.Stderr, "# minlabelscan.on_query=%s\n", time.Duration(onNs).Round(time.Microsecond))
	fmt.Fprintf(os.Stderr, "# minlabelscan.off_query=%s\n", time.Duration(offNs).Round(time.Microsecond))
	fmt.Fprintf(os.Stderr, "# minlabelscan.on_alloc=%s\n", humanBytes(onBytes))
	fmt.Fprintf(os.Stderr, "# minlabelscan.off_alloc=%s\n", humanBytes(offBytes))
	if onNs > 0 {
		fmt.Fprintf(os.Stderr, "# minlabelscan.time_speedup=%.1fx\n", float64(offNs)/float64(onNs))
	}
	if onBytes > 0 {
		fmt.Fprintf(os.Stderr, "# minlabelscan.alloc_reduction=%.1fx\n", float64(offBytes)/float64(onBytes))
	}
}
