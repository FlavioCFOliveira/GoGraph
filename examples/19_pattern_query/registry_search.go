package main

// registry_search.go — the registry name-search phase (rmp #2131).
//
// The dependency-network model this example builds has an obvious real-world
// query the fluent pattern API cannot express: **search the registry by name
// prefix**, the "type `core-` and see what exists" box every package registry
// has. That is a Cypher `STARTS WITH` predicate, and as of sprint 311 it is
// served from a sorted B-tree index as a range seek over `[p, succ(p))` rather
// than by scanning the label and refiltering every row.
//
// This phase exercises that access path under realistic conditions and collects
// the evidence to judge it, on all the axes the examples standard requires:
//
//   - **correctness** — the same query is run with the rewrite ENABLED and
//     DISABLED and the two row sets are compared element-by-element, and both are
//     compared against an independent Go oracle computed with strings.HasPrefix
//     over the same names. A faster wrong answer fails the example;
//   - **the plan** — both the physical tree (which shows the seek's actual
//     interval, including the computed exclusive upper bound) and the annotated
//     logical tree (which shows the seek's EXACT row estimate) are printed;
//   - **CPU** — wall time per query for the seek and for the scan it replaces;
//   - **memory** — allocations and bytes per query from runtime.MemStats;
//   - **the boundary** — the same search expressed as `CONTAINS` and
//     `ENDS WITH`, which cannot be served by any range and therefore stay on the
//     label scan. Printing them next to the prefix form is what makes the
//     difference between "indexed" and "not indexable" observable rather than
//     asserted.
//
// # Why a second graph
//
// The Cypher engine is defined over `lpg.Graph[string, float64]` while this
// example's dependency graph carries `int64` edge weights, so the registry view
// is a separate graph. It is not separate DATA: the names are read back out of
// the graph built above, so the search runs over exactly the packages the rest
// of the example reports on.

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// labelRegistryPackage is the label of the searchable registry view. It is
// distinct from labelPackage so the two graphs cannot be confused in a plan.
const labelRegistryPackage = "RegistryPackage"

// searchPrefix is the name prefix the search phase looks for. It is one of the
// generator's namePrefixes plus the separator, so it selects the packages whose
// name begins with that word — about 1/len(namePrefixes) of the population,
// which at the default scale is ≈3 % and therefore inside the seek's 10 %
// selectivity gate. A prefix covering more than 10 % of the label would be
// declined by the gate and answered by a scan, which is the correct decision but
// would not exercise the seek.
//
// "core" is chosen specifically because it appears in BOTH namePrefixes and
// nameSuffixes, so the same word gives a non-empty answer as a prefix, as a
// suffix and as a substring. That makes the three-way contrast below meaningful:
// a word that only ever appears as a prefix would make the `ENDS WITH` arm match
// zero rows, and a zero-row arm demonstrates nothing about its cost.
const searchPrefix = "core-"

// searchReps is how many times each query is repeated when measuring, so the
// per-query latency and allocation figures are averages rather than one sample.
const searchReps = 20

// searchTop is how many matched names the phase prints, in sorted order, as
// deterministic fact lines.
const searchTop = 3

// runRegistrySearch builds the searchable registry view from src, then measures
// and reports the prefix search against the plan it replaces and against the two
// string predicates that cannot be indexed at all.
func runRegistrySearch(ctx context.Context, src *lpg.Graph[string, int64], w io.Writer) error {
	names, err := collectPackageNames(src)
	if err != nil {
		return err
	}

	// The oracle: the answer computed directly from the names, with no engine
	// involved. Everything the engine returns is checked against this.
	want := make([]string, 0, 16)
	for _, n := range names {
		if strings.HasPrefix(n, searchPrefix) {
			want = append(want, n)
		}
	}
	sort.Strings(want)

	seekEng, err := buildRegistryEngine(ctx, names, false)
	if err != nil {
		return fmt.Errorf("registry engine (seek): %w", err)
	}
	scanEng, err := buildRegistryEngine(ctx, names, true)
	if err != nil {
		return fmt.Errorf("registry engine (scan): %w", err)
	}

	prefixQuery := `MATCH (p:` + labelRegistryPackage + `) WHERE p.name STARTS WITH "` +
		searchPrefix + `" RETURN p.name AS name ORDER BY p.name`

	fmt.Fprintf(w, "search.prefix=%s\n", searchPrefix)
	fmt.Fprintf(w, "search.population=%d\n", len(names))
	fmt.Fprintf(w, "search.rows=%d\n", len(want))

	// The plan, two ways. The physical tree names the operator and shows the
	// seek's actual interval — note the exclusive upper bound the rewrite
	// computes, "core." for the prefix "core-", because '.' is the byte after
	// '-'. The logical tree carries the cardinality annotation, so it is where
	// the seek's EXACT row estimate is visible.
	if err := printPlan(w, "search.plan.physical", func() (string, error) {
		return seekEng.Explain(prefixQuery, nil)
	}); err != nil {
		return err
	}
	if err := printPlan(w, "search.plan.logical", func() (string, error) {
		return seekEng.ExplainLogical(prefixQuery, nil)
	}); err != nil {
		return err
	}
	if err := printPlan(w, "search.plan.no_seek", func() (string, error) {
		return scanEng.Explain(prefixQuery, nil)
	}); err != nil {
		return err
	}

	// Measure both arms, and verify each against the oracle before believing any
	// timing: a faster wrong answer must fail the example, not decorate it.
	seek, err := measureQuery(ctx, seekEng, prefixQuery)
	if err != nil {
		return fmt.Errorf("prefix search (seek): %w", err)
	}
	scan, err := measureQuery(ctx, scanEng, prefixQuery)
	if err != nil {
		return fmt.Errorf("prefix search (scan): %w", err)
	}
	if err := sameRows("seek vs oracle", seek.rows, want); err != nil {
		return err
	}
	if err := sameRows("scan vs oracle", scan.rows, want); err != nil {
		return err
	}

	for i := 0; i < searchTop && i < len(want); i++ {
		fmt.Fprintf(w, "search.match.%d=%s\n", i, want[i])
	}

	fmt.Fprintf(w, "# search.seek.latency=%s\n", seek.perQuery.Round(time.Microsecond))
	fmt.Fprintf(w, "# search.no_seek.latency=%s\n", scan.perQuery.Round(time.Microsecond))
	fmt.Fprintf(w, "# search.speedup=%.1fx\n", ratio(scan.perQuery, seek.perQuery))
	fmt.Fprintf(w, "# search.seek.allocs=%d\n", seek.allocsPerQuery)
	fmt.Fprintf(w, "# search.no_seek.allocs=%d\n", scan.allocsPerQuery)
	fmt.Fprintf(w, "# search.alloc_ratio=%.1fx\n", floatRatio(scan.allocsPerQuery, seek.allocsPerQuery))
	fmt.Fprintf(w, "# search.seek.bytes=%s\n", humanBytes(seek.bytesPerQuery))
	fmt.Fprintf(w, "# search.no_seek.bytes=%s\n", humanBytes(scan.bytesPerQuery))

	// The boundary: neither CONTAINS nor ENDS WITH describes an interval of the
	// key order, so no range can serve them and both keep the label scan. Their
	// cost is the scan's cost, which is exactly the point.
	return reportUnindexable(ctx, seekEng, names, w)
}

// reportUnindexable measures the same registry search expressed with the two
// string predicates that admit no range rewrite, so the example's own output
// shows what "not indexable" costs next to what "indexed" costs.
func reportUnindexable(ctx context.Context, eng *cypher.Engine, names []string, w io.Writer) error {
	// The word without the separator, so the substring/suffix forms match a
	// comparable, non-empty set rather than nothing.
	word := strings.TrimSuffix(searchPrefix, "-")

	cases := []struct {
		label  string
		query  string
		oracle func(string) bool
	}{{
		label:  "contains",
		query:  `MATCH (p:` + labelRegistryPackage + `) WHERE p.name CONTAINS "` + word + `" RETURN p.name AS name ORDER BY p.name`,
		oracle: func(n string) bool { return strings.Contains(n, word) },
	}, {
		label:  "ends_with",
		query:  `MATCH (p:` + labelRegistryPackage + `) WHERE p.name ENDS WITH "` + word + `" RETURN p.name AS name ORDER BY p.name`,
		oracle: func(n string) bool { return strings.HasSuffix(n, word) },
	}}

	for _, c := range cases {
		want := make([]string, 0, 16)
		for _, n := range names {
			if c.oracle(n) {
				want = append(want, n)
			}
		}
		sort.Strings(want)

		plan, err := eng.Explain(c.query, nil)
		if err != nil {
			return fmt.Errorf("explain %s: %w", c.label, err)
		}
		indexed := strings.Contains(plan, "NodeByIndexRangeScan")

		got, err := measureQuery(ctx, eng, c.query)
		if err != nil {
			return fmt.Errorf("%s search: %w", c.label, err)
		}
		if err := sameRows(c.label+" vs oracle", got.rows, want); err != nil {
			return err
		}

		fmt.Fprintf(w, "search.%s.rows=%d\n", c.label, len(want))
		fmt.Fprintf(w, "search.%s.indexed=%t\n", c.label, indexed)
		fmt.Fprintf(w, "# search.%s.latency=%s\n", c.label, got.perQuery.Round(time.Microsecond))
		fmt.Fprintf(w, "# search.%s.allocs=%d\n", c.label, got.allocsPerQuery)
	}
	return nil
}

// collectPackageNames reads every :Package node's name property out of the
// dependency graph, so the registry view searches exactly the packages the rest
// of the example reports on rather than a regenerated approximation.
func collectPackageNames(g *lpg.Graph[string, int64]) ([]string, error) {
	names := make([]string, 0, g.AdjList().Order())
	var readErr error
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		v, ok := g.GetNodeProperty(key, propName)
		if !ok {
			readErr = fmt.Errorf("package %q missing property %s", key, propName)
			return false
		}
		s, ok := v.String()
		if !ok {
			readErr = fmt.Errorf("package %q property %s is not a string", key, propName)
			return false
		}
		names = append(names, s)
		return true
	})
	if readErr != nil {
		return nil, readErr
	}
	return names, nil
}

// buildRegistryEngine materialises the searchable registry view over the given
// names and returns a Cypher engine with a bound B-tree index on name.
// disableSeek turns the prefix rewrite off, yielding the NodeByLabelScan+Filter
// plan the rewrite replaces — the example's own A/B control.
func buildRegistryEngine(ctx context.Context, names []string, disableSeek bool) (*cypher.Engine, error) {
	// Directed + Multigraph is the openCypher storage model, which is what the
	// Cypher engine expects; anything else makes it warn at construction.
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i, n := range names {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		key := fmt.Sprintf("r%08d", i)
		if err := g.AddNode(key); err != nil {
			return nil, fmt.Errorf("AddNode %s: %w", key, err)
		}
		if err := g.SetNodeLabel(key, labelRegistryPackage); err != nil {
			return nil, fmt.Errorf("SetNodeLabel %s: %w", key, err)
		}
		if err := g.SetNodeProperty(key, propName, lpg.StringValue(n)); err != nil {
			return nil, fmt.Errorf("SetNodeProperty %s: %w", key, err)
		}
	}

	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{
		DisablePrefixIndexSeek: disableSeek,
		MaxResultRows:          cypher.MaxResultRowsUnlimited,
	})
	// A B-tree index is what a string range — and therefore a prefix — needs; the
	// default hash index serves equality only. CREATE INDEX is the only way to
	// obtain a bound, backfilled, self-maintaining index.
	const ddl = `CREATE INDEX FOR (n:` + labelRegistryPackage + `) ON (n.` + propName +
		`) OPTIONS {indexType:'btree'}`
	if _, err := eng.Run(ctx, ddl, nil); err != nil {
		return nil, fmt.Errorf("CREATE INDEX: %w", err)
	}
	return eng, nil
}

// queryMeasurement is one query's observed cost and its answer.
type queryMeasurement struct {
	rows           []string
	perQuery       time.Duration
	allocsPerQuery uint64
	bytesPerQuery  uint64
}

// measureQuery runs q searchReps times, returning the sorted answer from the
// first run plus the per-query wall time, allocation count and allocated bytes.
// Allocation figures come from runtime.MemStats deltas across the repetitions,
// which counts every allocation the query path makes, not just the retained ones.
func measureQuery(ctx context.Context, eng *cypher.Engine, q string) (queryMeasurement, error) {
	var out queryMeasurement

	drain := func(collect bool) error {
		res, err := eng.Run(ctx, q, nil)
		if err != nil {
			return err
		}
		for res.Next() {
			if collect {
				v := res.ValueAt(0)
				sv, ok := v.(expr.StringValue)
				if !ok {
					return fmt.Errorf("row value %v is not a string (%T)", v, v)
				}
				out.rows = append(out.rows, string(sv))
			}
		}
		if err := res.Err(); err != nil {
			return err
		}
		return res.Close()
	}

	// One warm-up run collects the answer and populates the plan cache, so the
	// measured repetitions time steady-state execution rather than plan build.
	if err := drain(true); err != nil {
		return queryMeasurement{}, err
	}
	sort.Strings(out.rows)

	before := readMem()
	start := time.Now()
	for i := 0; i < searchReps; i++ {
		if err := drain(false); err != nil {
			return queryMeasurement{}, err
		}
	}
	elapsed := time.Since(start)
	after := readMem()

	out.perQuery = elapsed / searchReps
	out.allocsPerQuery = (after.Mallocs - before.Mallocs) / searchReps
	out.bytesPerQuery = (after.TotalAlloc - before.TotalAlloc) / searchReps
	return out, nil
}

// printPlan renders a plan tree as numbered fact lines, so a multi-line plan
// still fits the example's one-fact-per-line output contract.
func printPlan(w io.Writer, key string, explain func() (string, error)) error {
	plan, err := explain()
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	for i, line := range strings.Split(strings.TrimRight(plan, "\n"), "\n") {
		fmt.Fprintf(w, "%s.%d=%s\n", key, i, line)
	}
	return nil
}

// sameRows reports an error unless got and want are the identical sequence. Both
// are sorted by the caller, so this is a multiset comparison.
func sameRows(what string, got, want []string) error {
	if len(got) != len(want) {
		return fmt.Errorf("%s: %d rows, want %d", what, len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			return fmt.Errorf("%s: row %d = %q, want %q", what, i, got[i], want[i])
		}
	}
	return nil
}

// ratio returns slow/fast as a float, guarding a zero denominator.
func ratio(slow, fast time.Duration) float64 {
	if fast <= 0 {
		return 0
	}
	return float64(slow) / float64(fast)
}

// floatRatio returns a/b as a float, guarding a zero denominator.
func floatRatio(a, b uint64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}
