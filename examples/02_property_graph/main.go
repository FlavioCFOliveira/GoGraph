// Example 02_property_graph — build a labelled property graph (LPG) with an
// optional type schema, then run label- and property-indexed MATCH-style
// queries and read the typed properties back out.
//
// This example is the property-graph counterpart to the scale benchmark in
// example 26: it models a realistic employee directory at a configurable
// scale, drives it through the [graph/query] engine's index-backed
// predicates, and reports the evidence that matters for an LPG — build
// throughput, index-backed query latency, live heap and bytes per node — while
// pinning the deterministic shape with a regression test.
//
// # Model
//
//	(:Person {id, name, age, salary, active, dept})   // id is a stable "p<NNN>" key
//	(:Person:Manager {…})                             // a fraction of persons are managers
//	(:Org    {id, name, founded, revenue})            // id is a stable "o<NNN>" key
//	(:Person)-[:WORKS_AT]->(:Org)                      // every person works at one org
//
// Each :Person carries five typed properties spanning the four scalar kinds:
// a string name, an int64 age, a float64 salary, a bool active flag, and a
// string dept (one of a fixed set of departments). A configurable fraction of
// persons additionally carry the :Manager label. Each :Org carries a string
// name, an int64 founded year, and a float64 revenue. All values are drawn
// from a seeded RNG, so fixing -seed fixes the data shape — and therefore every
// indexed match count and every read-back value — exactly.
//
// # Schema
//
// An optional [schema.Schema] is declared (the labels and the typed property
// keys) and installed as the graph's validator, so every property write is
// type-checked at the boundary: writing a value whose kind disagrees with its
// declaration is rejected before it lands. The schema is the optional half of
// the LPG contract this example demonstrates; disable it with -schema=false to
// see the same data built without validation.
//
// # Queries
//
// The label index (Roaring bitmaps over labels) and the per-property value
// index back four representative point lookups, each reported as a
// deterministic match count plus a volatile latency line:
//
//	MATCH (p:Person)                            -> count of persons
//	MATCH (p:Person:Manager)                    -> count of managers (label intersection)
//	MATCH (p:Person {dept:'Engineering'})       -> count by string property
//	MATCH (p:Person {active:true})              -> count by bool property
//	MATCH (m:Manager)-->(o:Org)                 -> orgs reachable one hop from a manager
//
// For a fixed sample person the example then reads its five typed properties
// back out through [lpg.Graph.GetNodeProperty], demonstrating typed property
// RETRIEVAL — the other half of the round trip.
//
// # Scale
//
// Run with no flags the example builds a small, deterministic default
// (2000 persons, 50 orgs) that the regression test pins and that builds in
// well under a second. Every dimension is a flag, so the same binary scales up
// to a size where the heap and latency figures become interesting:
//
//	go run ./examples/02_property_graph -persons 2000000 -orgs 5000 -seed 7
//
// The deterministic data shape is reproducible for a fixed -seed; only the
// telemetry (lines prefixed with "# ") varies between runs and machines.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"runtime"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg/schema"
	"github.com/FlavioCFOliveira/GoGraph/graph/query"
)

// Node labels, relationship type, and property keys. Centralised so the model
// is described in exactly one place and a rename surfaces as a compile error
// everywhere it is used.
const (
	labelPerson  = "Person"
	labelManager = "Manager" // secondary label on a fraction of persons
	labelOrg     = "Org"

	relWorksAt = "WORKS_AT" // (:Person)-[:WORKS_AT]->(:Org)

	propID      = "id"      // string
	propName    = "name"    // string
	propAge     = "age"     // int64
	propSalary  = "salary"  // float64
	propActive  = "active"  // bool
	propDept    = "dept"    // string
	propFounded = "founded" // int64 (Org)
	propRevenue = "revenue" // float64 (Org)
)

// departments is the fixed set of department names a :Person may belong to.
// Index 0 ("Engineering") is the department the indexed string-property query
// counts, so its share of the population is deterministic for a fixed seed.
var departments = []string{
	"Engineering", "Sales", "Marketing", "Finance", "Support", "Operations",
}

// Fixed value ranges for the synthetic typed properties. These shape the
// *values* a node carries, not the *scale* of the dataset, so they are kept as
// constants rather than flags (the standard reserves flags for scale/shape
// dimensions). Drawing every value from these fixed ranges keeps the dataset
// reproducible for a given seed.
const (
	minAge    = 21      // minimum :Person age (inclusive)
	maxAge    = 65      // maximum :Person age (inclusive)
	minSalary = 35_000  // minimum :Person salary (inclusive)
	maxSalary = 180_000 // maximum :Person salary (exclusive)
)

// config captures every scale and shape knob of the example. The zero value is
// not valid; build one with defaultConfig and override fields from flags (see
// main) or construct one directly (see the regression test).
type config struct {
	persons    int   // number of :Person nodes
	orgs       int   // number of :Org nodes
	managerPct int   // percentage of persons that also carry the :Manager label (0..100)
	activePct  int   // percentage of persons whose active flag is true (0..100)
	seed       int64 // RNG seed; fixes the deterministic data shape
	schemaOn   bool  // install the type schema as a runtime validator
}

// defaultConfig returns the small, deterministic default the regression test
// pins: 2000 persons across 50 organisations, a fifth of them managers and
// three-quarters active. It builds in well under a second.
func defaultConfig() config {
	return config{
		persons:    2000,
		orgs:       50,
		managerPct: 20,
		activePct:  75,
		seed:       1,
		schemaOn:   true,
	}
}

// validate rejects a configuration that cannot produce the requested shape. It
// is checked once, at the boundary, before any work.
func (c config) validate() error {
	switch {
	case c.persons <= 0:
		return fmt.Errorf("persons must be > 0, got %d", c.persons)
	case c.orgs <= 0:
		return fmt.Errorf("orgs must be > 0, got %d", c.orgs)
	case c.managerPct < 0 || c.managerPct > 100:
		return fmt.Errorf("managerPct must be in [0,100], got %d", c.managerPct)
	case c.activePct < 0 || c.activePct > 100:
		return fmt.Errorf("activePct must be in [0,100], got %d", c.activePct)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.persons, "persons", cfg.persons, "number of Person nodes")
	flag.IntVar(&cfg.orgs, "orgs", cfg.orgs, "number of Org nodes")
	flag.IntVar(&cfg.managerPct, "manager-pct", cfg.managerPct, "percentage of persons that are managers (0..100)")
	flag.IntVar(&cfg.activePct, "active-pct", cfg.activePct, "percentage of persons that are active (0..100)")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")
	flag.BoolVar(&cfg.schemaOn, "schema", cfg.schemaOn, "install the type schema as a runtime validator")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run builds the employee directory described by cfg, declares its optional
// schema, runs the index-backed query battery, reads typed properties back,
// and writes a report to w. Bare lines carry deterministic facts (counts and
// read-back values, reproducible for a fixed seed); lines prefixed with "# "
// carry volatile telemetry (durations, throughput, heap figures) that vary per
// run and per machine. All output goes to w so a test can capture and assert
// on the deterministic lines.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.persons=%d\n", cfg.persons)
	fmt.Fprintf(w, "config.orgs=%d\n", cfg.orgs)
	fmt.Fprintf(w, "config.manager_pct=%d\n", cfg.managerPct)
	fmt.Fprintf(w, "config.active_pct=%d\n", cfg.activePct)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)
	fmt.Fprintf(w, "config.schema=%t\n", cfg.schemaOn)

	base := readMem()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})

	// Optional schema: declare the labels and the typed property keys, then
	// install it as the graph's validator so every property write is
	// type-checked at the boundary before it lands.
	if cfg.schemaOn {
		if err := installSchema(g); err != nil {
			return fmt.Errorf("schema: %w", err)
		}
	}

	stats, err := build(ctx, g, cfg)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}

	// This is a build-then-query workload: the graph is fully assembled above
	// and only read from here on. Compact right-sizes the adjacency backing
	// arrays so the resident-heap figures below reflect the tight arrays the
	// query phase runs against.
	if err := ctx.Err(); err != nil {
		return err
	}
	g.AdjList().Compact(ctx)

	fmt.Fprintf(w, "nodes.persons=%d\n", stats.persons)
	fmt.Fprintf(w, "nodes.orgs=%d\n", stats.orgs)
	fmt.Fprintf(w, "edges.works_at=%d\n", stats.worksAt)

	built := readMem()
	totalNodes := stats.persons + stats.orgs
	fmt.Fprintf(w, "# build.elapsed=%s\n", stats.elapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# build.node_rate=%.0f nodes/s\n", rate(totalNodes, stats.elapsed))
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(built.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(built.HeapAlloc-base.HeapAlloc))
	fmt.Fprintf(w, "# mem.total_alloc=%s\n", humanBytes(built.TotalAlloc-base.TotalAlloc))
	fmt.Fprintf(w, "# mem.num_gc=%d\n", built.NumGC-base.NumGC)
	fmt.Fprintf(w, "# bytes_per_node=%.1f\n",
		safeDiv(float64(built.HeapAlloc-base.HeapAlloc), float64(totalNodes)))

	// Freeze the live graph into the immutable CSR snapshot the query engine
	// reads for adjacency, then run the index-backed query battery.
	c := csr.BuildFromAdjList(g.AdjList())
	eng := query.New(g, c)
	if err := runQueries(ctx, eng, w); err != nil {
		return fmt.Errorf("queries: %w", err)
	}

	// Typed property read-back for a fixed sample person.
	if err := readBack(g, stats.samplePerson, w); err != nil {
		return fmt.Errorf("read-back: %w", err)
	}
	return nil
}

// installSchema declares the labels and the typed property keys of the model
// and installs the schema as the graph's runtime validator, so every property
// write is type-checked before it is applied.
func installSchema(g *lpg.Graph[string, int64]) error {
	s := schema.New(g.Registry(), g.PropertyKeys())
	s.RegisterLabel(labelPerson)
	s.RegisterLabel(labelManager)
	s.RegisterLabel(labelOrg)

	decls := []struct {
		key  string
		kind lpg.PropertyKind
	}{
		{propID, lpg.PropString},
		{propName, lpg.PropString},
		{propAge, lpg.PropInt64},
		{propSalary, lpg.PropFloat64},
		{propActive, lpg.PropBool},
		{propDept, lpg.PropString},
		{propFounded, lpg.PropInt64},
		{propRevenue, lpg.PropFloat64},
	}
	for _, d := range decls {
		if _, err := s.RegisterProperty(d.key, d.kind); err != nil {
			return fmt.Errorf("RegisterProperty %s: %w", d.key, err)
		}
	}
	g.SetValidator(s)
	return nil
}

// buildStats reports the realised shape of a build plus its wall-clock cost and
// a sample person to anchor the property read-back.
type buildStats struct {
	persons      int
	orgs         int
	worksAt      int
	samplePerson string // key of a fixed person for the read-back
	elapsed      time.Duration
}

// build materialises the directory described by cfg into g. It first creates
// the orgs (so WORKS_AT targets exist before the edges reference them), then
// the persons with their typed properties and labels, wiring each to a randomly
// chosen org. Keys are stable ("o<NNN>" / "p<NNN>") so the shape — and the
// sample person — is reproducible for a fixed seed. The build honours ctx
// cancellation on a periodic check.
func build(ctx context.Context, g *lpg.Graph[string, int64], cfg config) (buildStats, error) {
	//nolint:gosec // G404: a seeded math/rand is intentional here — the example
	// must reproduce a fixed dataset for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))
	start := time.Now()

	orgIDs := make([]string, cfg.orgs)
	for i := 0; i < cfg.orgs; i++ {
		id := fmt.Sprintf("o%d", i)
		orgIDs[i] = id
		if err := g.SetNodeLabel(id, labelOrg); err != nil {
			return buildStats{}, fmt.Errorf("SetNodeLabel Org on %s: %w", id, err)
		}
		props := map[string]lpg.PropertyValue{
			propID:      lpg.StringValue(id),
			propName:    lpg.StringValue(orgName(rng)),
			propFounded: lpg.Int64Value(int64(1950 + rng.Intn(75))),
			propRevenue: lpg.Float64Value(roundCents(1_000_000 + rng.Float64()*5_000_000_000)),
		}
		if err := setProps(g, id, props); err != nil {
			return buildStats{}, err
		}
	}

	worksAt := 0
	const ageSpan = maxAge - minAge + 1
	for i := 0; i < cfg.persons; i++ {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return buildStats{}, err
			}
		}
		id := fmt.Sprintf("p%d", i)
		if err := g.SetNodeLabel(id, labelPerson); err != nil {
			return buildStats{}, fmt.Errorf("SetNodeLabel Person on %s: %w", id, err)
		}
		// A fraction of persons additionally carry the :Manager label.
		if rng.Intn(100) < cfg.managerPct {
			if err := g.SetNodeLabel(id, labelManager); err != nil {
				return buildStats{}, fmt.Errorf("SetNodeLabel Manager on %s: %w", id, err)
			}
		}
		props := map[string]lpg.PropertyValue{
			propID:     lpg.StringValue(id),
			propName:   lpg.StringValue(personName(rng)),
			propAge:    lpg.Int64Value(int64(minAge + rng.Intn(ageSpan))),
			propSalary: lpg.Float64Value(roundCents(minSalary + rng.Float64()*(maxSalary-minSalary))),
			propActive: lpg.BoolValue(rng.Intn(100) < cfg.activePct),
			propDept:   lpg.StringValue(departments[rng.Intn(len(departments))]),
		}
		if err := setProps(g, id, props); err != nil {
			return buildStats{}, err
		}
		// Every person works at one randomly chosen org.
		if err := g.AddEdgeLabeled(id, orgIDs[rng.Intn(cfg.orgs)], 1, relWorksAt); err != nil {
			return buildStats{}, fmt.Errorf("AddEdgeLabeled %s-[WORKS_AT]: %w", id, err)
		}
		worksAt++
	}

	return buildStats{
		persons:      cfg.persons,
		orgs:         cfg.orgs,
		worksAt:      worksAt,
		samplePerson: "p0",
		elapsed:      time.Since(start),
	}, nil
}

// checkEvery bounds how often the build polls ctx for cancellation: often
// enough that a cancelled large build stops promptly, rare enough that the
// check is free relative to the surrounding work.
const checkEvery = 4096

// setProps writes a node's typed properties. When a schema validator is
// installed each write is type-checked before it lands, so a kind mismatch
// surfaces here as an error.
func setProps(g *lpg.Graph[string, int64], id string, props map[string]lpg.PropertyValue) error {
	for k, v := range props {
		if err := g.SetNodeProperty(id, k, v); err != nil {
			return fmt.Errorf("SetNodeProperty %s on %s: %w", k, id, err)
		}
	}
	return nil
}

// roundCents rounds a monetary amount to two decimal places so the float64
// values are clean and reproducible across platforms.
func roundCents(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// ─────────────────────────────────────────────────────────────────────────────
// Query battery
// ─────────────────────────────────────────────────────────────────────────────

// runQueries executes the index-backed point lookups against eng, printing one
// deterministic count line and one volatile latency line ("# ...") per query.
// Each count is produced by the label/property index (Roaring bitmaps over
// labels and per-property value indexes), exercising the indexed-lookup path
// rather than a full scan.
func runQueries(ctx context.Context, eng *query.Engine[string, int64], w io.Writer) error {
	// Label scan: count of persons.
	count(w, "persons", func() uint64 {
		return eng.Match().
			Vertex(query.WithLabel[string, int64](labelPerson)).
			Cardinality()
	})

	// Label intersection: persons that are also managers.
	count(w, "managers", func() uint64 {
		return eng.Match().
			Vertex(
				query.WithLabel[string, int64](labelPerson),
				query.WithLabel[string, int64](labelManager),
			).
			Cardinality()
	})

	// Indexed lookup by string property: persons in Engineering.
	count(w, "dept_engineering", func() uint64 {
		return eng.Match().
			Vertex(
				query.WithLabel[string, int64](labelPerson),
				query.WithProperty[string, int64](propDept, lpg.StringValue(departments[0])),
			).
			Cardinality()
	})

	// Indexed lookup by bool property: active persons.
	count(w, "active", func() uint64 {
		return eng.Match().
			Vertex(
				query.WithLabel[string, int64](labelPerson),
				query.WithProperty[string, int64](propActive, lpg.BoolValue(true)),
			).
			Cardinality()
	})

	// One-hop expansion from the label index: distinct orgs reachable from a
	// manager. WORKS_AT is the only Person->Org edge, so the out-neighbours of
	// the managers are exactly the orgs that employ at least one manager.
	count(w, "manager_orgs", func() uint64 {
		return eng.Match().
			Vertex(query.WithLabel[string, int64](labelManager)).
			Out().
			Cardinality()
	})

	return ctx.Err()
}

// count runs one indexed query, timing it, and prints the deterministic match
// count as a fact line and the volatile latency as a "# " telemetry line.
func count(w io.Writer, name string, fn func() uint64) {
	start := time.Now()
	n := fn()
	d := time.Since(start)
	fmt.Fprintf(w, "q.%s=%d\n", name, n)
	fmt.Fprintf(w, "# q.%s.latency=%s\n", name, d.Round(time.Nanosecond))
}

// ─────────────────────────────────────────────────────────────────────────────
// Typed property read-back
// ─────────────────────────────────────────────────────────────────────────────

// readBack fetches the five typed properties of the sample person back out of
// the graph through lpg.Graph.GetNodeProperty, demonstrating typed property
// RETRIEVAL — the read half of the round trip. Each value is printed as a
// deterministic fact line (reproducible for a fixed seed).
func readBack(g *lpg.Graph[string, int64], key string, w io.Writer) error {
	name, err := stringProp(g, key, propName)
	if err != nil {
		return err
	}
	age, err := int64Prop(g, key, propAge)
	if err != nil {
		return err
	}
	salary, err := float64Prop(g, key, propSalary)
	if err != nil {
		return err
	}
	active, err := boolProp(g, key, propActive)
	if err != nil {
		return err
	}
	dept, err := stringProp(g, key, propDept)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "sample.key=%s\n", key)
	fmt.Fprintf(w, "sample.name=%s\n", name)
	fmt.Fprintf(w, "sample.age=%d\n", age)
	fmt.Fprintf(w, "sample.salary=%.2f\n", salary)
	fmt.Fprintf(w, "sample.active=%t\n", active)
	fmt.Fprintf(w, "sample.dept=%s\n", dept)
	return nil
}

// stringProp reads a string-typed property off node key, returning an error if
// the property is missing or not a string — either of which would mean the
// graph does not match the schema this example declares.
func stringProp(g *lpg.Graph[string, int64], key, prop string) (string, error) {
	pv, ok := g.GetNodeProperty(key, prop)
	if !ok {
		return "", fmt.Errorf("node %q has no %q property", key, prop)
	}
	v, ok := pv.String()
	if !ok {
		return "", fmt.Errorf("property %q on node %q is not a string", prop, key)
	}
	return v, nil
}

// int64Prop reads an int64-typed property off node key.
func int64Prop(g *lpg.Graph[string, int64], key, prop string) (int64, error) {
	pv, ok := g.GetNodeProperty(key, prop)
	if !ok {
		return 0, fmt.Errorf("node %q has no %q property", key, prop)
	}
	v, ok := pv.Int64()
	if !ok {
		return 0, fmt.Errorf("property %q on node %q is not an int64", prop, key)
	}
	return v, nil
}

// float64Prop reads a float64-typed property off node key.
func float64Prop(g *lpg.Graph[string, int64], key, prop string) (float64, error) {
	pv, ok := g.GetNodeProperty(key, prop)
	if !ok {
		return 0, fmt.Errorf("node %q has no %q property", key, prop)
	}
	v, ok := pv.Float64()
	if !ok {
		return 0, fmt.Errorf("property %q on node %q is not a float64", prop, key)
	}
	return v, nil
}

// boolProp reads a bool-typed property off node key.
func boolProp(g *lpg.Graph[string, int64], key, prop string) (bool, error) {
	pv, ok := g.GetNodeProperty(key, prop)
	if !ok {
		return false, fmt.Errorf("node %q has no %q property", key, prop)
	}
	v, ok := pv.Bool()
	if !ok {
		return false, fmt.Errorf("property %q on node %q is not a bool", prop, key)
	}
	return v, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Telemetry helpers (mirrors example 26)
// ─────────────────────────────────────────────────────────────────────────────

// readMem returns a memory snapshot after forcing a GC so HeapAlloc reflects
// live (reachable) bytes rather than floating garbage.
func readMem() runtime.MemStats {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m
}

// rate returns count/elapsed in units per second, or 0 for a zero-length
// interval.
func rate(count int, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(count) / elapsed.Seconds()
}

// safeDiv divides a by b, returning 0 when b is 0.
func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// humanBytes formats a byte count with a binary (KiB/MiB/GiB) suffix.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ─────────────────────────────────────────────────────────────────────────────
// Realistic-data word lists. Fixed so the dataset is reproducible.
// ─────────────────────────────────────────────────────────────────────────────

// personName assembles a plausible "First Last" name from fixed word lists.
// Names are intentionally allowed to repeat — the unique key is the stable id,
// not the name, which mirrors reality.
func personName(rng *rand.Rand) string {
	return firstNames[rng.Intn(len(firstNames))] + " " + lastNames[rng.Intn(len(lastNames))]
}

// orgName assembles a plausible company name of the form "<Prefix> <Suffix>".
func orgName(rng *rand.Rand) string {
	return orgPrefixes[rng.Intn(len(orgPrefixes))] + " " + orgSuffixes[rng.Intn(len(orgSuffixes))]
}

var firstNames = []string{
	"Olivia", "Liam", "Emma", "Noah", "Ava", "Oliver", "Sophia", "Elijah",
	"Isabella", "James", "Mia", "Lucas", "Charlotte", "Mateo", "Amelia",
	"Ethan", "Harper", "Leo", "Evelyn", "Sebastian", "Abigail", "Daniel",
	"Emily", "Henry", "Ella", "Alexander", "Scarlett", "Jack", "Aria",
	"Benjamin", "Camila", "Theodore", "Luna", "Samuel", "Chloe", "David",
}

var lastNames = []string{
	"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller",
	"Davis", "Rodriguez", "Martinez", "Hernandez", "Lopez", "Gonzalez",
	"Wilson", "Anderson", "Thomas", "Taylor", "Moore", "Jackson", "Martin",
	"Lee", "Perez", "Thompson", "White", "Harris", "Sanchez", "Clark",
}

var orgPrefixes = []string{
	"Acme", "Globex", "Initech", "Umbrella", "Stark", "Wayne", "Hooli",
	"Vandelay", "Wonka", "Cyberdyne", "Tyrell", "Aperture", "Gekko", "Soylent",
}

var orgSuffixes = []string{
	"Industries", "Corp", "Labs", "Systems", "Holdings", "Group", "Partners",
	"Technologies", "Solutions", "Dynamics",
}
