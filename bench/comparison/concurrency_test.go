//go:build threeway

// Concurrency head-to-head: GoGraph, Neo4j and Memgraph under identical
// concurrent load.
//
// The sibling harness in threeway_test.go measures one client issuing one query
// at a time, so it answers "how fast is a query" and says nothing about how an
// engine behaves when many clients arrive together. This file answers the other
// question: how throughput and latency move as the number of concurrent clients
// rises, and where each engine stops scaling.
//
// # Method
//
// Every target is a CPU-capped server CONTAINER on one docker network,
// addressed over TCP by the same client (neo4j-go-driver/v5) with a connection
// pool sized above the highest concurrency level, so that no arm is measuring
// connection churn — 60% of the cost of this project's earlier Bolt sweep
// turned out to be the handshake.
//
//	gograph-bolt   ./bench/comparison/ggserver, GOMAXPROCS=4, --cpus=4
//	neo4j-bolt     neo4j:5.26-community                       --cpus=4
//	memgraph-bolt  memgraph/memgraph:3.9.0                    --cpus=4
//
// # The client must run inside the virtual machine
//
// On a macOS host the engines run in a Linux VM, and reaching them through the
// VM's port-forward costs roughly 170 µs per round trip: the same GoGraph
// binary measured p50 29 µs natively against 213 µs through the forward, which
// pinned two unrelated engines to an identical ~4 700 ops/s floor. That floor
// is the port-forward, not the engines. Build this test as a linux binary, put
// it in a container on the same network, and address the engines by container
// name. Running it from the host measures colima.
//
// # What may and may not be concluded
//
// The primary metric is each engine's SELF-NORMALISED scaling — throughput at
// concurrency N divided by its own throughput at concurrency 1. That ratio is
// the concurrency property being evaluated, and it survives what absolute
// throughput does not: a cross-engine ops/s comparison also contains the JVM's
// warm-up state and each engine's Bolt implementation. Absolute figures are
// reported because they are the measured quantity, but the ranking this
// harness supports is the shape of the curve, not the height of it.
//
// The write workload is NOT a durability-comparable measurement. GoGraph's
// ggserver is in-memory, Memgraph's WAL is off by default, and Neo4j fsyncs
// every commit — two non-durable engines against one durable one.
//
// Prerequisites and the full invocation are in
// docs/concurrency-vs-neo4j-memgraph-2026-08-11.md §9.
package comparison

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/config"
)

// ── configuration ────────────────────────────────────────────────────────────

var (
	ccNodes    = envInt("CC_NODES", 5_000)
	ccDegree   = envInt("CC_DEGREE", 8)
	ccDuration = time.Duration(envInt("CC_DURATION_MS", 3_000)) * time.Millisecond
	ccRounds   = envInt("CC_ROUNDS", 3)
	ccOnly     = os.Getenv("CC_ONLY")
	ccLevels   = ccParseLevels(os.Getenv("CC_LEVELS"), []int{1, 2, 4, 8, 16, 32, 64})

	// ccWarmup is untimed load applied at the arm's own concurrency before the
	// measured window opens.
	//
	// It is not a nicety. One of the three engines runs on a JVM that compiles
	// its hot paths only after thousands of iterations, so a single warm-up
	// operation per client measures the interpreter and reports it as the
	// engine: in a trial run Neo4j's conc=1 throughput swung between 1 554 and
	// 3 675 ops/s across workloads purely on how warm it happened to be. A
	// comparison that lets one competitor run cold is not a comparison.
	ccWarmup = time.Duration(envInt("CC_WARMUP_MS", 2_000)) * time.Millisecond

	// ccPrewarm is a single, longer warm-up per target before the sweep starts,
	// so the first measured arm is not the one paying for JIT compilation and
	// first page-cache population.
	ccPrewarm = time.Duration(envInt("CC_PREWARM_MS", 8_000)) * time.Millisecond
)

func ccParseLevels(s string, def []int) []int {
	if strings.TrimSpace(s) == "" {
		return def
	}
	var out []int
	for _, f := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err == nil && n > 0 {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

// ccTargetSpec names one engine and how to reach it.
type ccTargetSpec struct {
	Name string
	URI  string
	Auth neo4j.AuthToken
	Dia  dialect
}

func ccTargets() []ccTargetSpec {
	all := []ccTargetSpec{
		{"gograph-bolt", ccEnv("CC_GOGRAPH_URI", "bolt://127.0.0.1:7690"), neo4j.NoAuth(), gographDialect},
		{"neo4j-bolt", ccEnv("CC_NEO4J_URI", "bolt://127.0.0.1:7687"), neo4j.BasicAuth("neo4j", "gographbench", ""), neo4jDialect},
		{"memgraph-bolt", ccEnv("CC_MEMGRAPH_URI", "bolt://127.0.0.1:7688"), neo4j.NoAuth(), memgraphDialect},
		// Same GoGraph binary, same CPU budget, but running natively on the
		// host instead of inside the virtual machine. It is not part of the
		// three-way comparison: it is the control that turns the virtual
		// machine's port-forwarding cost from a caveat into a measured number,
		// by differing from gograph-bolt in transport alone.
		{"gograph-native", ccEnv("CC_GOGRAPH_NATIVE_URI", "bolt://127.0.0.1:7689"), neo4j.NoAuth(), gographDialect},
	}
	if ccOnly == "" {
		return all
	}
	var out []ccTargetSpec
	for _, t := range all {
		for _, want := range strings.Split(ccOnly, ",") {
			if strings.TrimSpace(want) == t.Name {
				out = append(out, t)
			}
		}
	}
	return out
}

func ccEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ── workloads ────────────────────────────────────────────────────────────────

// ccWorkload is one query shape driven concurrently. params is called per
// operation with the client's index and its op counter, so that each client
// touches a different part of the graph and writers never collide on a key.
//
// wantRows is the exact row count one operation must return. It is asserted on
// every single operation: an arm whose oracle cannot fail would report
// throughput for a query that silently returned nothing.
type ccWorkload struct {
	Key      string
	Desc     string
	Cypher   map[string]string // per-target override; "" key is the default
	Params   func(client, op int) map[string]any
	WantRows int
	Write    bool

	// TxBatch, when > 0, runs this many queries inside ONE explicit
	// transaction instead of one autocommit transaction per query.
	//
	// This is the causal probe for §4.1's claim about Memgraph. Its
	// per-query storage accessor — and therefore the global std::mutex inside
	// ResourceLock::lock_shared — is acquired in SetupDatabaseTransaction,
	// which interpreter.cpp guards with `if (!in_explicit_transaction_)`. So
	// batching K queries into one explicit transaction divides the number of
	// acquisitions by K while leaving the query work identical. If the
	// acquisition is what limits Memgraph's scaling, batching must lift it;
	// if scaling is unchanged, the mechanism is elsewhere and the claim is
	// wrong.
	TxBatch int
}

// ccWorkloadFilter restricts the sweep to named workloads. The write workload
// is separable on purpose: it accumulates nodes that must be deleted between
// arms, and GoGraph's delete path degrades under repeated create/delete churn
// (see TestDeleteBatchScaling), so a long mixed run spends most of its time in
// cleanup rather than measurement.
var ccWorkloadFilter = os.Getenv("CC_WORKLOADS")

func ccWorkloads() []ccWorkload {
	all := ccAllWorkloads()
	if ccWorkloadFilter == "" {
		return all
	}
	var out []ccWorkload
	for _, w := range all {
		for _, want := range strings.Split(ccWorkloadFilter, ",") {
			if strings.TrimSpace(want) == w.Key {
				out = append(out, w)
			}
		}
	}
	return out
}

func ccAllWorkloads() []ccWorkload {
	return []ccWorkload{
		{
			Key:  "read_point",
			Desc: "indexed point lookup on a string key",
			Cypher: map[string]string{
				"": `MATCH (p:Person {sid: $s}) RETURN p.name AS name`,
			},
			Params: func(client, op int) map[string]any {
				// Deterministic but spread across the whole key space, so no
				// two clients share a cache line of interest and the access
				// pattern does not degenerate to one hot key.
				id := (client*7919 + op*104729) % ccNodes
				return map[string]any{"s": fmt.Sprintf("p%08d", id)}
			},
			WantRows: 1,
		},
		{
			Key:  "read_2hop",
			Desc: "two-hop expansion from an indexed seed, aggregated",
			Cypher: map[string]string{
				"": `MATCH (a:Person {sid: $s})-[:KNOWS]->()-[:KNOWS]->(c) RETURN count(c) AS n`,
			},
			Params: func(client, op int) map[string]any {
				id := (client*7919 + op*104729) % ccNodes
				return map[string]any{"s": fmt.Sprintf("p%08d", id)}
			},
			WantRows: 1,
		},
		{
			// Matched pair with read_2hop: identical query, identical
			// parameters, differing ONLY in how many explicit transactions
			// the same number of queries is spread across.
			Key:  "read_2hop_tx20",
			Desc: "the read_2hop query, 20 per explicit transaction instead of one autocommit each",
			Cypher: map[string]string{
				"": `MATCH (a:Person {sid: $s})-[:KNOWS]->()-[:KNOWS]->(c) RETURN count(c) AS n`,
			},
			Params: func(client, op int) map[string]any {
				id := (client*7919 + op*104729) % ccNodes
				return map[string]any{"s": fmt.Sprintf("p%08d", id)}
			},
			WantRows: 1,
			TxBatch:  20,
		},
		{
			Key:  "write_create",
			Desc: "autocommit CREATE of a node on a key disjoint per client",
			Cypher: map[string]string{
				"": `CREATE (t:Tmp {k: $k})`,
			},
			Params: func(client, op int) map[string]any {
				return map[string]any{"k": fmt.Sprintf("c%d-%d", client, op)}
			},
			WantRows: 0,
			Write:    true,
		},
	}
}

func (w ccWorkload) cypherFor(target string) string {
	if q, ok := w.Cypher[target]; ok {
		return q
	}
	return w.Cypher[""]
}

// ── driver ───────────────────────────────────────────────────────────────────

// ccNewDriver builds a driver whose pool cannot be the bottleneck. The pool is
// sized above the highest concurrency level so that every client holds its own
// connection for the whole arm and no operation waits to acquire one.
func ccNewDriver(ctx context.Context, spec ccTargetSpec, poolSize int) (neo4j.DriverWithContext, error) {
	d, err := neo4j.NewDriverWithContext(spec.URI, spec.Auth, func(c *config.Config) {
		c.MaxConnectionPoolSize = poolSize
		c.ConnectionAcquisitionTimeout = 60 * time.Second
		c.SocketConnectTimeout = 30 * time.Second
		c.MaxConnectionLifetime = time.Hour
	})
	if err != nil {
		return nil, err
	}
	var lastErr error
	for i := 0; i < 60; i++ {
		if lastErr = d.VerifyConnectivity(ctx); lastErr == nil {
			return d, nil
		}
		time.Sleep(2 * time.Second)
	}
	_ = d.Close(ctx)
	return nil, fmt.Errorf("%s: VerifyConnectivity: %w", spec.Name, lastErr)
}

// ccExecInTx runs w.TxBatch queries inside one explicit transaction and
// reports w.WantRows when every one of them returned the expected row count,
// so the caller's oracle stays exactly as strict as in the autocommit path.
func ccExecInTx(ctx context.Context, s neo4j.SessionWithContext, cy string, w ccWorkload, client, op int) (int, error) {
	tx, err := s.BeginTransaction(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Close(ctx) }()
	for i := 0; i < w.TxBatch; i++ {
		res, err := tx.Run(ctx, cy, w.Params(client, op*w.TxBatch+i))
		if err != nil {
			return 0, err
		}
		n := 0
		for res.Next(ctx) {
			n++
		}
		if err := res.Err(); err != nil {
			return 0, err
		}
		if n != w.WantRows {
			return -1, nil
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return w.WantRows, nil
}

func ccExec(ctx context.Context, s neo4j.SessionWithContext, cy string, params map[string]any) (int, error) {
	res, err := s.Run(ctx, cy, params)
	if err != nil {
		return 0, err
	}
	n := 0
	for res.Next(ctx) {
		n++
	}
	if err := res.Err(); err != nil {
		return 0, err
	}
	return n, nil
}

// ── dataset load ─────────────────────────────────────────────────────────────

// ccLoad wipes the target and loads a deterministic Person/KNOWS graph. Every
// target receives byte-identical data, built from the same seedless arithmetic,
// so a row count that differs between targets is a defect and not a fixture.
func ccLoad(ctx context.Context, t *testing.T, spec ccTargetSpec, d neo4j.DriverWithContext) time.Duration {
	t.Helper()
	s := d.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer func() { _ = s.Close(ctx) }()

	ccWipeAll(ctx, t, spec.Name, d)
	// The seed index must exist before the edge phase, or every edge row is a
	// full scan and the load time measures the planner rather than the loader.
	for _, ddl := range []string{spec.Dia.CreateIndex("Person", "sid")} {
		if _, err := ccExec(ctx, s, ddl, nil); err != nil &&
			!strings.Contains(err.Error(), "AlreadyExists") &&
			!strings.Contains(err.Error(), "already exists") {
			t.Fatalf("%s: index %q: %v", spec.Name, ddl, err)
		}
	}

	start := time.Now()
	const batch = 1_000
	for lo := 0; lo < ccNodes; lo += batch {
		rows := make([]any, 0, batch)
		for i := lo; i < lo+batch && i < ccNodes; i++ {
			rows = append(rows, map[string]any{
				"sid":  fmt.Sprintf("p%08d", i),
				"name": fmt.Sprintf("person-%d", i),
				"age":  int64(18 + i%60),
			})
		}
		if _, err := ccExec(ctx, s,
			`UNWIND $rows AS r CREATE (p:Person {sid: r.sid, name: r.name, age: r.age})`,
			map[string]any{"rows": rows}); err != nil {
			t.Fatalf("%s: load persons: %v", spec.Name, err)
		}
	}
	for lo := 0; lo < ccNodes; lo += batch {
		rows := make([]any, 0, batch*ccDegree)
		for i := lo; i < lo+batch && i < ccNodes; i++ {
			for k := 1; k <= ccDegree; k++ {
				dst := (i*31 + k*2654435761) % ccNodes
				rows = append(rows, map[string]any{
					"ss": fmt.Sprintf("p%08d", i),
					"ts": fmt.Sprintf("p%08d", dst),
				})
			}
		}
		if _, err := ccExec(ctx, s,
			`UNWIND $rows AS r MATCH (a:Person {sid: r.ss}), (b:Person {sid: r.ts}) CREATE (a)-[:KNOWS]->(b)`,
			map[string]any{"rows": rows}); err != nil {
			t.Fatalf("%s: load edges: %v", spec.Name, err)
		}
	}
	return time.Since(start)
}

// ccCount returns a scalar count, used to prove that every target holds the
// same graph before any timing is compared.
func ccCount(ctx context.Context, t *testing.T, d neo4j.DriverWithContext, cy string) int64 {
	t.Helper()
	s := d.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer func() { _ = s.Close(ctx) }()
	res, err := s.Run(ctx, cy, nil)
	if err != nil {
		t.Fatalf("count %q: %v", cy, err)
	}
	rec, err := res.Single(ctx)
	if err != nil {
		t.Fatalf("count %q: single: %v", cy, err)
	}
	v, _ := rec.Values[0].(int64)
	return v
}

// ccWipeTmp removes the nodes the write workload created and proves the fixture
// is back to its loaded state. A residue would silently change the graph every
// later arm runs against.
// ccWipeAll clears the whole graph in bounded batches, so that a leftover
// fixture from an earlier run cannot make the harness unable to start.
func ccWipeAll(ctx context.Context, t *testing.T, name string, d neo4j.DriverWithContext) {
	t.Helper()
	s := d.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer func() { _ = s.Close(ctx) }()
	for i := 0; ; i++ {
		n := ccCount(ctx, t, d, `MATCH (n) RETURN count(n) AS n`)
		if n == 0 {
			return
		}
		if i > 10_000 {
			t.Fatalf("%s: wipe made no progress, %d remain", name, n)
		}
		if _, err := ccExec(ctx, s, `MATCH (n) WITH n LIMIT 5000 DETACH DELETE n`, nil); err != nil {
			t.Fatalf("%s: wipe: %v", name, err)
		}
	}
}

// The wipe is BATCHED because a single-statement delete of the whole set does
// not survive on every engine: a write arm leaves of the order of 10^5 Tmp
// nodes, and deleting them in one transaction exceeded GoGraph's 30 s
// DefaultTxTimeout, failing the sweep. Bounding the work per transaction is
// also what a production caller would do, and it keeps the wipe comparable
// across the three engines instead of measuring one engine's timeout policy.
func ccWipeTmp(ctx context.Context, t *testing.T, name string, d neo4j.DriverWithContext) {
	t.Helper()
	s := d.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer func() { _ = s.Close(ctx) }()
	for i := 0; ; i++ {
		n := ccCount(ctx, t, d, `MATCH (t:Tmp) RETURN count(t) AS n`)
		if n == 0 {
			return
		}
		if i > 10_000 {
			t.Fatalf("%s: wipe Tmp made no progress, %d remain", name, n)
		}
		if _, err := ccExec(ctx, s, `MATCH (t:Tmp) WITH t LIMIT 5000 DELETE t`, nil); err != nil {
			t.Fatalf("%s: wipe Tmp: %v", name, err)
		}
	}
}

// ── the sweep ────────────────────────────────────────────────────────────────

type ccResult struct {
	Target  string
	Work    string
	Conc    int
	Ops     int64
	WarmOps int64
	Errors  int64
	BadRows int64
	Elapsed time.Duration
	P50     time.Duration
	P99     time.Duration
	// Lost is the write-path oracle: operations the client counted as
	// committed that the engine cannot afterwards produce. Any non-zero value
	// is a durability or visibility defect, not a performance result.
	Lost int64
}

func (r ccResult) OpsPerSec() float64 {
	if r.Elapsed == 0 {
		return 0
	}
	return float64(r.Ops) / r.Elapsed.Seconds()
}

// ccRunArm drives conc concurrent clients against one target for a fixed
// duration and reports aggregate throughput plus the latency distribution.
//
// The duration is fixed rather than the operation count, because a fixed op
// count turns a faster engine into a shorter run instead of a higher number,
// and because a saturated engine must be allowed to show its queueing.
func ccRunArm(ctx context.Context, d neo4j.DriverWithContext, target string, w ccWorkload, conc int) (ccResult, error) {
	cy := w.cypherFor(target)
	mode := neo4j.AccessModeRead
	if w.Write {
		mode = neo4j.AccessModeWrite
	}

	sessions := make([]neo4j.SessionWithContext, conc)
	for i := range sessions {
		sessions[i] = d.NewSession(ctx, neo4j.SessionConfig{AccessMode: mode})
	}
	defer func() {
		for _, s := range sessions {
			_ = s.Close(ctx)
		}
	}()

	// Warm-up: the same concurrent load, at the same concurrency, discarded.
	// It establishes every pooled connection, populates each engine's plan
	// cache and page cache, and gives a JIT compiler enough iterations to
	// reach compiled code before the timed window opens.
	warmOps, err := ccDrive(ctx, sessions, cy, w, ccWarmup, nil)
	if err != nil {
		return ccResult{}, fmt.Errorf("%s/%s/conc=%d: warm-up: %w", target, w.Key, conc, err)
	}

	tally := &ccTally{}
	elapsed, err := ccDriveTimed(ctx, sessions, cy, w, ccDuration, tally)
	if err != nil {
		return ccResult{}, err
	}

	all := make([]time.Duration, 0, 1<<16)
	for _, l := range tally.lat {
		all = append(all, l...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })

	res := ccResult{
		Target: target, Work: w.Key, Conc: conc,
		Ops: tally.ops.Load(), Errors: tally.errs.Load(), BadRows: tally.badRows.Load(),
		Elapsed: elapsed, WarmOps: warmOps,
	}
	if n := len(all); n > 0 {
		res.P50 = all[n*50/100]
		res.P99 = all[min(n*99/100, n-1)]
	}
	return res, nil
}

// ccTally collects what one driven window observed.
type ccTally struct {
	ops     atomic.Int64
	errs    atomic.Int64
	badRows atomic.Int64
	lat     [][]time.Duration
}

// ccDrive runs the workload on every session concurrently for d, and returns
// the number of successful operations. When tally is nil the window is a
// warm-up: nothing is recorded beyond the count, which the write-path oracle
// still needs in order to know how many nodes the warm-up itself created.
func ccDrive(ctx context.Context, sessions []neo4j.SessionWithContext, cy string, w ccWorkload, d time.Duration, tally *ccTally) (int64, error) {
	_, n, err := ccDriveInner(ctx, sessions, cy, w, d, tally)
	return n, err
}

func ccDriveTimed(ctx context.Context, sessions []neo4j.SessionWithContext, cy string, w ccWorkload, d time.Duration, tally *ccTally) (time.Duration, error) {
	el, _, err := ccDriveInner(ctx, sessions, cy, w, d, tally)
	return el, err
}

func ccDriveInner(ctx context.Context, sessions []neo4j.SessionWithContext, cy string, w ccWorkload, d time.Duration, tally *ccTally) (time.Duration, int64, error) {
	conc := len(sessions)
	if tally != nil {
		tally.lat = make([][]time.Duration, conc)
	}
	var done atomic.Int64
	var firstErr atomic.Pointer[error]

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func(client int, s neo4j.SessionWithContext) {
			defer wg.Done()
			var local []time.Duration
			if tally != nil {
				local = make([]time.Duration, 0, 4096)
			}
			<-start
			for op := 0; ; op++ {
				if runCtx.Err() != nil {
					break
				}
				t0 := time.Now()
				var rows int
				var err error
				if w.TxBatch > 0 {
					rows, err = ccExecInTx(runCtx, s, cy, w, client, op)
				} else {
					rows, err = ccExec(runCtx, s, cy, w.Params(client, op))
				}
				lat := time.Since(t0)
				if err != nil {
					if runCtx.Err() != nil {
						break
					}
					// A warm-up error is fatal: it means the arm cannot run at
					// all, and silently continuing would report a throughput
					// figure for a workload the engine is rejecting.
					if tally == nil {
						e := err
						firstErr.CompareAndSwap(nil, &e)
						break
					}
					tally.errs.Add(1)
					continue
				}
				// One iteration completes TxBatch queries when batching, and
				// throughput is counted in QUERIES so the batched and
				// autocommit arms are directly comparable. Latency is
				// likewise divided down to a per-query figure.
				perIter := int64(1)
				if w.TxBatch > 0 {
					perIter = int64(w.TxBatch)
					lat /= time.Duration(w.TxBatch)
				}
				if tally != nil {
					if rows != w.WantRows {
						tally.badRows.Add(perIter)
					}
					local = append(local, lat)
					tally.ops.Add(perIter)
				}
				done.Add(perIter)
			}
			if tally != nil {
				tally.lat[client] = local
			}
		}(i, sessions[i])
	}

	t0 := time.Now()
	close(start)
	timer := time.NewTimer(d)
	defer timer.Stop()
	<-timer.C
	cancel()
	wg.Wait()
	elapsed := time.Since(t0)

	if p := firstErr.Load(); p != nil {
		return elapsed, done.Load(), *p
	}
	return elapsed, done.Load(), nil
}

// TestDeleteBatchScaling measures how the cost of deleting a large label
// behaves as the deletion proceeds, on every engine.
//
// It exists because the concurrency sweep could not clear its own fixture: a
// single-statement delete of ~90 000 nodes exceeded GoGraph's 30 s transaction
// timeout, and the batched replacement then ran for minutes at exactly one core
// of the four it had, while Neo4j and Memgraph cleared the same set promptly.
//
// The hypothesis this tests is that deleted-but-not-yet-reclaimed versions stay
// visible to the label scan, so each successive batch re-walks everything
// already deleted and the whole wipe is quadratic. A flat per-batch curve
// refutes it; a rising one confirms it. Either way the answer is measured
// rather than inferred from a timeout.
func TestDeleteBatchScaling(t *testing.T) {
	ctx := context.Background()
	const total, batch = 40_000, 5_000

	for _, spec := range ccTargets() {
		d, err := ccNewDriver(ctx, spec, 8)
		if err != nil {
			t.Fatalf("connect %s: %v", spec.Name, err)
		}
		func() {
			defer func() { _ = d.Close(ctx) }()
			ccWipeAll(ctx, t, spec.Name, d)

			s := d.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
			defer func() { _ = s.Close(ctx) }()
			for lo := 0; lo < total; lo += batch {
				rows := make([]any, 0, batch)
				for i := lo; i < lo+batch && i < total; i++ {
					rows = append(rows, map[string]any{"k": fmt.Sprintf("k%d", i)})
				}
				if _, err := ccExec(ctx, s, `UNWIND $rows AS r CREATE (t:Tmp {k: r.k})`,
					map[string]any{"rows": rows}); err != nil {
					t.Fatalf("%s: seed: %v", spec.Name, err)
				}
			}
			if n := ccCount(ctx, t, d, `MATCH (t:Tmp) RETURN count(t) AS n`); n != total {
				t.Fatalf("%s: seeded %d, want %d", spec.Name, n, total)
			}

			ccDeleteCycle(ctx, t, spec, d, s, total, batch)
		}()
	}
}

// ccDeleteCycle seeds and wipes the label ccCycles times against the SAME live
// engine, because the flat per-batch curve within one wipe did not explain what
// was observed: the sweep stalled for minutes on a set whose measured per-node
// cost predicts seconds. The difference between the two is that the sweep had
// already run many create/delete cycles against that engine. If cost per cycle
// rises, the degradation is caused by churn — versions retained across cycles —
// rather than by set size.
var ccCycles = envInt("CC_CYCLES", 1)

func ccDeleteCycle(ctx context.Context, t *testing.T, spec ccTargetSpec, d neo4j.DriverWithContext, s neo4j.SessionWithContext, total, batch int) {
	t.Helper()
	for cycle := 1; cycle <= ccCycles; cycle++ {
		if cycle > 1 {
			for lo := 0; lo < total; lo += batch {
				rows := make([]any, 0, batch)
				for i := lo; i < lo+batch && i < total; i++ {
					rows = append(rows, map[string]any{"k": fmt.Sprintf("c%d-k%d", cycle, i)})
				}
				if _, err := ccExec(ctx, s, `UNWIND $rows AS r CREATE (t:Tmp {k: r.k})`,
					map[string]any{"rows": rows}); err != nil {
					t.Fatalf("%s: reseed cycle %d: %v", spec.Name, cycle, err)
				}
			}
		}
		var times []time.Duration
		var counts []time.Duration
		for i := 0; ; i++ {
			c0 := time.Now()
			n := ccCount(ctx, t, d, `MATCH (t:Tmp) RETURN count(t) AS n`)
			counts = append(counts, time.Since(c0))
			if n == 0 {
				break
			}
			if i > 100 {
				t.Fatalf("%s: no progress, %d remain", spec.Name, n)
			}
			t0 := time.Now()
			if _, err := ccExec(ctx, s, `MATCH (t:Tmp) WITH t LIMIT 5000 DELETE t`, nil); err != nil {
				t.Fatalf("%s: delete batch %d: %v", spec.Name, i, err)
			}
			times = append(times, time.Since(t0))
		}
		var sum, maxCount time.Duration
		for _, d := range times {
			sum += d
		}
		for _, c := range counts {
			maxCount = max(maxCount, c)
		}
		t.Logf("%-14s cycle %d/%d: wipe %s over %d batches of %d (slowest count-before %s)",
			spec.Name, cycle, ccCycles, sum.Round(time.Millisecond), len(times), batch,
			maxCount.Round(time.Millisecond))
		for i, d := range times {
			t.Logf("    %-14s c%d batch %2d delete %-12s count-before %s",
				spec.Name, cycle, i+1, d.Round(time.Millisecond), counts[i].Round(time.Millisecond))
		}
	}
}

func TestConcurrencySweep(t *testing.T) {
	ctx := context.Background()
	specs := ccTargets()
	if len(specs) == 0 {
		t.Skip("no targets selected")
	}

	maxConc := 0
	for _, c := range ccLevels {
		maxConc = max(maxConc, c)
	}

	type live struct {
		spec ccTargetSpec
		drv  neo4j.DriverWithContext
	}
	var targets []live
	for _, spec := range specs {
		d, err := ccNewDriver(ctx, spec, maxConc+8)
		if err != nil {
			t.Fatalf("connect %s: %v", spec.Name, err)
		}
		targets = append(targets, live{spec, d})
		t.Cleanup(func() { _ = d.Close(ctx) })
	}

	// Load, then prove every target holds the same graph. Comparing timings
	// across targets that hold different data is the classic way to publish a
	// confident number about nothing.
	nodeCounts := map[string]int64{}
	edgeCounts := map[string]int64{}
	for _, tg := range targets {
		d := ccLoad(ctx, t, tg.spec, tg.drv)
		nodeCounts[tg.spec.Name] = ccCount(ctx, t, tg.drv, `MATCH (n:Person) RETURN count(n) AS n`)
		edgeCounts[tg.spec.Name] = ccCount(ctx, t, tg.drv, `MATCH ()-[r:KNOWS]->() RETURN count(r) AS n`)
		t.Logf("loaded %-14s in %-10s nodes=%d edges=%d",
			tg.spec.Name, d.Round(time.Millisecond), nodeCounts[tg.spec.Name], edgeCounts[tg.spec.Name])
	}
	var refN, refE int64
	for _, tg := range targets {
		if refN == 0 {
			refN, refE = nodeCounts[tg.spec.Name], edgeCounts[tg.spec.Name]
			continue
		}
		if nodeCounts[tg.spec.Name] != refN || edgeCounts[tg.spec.Name] != refE {
			t.Fatalf("dataset mismatch: %s has %d/%d, reference has %d/%d",
				tg.spec.Name, nodeCounts[tg.spec.Name], edgeCounts[tg.spec.Name], refN, refE)
		}
	}
	if refN != int64(ccNodes) {
		t.Fatalf("loaded %d Person nodes, want %d", refN, ccNodes)
	}

	// One longer warm-up per target before anything is timed, so that the first
	// measured cell is not the one paying for JIT compilation and first-touch
	// page-cache population.
	if ccPrewarm > 0 {
		for _, tg := range targets {
			for _, w := range ccWorkloads() {
				s := []neo4j.SessionWithContext{tg.drv.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})}
				n, err := ccDrive(ctx, s, w.cypherFor(tg.spec.Name), w, ccPrewarm/time.Duration(len(ccWorkloads())), nil)
				_ = s[0].Close(ctx)
				if err != nil {
					t.Fatalf("pre-warm %s/%s: %v", tg.spec.Name, w.Key, err)
				}
				t.Logf("pre-warmed %-14s %-12s %d ops", tg.spec.Name, w.Key, n)
			}
			ccWipeTmp(ctx, t, tg.spec.Name, tg.drv)
		}
	}

	// Interleaved rounds: every (workload, concurrency) cell is measured once
	// per round for every target before any target is measured again, so that
	// host drift over the run lands on all three engines alike instead of
	// accumulating in whichever was measured last.
	byCell := map[string][]ccResult{}
	for round := 1; round <= ccRounds; round++ {
		for _, w := range ccWorkloads() {
			for _, conc := range ccLevels {
				for _, tg := range targets {
					r, err := ccRunArm(ctx, tg.drv, tg.spec.Name, w, conc)
					if err != nil {
						t.Errorf("round %d %s/%s/conc=%d: %v", round, tg.spec.Name, w.Key, conc, err)
						continue
					}
					if w.Write {
						// Every CREATE the client counted must be findable
						// afterwards, warm-up included. This is the only check
						// in the harness that can catch an engine reporting a
						// commit it did not perform — a throughput number for
						// dropped writes would otherwise look like a win.
						got := ccCount(ctx, t, tg.drv, `MATCH (t:Tmp) RETURN count(t) AS n`)
						want := r.Ops + r.WarmOps
						r.Lost = want - got
						// The client can only UNDERCOUNT: when the window
						// closes, an operation already on the wire may commit
						// after its caller stopped counting. That excess is
						// bounded by one in-flight operation per client per
						// window, and the two windows are warm-up and timed.
						//
						// The other direction has no benign explanation. If the
						// engine cannot produce a node whose CREATE it
						// acknowledged, it acknowledged a write it did not
						// perform, and no throughput figure from that arm means
						// anything.
						switch {
						case r.Lost > 0:
							t.Errorf("%s/%s/conc=%d: LOST WRITES — acknowledged %d (%d timed + %d warm-up), found %d",
								tg.spec.Name, w.Key, conc, want, r.Ops, r.WarmOps, got)
						case -r.Lost > int64(2*conc):
							t.Errorf("%s/%s/conc=%d: %d more nodes than acknowledged, above the %d in-flight bound",
								tg.spec.Name, w.Key, conc, -r.Lost, 2*conc)
						}
						// Restore the fixture: left in place, the sweep would
						// accumulate millions of nodes and every later arm
						// would run against a different, larger graph.
						ccWipeTmp(ctx, t, tg.spec.Name, tg.drv)
					}
					key := tg.spec.Name + "|" + w.Key + "|" + strconv.Itoa(conc)
					byCell[key] = append(byCell[key], r)
					t.Logf("r%d %-14s %-12s conc=%-4d %10.0f ops/s p50=%-10s p99=%-10s errs=%d badrows=%d lost=%d",
						round, tg.spec.Name, w.Key, conc, r.OpsPerSec(),
						r.P50.Round(time.Microsecond), r.P99.Round(time.Microsecond), r.Errors, r.BadRows, r.Lost)
				}
			}
		}
	}

	// Median across rounds per cell.
	median := func(rs []ccResult) ccResult {
		sort.Slice(rs, func(i, j int) bool { return rs[i].OpsPerSec() < rs[j].OpsPerSec() })
		return rs[len(rs)/2]
	}

	var out strings.Builder
	fmt.Fprintf(&out, "# Concurrency sweep — GoGraph vs Neo4j vs Memgraph\n\n")
	fmt.Fprintf(&out, "Dataset: %d Person nodes, %d KNOWS edges. Duration %s per arm, %d rounds, median reported.\n\n",
		refN, refE, ccDuration, ccRounds)

	for _, w := range ccWorkloads() {
		fmt.Fprintf(&out, "## %s — %s\n\n", w.Key, w.Desc)
		fmt.Fprintf(&out, "| target | conc | ops/s | scaling vs own conc=1 | p50 | p99 | errors | bad rows | lost writes |\n")
		fmt.Fprintf(&out, "|---|---:|---:|---:|---:|---:|---:|---:|---:|\n")
		for _, tg := range targets {
			base := math.NaN()
			for _, conc := range ccLevels {
				rs := byCell[tg.spec.Name+"|"+w.Key+"|"+strconv.Itoa(conc)]
				if len(rs) == 0 {
					continue
				}
				m := median(rs)
				if conc == ccLevels[0] {
					base = m.OpsPerSec()
				}
				scale := m.OpsPerSec() / base
				fmt.Fprintf(&out, "| %s | %d | %.0f | %.2fx | %s | %s | %d | %d | %d |\n",
					tg.spec.Name, conc, m.OpsPerSec(), scale,
					m.P50.Round(time.Microsecond), m.P99.Round(time.Microsecond), m.Errors, m.BadRows, m.Lost)
			}
		}
		fmt.Fprintf(&out, "\n")
	}

	t.Log("\n" + out.String())
	if path := os.Getenv("CC_OUT"); path != "" {
		if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
			t.Errorf("write %s: %v", path, err)
		} else {
			t.Logf("wrote %s", path)
		}
	}
}
