//go:build threeway

// Package comparison — three-way head-to-head harness.
//
// This file measures GoGraph against Neo4j and Memgraph on an identical
// dataset with identical query semantics. It is gated behind the `threeway`
// build tag because it requires two running containers and takes minutes.
//
// Prerequisites:
//
//	docker run -d --name gg-neo4j    -p 7687:7687 -e NEO4J_AUTH=neo4j/gographbench neo4j:5.26-community
//	docker run -d --name gg-memgraph -p 7688:7687 memgraph/memgraph:2.22.0 --telemetry-enabled=false
//
// Run with:
//
//	go test -tags=threeway -run TestThreeWay -v -timeout 60m ./bench/comparison/
//
// Four targets are measured so that transport cost is separated from engine
// cost:
//
//	gograph-embedded — in-process, no serialisation (GoGraph's actual mode)
//	gograph-bolt     — GoGraph behind its own Bolt server, same driver as below
//	neo4j-bolt       — Neo4j 5.26 Community in Docker
//	memgraph-bolt    — Memgraph 2.22 in Docker
//
// Comparing gograph-embedded to gograph-bolt gives the transport tax; comparing
// gograph-bolt to the other two is the apples-to-apples engine comparison.
package comparison

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/config"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ── dataset ──────────────────────────────────────────────────────────────────

const (
	twDegree  = 10    // average out-degree of KNOWS
	twBatch   = 5_000 // UNWIND batch size for loading
	twCities  = 20
	twWarmup  = 3
	twRepeats = 9 // odd, so the median is a real sample
)

// twNodes is the Person count. Overridable via THREEWAY_NODES so the harness
// can be smoke-tested cheaply before a full run.
var twNodes = envInt("THREEWAY_NODES", 50_000)

// twOnly, when set (comma-separated target names via THREEWAY_ONLY), restricts
// the run to those targets. Useful for filling in one column without repeating
// another target's expensive load — the dataset is deterministic, so results
// from separate runs of the same THREEWAY_NODES are directly comparable.
var twOnly = os.Getenv("THREEWAY_ONLY")

func targetEnabled(name string) bool {
	if twOnly == "" {
		return true
	}
	for _, want := range strings.Split(twOnly, ",") {
		if strings.TrimSpace(want) == name {
			return true
		}
	}
	return false
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return def
}

type person struct {
	ID   int64
	SID  string // string mirror of ID, used as the edge-loading join key
	Name string
	Age  int64
	City string
}

type edge struct{ S, T int64 }

type dataset struct {
	People []person
	Edges  []edge
}

// buildDataset generates a deterministic social graph. Every target loads
// exactly these rows, so any measured difference is the engine, not the data.
//
// Labels: every node is :Person. id%2==0 also gets :Employee (25 000 nodes).
// id%1000==0 also gets :Manager (50 nodes). The Employee/Manager intersection
// is 50 nodes out of a 25 000-node Employee set — the shape that exposes
// whether an engine intersects label sets or scans one and filters.
func buildDataset() *dataset {
	r := rand.New(rand.NewPCG(31, 1)) //nolint:gosec // deterministic seed
	ds := &dataset{
		People: make([]person, 0, twNodes),
		Edges:  make([]edge, 0, twNodes*twDegree),
	}
	for i := 0; i < twNodes; i++ {
		ds.People = append(ds.People, person{
			ID:   int64(i),
			SID:  fmt.Sprintf("sid%08d", i),
			Name: fmt.Sprintf("name%06d", i),
			Age:  int64(18 + r.IntN(60)),
			City: fmt.Sprintf("city%02d", r.IntN(twCities)),
		})
	}
	seen := make(map[[2]int64]struct{}, twNodes*twDegree)
	for i := 0; i < twNodes*twDegree; i++ {
		s := int64(r.IntN(twNodes))
		t := int64(r.IntN(twNodes))
		if s == t {
			continue
		}
		k := [2]int64{s, t}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		ds.Edges = append(ds.Edges, edge{S: s, T: t})
	}
	return ds
}

// Batches are []any rather than []map[string]any because GoGraph's
// cypher.BindParams accepts only []any for list parameters (see its docstring);
// the Neo4j driver accepts both, so []any is the common denominator and keeps
// every target on byte-identical parameter data.
func (ds *dataset) personBatches() [][]any {
	var out [][]any
	for i := 0; i < len(ds.People); i += twBatch {
		end := min(i+twBatch, len(ds.People))
		rows := make([]any, 0, end-i)
		for _, p := range ds.People[i:end] {
			rows = append(rows, map[string]any{
				"id": p.ID, "sid": p.SID, "name": p.Name, "age": p.Age, "city": p.City,
			})
		}
		out = append(out, rows)
	}
	return out
}

func (ds *dataset) edgeBatches() [][]any {
	var out [][]any
	for i := 0; i < len(ds.Edges); i += twBatch {
		end := min(i+twBatch, len(ds.Edges))
		rows := make([]any, 0, end-i)
		for _, e := range ds.Edges[i:end] {
			rows = append(rows, map[string]any{
				"s": e.S, "t": e.T,
				"ss": ds.People[e.S].SID, "ts": ds.People[e.T].SID,
			})
		}
		out = append(out, rows)
	}
	return out
}

// ── dialect ──────────────────────────────────────────────────────────────────

// dialect carries the statements that differ between engines. Everything not
// listed here is byte-identical across all four targets.
type dialect struct {
	Name string
	// CreateIndex returns the DDL for a single-property index.
	CreateIndex func(label, prop string) string
	// Skip lists query keys this engine cannot express.
	Skip map[string]string
	// Override replaces a query body for this engine (same semantics,
	// different syntax).
	Override map[string]string
}

var (
	neo4jDialect = dialect{
		Name:        "neo4j-bolt",
		CreateIndex: func(l, p string) string { return fmt.Sprintf("CREATE INDEX FOR (n:%s) ON (n.%s)", l, p) },
		Skip:        map[string]string{},
		Override:    map[string]string{},
	}
	memgraphDialect = dialect{
		Name:        "memgraph-bolt",
		CreateIndex: func(l, p string) string { return fmt.Sprintf("CREATE INDEX ON :%s(%s)", l, p) },
		Skip:        map[string]string{},
		Override: map[string]string{
			// Memgraph has no shortestPath() function; the BFS expansion is
			// its documented equivalent for an unweighted shortest path.
			"q13_shortest_path": `MATCH p = (a:Person {id: $x})-[:KNOWS *BFS ..6]-(b:Person {id: $y}) RETURN size(relationships(p)) AS len`,
		},
	}
	gographDialect = dialect{
		Name: "gograph",
		CreateIndex: func(l, p string) string {
			return fmt.Sprintf("CREATE INDEX idx_%s_%s FOR (n:%s) ON (n.%s)", l, p, l, p)
		},
		Skip:     map[string]string{},
		Override: map[string]string{},
	}
)

// ── query set ────────────────────────────────────────────────────────────────

type query struct {
	Key    string
	Desc   string
	Cypher string
	Params map[string]any
	Write  bool
}

func queries() []query {
	return []query{
		{
			Key: "q01_point_lookup", Desc: "Indexed point lookup",
			Cypher: `MATCH (p:Person {id: $id}) RETURN p.name AS n`,
			Params: map[string]any{"id": int64(12345)},
		},
		{
			Key: "q01b_point_lookup_str", Desc: "Point lookup on a STRING key (contrast with q01)",
			Cypher: `MATCH (p:Person {sid: $sid}) RETURN p.name AS n`,
			Params: map[string]any{"sid": "sid00012345"},
		},
		{
			Key: "q02_range_scan", Desc: "Indexed range scan + count",
			Cypher: `MATCH (p:Person) WHERE p.age >= $lo AND p.age < $hi RETURN count(p) AS c`,
			Params: map[string]any{"lo": int64(30), "hi": int64(35)},
		},
		{
			Key: "q03_starts_with", Desc: "STARTS WITH prefix (round-2 finding 1)",
			Cypher: `MATCH (p:Person) WHERE p.name STARTS WITH $pfx RETURN count(p) AS c`,
			Params: map[string]any{"pfx": "name0123"},
		},
		{
			Key: "q04_one_hop", Desc: "1-hop expand from a bound node",
			Cypher: `MATCH (p:Person {id: $id})-[:KNOWS]->(f) RETURN count(f) AS c`,
			Params: map[string]any{"id": int64(12345)},
		},
		{
			Key: "q05_two_hop", Desc: "2-hop friends-of-friends, DISTINCT",
			Cypher: `MATCH (p:Person {id: $id})-[:KNOWS]->()-[:KNOWS]->(f) RETURN count(DISTINCT f) AS c`,
			Params: map[string]any{"id": int64(12345)},
		},
		{
			Key: "q06_varlen_3", Desc: "Variable-length 1..3 DISTINCT",
			Cypher: `MATCH (p:Person {id: $id})-[:KNOWS*1..3]->(f) RETURN count(DISTINCT f) AS c`,
			Params: map[string]any{"id": int64(12345)},
		},
		{
			Key: "q07_global_count", Desc: "Global label count (count-store shape)",
			Cypher: `MATCH (p:Person) RETURN count(p) AS c`,
			Params: map[string]any{},
		},
		{
			Key: "q08_group_by", Desc: "Group-by + order + limit",
			Cypher: `MATCH (p:Person) RETURN p.city AS city, count(*) AS c ORDER BY c DESC LIMIT 10`,
			Params: map[string]any{},
		},
		{
			Key: "q09_top_k", Desc: "Top-k by unindexed property",
			Cypher: `MATCH (p:Person) RETURN p.id AS id ORDER BY p.age DESC LIMIT 10`,
			Params: map[string]any{},
		},
		{
			Key: "q10_triangle", Desc: "Cyclic 3-clique count (WCOJ shape)",
			Cypher: `MATCH (a:Person)-[:KNOWS]->(b:Person)-[:KNOWS]->(c:Person)-[:KNOWS]->(a) RETURN count(*) AS c`,
			Params: map[string]any{},
		},
		{
			Key: "q11_expand_into", Desc: "Both endpoints bound (ExpandInto shape)",
			Cypher: `MATCH (a:Person {id: $x})-[:KNOWS]->(b:Person {id: $y}) RETURN count(*) AS c`,
			Params: map[string]any{"x": int64(12345), "y": int64(23456)},
		},
		{
			Key: "q12_multi_label", Desc: "Multi-label conjunction (round-2 finding 2)",
			Cypher: `MATCH (p:Employee:Manager) RETURN count(p) AS c`,
			Params: map[string]any{},
		},
		{
			Key: "q13_shortest_path", Desc: "Unweighted shortest path <=6",
			Cypher: `MATCH (a:Person {id: $x}), (b:Person {id: $y}), p = shortestPath((a)-[:KNOWS*..6]-(b)) RETURN length(p) AS len`,
			Params: map[string]any{"x": int64(1), "y": int64(4242)},
		},
		{
			Key: "q14_property_filter", Desc: "Unindexed property equality scan",
			Cypher: `MATCH (p:Person) WHERE p.city = $c RETURN count(p) AS c`,
			Params: map[string]any{"c": "city07"},
		},
		{
			Key: "q15_create", Desc: "Single-node write", Write: true,
			Cypher: `CREATE (n:Scratch {v: $v}) RETURN n.v AS v`,
			Params: map[string]any{"v": int64(1)},
		},
	}
}

// ── targets ──────────────────────────────────────────────────────────────────

type target interface {
	Name() string
	Load(ctx context.Context, ds *dataset) (time.Duration, error)
	CreateIndexes(ctx context.Context) error
	Run(ctx context.Context, q query) (rows int, err error)
	Close(ctx context.Context)
}

// boltTarget drives any Bolt-speaking engine through the official driver.
type boltTarget struct {
	name    string
	dia     dialect
	driver  neo4j.DriverWithContext
	cleanup func()
}

func newBoltTarget(ctx context.Context, name, uri string, auth neo4j.AuthToken, dia dialect) (*boltTarget, error) {
	d, err := neo4j.NewDriverWithContext(uri, auth, func(c *config.Config) {
		c.MaxConnectionPoolSize = 8
		c.ConnectionAcquisitionTimeout = 30 * time.Second
		c.SocketConnectTimeout = 30 * time.Second
	})
	if err != nil {
		return nil, err
	}
	// Retry connectivity: containers may still be warming up.
	var lastErr error
	for i := 0; i < 30; i++ {
		if lastErr = d.VerifyConnectivity(ctx); lastErr == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if lastErr != nil {
		_ = d.Close(ctx)
		return nil, fmt.Errorf("%s: VerifyConnectivity: %w", name, lastErr)
	}
	return &boltTarget{name: name, dia: dia, driver: d}, nil
}

func (b *boltTarget) Name() string { return b.name }

func (b *boltTarget) exec(ctx context.Context, cy string, params map[string]any) (int, error) {
	s := b.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer func() { _ = s.Close(ctx) }()
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

// execIdempotentDDL runs index DDL and tolerates "already exists", which Neo4j
// raises when an index survives a MATCH (n) DETACH DELETE n wipe (deleting data
// does not drop schema).
func (b *boltTarget) execIdempotentDDL(ctx context.Context, ddl string) error {
	_, err := b.exec(ctx, ddl, nil)
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, benign := range []string{"EquivalentSchemaRuleAlreadyExists", "already exists", "AlreadyExists"} {
		if strings.Contains(msg, benign) {
			return nil
		}
	}
	return err
}

func (b *boltTarget) Load(ctx context.Context, ds *dataset) (time.Duration, error) {
	// Clean slate.
	if _, err := b.exec(ctx, `MATCH (n) DETACH DELETE n`, nil); err != nil {
		return 0, fmt.Errorf("%s: wipe: %w", b.name, err)
	}
	start := time.Now()
	for _, rows := range ds.personBatches() {
		if _, err := b.exec(ctx,
			`UNWIND $rows AS r CREATE (p:Person {id: r.id, sid: r.sid, name: r.name, age: r.age, city: r.city})`,
			map[string]any{"rows": rows}); err != nil {
			return 0, fmt.Errorf("%s: load persons: %w", b.name, err)
		}
	}
	// Secondary labels.
	if _, err := b.exec(ctx, `MATCH (p:Person) WHERE p.id % 2 = 0 SET p:Employee`, nil); err != nil {
		return 0, fmt.Errorf("%s: label Employee: %w", b.name, err)
	}
	if _, err := b.exec(ctx, `MATCH (p:Person) WHERE p.id % 1000 = 0 SET p:Manager`, nil); err != nil {
		return 0, fmt.Errorf("%s: label Manager: %w", b.name, err)
	}
	// Index before edge loading, or the MATCH per row is a full scan.
	// Both keys are indexed; edges join on the STRING key (sid) because
	// GoGraph's Cypher CREATE INDEX can only build string-keyed indexes
	// (see cypher/index_binding.go projectStringPropValue), so an integer
	// join key would force a full label scan per row on GoGraph alone and
	// make the load comparison measure that defect rather than the write path.
	for _, ddl := range []string{b.dia.CreateIndex("Person", "id"), b.dia.CreateIndex("Person", "sid")} {
		if err := b.execIdempotentDDL(ctx, ddl); err != nil {
			return 0, fmt.Errorf("%s: pre-index: %w", b.name, err)
		}
	}
	for _, rows := range ds.edgeBatches() {
		if _, err := b.exec(ctx,
			`UNWIND $rows AS r MATCH (a:Person {sid: r.ss}), (b:Person {sid: r.ts}) CREATE (a)-[:KNOWS]->(b)`,
			map[string]any{"rows": rows}); err != nil {
			return 0, fmt.Errorf("%s: load edges: %w", b.name, err)
		}
	}
	return time.Since(start), nil
}

func (b *boltTarget) CreateIndexes(ctx context.Context) error {
	for _, ddl := range []string{
		b.dia.CreateIndex("Person", "name"),
		b.dia.CreateIndex("Person", "age"),
	} {
		if err := b.execIdempotentDDL(ctx, ddl); err != nil {
			return fmt.Errorf("%s: %q: %w", b.name, ddl, err)
		}
	}
	return nil
}

func (b *boltTarget) Run(ctx context.Context, q query) (int, error) {
	cy := q.Cypher
	if ov, ok := b.dia.Override[q.Key]; ok {
		cy = ov
	}
	return b.exec(ctx, cy, q.Params)
}

func (b *boltTarget) Close(ctx context.Context) {
	_ = b.driver.Close(ctx)
	if b.cleanup != nil {
		b.cleanup()
	}
}

// embeddedTarget drives GoGraph in-process, with no serialisation at all.
type embeddedTarget struct {
	eng *cypher.Engine
}

func newEmbeddedTarget() *embeddedTarget {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	return &embeddedTarget{eng: cypher.NewEngine(g)}
}

func (e *embeddedTarget) Name() string { return "gograph-embedded" }

func (e *embeddedTarget) exec(ctx context.Context, cy string, params map[string]any) (int, error) {
	res, err := e.eng.RunInTxAny(ctx, cy, params)
	if err != nil {
		return 0, err
	}
	n := 0
	for res.Next() {
		n++
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		return 0, err
	}
	return n, res.Close()
}

func (e *embeddedTarget) execRead(ctx context.Context, cy string, params map[string]any) (int, error) {
	res, err := e.eng.RunAny(ctx, cy, params)
	if err != nil {
		return 0, err
	}
	n := 0
	for res.Next() {
		n++
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		return 0, err
	}
	return n, res.Close()
}

func (e *embeddedTarget) Load(ctx context.Context, ds *dataset) (time.Duration, error) {
	start := time.Now()
	for _, rows := range ds.personBatches() {
		if _, err := e.exec(ctx,
			`UNWIND $rows AS r CREATE (p:Person {id: r.id, sid: r.sid, name: r.name, age: r.age, city: r.city})`,
			map[string]any{"rows": rows}); err != nil {
			return 0, fmt.Errorf("embedded: load persons: %w", err)
		}
	}
	if _, err := e.exec(ctx, `MATCH (p:Person) WHERE p.id % 2 = 0 SET p:Employee`, nil); err != nil {
		return 0, fmt.Errorf("embedded: label Employee: %w", err)
	}
	if _, err := e.exec(ctx, `MATCH (p:Person) WHERE p.id % 1000 = 0 SET p:Manager`, nil); err != nil {
		return 0, fmt.Errorf("embedded: label Manager: %w", err)
	}
	for _, ddl := range []string{gographDialect.CreateIndex("Person", "id"), gographDialect.CreateIndex("Person", "sid")} {
		if _, err := e.exec(ctx, ddl, nil); err != nil {
			return 0, fmt.Errorf("embedded: pre-index: %w", err)
		}
	}
	for _, rows := range ds.edgeBatches() {
		if _, err := e.exec(ctx,
			`UNWIND $rows AS r MATCH (a:Person {sid: r.ss}), (b:Person {sid: r.ts}) CREATE (a)-[:KNOWS]->(b)`,
			map[string]any{"rows": rows}); err != nil {
			return 0, fmt.Errorf("embedded: load edges: %w", err)
		}
	}
	return time.Since(start), nil
}

func (e *embeddedTarget) CreateIndexes(ctx context.Context) error {
	for _, ddl := range []string{
		gographDialect.CreateIndex("Person", "name"),
		gographDialect.CreateIndex("Person", "age"),
	} {
		if _, err := e.exec(ctx, ddl, nil); err != nil {
			return fmt.Errorf("embedded: %q: %w", ddl, err)
		}
	}
	return nil
}

func (e *embeddedTarget) Run(ctx context.Context, q query) (int, error) {
	if q.Write {
		return e.exec(ctx, q.Cypher, q.Params)
	}
	return e.execRead(ctx, q.Cypher, q.Params)
}

func (e *embeddedTarget) Close(context.Context) {}

// newGoGraphBoltTarget starts an in-process Bolt server over a fresh engine so
// the same driver path used for Neo4j and Memgraph also measures GoGraph.
func newGoGraphBoltTarget(ctx context.Context) (*boltTarget, error) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	eng := cypher.NewEngine(g)

	srv, err := server.NewServer(eng, server.Options{
		MaxConnections: 32,
		ConnTimeout:    120 * time.Second,
		Auth:           server.NoAuthHandler{},
	})
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	srvCtx, srvCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(srvCtx, ln) }()
	time.Sleep(50 * time.Millisecond)

	bt, err := newBoltTarget(ctx, "gograph-bolt", "bolt://"+ln.Addr().String(), neo4j.NoAuth(), gographDialect)
	if err != nil {
		srvCancel()
		return nil, err
	}
	bt.cleanup = func() {
		shutCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
		srvCancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}
	return bt, nil
}

// ── measurement ──────────────────────────────────────────────────────────────

type sample struct {
	Target  string
	Query   string
	Median  time.Duration
	Min     time.Duration
	Samples int
	Rows    int
	Err     string
}

func measure(ctx context.Context, tg target, q query) sample {
	s := sample{Target: tg.Name(), Query: q.Key}

	// A single probe run sets the budget: a multi-second query must not
	// dominate the whole run, while cheap queries still get enough samples
	// for a stable median.
	probeStart := time.Now()
	rows, err := tg.Run(ctx, q)
	probe := time.Since(probeStart)
	if err != nil {
		s.Err = err.Error()
		return s
	}
	warmup, repeats := twWarmup, twRepeats
	switch {
	case probe > 5*time.Second:
		warmup, repeats = 0, 1
	case probe > 500*time.Millisecond:
		warmup, repeats = 0, 3
	case probe > 50*time.Millisecond:
		warmup, repeats = 1, 5
	}

	for i := 0; i < warmup; i++ {
		if _, err := tg.Run(ctx, q); err != nil {
			s.Err = err.Error()
			return s
		}
	}
	ds := make([]time.Duration, 0, repeats)
	for i := 0; i < repeats; i++ {
		start := time.Now()
		n, err := tg.Run(ctx, q)
		el := time.Since(start)
		if err != nil {
			s.Err = err.Error()
			return s
		}
		rows = n
		ds = append(ds, el)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	s.Median = ds[len(ds)/2]
	s.Min = ds[0]
	s.Samples = repeats
	s.Rows = rows
	return s
}

func TestThreeWay(t *testing.T) {
	ctx := context.Background()
	dsData := buildDataset()
	t.Logf("dataset: %d Person nodes, %d KNOWS edges (avg out-degree %.2f)",
		len(dsData.People), len(dsData.Edges), float64(len(dsData.Edges))/float64(len(dsData.People)))

	type built struct {
		tg       target
		loadTime time.Duration
	}
	var targets []built

	if targetEnabled("gograph-embedded") {
		emb := newEmbeddedTarget()
		lt, err := emb.Load(ctx, dsData)
		if err != nil {
			t.Fatalf("embedded load: %v", err)
		}
		if err := emb.CreateIndexes(ctx); err != nil {
			t.Fatalf("embedded indexes: %v", err)
		}
		targets = append(targets, built{emb, lt})
		t.Logf("gograph-embedded loaded in %v", lt)
	}

	if !targetEnabled("gograph-bolt") {
		t.Log("gograph-bolt disabled by THREEWAY_ONLY")
	} else if gb, err := newGoGraphBoltTarget(ctx); err != nil {
		t.Logf("SKIP gograph-bolt: %v", err)
	} else {
		defer gb.Close(ctx)
		lt, err := gb.Load(ctx, dsData)
		if err != nil {
			t.Logf("SKIP gograph-bolt: load: %v", err)
		} else if err := gb.CreateIndexes(ctx); err != nil {
			t.Logf("SKIP gograph-bolt: indexes: %v", err)
		} else {
			targets = append(targets, built{gb, lt})
			t.Logf("gograph-bolt loaded in %v", lt)
		}
	}

	remote := []struct {
		name string
		uri  string
		auth neo4j.AuthToken
		dia  dialect
	}{
		{"neo4j-bolt", "bolt://localhost:7687", neo4j.BasicAuth("neo4j", "gographbench", ""), neo4jDialect},
		{"memgraph-bolt", "bolt://localhost:7688", neo4j.NoAuth(), memgraphDialect},
	}
	for _, r := range remote {
		if !targetEnabled(r.name) {
			t.Logf("%s disabled by THREEWAY_ONLY", r.name)
			continue
		}
		bt, err := newBoltTarget(ctx, r.name, r.uri, r.auth, r.dia)
		if err != nil {
			t.Logf("SKIP %s: %v", r.name, err)
			continue
		}
		defer bt.Close(ctx)
		lt, err := bt.Load(ctx, dsData)
		if err != nil {
			t.Logf("SKIP %s: load: %v", r.name, err)
			continue
		}
		if err := bt.CreateIndexes(ctx); err != nil {
			t.Logf("%s: indexes: %v", r.name, err)
		}
		targets = append(targets, built{bt, lt})
		t.Logf("%s loaded in %v", r.name, lt)
	}

	if len(targets) < 1 {
		t.Fatalf("no targets available")
	}

	var out strings.Builder
	fmt.Fprintf(&out, "\n## Load time (%d nodes, %d edges, UNWIND batches of %d)\n\n",
		len(dsData.People), len(dsData.Edges), twBatch)
	fmt.Fprintf(&out, "| Target | Load |\n|---|---|\n")
	for _, b := range targets {
		fmt.Fprintf(&out, "| %s | %v |\n", b.tg.Name(), b.loadTime.Round(time.Millisecond))
	}

	results := map[string]map[string]sample{}
	for _, q := range queries() {
		results[q.Key] = map[string]sample{}
		for _, b := range targets {
			s := measure(ctx, b.tg, q)
			results[q.Key][b.tg.Name()] = s
			if s.Err != "" {
				t.Logf("%-16s %-20s ERROR %s", b.tg.Name(), q.Key, truncate(s.Err, 140))
			} else {
				t.Logf("%-16s %-20s median=%-12v rows=%d", b.tg.Name(), q.Key, s.Median, s.Rows)
			}
		}
	}

	fmt.Fprintf(&out, "\n## Query latency (median of %d, after %d warm-ups)\n\n", twRepeats, twWarmup)
	fmt.Fprintf(&out, "| Query | Description |")
	for _, b := range targets {
		fmt.Fprintf(&out, " %s |", b.tg.Name())
	}
	fmt.Fprintf(&out, "\n|---|---|")
	for range targets {
		fmt.Fprintf(&out, "---|")
	}
	fmt.Fprintln(&out)
	for _, q := range queries() {
		fmt.Fprintf(&out, "| `%s` | %s |", q.Key, q.Desc)
		for _, b := range targets {
			s := results[q.Key][b.tg.Name()]
			if s.Err != "" {
				fmt.Fprintf(&out, " ERR |")
			} else {
				fmt.Fprintf(&out, " %v |", s.Median.Round(time.Microsecond))
			}
		}
		fmt.Fprintln(&out)
	}

	fmt.Fprintf(&out, "\n## Row counts returned (semantic cross-check)\n\n")
	fmt.Fprintf(&out, "| Query |")
	for _, b := range targets {
		fmt.Fprintf(&out, " %s |", b.tg.Name())
	}
	fmt.Fprintf(&out, "\n|---|")
	for range targets {
		fmt.Fprintf(&out, "---|")
	}
	fmt.Fprintln(&out)
	for _, q := range queries() {
		fmt.Fprintf(&out, "| `%s` |", q.Key)
		for _, b := range targets {
			s := results[q.Key][b.tg.Name()]
			if s.Err != "" {
				fmt.Fprintf(&out, " ERR |")
			} else {
				fmt.Fprintf(&out, " %d |", s.Rows)
			}
		}
		fmt.Fprintln(&out)
	}

	fmt.Fprintf(&out, "\n## Errors\n\n")
	for _, q := range queries() {
		for _, b := range targets {
			s := results[q.Key][b.tg.Name()]
			if s.Err != "" {
				fmt.Fprintf(&out, "- `%s` on **%s**: %s\n", q.Key, b.tg.Name(), truncate(s.Err, 300))
			}
		}
	}

	t.Log(out.String())
	if path := os.Getenv("THREEWAY_OUT"); path != "" {
		if err := os.WriteFile(path, []byte(out.String()), 0o600); err != nil {
			t.Logf("write %s: %v", path, err)
		} else {
			t.Logf("report written to %s", path)
		}
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
