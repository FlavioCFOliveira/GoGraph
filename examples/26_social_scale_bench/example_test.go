package main

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// testConfig is a laptop-sized version of the full specification: same
// model and code path, small enough to build and query well under the
// short-layer 60 s package budget. The shape is deterministic for the
// fixed seed, so the invariants asserted below are stable across
// machines.
func testConfig() config {
	return config{
		users:      2000,
		articles:   200,
		friendsMin: 5,
		friendsMax: 8,
		likesMax:   10,
		seed:       42,
		relTypes:   true,
	}
}

// TestRun drives run into a buffer and asserts only the deterministic
// invariants — node counts, edge-count bounds, and that every query in
// the battery returned a self-consistent answer. The volatile telemetry
// lines (prefixed "# ") are ignored, as required by the examples
// standard for non-deterministic output.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, testConfig()); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseFacts(t, out)
	cfg := testConfig()

	// Node counts are exact and independent of the RNG.
	if got := facts["nodes.users"]; got != int64(cfg.users) {
		t.Errorf("nodes.users = %d, want %d", got, cfg.users)
	}
	if got := facts["nodes.articles"]; got != int64(cfg.articles) {
		t.Errorf("nodes.articles = %d, want %d", got, cfg.articles)
	}

	// FRIEND out-degree is in [friendsMin, friendsMax] per user, so the
	// total lands in the corresponding band.
	friend := facts["edges.friend"]
	if lo, hi := int64(cfg.users*cfg.friendsMin), int64(cfg.users*cfg.friendsMax); friend < lo || friend > hi {
		t.Errorf("edges.friend = %d, want within [%d,%d]", friend, lo, hi)
	}

	// LIKE out-degree is in [0, likesMax] per user.
	like := facts["edges.like"]
	if hi := int64(cfg.users * cfg.likesMax); like < 0 || like > hi {
		t.Errorf("edges.like = %d, want within [0,%d]", like, hi)
	}

	// The query battery must agree with the materialised graph: the
	// label-scan counts equal the node counts, and the relationship
	// counts equal the edge totals reported during the build.
	checks := []struct {
		col  string
		want int64
	}{
		{"q.count_users", int64(cfg.users)},
		{"q.count_articles", int64(cfg.articles)},
		{"q.count_friend", friend},
		{"q.count_like", like},
		// Every relationship's mandatory date is filled, so the IS NOT NULL
		// coverage counts must equal the total relationship counts.
		{"q.friend_since_filled", friend},
		{"q.like_when_filled", like},
	}
	for _, c := range checks {
		if got := facts[c.col]; got != c.want {
			t.Errorf("%s = %d, want %d", c.col, got, c.want)
		}
	}

	// Friend-of-friend reach is non-negative and cannot exceed the
	// number of users; with the chosen degrees it is comfortably > 0.
	if fof := facts["q.fof_reach"]; fof <= 0 || fof > int64(cfg.users) {
		t.Errorf("q.fof_reach = %d, want within (0,%d]", fof, cfg.users)
	}

	// The trending query is LIMIT 10, so it returns exactly 10 rows
	// whenever at least 10 distinct articles were liked (true at this
	// scale).
	if rows := facts["q.top_articles.rows"]; rows != 10 {
		t.Errorf("q.top_articles.rows = %d, want 10", rows)
	}

	assertAnalytical(t, out, facts, cfg, friend)
	assertTemporal(t, out, facts, friend)
	assertCSROrdering(t, out, facts, cfg)
}

// assertCSROrdering pins the CSR neighbour-ordering section (#2147).
//
// The load-bearing assertions are the ORDERING INVARIANT and the SHAPE
// DISCLOSURE. The example's purpose in sprint 313 is to show the ordered path
// engaging on a realistically-sized graph while being honest that its data shape
// cannot demonstrate a hub win, so a silent loss of either the invariant or the
// disclosure is exactly the regression worth gating.
func assertCSROrdering(t *testing.T, out string, facts map[string]int64, cfg config) {
	t.Helper()

	// The #2141 invariant: every CSR this module builds has ordered runs. The
	// example fails hard if this is false, so reaching here already implies it —
	// asserting the printed line as well guards against the report silently
	// dropping the check.
	mustContain(t, out, "csr.runs_ordered=true")

	// The shape must be disclosed, and disclosed CORRECTLY. A CSR run carries
	// FRIEND and LIKE arcs together, so the degree reported is the combined
	// out-degree, not the FRIEND degree the config names. Getting this wrong
	// (as the first draft of this section did) misstates the fixture by 1.67x
	// and invites a reader to mistake it for a hub-heavy shape.
	mustContain(t, out, "degree.measured=FRIEND+LIKE combined (a CSR run carries both types)")
	mustContain(t, out, "degree.distribution=BOUNDED, LIGHT-TAILED (not power-law)")

	// The combined degree must lie inside the range the generator can produce:
	// FRIEND from [friendsMin, friendsMax] plus LIKE from [0, likesMax].
	loBound := int64(cfg.friendsMin)
	hiBound := int64(cfg.friendsMax + cfg.likesMax)
	if got := facts["degree.min"]; got < loBound {
		t.Errorf("degree.min = %d, want >= %d", got, loBound)
	}
	if got := facts["degree.max"]; got > hiBound {
		t.Errorf("degree.max = %d, want <= %d", got, hiBound)
	}
	if got := facts["degree.sources"]; got != int64(cfg.users) {
		t.Errorf("degree.sources = %d, want %d (every user has FRIEND arcs)", got, int64(cfg.users))
	}

	// The threshold is the CALIBRATED crossover of 16, not the refuted 64. If
	// this ever reads 64 again, the example is reporting leverage against a
	// model that was shown to be physically unattainable.
	if got := facts["degree.threshold"]; got != 16 {
		t.Errorf("degree.threshold = %d, want 16 (the calibrated crossover; 64 is refuted)", got)
	}

	// The reverse expand must find rows — a zero would mean the pattern stopped
	// matching and the section is timing nothing. The count is label-constrained
	// to USER on both ends, so it is identical whether or not the FRIEND type is
	// named, which is what keeps it comparable across the -rel-types modes that
	// TestRunCompact diffs.
	if rows := facts["hub.reverse_expand_rows"]; rows <= 0 {
		t.Errorf("hub.reverse_expand_rows = %d, want > 0", rows)
	}

	// The snapshot footprint must be per-arc sane: three uint64 arrays over
	// Size() arcs plus Order()+1 offsets, so 8 B/arc is the floor and the
	// offsets push it a little above.
	// Every slot carries a stable identity since rmp #2317, whatever route created
	// it, so the handle column is present even for a graph built with plain AddEdge.
	mustContain(t, out, "csr.has_handles=true")
}

// assertAnalytical pins the analytical-aggregation and subquery facts (#1971):
// exact seed-pinned results plus the conservation laws that must hold for any
// config (bands and the EXISTS/NOT-EXISTS split partition the users; the UNION
// streams the label totals; every requested UNWIND id resolves).
func assertAnalytical(t *testing.T, out string, facts map[string]int64, cfg config, friend int64) {
	t.Helper()

	// Friend out-degree distribution. min/max equal the configured band; the
	// avg and median are seed-pinned float facts (asserted textually because
	// parseFacts keeps only integer facts).
	if got := facts["q.friend_degree.min"]; got != int64(cfg.friendsMin) {
		t.Errorf("q.friend_degree.min = %d, want %d", got, cfg.friendsMin)
	}
	if got := facts["q.friend_degree.max"]; got != int64(cfg.friendsMax) {
		t.Errorf("q.friend_degree.max = %d, want %d", got, cfg.friendsMax)
	}
	mustContain(t, out, "q.friend_degree.avg=6.5055")    // 13011 friend edges over 2000 users
	mustContain(t, out, "q.friend_degree.median=6.5000") // the interpolated 50th percentile

	// EXISTS { } and NOT EXISTS { } partition the users exactly.
	withLike, withoutLike := facts["q.users_with_like"], facts["q.users_without_like"]
	if withLike+withoutLike != int64(cfg.users) {
		t.Errorf("users_with_like(%d) + users_without_like(%d) != users(%d)", withLike, withoutLike, cfg.users)
	}
	if withLike != 1804 {
		t.Errorf("q.users_with_like = %d, want 1804", withLike)
	}

	// CASE bands partition the users, whichever of low/mid/high are populated.
	var bandSum int64
	for k, v := range facts {
		if strings.HasPrefix(k, "q.degree_band.") {
			bandSum += v
		}
	}
	if bandSum != int64(cfg.users) {
		t.Errorf("degree bands sum to %d, want users(%d)", bandSum, cfg.users)
	}

	// UNION ALL streams the two label counts.
	if facts["q.union.rows"] != 2 {
		t.Errorf("q.union.rows = %d, want 2", facts["q.union.rows"])
	}
	if facts["q.union.users"] != int64(cfg.users) {
		t.Errorf("q.union.users = %d, want %d", facts["q.union.users"], cfg.users)
	}
	if facts["q.union.articles"] != int64(cfg.articles) {
		t.Errorf("q.union.articles = %d, want %d", facts["q.union.articles"], cfg.articles)
	}

	// UNWIND batch: every requested id is real, so matched == requested.
	wantBatch := int64(min(unwindBatch, cfg.users))
	if facts["q.unwind_requested"] != wantBatch {
		t.Errorf("q.unwind_requested = %d, want %d", facts["q.unwind_requested"], wantBatch)
	}
	if facts["q.unwind_matched"] != wantBatch {
		t.Errorf("q.unwind_matched = %d, want %d", facts["q.unwind_matched"], wantBatch)
	}

	// id()/elementId() on the sample user. idPair has already verified the Go
	// kinds (Integer/String) at runtime. id() is the interned NodeID — a valid
	// non-negative id, deterministic for the seed but implementation-defined in
	// value (hash/shard-dependent, not insertion order); elementId() is exactly
	// its decimal string form, which is the relationship this pins.
	nodeID := facts["q.sample_node_id"]
	if nodeID < 0 {
		t.Errorf("q.sample_node_id = %d, want >= 0", nodeID)
	}
	mustContain(t, out, fmt.Sprintf("q.sample_element_id=%d", nodeID))
}

// assertTemporal pins the temporal-function facts (#1972): the two constructor
// sanity checks with known answers, the friendship-age extent, the last-30-days
// window, and the per-year bucket conservation law (buckets sum to the FRIEND
// edge total).
func assertTemporal(t *testing.T, out string, facts map[string]int64, friend int64) {
	t.Helper()

	if got := facts["q.temporal.window_days"]; got != int64(edgeDateWindowDays) {
		t.Errorf("q.temporal.window_days = %d, want %d", got, edgeDateWindowDays)
	}
	if got := facts["q.temporal.dt_span_seconds"]; got != 5400 {
		t.Errorf("q.temporal.dt_span_seconds = %d, want 5400", got)
	}

	// Friendship ages span the whole edge-date window [0, edgeDateWindowDays].
	if got := facts["q.friend_age_days.min"]; got != 0 {
		t.Errorf("q.friend_age_days.min = %d, want 0", got)
	}
	if got := facts["q.friend_age_days.max"]; got != int64(edgeDateWindowDays) {
		t.Errorf("q.friend_age_days.max = %d, want %d", got, edgeDateWindowDays)
	}

	// The last-30-days count is a strict, non-empty subset of the friend edges.
	if r := facts["q.friend_recent_30d"]; r <= 0 || r >= friend {
		t.Errorf("q.friend_recent_30d = %d, want within (0,%d)", r, friend)
	}

	// The per-year buckets partition the FRIEND edges.
	var yearSum int64
	for k, v := range facts {
		if strings.HasPrefix(k, "q.friend_by_year.") {
			yearSum += v
		}
	}
	if yearSum != friend {
		t.Errorf("friend_by_year buckets sum to %d, want edges.friend %d", yearSum, friend)
	}
	mustContain(t, out, "q.friend_by_year.2019=2153")
}

// mustContain fails the test if out does not contain the exact line s (matched
// as a full line, so a prefix match cannot pass by accident).
func mustContain(t *testing.T, out, s string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if line == s {
			return
		}
	}
	t.Errorf("output missing expected fact line %q", s)
}

// TestRunCompact confirms the implicit-type mode (relTypes=false) is
// functionally equivalent: it stores no per-edge labels, yet the
// endpoint-inferred relationship queries return the same counts as the
// explicit-type mode for the same seed.
func TestRunCompact(t *testing.T) {
	explicit := testConfig()
	compact := testConfig()
	compact.relTypes = false

	var eb, cb bytes.Buffer
	if err := run(context.Background(), &eb, explicit); err != nil {
		t.Fatalf("run explicit: %v", err)
	}
	if err := run(context.Background(), &cb, compact); err != nil {
		t.Fatalf("run compact: %v", err)
	}
	// The graph shape is identical for the same seed, so every query answer is
	// identical too: the only deterministic fact line that may differ between
	// the two relationship-encoding modes is config.rel_types. Asserting the
	// full fact-line set (minus that one line) is equal covers every fact —
	// the count/coverage/traversal battery and the analytical + temporal
	// facts, including the float lines parseFacts cannot capture.
	e := dropLine(factLines(eb.String()), "config.rel_types=")
	c := dropLine(factLines(cb.String()), "config.rel_types=")
	if e != c {
		t.Errorf("explicit vs compact fact lines differ beyond config.rel_types:\n--- explicit ---\n%s\n--- compact ---\n%s", e, c)
	}
}

// dropLine returns lines with every line that has the given prefix removed.
func dropLine(lines, prefix string) string {
	var keep []string
	for _, line := range strings.Split(lines, "\n") {
		if strings.HasPrefix(line, prefix) {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

// TestRunRejectsBadConfig confirms the boundary validation: asking for
// more friends than there are other users is rejected before any work.
func TestRunRejectsBadConfig(t *testing.T) {
	bad := config{users: 10, articles: 5, friendsMin: 0, friendsMax: 20, likesMax: 0, seed: 1}
	if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
		t.Fatal("run accepted a config with friendsMax > users-1; want error")
	}
}

// TestRunHonoursCancellation confirms the build aborts promptly when the
// context is already cancelled, returning the context error.
func TestRunHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := run(ctx, &bytes.Buffer{}, testConfig())
	if err == nil {
		t.Fatal("run ignored a cancelled context; want error")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("run error = %v, want context canceled", err)
	}
}

// TestDeterministic confirms the dataset shape is reproducible: two runs
// with the same config produce identical deterministic fact lines.
func TestDeterministic(t *testing.T) {
	var a, b bytes.Buffer
	if err := run(context.Background(), &a, testConfig()); err != nil {
		t.Fatalf("run a: %v", err)
	}
	if err := run(context.Background(), &b, testConfig()); err != nil {
		t.Fatalf("run b: %v", err)
	}
	if factLines(a.String()) != factLines(b.String()) {
		t.Errorf("deterministic fact lines differ between runs:\n--- a ---\n%s\n--- b ---\n%s",
			factLines(a.String()), factLines(b.String()))
	}
}

// TestStatisticsExercise pins the planner-statistics observability section
// (#2120). Its lines are telemetry (prefixed "# "), so they are absent from the
// deterministic fact-line set; this test reads them by name instead. It asserts
// the three provenance tags EXPLAIN surfaces (exact / heuristic / stats+error),
// the tracked-pair count for this schema, and the estimate-vs-actual accuracy —
// exact for the label scan, and within the equi-depth histogram's certified error
// for the range.
func TestStatisticsExercise(t *testing.T) {
	var buf bytes.Buffer
	cfg := testConfig()
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()

	// Five (label, property) pairs are tracked: (USER,id), (USER,name),
	// (USER,country), (ARTICLE,id), (ARTICLE,title). USER gained the country
	// dimension the columnar-aggregation exercise groups on (#2121).
	if got := telemetryInt(t, out, "stats.tracked_pairs"); got != 5 {
		t.Errorf("stats.tracked_pairs = %d, want 5", got)
	}

	// The three representative provenance tags must each appear in the EXPLAIN
	// blocks: the label scan is exact, the absent-value equality is the 1/NDV
	// heuristic, and the range is a histogram estimate carrying its certified error.
	for _, want := range []string{
		"NodeByLabelScan [u:USER] (est. rows=2000, exact)",
		", heuristic)",
		", stats, err=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("statistics EXPLAIN output missing %q\n--- output ---\n%s", want, out)
		}
	}

	// The label scan is estExact: its annotated estimate must equal the real count,
	// which is exactly the number of USER nodes.
	labelEst := telemetryInt(t, out, "stats.label.est_rows")
	labelActual := telemetryInt(t, out, "stats.label.actual_rows")
	if labelEst != int64(cfg.users) || labelActual != int64(cfg.users) {
		t.Errorf("label est/actual = %d/%d, want %d/%d (exact)", labelEst, labelActual, cfg.users, cfg.users)
	}

	// The range is estStats: an approximate estimate whose absolute row error the
	// equi-depth guarantee bounds (δ = 1/B over the summarised rows ≈ users/256).
	// Assert both are populated and the gap sits inside a generous multiple of that
	// bound, so the test guards the accuracy without pinning an internal estimator
	// value.
	rangeEst := telemetryInt(t, out, "stats.range.est_rows")
	rangeActual := telemetryInt(t, out, "stats.range.actual_rows")
	if rangeEst <= 0 || rangeActual <= 0 {
		t.Errorf("range est/actual = %d/%d, want both > 0", rangeEst, rangeActual)
	}
	absErr := telemetryInt(t, out, "stats.range.abs_row_error")
	if got := abs64(rangeEst - rangeActual); got != absErr {
		t.Errorf("stats.range.abs_row_error = %d, but |est-actual| = %d", absErr, got)
	}
	if bound := int64(cfg.users)/256 + 64; absErr > bound {
		t.Errorf("range abs_row_error = %d, want <= %d (certified 1/B bound + slack)", absErr, bound)
	}
}

// TestParallelismExercise pins the intra-query parallelism observability section
// (#2122). Its lines are telemetry (prefixed "# "), so they are absent from the
// deterministic fact-line set; this test reads them by name and asserts the
// DETERMINISTIC invariants the exercise verified internally — the parallel and serial
// aggregate values are identical (the exercise fails the run otherwise) and the count
// pushdown, the full scan, and the population all agree — plus the presence of the
// volatile timing/speedup lines. It deliberately does NOT assert the parallel arm won:
// intra-query parallelism is idle-core-bound, so a loaded or single-core box may show
// no speedup, and gating on it would be a flaky, dishonest assertion.
func TestParallelismExercise(t *testing.T) {
	var buf bytes.Buffer
	if err := parallelismExercise(context.Background(), testConfig(), &buf); err != nil {
		t.Fatalf("parallelismExercise: %v", err)
	}
	out := buf.String()

	// The working set is a fixed constant, independent of -users, and the threshold
	// must sit below it or the parallel path could never engage.
	if got := telemetryInt(t, out, "parallel.scale.nodes"); got != parScaleUsers {
		t.Errorf("parallel.scale.nodes = %d, want %d", got, parScaleUsers)
	}
	if thr := telemetryInt(t, out, "parallel.scale.threshold"); thr >= parScaleUsers {
		t.Errorf("parallel.scale.threshold = %d, want < scale %d (else the parallel path cannot engage)", thr, parScaleUsers)
	}

	// count(*): the O(1) pushdown, the O(N) scan, and the known population all agree
	// (the exercise fails the run otherwise); this re-checks the exact count here.
	if got := telemetryInt(t, out, "parallel.count.value"); got != parScaleUsers {
		t.Errorf("parallel.count.value = %d, want %d (every node counted once)", got, parScaleUsers)
	}
	if got := telemetryInt(t, out, "parallel.count.nodes"); got != parScaleUsers {
		t.Errorf("parallel.count.nodes = %d, want %d", got, parScaleUsers)
	}

	// min/max: the parallel and serial engines returned an identical value (verified
	// inside the exercise), and the extrema bracket the reputation domain.
	minV := telemetryInt(t, out, "parallel.min.value")
	maxV := telemetryInt(t, out, "parallel.max.value")
	if minV < 0 || minV > maxV || maxV >= reputationRange {
		t.Errorf("reputation extrema out of range: min=%d max=%d, want 0 <= min <= max < %d", minV, maxV, reputationRange)
	}

	// The speedup and elapsed lines are volatile telemetry: assert only that both arms
	// ran (the lines are present), never that the parallel arm was faster.
	for _, key := range []string{
		"parallel.min.parallel_elapsed", "parallel.min.serial_elapsed", "parallel.min.speedup",
		"parallel.max.parallel_elapsed", "parallel.max.serial_elapsed", "parallel.max.speedup",
		"parallel.count.o1_elapsed", "parallel.count.scan_elapsed", "parallel.count.speedup",
	} {
		if !hasTelemetry(out, key) {
			t.Errorf("missing telemetry line %q", key)
		}
	}
}

// hasTelemetry reports whether out carries a "# <key>=…" telemetry line, without
// constraining the value's type (used for the volatile duration/speedup lines).
func hasTelemetry(out, key string) bool {
	prefix := "# " + key + "="
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// telemetryInt returns the integer value of the "# <key>=<int>" telemetry line,
// failing the test when the line is absent or its value is not an integer.
func telemetryInt(t *testing.T, out, key string) int64 {
	t.Helper()
	prefix := "# " + key + "="
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			n, err := strconv.ParseInt(strings.TrimPrefix(line, prefix), 10, 64)
			if err != nil {
				t.Fatalf("telemetry %q value is not an integer: %q", key, line)
			}
			return n
		}
	}
	t.Fatalf("telemetry line %q not found in output", key)
	return 0
}

// abs64 returns the absolute value of n.
func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// parseFacts extracts the deterministic "key=int" lines (everything not
// prefixed with "# ") whose value parses as an integer, returning them
// as a map. Lines whose value is not an integer (e.g. the config range
// lines) are skipped.
func parseFacts(t *testing.T, out string) map[string]int64 {
	t.Helper()
	facts := make(map[string]int64)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" || strings.HasPrefix(line, "# ") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("malformed fact line: %q", line)
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			facts[k] = n
		}
	}
	return facts
}

// factLines returns only the deterministic lines of out (dropping the
// volatile "# " telemetry), joined back into a single string for
// equality comparison.
func factLines(out string) string {
	var keep []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "# ") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

// TestCyclicJoinExercise pins the DETERMINISTIC facts of the fused-cyclic-expand
// exercise (#2161) and deliberately pins no timing.
//
// The load-bearing assertions are the two that a differential alone cannot give:
//
//   - `fused_engaged` must be true and `twoexpand_engaged` false. SPIKE #2155 verified
//     the openCypher TCK contains no directed cycle over three or more distinct node
//     variables, so TCK 3897/3897 cannot see this operator; and if the recogniser
//     silently declined, BOTH arms would run today's plan and `plans_agree` would be
//     trivially true. Only the engagement flags separate "identical because correct"
//     from "identical because it never fired".
//   - `labelled_declines` must be true. That is not a defect being tolerated quietly:
//     a node-label predicate interposes a Selection between the cycle's hops, so the
//     fusion correctly refuses, and pinning it keeps the limitation visible in the
//     example's own output rather than buried in a design document.
func TestCyclicJoinExercise(t *testing.T) {
	var buf bytes.Buffer
	if err := cyclicJoinExercise(context.Background(), testConfig(), &buf); err != nil {
		t.Fatalf("cyclicJoinExercise: %v", err)
	}
	out := buf.String()

	// Both scales must appear, so the SCALING behaviour is observable in the example's
	// own output rather than only in a benchmark.
	for _, sc := range []struct {
		users int
		pref  string
	}{
		{cyclicScaleSmall, "cyclic.n" + strconv.Itoa(cyclicScaleSmall)},
		{cyclicScaleLarge, "cyclic.n" + strconv.Itoa(cyclicScaleLarge)},
	} {
		if got := telemetryInt(t, out, sc.pref+".users"); got != int64(sc.users) {
			t.Errorf("%s.users = %d, want %d", sc.pref, got, sc.users)
		}

		// A ring of n nodes with this degree closes exactly 3n directed triangles: each
		// of the n triples {k, k+1, k+2} is enumerated once per starting node, and no
		// other offset combination sums to 0 mod n within the degree window. Pinning the
		// exact value rather than ">0" is what makes this a regression gate: a fusion
		// that dropped or duplicated rows would still satisfy plans_agree if it broke
		// BOTH arms, but it cannot satisfy this.
		wantTri := int64(3 * sc.users)
		if got := factInt(t, out, sc.pref+".triangles"); got != wantTri {
			t.Errorf("%s.triangles = %d, want %d (3 rotations per ring triple)", sc.pref, got, wantTri)
		}

		for _, key := range []string{".plans_agree", ".fused_engaged", ".labelled_declines", ".labelled_agrees"} {
			if !telemetryBoolFact(t, out, sc.pref+key) {
				t.Errorf("%s%s = false, want true", sc.pref, key)
			}
		}
		// The control: the two-Expand arm must NOT engage the operator, or the contrast
		// is measuring one plan against itself.
		if telemetryBoolFact(t, out, sc.pref+".twoexpand_engaged") {
			t.Errorf("%s.twoexpand_engaged = true; the flag-off arm must never engage the operator", sc.pref)
		}
	}

	// The larger scale must report strictly more triangles, or the two rows are not
	// actually measuring different scales.
	small := factInt(t, out, "cyclic.n"+strconv.Itoa(cyclicScaleSmall)+".triangles")
	large := factInt(t, out, "cyclic.n"+strconv.Itoa(cyclicScaleLarge)+".triangles")
	if large <= small {
		t.Errorf("triangles did not grow with scale: small=%d large=%d", small, large)
	}
}

// factInt reads a deterministic `key=<int>` line — one WITHOUT the "# " prefix that
// marks volatile telemetry. telemetryInt deliberately requires that prefix, so the two
// readers cannot be confused: a fact read as telemetry, or vice versa, fails loudly
// rather than silently matching the wrong line.
func factInt(t *testing.T, out, key string) int64 {
	t.Helper()
	prefix := key + "="
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			n, err := strconv.ParseInt(strings.TrimPrefix(line, prefix), 10, 64)
			if err != nil {
				t.Fatalf("fact %q value is not an integer: %q", key, line)
			}
			return n
		}
	}
	t.Fatalf("deterministic fact %q not found in output", key)
	return 0
}

// telemetryBoolFact reads a `key=true|false` deterministic fact line.
func telemetryBoolFact(t *testing.T, out, key string) bool {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, key+"=") {
			return strings.TrimPrefix(line, key+"=") == "true"
		}
	}
	t.Fatalf("telemetry key %q not found in output", key)
	return false
}
