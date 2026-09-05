//go:build threeway

// Memory-footprint head-to-head: GoGraph, Neo4j and Memgraph holding the same
// graph.
//
// The sibling harnesses measure time — how fast one query runs
// (threeway_test.go) and how throughput moves with concurrency
// (concurrency_test.go). This one measures the other axis of the efficiency
// mandate: how many bytes of memory an engine spends to hold a graph, and on
// what.
//
// # The measurement is a SLOPE, not a difference
//
// The reported quantity is the marginal resident cost per element, obtained as
// the slope of a least-squares fit of resident bytes against element count over
// several load increments — never as a single before/after difference.
//
// That is not fastidiousness; a single difference is measurably wrong here. All
// three engines hold their data in structures that grow in steps: Go maps
// double their bucket array, the JVM heap grows in regions, and an allocator
// takes arenas from the operating system in chunks. A difference taken across
// one such step charges the whole step to the elements that happened to be
// loaded when it fired, and a difference taken between two steps charges them
// nothing. A first version of this harness did exactly that and reported a
// three-property node as CHEAPER than a one-property node — an impossibility
// that only the fit made visible. The fit spreads the steps across the whole
// range, and the reported R² states how well the straight line actually
// described the readings, so a reader can see when it did not.
//
// Every increment only ever ADDS data. Deleting between readings would report
// allocator hysteresis — none of the three return freed memory promptly — as
// engine footprint.
//
// # One shape per engine lifetime
//
// Each shape is measured in a FRESHLY STARTED engine holding nothing else, so
// that the structures whose growth is being measured start empty. Mixing
// shapes in one process makes every shape's slope depend on how full the
// shared maps happened to be when it started.
//
// # Why this harness may run from the host, when the concurrency one may not
//
// concurrency_test.go must run inside the virtual machine because the host
// port-forward costs ~170 µs per round trip, which swamps a query that takes
// tens of microseconds. This harness measures BYTES RESIDENT, which the
// transport does not change: a slower load path makes the run take longer, it
// does not make a node occupy more memory.
//
// # Quiescing before reading
//
// A reading taken while an engine still holds transaction state, unreclaimed
// garbage or an unflushed write buffer measures the workload, not the data.
// Each engine is driven to its own steady state before every reading, by the
// mechanism that engine provides:
//
//	GoGraph   GET /gc     — runtime.GC() twice, then debug.FreeOSMemory()
//	Memgraph  FREE MEMORY — its documented reclaim command
//	Neo4j     jcmd GC.run, then wait out a timed checkpoint (see memNeo4jSettle)
//
// Readings come from the cgroup the container is confined to — the one
// instrument all three share — and are cross-checked against each engine's own
// counter (Go's HeapAlloc, Memgraph's graph_memory_tracked, the JVM's heap).
//
// Prerequisites and invocation: docs/memory-vs-neo4j-memgraph-2026-08-11.md.
package comparison

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// ── configuration ────────────────────────────────────────────────────────────

var (
	memNodes  = envInt("MEM_NODES", 1_000_000)
	memDegree = envInt("MEM_DEGREE", 8)
	memSteps  = envInt("MEM_STEPS", 8)
	memShape  = memEnv("MEM_SHAPE", "bare")
	memOnly   = os.Getenv("MEM_ONLY")
	memBatch  = envInt("MEM_BATCH", 1_000)
)

func memEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ── targets ──────────────────────────────────────────────────────────────────

// memTarget names one engine, how to reach it, how to make it quiesce, and
// where to read its own opinion of its memory use.
type memTarget struct {
	Name      string
	Container string
	URI       string
	Auth      neo4j.AuthToken
	Dia       dialect
	// DataDir is the path inside the container whose growth is the engine's
	// on-disk footprint. Empty when the engine stores nothing.
	DataDir string
	// NativeKey names the engine counter that is its own best statement of
	// what it holds, so the cgroup reading is never the only witness.
	NativeKey string
	Quiesce   func(ctx context.Context, t *testing.T, tg *memTarget, d neo4j.DriverWithContext)
	Native    func(ctx context.Context, t *testing.T, tg *memTarget, d neo4j.DriverWithContext) map[string]int64
}

func memTargets() []memTarget {
	all := []memTarget{
		{
			Name: "gograph", Container: "gg-gograph",
			URI: memEnv("MEM_GOGRAPH_URI", "bolt://127.0.0.1:7690"), Auth: neo4j.NoAuth(),
			Dia: gographDialect, DataDir: "", NativeKey: "heap_alloc",
			Quiesce: memQuiesceGoGraph, Native: memNativeGoGraph,
		},
		{
			Name: "neo4j", Container: "gg-neo4j",
			URI:  memEnv("MEM_NEO4J_URI", "bolt://127.0.0.1:7687"),
			Auth: neo4j.BasicAuth("neo4j", "gographbench", ""),
			Dia:  neo4jDialect, DataDir: "/var/lib/neo4j/data/databases/neo4j",
			NativeKey: "heap_used_bytes",
			Quiesce:   memQuiesceNeo4j, Native: memNativeNeo4j,
		},
		{
			Name: "memgraph", Container: "gg-memgraph",
			URI: memEnv("MEM_MEMGRAPH_URI", "bolt://127.0.0.1:7688"), Auth: neo4j.NoAuth(),
			Dia: memgraphDialect, DataDir: "/var/lib/memgraph", NativeKey: "graph_memory_tracked",
			Quiesce: memQuiesceMemgraph, Native: memNativeMemgraph,
		},
		{
			// The same Memgraph build with edge objects switched off. It is not
			// a fourth engine: it is the A/B that turns Memgraph's single
			// largest memory decision from an assertion in its source into a
			// measured number.
			Name: "memgraph-noedgeprops", Container: "gg-memgraph-nep",
			URI: memEnv("MEM_MEMGRAPH_NEP_URI", "bolt://127.0.0.1:7689"), Auth: neo4j.NoAuth(),
			Dia: memgraphDialect, DataDir: "/var/lib/memgraph", NativeKey: "graph_memory_tracked",
			Quiesce: memQuiesceMemgraph, Native: memNativeMemgraph,
		},
	}
	if memOnly == "" {
		// The A/B arm is opt-in: it needs a second container the default
		// three-engine setup does not start.
		return all[:3]
	}
	out := make([]memTarget, 0, len(all))
	for _, want := range strings.Split(memOnly, ",") {
		for i := range all {
			if strings.TrimSpace(want) == all[i].Name {
				out = append(out, all[i])
			}
		}
	}
	return out
}

// ── measurement ──────────────────────────────────────────────────────────────

// memReading is one observation of an engine's footprint.
type memReading struct {
	Elements int64 // elements of the measured shape present at this reading
	// Cgroup fields come from the container's cgroup v2 accounting, which is
	// the one instrument all three engines are measured by identically.
	CgroupCurrent int64 // memory.current: everything charged to the container
	Anon          int64 // anonymous pages: heap, stacks, allocator arenas
	File          int64 // page cache: file-backed pages, i.e. the store files
	DiskBytes     int64 // on-disk footprint of the data directory
	Native        int64 // the engine's own counter, named by NativeKey
	Nodes, Edges  int64
}

func memCgroup(t *testing.T, container string) (current, anon, file int64) {
	t.Helper()
	out := memDockerExec(t, container, "cat /sys/fs/cgroup/memory.current; echo ---; cat /sys/fs/cgroup/memory.stat")
	parts := strings.SplitN(out, "---", 2)
	if len(parts) != 2 {
		t.Fatalf("%s: cgroup read malformed: %q", container, out)
	}
	current = memAtoi(t, strings.TrimSpace(parts[0]))
	for _, line := range strings.Split(parts[1], "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		switch f[0] {
		case "anon":
			anon = memAtoi(t, f[1])
		case "file":
			file = memAtoi(t, f[1])
		}
	}
	// An engine reporting zero anonymous memory is not an engine using none;
	// it is a failed read. Fail loudly rather than publish a zero.
	if current == 0 || anon == 0 {
		t.Fatalf("%s: implausible cgroup reading current=%d anon=%d", container, current, anon)
	}
	return current, anon, file
}

// memDisk reports the blocks the data directory actually occupies, not the sum
// of its files' apparent sizes. Neo4j PREALLOCATES its store files, so the
// apparent size (du -sb) counts space that has been reserved but never
// written, and would report a footprint several times the data's. The
// allocated-block figure is what the filesystem has really spent.
func memDisk(t *testing.T, container, dir string) int64 {
	t.Helper()
	if dir == "" {
		return 0
	}
	return 1024 * memAtoi(t, strings.TrimSpace(
		memDockerExec(t, container, fmt.Sprintf("du -sk %s 2>/dev/null | cut -f1 || echo 0", dir))))
}

func memDockerExec(t *testing.T, container, script string) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", container, "sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("docker exec %s %q: %v: %s", container, script, err, out)
	}
	return string(out)
}

func memAtoi(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return n
}

// ── per-engine quiesce and native counters ───────────────────────────────────

func memQuiesceGoGraph(_ context.Context, t *testing.T, _ *memTarget, _ neo4j.DriverWithContext) {
	t.Helper()
	memGoGraphGet(t, "/gc")
	// FreeOSMemory issues MADV_DONTNEED; the kernel unaccounts those pages
	// asynchronously, so an immediate reading still sees them.
	time.Sleep(2 * time.Second)
}

func memGoGraphGet(t *testing.T, path string) []byte {
	t.Helper()
	url := memEnv("MEM_GOGRAPH_DEBUG", "http://127.0.0.1:6060") + path
	resp, err := http.Get(url) //nolint:gosec // bench helper; url is this harness's own MEM_GOGRAPH_DEBUG, bounded by the test timeout
	if err != nil {
		t.Fatalf("gograph %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gograph %s: status %d", path, resp.StatusCode)
	}
	body := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	return body
}

func memNativeGoGraph(_ context.Context, t *testing.T, _ *memTarget, _ neo4j.DriverWithContext) map[string]int64 {
	t.Helper()
	var raw map[string]int64
	if err := json.Unmarshal(memGoGraphGet(t, "/memstats"), &raw); err != nil {
		t.Fatalf("gograph /memstats decode: %v", err)
	}
	return raw
}

func memQuiesceMemgraph(ctx context.Context, t *testing.T, tg *memTarget, d neo4j.DriverWithContext) {
	t.Helper()
	s := d.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer func() { _ = s.Close(ctx) }()
	if _, err := ccExec(ctx, s, `FREE MEMORY`, nil); err != nil {
		t.Fatalf("%s: FREE MEMORY: %v", tg.Name, err)
	}
	time.Sleep(2 * time.Second)
}

func memNativeMemgraph(ctx context.Context, t *testing.T, tg *memTarget, d neo4j.DriverWithContext) map[string]int64 {
	t.Helper()
	s := d.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer func() { _ = s.Close(ctx) }()
	res, err := s.Run(ctx, `SHOW STORAGE INFO`, nil)
	if err != nil {
		t.Fatalf("%s: SHOW STORAGE INFO: %v", tg.Name, err)
	}
	out := map[string]int64{}
	for res.Next(ctx) {
		rec := res.Record()
		if len(rec.Values) < 2 {
			continue
		}
		k, _ := rec.Values[0].(string)
		switch v := rec.Values[1].(type) {
		case int64:
			out[k] = v
		case string:
			// Memgraph reports every MEMORY figure as a human-readable
			// string — graph_memory_tracked comes back as "28.70MiB", not as
			// a number. A version of this harness that accepted only int64
			// therefore recorded a silent ZERO for the one counter that is
			// Memgraph's own statement of what it holds, and the cross-check
			// that was supposed to corroborate the cgroup reading corroborated
			// nothing while appearing to pass.
			if n, err := memParseHuman(v); err == nil {
				out[k] = n
			}
		}
	}
	// The counter this harness cross-checks against must actually have been
	// read. A zero here is the failure mode described above, not a real value.
	if out[tg.NativeKey] == 0 {
		t.Fatalf("%s: %s missing or zero in SHOW STORAGE INFO (got keys %d)", tg.Name, tg.NativeKey, len(out))
	}
	return out
}

// memParseHuman converts Memgraph's "28.70MiB" style figures to bytes.
func memParseHuman(s string) (int64, error) {
	s = strings.TrimSpace(s)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40}, {"B", 1}} {
		if !strings.HasSuffix(s, u.suffix) {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), 64)
		if err != nil {
			return 0, err
		}
		return int64(f * float64(u.mult)), nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// memNeo4jSettle is how long Neo4j is given to run a checkpoint of its own.
//
// Neo4j 5.26 COMMUNITY has no procedure that triggers one: db.checkpoint()
// exists only in Enterprise, and calling it here fails with
// ProcedureNotFound — which an earlier version of this harness did, on every
// reading. Community checkpoints on a timer instead, so the container is
// configured with db.checkpoint.interval.time=5s and each reading simply waits
// out more than one interval. Without this the store-file figure lags the data
// by up to fifteen minutes and reports a footprint the engine has already
// exceeded.
const memNeo4jSettle = 14 * time.Second

func memQuiesceNeo4j(_ context.Context, t *testing.T, tg *memTarget, _ neo4j.DriverWithContext) {
	t.Helper()
	// The JVM holds the load's garbage until told otherwise; only a full
	// collection makes a heap reading mean "live" rather than "allocated
	// since the last collection".
	memDockerExec(t, tg.Container, `jcmd $(pgrep -f 'org.neo4j' | head -1) GC.run >/dev/null 2>&1 || true`)
	time.Sleep(memNeo4jSettle)
}

func memNativeNeo4j(_ context.Context, t *testing.T, tg *memTarget, _ neo4j.DriverWithContext) map[string]int64 {
	t.Helper()
	out := memDockerExec(t, tg.Container, `jcmd $(pgrep -f 'org.neo4j' | head -1) GC.heap_info 2>/dev/null || true`)
	stats := map[string]int64{}
	// GC.heap_info prints e.g. "garbage-first heap total 2097152K, used 524288K".
	for _, tok := range strings.Fields(strings.ReplaceAll(out, ",", " ")) {
		if !strings.HasSuffix(tok, "K") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSuffix(tok, "K"), 10, 64)
		if err != nil {
			continue
		}
		if _, ok := stats["heap_total_bytes"]; !ok {
			stats["heap_total_bytes"] = n * 1024
		} else if _, ok := stats["heap_used_bytes"]; !ok {
			stats["heap_used_bytes"] = n * 1024
		}
	}
	return stats
}

// ── shapes ───────────────────────────────────────────────────────────────────

// memShapeSpec is one thing whose per-element memory cost is being measured.
// Preamble builds whatever must already exist (a node population, an index)
// and is completed BEFORE the baseline reading, so its cost never lands in the
// slope. Step then adds Elements more of the measured thing, `memSteps` times.
type memShapeSpec struct {
	Unit     string // "node" or "edge": which count proves the load landed
	Total    int    // elements added across all steps
	Preamble func(ctx context.Context, t *testing.T, tg *memTarget, d neo4j.DriverWithContext)
	Step     func(ctx context.Context, t *testing.T, tg *memTarget, d neo4j.DriverWithContext, lo, hi int)
}

func memShapes() map[string]memShapeSpec {
	noPreamble := func(context.Context, *testing.T, *memTarget, neo4j.DriverWithContext) {}

	// nodePreambleForEdges builds the node population and the seek index the
	// edge shapes join through. Without the index every edge row is a full
	// scan, and the load would take longer than the measurement is worth.
	nodePreambleForEdges := func(ctx context.Context, t *testing.T, tg *memTarget, d neo4j.DriverWithContext) {
		memDDL(ctx, t, tg, d, tg.Dia.CreateIndex("Person", "sid"))
		memAwaitIndex(ctx, t, tg, d)
		memCreateNodes(ctx, t, tg, d, `UNWIND $rows AS r CREATE (:Person {sid: r.sid})`,
			func(i int) map[string]any { return map[string]any{"sid": memSid(i)} }, 0, memNodes)
	}

	return map[string]memShapeSpec{
		// A node carrying nothing but one label: the floor of what an engine
		// spends to know a node exists.
		"bare": {Unit: "node", Total: memNodes, Preamble: noPreamble,
			Step: func(ctx context.Context, t *testing.T, tg *memTarget, d neo4j.DriverWithContext, lo, hi int) {
				memCreateNodes(ctx, t, tg, d, `UNWIND $rows AS r CREATE (:Bare)`,
					func(int) map[string]any { return map[string]any{} }, lo, hi)
			}},
		// The same node with one 9-character string property. Against `bare`
		// this isolates the cost of a single short string.
		"prop1": {Unit: "node", Total: memNodes, Preamble: noPreamble,
			Step: func(ctx context.Context, t *testing.T, tg *memTarget, d neo4j.DriverWithContext, lo, hi int) {
				memCreateNodes(ctx, t, tg, d, `UNWIND $rows AS r CREATE (:P1 {sid: r.sid})`,
					func(i int) map[string]any { return map[string]any{"sid": memSid(i)} }, lo, hi)
			}},
		// Two short strings and one small integer: the shape a realistic
		// record has. Against `prop1` this isolates a string plus an integer.
		"prop3": {Unit: "node", Total: memNodes, Preamble: noPreamble,
			Step: func(ctx context.Context, t *testing.T, tg *memTarget, d neo4j.DriverWithContext, lo, hi int) {
				memCreateNodes(ctx, t, tg, d,
					`UNWIND $rows AS r CREATE (:P3 {sid: r.sid, name: r.name, age: r.age})`,
					func(i int) map[string]any {
						return map[string]any{"sid": memSid(i), "name": fmt.Sprintf("person-%d", i), "age": int64(18 + i%60)}
					}, lo, hi)
			}},
		// A typed relationship with no properties: the floor of an edge.
		"edge": {Unit: "edge", Total: memNodes * memDegree, Preamble: nodePreambleForEdges,
			Step: func(ctx context.Context, t *testing.T, tg *memTarget, d neo4j.DriverWithContext, lo, hi int) {
				memCreateEdges(ctx, t, tg, d,
					`UNWIND $rows AS r MATCH (a:Person {sid: r.ss}), (b:Person {sid: r.ts}) CREATE (a)-[:KNOWS]->(b)`,
					lo, hi, false)
			}},
		// The same relationship carrying one small integer.
		"edgeprop": {Unit: "edge", Total: memNodes * memDegree, Preamble: nodePreambleForEdges,
			Step: func(ctx context.Context, t *testing.T, tg *memTarget, d neo4j.DriverWithContext, lo, hi int) {
				memCreateEdges(ctx, t, tg, d,
					`UNWIND $rows AS r MATCH (a:Person {sid: r.ss}), (b:Person {sid: r.ts}) CREATE (a)-[:KNOWS {since: r.since}]->(b)`,
					lo, hi, true)
			}},
		// The `prop1` population held WITH a secondary index over its string
		// key. The index exists before the first node does, so every engine
		// maintains it incrementally and each reading holds an index covering
		// exactly the population present — no rebuild lands inside a step.
		// Against `prop1` the difference in slope is the index's per-entry cost.
		"index": {Unit: "node", Total: memNodes,
			Preamble: func(ctx context.Context, t *testing.T, tg *memTarget, d neo4j.DriverWithContext) {
				memDDL(ctx, t, tg, d, tg.Dia.CreateIndex("P1", "sid"))
				memAwaitIndex(ctx, t, tg, d)
			},
			Step: func(ctx context.Context, t *testing.T, tg *memTarget, d neo4j.DriverWithContext, lo, hi int) {
				memCreateNodes(ctx, t, tg, d, `UNWIND $rows AS r CREATE (:P1 {sid: r.sid})`,
					func(i int) map[string]any { return map[string]any{"sid": memSid(i)} }, lo, hi)
			}},
	}
}

func memSid(i int) string { return fmt.Sprintf("p%08d", i) }

func memCreateNodes(ctx context.Context, t *testing.T, tg *memTarget, d neo4j.DriverWithContext,
	cy string, row func(i int) map[string]any, lo, hi int) {
	t.Helper()
	s := d.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer func() { _ = s.Close(ctx) }()
	for b := lo; b < hi; b += memBatch {
		rows := make([]any, 0, memBatch)
		for i := b; i < b+memBatch && i < hi; i++ {
			rows = append(rows, row(i))
		}
		if _, err := ccExec(ctx, s, cy, map[string]any{"rows": rows}); err != nil {
			t.Fatalf("%s: %q at %d: %v", tg.Name, cy, b, err)
		}
	}
}

// memCreateEdges adds the edges whose source node index lies in [lo,hi), so
// that a step's edges are a contiguous slice of the same deterministic graph
// every engine receives.
func memCreateEdges(ctx context.Context, t *testing.T, tg *memTarget, d neo4j.DriverWithContext,
	cy string, lo, hi int, withProp bool) {
	t.Helper()
	s := d.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer func() { _ = s.Close(ctx) }()
	for b := lo; b < hi; b += memBatch {
		rows := make([]any, 0, memBatch*memDegree)
		for i := b; i < b+memBatch && i < hi; i++ {
			for k := 1; k <= memDegree; k++ {
				dst := (i*31 + k*2654435761) % memNodes
				r := map[string]any{"ss": memSid(i), "ts": memSid(dst)}
				if withProp {
					r["since"] = int64(2000 + (i+k)%25)
				}
				rows = append(rows, r)
			}
		}
		if _, err := ccExec(ctx, s, cy, map[string]any{"rows": rows}); err != nil {
			t.Fatalf("%s: edges at %d: %v", tg.Name, b, err)
		}
	}
}

func memDDL(ctx context.Context, t *testing.T, tg *memTarget, d neo4j.DriverWithContext, ddl string) {
	t.Helper()
	s := d.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer func() { _ = s.Close(ctx) }()
	if _, err := ccExec(ctx, s, ddl, nil); err != nil &&
		!strings.Contains(err.Error(), "AlreadyExists") && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("%s: %q: %v", tg.Name, ddl, err)
	}
}

// memAwaitIndex blocks until a just-created index is populated. Neo4j builds
// indexes in the background, so a reading taken as soon as the DDL returns
// would charge the build to whichever step happened to be running when it
// finished.
func memAwaitIndex(ctx context.Context, t *testing.T, tg *memTarget, d neo4j.DriverWithContext) {
	t.Helper()
	if tg.Name != "neo4j" {
		return
	}
	s := d.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer func() { _ = s.Close(ctx) }()
	if _, err := ccExec(ctx, s, `CALL db.awaitIndexes(600)`, nil); err != nil {
		t.Fatalf("%s: awaitIndexes: %v", tg.Name, err)
	}
}

// ── the fit ──────────────────────────────────────────────────────────────────

// memFit returns the least-squares slope and intercept of ys against xs, and
// the coefficient of determination. The slope is the per-element cost; R²
// reports how straight the readings actually were, which is the harness's own
// statement of how much the slope may be trusted.
func memFit(xs, ys []float64) (slope, intercept, r2 float64) {
	n := float64(len(xs))
	if n < 2 {
		return math.NaN(), math.NaN(), math.NaN()
	}
	var sx, sy, sxx, sxy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return math.NaN(), math.NaN(), math.NaN()
	}
	slope = (n*sxy - sx*sy) / den
	intercept = (sy - slope*sx) / n
	mean := sy / n
	var ssRes, ssTot float64
	for i := range xs {
		pred := slope*xs[i] + intercept
		ssRes += (ys[i] - pred) * (ys[i] - pred)
		ssTot += (ys[i] - mean) * (ys[i] - mean)
	}
	if ssTot == 0 {
		return slope, intercept, math.NaN()
	}
	return slope, intercept, 1 - ssRes/ssTot
}

// ── the test ─────────────────────────────────────────────────────────────────

func TestMemoryFootprint(t *testing.T) {
	spec, ok := memShapes()[memShape]
	if !ok {
		keys := make([]string, 0, len(memShapes()))
		for k := range memShapes() {
			keys = append(keys, k)
		}
		t.Fatalf("MEM_SHAPE=%q unknown; have %v", memShape, keys)
	}

	ctx := context.Background()
	targets := memTargets()
	// Ranged by index and addressed by pointer: memTarget is 144 bytes, and
	// it is threaded through every phase, quiesce and reading.
	for i := range targets {
		tg := &targets[i]
		t.Run(tg.Name, func(t *testing.T) {
			d, err := neo4j.NewDriverWithContext(tg.URI, tg.Auth)
			if err != nil {
				t.Fatalf("driver: %v", err)
			}
			defer func() { _ = d.Close(ctx) }()
			if err := d.VerifyConnectivity(ctx); err != nil {
				t.Fatalf("connect %s: %v", tg.URI, err)
			}

			// The engine must be empty. A shape measured on top of a previous
			// shape's residue has a contaminated intercept and, worse, shares
			// its structures — which is the whole reason each shape gets its
			// own engine lifetime.
			if n := ccCount(ctx, t, d, `MATCH (n) RETURN count(n) AS n`); n != 0 {
				t.Fatalf("%s: engine is not empty (%d nodes) — restart the container before measuring a shape", tg.Name, n)
			}

			spec.Preamble(ctx, t, tg, d)
			readings := []memReading{memRead(ctx, t, tg, d, 0)}
			t.Logf("MEMPOINT %s %s baseline %+v", tg.Name, memShape, readings[0])

			per := spec.Total / memSteps
			for s := 1; s <= memSteps; s++ {
				lo, hi := (s-1)*per, s*per
				if spec.Unit == "edge" {
					// Edge steps are indexed by SOURCE NODE, and each source
					// contributes `memDegree` edges.
					lo, hi = lo/memDegree, hi/memDegree
				}
				start := time.Now()
				spec.Step(ctx, t, tg, d, lo, hi)
				load := time.Since(start)
				r := memRead(ctx, t, tg, d, int64(s*per))
				readings = append(readings, r)
				t.Logf("MEMPOINT %s %s step=%d load=%s %+v", tg.Name, memShape, s, load.Round(time.Millisecond), r)
			}

			memAssertGrowth(t, spec, readings)
			memReport(t, tg, spec, readings)
		})
	}
}

func memRead(ctx context.Context, t *testing.T, tg *memTarget, d neo4j.DriverWithContext, elements int64) memReading {
	t.Helper()
	tg.Quiesce(ctx, t, tg, d)
	cur, anon, file := memCgroup(t, tg.Container)
	native := tg.Native(ctx, t, tg, d)
	return memReading{
		Elements: elements, CgroupCurrent: cur, Anon: anon, File: file,
		DiskBytes: memDisk(t, tg.Container, tg.DataDir),
		Native:    native[tg.NativeKey],
		Nodes:     ccCount(ctx, t, d, `MATCH (n) RETURN count(n) AS n`),
		Edges:     ccCount(ctx, t, d, `MATCH ()-[r]->() RETURN count(r) AS n`),
	}
}

// memAssertGrowth proves the fixture the readings describe is the fixture the
// shape asked for. Without it the slope's denominator is an assumption.
func memAssertGrowth(t *testing.T, spec memShapeSpec, readings []memReading) {
	t.Helper()
	base := readings[0]
	for i, r := range readings {
		var got int64
		switch spec.Unit {
		case "node":
			got = r.Nodes - base.Nodes
		case "edge":
			got = r.Edges - base.Edges
		default:
			t.Fatalf("unknown unit %q", spec.Unit)
		}
		if got != r.Elements {
			t.Fatalf("reading %d: expected %d %ss above baseline, engine holds %d — the slope would be fitted against a count the engine did not honour",
				i, r.Elements, spec.Unit, got)
		}
	}
}

func memReport(t *testing.T, tg *memTarget, spec memShapeSpec, readings []memReading) {
	t.Helper()
	xs := make([]float64, len(readings))
	for i, r := range readings {
		xs[i] = float64(r.Elements)
	}
	col := func(pick func(memReading) int64) []float64 {
		ys := make([]float64, len(readings))
		for i, r := range readings {
			ys[i] = float64(pick(r))
		}
		return ys
	}
	for _, m := range []struct {
		name string
		pick func(memReading) int64
	}{
		{"cgroup_current", func(r memReading) int64 { return r.CgroupCurrent }},
		{"anon", func(r memReading) int64 { return r.Anon }},
		{"page_cache", func(r memReading) int64 { return r.File }},
		{"disk", func(r memReading) int64 { return r.DiskBytes }},
		{"native_" + tg.NativeKey, func(r memReading) int64 { return r.Native }},
	} {
		slope, intercept, r2 := memFit(xs, col(m.pick))
		t.Logf("MEMFIT %s shape=%s metric=%s bytes_per_%s=%.2f intercept=%.0f r2=%.4f n=%d",
			tg.Name, memShape, m.name, spec.Unit, slope, intercept, r2, len(readings))
	}
}
