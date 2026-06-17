package sim

import (
	"context"
	"fmt"
)

// maxSamplesPerKind bounds how many oracle nodes and oracle edges the checker
// probes against the engine on each invocation, keeping the per-tick cost
// O(maxSamplesPerKind) regardless of graph size. The sampled subset is chosen
// deterministically from the seed so a violation is reproducible.
const maxSamplesPerKind = 8

// ViolationKind classifies an invariant breach. The ACID_* kinds map to the
// module's four transactional guarantees; GRAPH_INTEGRITY covers structural
// invariants (e.g. an edge whose endpoints are absent); ORACLE_DEVIATION covers
// any disagreement between the shadow model and the engine that is not more
// specifically classified.
type ViolationKind string

// Violation kinds.
const (
	ViolationACIDAtomicity   ViolationKind = "ACID_ATOMICITY"
	ViolationACIDConsistency ViolationKind = "ACID_CONSISTENCY"
	ViolationACIDIsolation   ViolationKind = "ACID_ISOLATION"
	ViolationACIDDurability  ViolationKind = "ACID_DURABILITY"
	ViolationGraphIntegrity  ViolationKind = "GRAPH_INTEGRITY"
	ViolationOracleDeviation ViolationKind = "ORACLE_DEVIATION"
)

// Violation is a single detected invariant breach, tagged with its kind, a
// human-readable message, the tick at which it was found, and the operation
// that immediately preceded it.
type Violation struct {
	Kind    ViolationKind
	Message string
	Tick    int64
	Op      string
}

// String renders a Violation for a report.
func (v Violation) String() string {
	return fmt.Sprintf("[%s] tick=%d op=%q: %s", v.Kind, v.Tick, v.Op, v.Message)
}

// Result is the minimal row-iterator the checker needs from a query. It is a
// thin projection of the engine's real result type, exposing only forward
// iteration and a single scalar read, which is all the count and existence
// probes require.
//
// # Concurrency contract
//
// A Result is single-use and not safe for concurrent use; drive it from one
// goroutine and Close it when done.
type Result interface {
	// Next advances to the next row and reports whether one is available.
	Next() bool
	// ScalarInt returns the integer value of the first column of the current
	// row. It is only valid after a successful Next.
	ScalarInt() (int64, bool)
	// IntAt returns the integer value of column i of the current row. It is only
	// valid after a successful Next.
	IntAt(i int) (int64, bool)
	// StringAt returns the string value of column i of the current row. It is
	// only valid after a successful Next.
	StringAt(i int) (string, bool)
	// RowCount reports how many rows the result has produced so far via Next.
	RowCount() int
	// Err returns any error accumulated during iteration.
	Err() error
	// Close releases the result.
	Close() error
}

// Engine is the minimal surface the checker drives. The simulator supplies a
// thin adapter over the real cypher.Engine (see [EngineAdapter]).
//
// # Concurrency contract
//
// Implementations need only be safe for single-goroutine use; the simulator
// never calls them concurrently.
type Engine interface {
	// Run executes a Cypher query with string-keyed parameters and returns a
	// Result the caller must Close.
	Run(ctx context.Context, query string, params map[string]any) (Result, error)
	// NodeCount returns the number of live nodes in the engine.
	NodeCount() (int64, error)
	// EdgeCount returns the number of live edges in the engine.
	EdgeCount() (int64, error)
}

// InvariantChecker compares the engine against the oracle after operations and
// accumulates any [Violation] it finds. It samples a bounded, seed-driven
// subset of oracle state per call so its cost stays bounded on large graphs.
//
// # Concurrency contract
//
// InvariantChecker is NOT safe for concurrent use; it is driven from the single
// simulation goroutine.
type InvariantChecker struct {
	seed       *Seed
	violations []Violation
	// checksRun counts how many times [InvariantChecker.Check] has executed. It
	// makes the invariant-check cadence observable: under CheckEvery>1 it lets a
	// caller (and the cadence tests) confirm the checker ran the expected number
	// of times, including the simulator's terminal check.
	checksRun int
}

// NewInvariantChecker returns a checker whose sampling draws from seed.
func NewInvariantChecker(seed *Seed) *InvariantChecker {
	return &InvariantChecker{seed: seed}
}

// Check verifies the engine against the oracle at the given tick and returns
// any newly-found violations (also accumulated internally). It performs:
//
//   - node- and edge-count parity (oracle vs engine);
//   - sampled oracle-node existence in the engine (no missing nodes);
//   - sampled oracle-edge existence in the engine (no ghost or missing edges).
//
// Each check that fails appends a typed Violation; a clean pass returns nil.
func (c *InvariantChecker) Check(tick int64, oracle *GraphOracle, engine Engine) []Violation {
	c.checksRun++
	before := len(c.violations)

	c.checkNodeCount(tick, oracle, engine)
	c.checkEdgeCount(tick, oracle, engine)
	c.checkSampledNodes(tick, oracle, engine)
	c.checkSampledEdges(tick, oracle, engine)

	if len(c.violations) == before {
		return nil
	}
	out := make([]Violation, len(c.violations)-before)
	copy(out, c.violations[before:])
	return out
}

// CheckDurability verifies ACID Durability at a crash-recovery boundary: every
// operation the engine ACKed as committed before the crash (which the oracle
// models exactly, because [Simulator.applyToOracle] advances the oracle only on
// a committed write) must be present in the recovered engine, and nothing that
// was never committed may have leaked in as partial state. Unlike
// [InvariantChecker.Check] it scans the FULL oracle node and edge set, not a
// bounded sample, because a single dropped committed op is a durability
// violation that sampling could miss.
//
// It performs:
//
//   - exact node- and edge-count parity (a recovered count below the oracle's
//     means a committed op was lost — a Durability breach; a count above means
//     uncommitted state leaked in — an Atomicity breach at the crash boundary);
//   - full-scan oracle-node presence (every committed node survived recovery);
//   - full-scan oracle-edge presence (every committed edge survived recovery).
//
// Count mismatches are tagged [ViolationACIDDurability]; a missing node or edge
// is tagged [ViolationACIDDurability] (the committed datum did not survive).
// Each failing check appends a typed Violation; a clean pass returns nil.
func (c *InvariantChecker) CheckDurability(tick int64, oracle *GraphOracle, engine Engine) []Violation {
	before := len(c.violations)

	c.checkDurableCounts(tick, oracle, engine)
	c.checkAllNodesDurable(tick, oracle, engine)
	c.checkAllEdgesDurable(tick, oracle, engine)

	if len(c.violations) == before {
		return nil
	}
	out := make([]Violation, len(c.violations)-before)
	copy(out, c.violations[before:])
	return out
}

// checkDurableCounts asserts exact count parity at the crash boundary, tagging a
// shortfall as a durability loss and a surplus as a crash-boundary atomicity
// breach (uncommitted state leaked in).
func (c *InvariantChecker) checkDurableCounts(tick int64, oracle *GraphOracle, engine Engine) {
	gotN, err := engine.NodeCount()
	if err != nil {
		c.add(ViolationOracleDeviation, tick, "durable node count", fmt.Sprintf("engine.NodeCount failed: %v", err))
	} else if wantN := int64(oracle.NodeCount()); gotN != wantN {
		kind := ViolationACIDDurability
		if gotN > wantN {
			kind = ViolationACIDAtomicity
		}
		c.add(kind, tick, "durable node count",
			fmt.Sprintf("post-recovery node-count mismatch: committed(oracle)=%d recovered(engine)=%d", wantN, gotN))
	}

	gotE, err := engine.EdgeCount()
	if err != nil {
		c.add(ViolationOracleDeviation, tick, "durable edge count", fmt.Sprintf("engine.EdgeCount failed: %v", err))
	} else if wantE := int64(oracle.EdgeCount()); gotE != wantE {
		kind := ViolationACIDDurability
		if gotE > wantE {
			kind = ViolationACIDAtomicity
		}
		c.add(kind, tick, "durable edge count",
			fmt.Sprintf("post-recovery edge-count mismatch: committed(oracle)=%d recovered(engine)=%d", wantE, gotE))
	}
}

// checkAllNodesDurable verifies every modelled (committed) node survived
// recovery, scanning the full oracle node set rather than a sample.
func (c *InvariantChecker) checkAllNodesDurable(tick int64, oracle *GraphOracle, engine Engine) {
	for _, name := range oracle.NodeNames() {
		n, err := c.countQuery(engine,
			"MATCH (n:Person {name:$name}) RETURN count(n)",
			map[string]any{"name": name})
		if err != nil {
			c.add(ViolationOracleDeviation, tick, "durable node existence", fmt.Sprintf("probe %q failed: %v", name, err))
			continue
		}
		if n == 0 {
			c.add(ViolationACIDDurability, tick, "durable node existence",
				fmt.Sprintf("committed node name=%q did not survive recovery", name))
		}
	}
}

// checkAllEdgesDurable verifies every modelled (committed) edge survived
// recovery, scanning the full oracle edge set rather than a sample.
func (c *InvariantChecker) checkAllEdgesDurable(tick int64, oracle *GraphOracle, engine Engine) {
	for _, e := range oracle.edgeStates() {
		src := oracle.nameOf(e.SrcID)
		dst := oracle.nameOf(e.DstID)
		if src == "" || dst == "" {
			c.add(ViolationGraphIntegrity, tick, "durable edge endpoint",
				fmt.Sprintf("committed edge %d-[%s]->%d has a missing endpoint", e.SrcID, e.Label, e.DstID))
			continue
		}
		n, err := c.countQuery(engine,
			"MATCH (a:Person {name:$a})-[r:KNOWS]->(b:Person {name:$b}) RETURN count(r)",
			map[string]any{"a": src, "b": dst})
		if err != nil {
			c.add(ViolationOracleDeviation, tick, "durable edge existence", fmt.Sprintf("probe %s->%s failed: %v", src, dst, err))
			continue
		}
		if n == 0 {
			c.add(ViolationACIDDurability, tick, "durable edge existence",
				fmt.Sprintf("committed edge %s-[KNOWS]->%s did not survive recovery", src, dst))
		}
	}
}

// checkNodeCount compares the modelled node count with the engine's.
func (c *InvariantChecker) checkNodeCount(tick int64, oracle *GraphOracle, engine Engine) {
	got, err := engine.NodeCount()
	if err != nil {
		c.add(ViolationOracleDeviation, tick, "node count", fmt.Sprintf("engine.NodeCount failed: %v", err))
		return
	}
	if want := int64(oracle.NodeCount()); got != want {
		c.add(ViolationACIDConsistency, tick, "node count",
			fmt.Sprintf("node-count mismatch: oracle=%d engine=%d", want, got))
	}
}

// checkEdgeCount compares the modelled edge count with the engine's.
func (c *InvariantChecker) checkEdgeCount(tick int64, oracle *GraphOracle, engine Engine) {
	got, err := engine.EdgeCount()
	if err != nil {
		c.add(ViolationOracleDeviation, tick, "edge count", fmt.Sprintf("engine.EdgeCount failed: %v", err))
		return
	}
	if want := int64(oracle.EdgeCount()); got != want {
		c.add(ViolationACIDConsistency, tick, "edge count",
			fmt.Sprintf("edge-count mismatch: oracle=%d engine=%d", want, got))
	}
}

// checkSampledNodes verifies that a bounded, seed-chosen sample of oracle nodes
// exists in the engine (probed by the Person name key).
func (c *InvariantChecker) checkSampledNodes(tick int64, oracle *GraphOracle, engine Engine) {
	names := c.sample(oracle.NodeNames())
	for _, name := range names {
		n, err := c.countQuery(engine,
			"MATCH (n:Person {name:$name}) RETURN count(n)",
			map[string]any{"name": name})
		if err != nil {
			c.add(ViolationOracleDeviation, tick, "node existence", fmt.Sprintf("probe %q failed: %v", name, err))
			continue
		}
		if n == 0 {
			c.add(ViolationGraphIntegrity, tick, "node existence",
				fmt.Sprintf("oracle node name=%q absent in engine", name))
		}
	}
}

// checkSampledEdges verifies that a bounded, seed-chosen sample of oracle edges
// exists in the engine (probed by endpoint Person names), catching both missing
// and (via count parity above) ghost edges.
func (c *InvariantChecker) checkSampledEdges(tick int64, oracle *GraphOracle, engine Engine) {
	edges := oracle.edgeStates()
	// Sample indices deterministically.
	idxs := c.sampleIndices(len(edges))
	for _, i := range idxs {
		e := edges[i]
		src := oracle.nameOf(e.SrcID)
		dst := oracle.nameOf(e.DstID)
		if src == "" || dst == "" {
			c.add(ViolationGraphIntegrity, tick, "edge endpoint",
				fmt.Sprintf("oracle edge %d-[%s]->%d has a missing endpoint", e.SrcID, e.Label, e.DstID))
			continue
		}
		n, err := c.countQuery(engine,
			"MATCH (a:Person {name:$a})-[r:KNOWS]->(b:Person {name:$b}) RETURN count(r)",
			map[string]any{"a": src, "b": dst})
		if err != nil {
			c.add(ViolationOracleDeviation, tick, "edge existence", fmt.Sprintf("probe %s->%s failed: %v", src, dst, err))
			continue
		}
		if n == 0 {
			c.add(ViolationGraphIntegrity, tick, "edge existence",
				fmt.Sprintf("oracle edge %s-[KNOWS]->%s absent in engine", src, dst))
		}
	}
}

// countQuery runs a single-scalar count query and returns the integer.
func (c *InvariantChecker) countQuery(engine Engine, query string, params map[string]any) (int64, error) {
	res, err := engine.Run(context.Background(), query, params)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Close() }()
	var n int64
	if res.Next() {
		if v, ok := res.ScalarInt(); ok {
			n = v
		}
	}
	if err := res.Err(); err != nil {
		return 0, err
	}
	return n, nil
}

// sample returns a bounded, deterministically-shuffled prefix of items.
func (c *InvariantChecker) sample(items []string) []string {
	if len(items) <= maxSamplesPerKind {
		return items
	}
	shuffled := c.seed.Shuffle(items)
	return shuffled[:maxSamplesPerKind]
}

// sampleIndices returns up to maxSamplesPerKind distinct indices in [0,n),
// chosen deterministically.
func (c *InvariantChecker) sampleIndices(n int) []int {
	if n == 0 {
		return nil
	}
	all := make([]int, n)
	for i := range all {
		all[i] = i
	}
	// Fisher–Yates on the index slice via the seed, then take a prefix.
	for i := n - 1; i > 0; i-- {
		j := c.seed.IntN(i + 1)
		all[i], all[j] = all[j], all[i]
	}
	if n > maxSamplesPerKind {
		return all[:maxSamplesPerKind]
	}
	return all
}

// add appends a violation.
func (c *InvariantChecker) add(kind ViolationKind, tick int64, op, msg string) {
	c.violations = append(c.violations, Violation{Kind: kind, Tick: tick, Op: op, Message: msg})
}

// IndexSpec declares one secondary index the simulator created during a run, by
// the (Label, Property) it covers. The index-consistency checker cross-checks
// each declared spec against the engine's base data. Specs are declared by the
// scenario (the simulator does not introspect (label, property) from the engine
// index manager, which exposes only opaque names), so the registry of specs is
// the authoritative set the checker walks.
type IndexSpec struct {
	// Label is the node label the index is declared on (e.g. "Person").
	Label string
	// Property is the property key the index covers (e.g. "name").
	Property string
}

// indexConsistencyEngine is the slice of the [Engine] surface
// [CheckIndexConsistency] needs: the generic Run (to drive both the full-scan
// and the index-seek probe queries). The simulator's [EngineAdapter] satisfies
// it.
type indexConsistencyEngine = Engine

// CheckIndexConsistency performs a THOROUGH (not sampled) index-vs-base-data
// consistency check for every declared index in specs. For each index on
// (Label, Property) it:
//
//   - full-scans the base data via the engine
//     (MATCH (n:Label) RETURN id(n), n.Property) and builds the authoritative
//     value -> {node id} map directly from the nodes that carry the property;
//   - for every distinct indexed value, runs the index-seek probe
//     (MATCH (n:Label {Property:$v}) RETURN id(n)) — which the engine resolves
//     through the index when one is present — and asserts the seek returns
//     EXACTLY the node ids the full scan attributed to that value.
//
// A value the seek over-reports (a node id the full scan does not carry) is a
// torn/orphaned index entry; a value the seek under-reports (a node id the full
// scan carries but the seek misses) is a stale/lost index entry. Either is a
// [ViolationACIDConsistency] (the index disagrees with the base data it
// indexes). The check is bounded but exhaustive over the CURRENT graph: it
// walks every node of each indexed label exactly once for the scan and issues
// one seek per distinct value.
//
// The check probes the engine through the same execution path the workload
// uses, so it observes whatever the engine would serve a real query — which is
// precisely the property an index must preserve. It cross-checks the engine
// against itself (seek path vs scan path) rather than against the oracle,
// because an index covers DDL-created labels the minimal Phase-1 oracle does
// not model.
func CheckIndexConsistency(tick int64, _ *GraphOracle, engine indexConsistencyEngine, specs ...IndexSpec) []Violation {
	c := &InvariantChecker{}
	for _, spec := range specs {
		c.checkOneIndex(tick, engine, spec)
	}
	return c.violations
}

// checkOneIndex cross-checks a single (Label, Property) index against the base
// data, appending a violation per divergence found.
func (c *InvariantChecker) checkOneIndex(tick int64, engine indexConsistencyEngine, spec IndexSpec) {
	// Authoritative value -> node-id set, built from a full label scan.
	want, err := c.scanByValue(engine, spec)
	if err != nil {
		c.add(ViolationOracleDeviation, tick, "index scan",
			fmt.Sprintf("full scan of (:%s).%s failed: %v", spec.Label, spec.Property, err))
		return
	}
	for value, wantIDs := range want {
		gotIDs, err := c.seekByValue(engine, spec, value)
		if err != nil {
			c.add(ViolationOracleDeviation, tick, "index seek",
				fmt.Sprintf("index seek (:%s {%s:%q}) failed: %v", spec.Label, spec.Property, value, err))
			continue
		}
		c.diffIDSets(tick, spec, value, wantIDs, gotIDs)
	}
}

// diffIDSets compares the full-scan node-id set (want) with the index-seek set
// (got) for one indexed value and records a violation for every divergence.
func (c *InvariantChecker) diffIDSets(tick int64, spec IndexSpec, value string, want, got map[int64]struct{}) {
	for id := range want {
		if _, ok := got[id]; !ok {
			c.add(ViolationACIDConsistency, tick, "index consistency",
				fmt.Sprintf("STALE/LOST index entry: node %d carries (:%s).%s=%q but the index seek misses it",
					id, spec.Label, spec.Property, value))
		}
	}
	for id := range got {
		if _, ok := want[id]; !ok {
			c.add(ViolationACIDConsistency, tick, "index consistency",
				fmt.Sprintf("TORN/ORPHANED index entry: index seek for (:%s).%s=%q returns node %d which does not carry it",
					spec.Label, spec.Property, value, id))
		}
	}
}

// scanByValue full-scans the indexed label and groups node ids by the indexed
// property value, skipping nodes that do not carry the property (a null value).
// The returned map is the authoritative model the index must match.
func (c *InvariantChecker) scanByValue(engine indexConsistencyEngine, spec IndexSpec) (map[string]map[int64]struct{}, error) {
	query := fmt.Sprintf("MATCH (n:%s) WHERE n.%s IS NOT NULL RETURN id(n), n.%s",
		spec.Label, spec.Property, spec.Property)
	res, err := engine.Run(context.Background(), query, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()

	out := make(map[string]map[int64]struct{})
	for res.Next() {
		id, okID := res.IntAt(0)
		val, okVal := res.StringAt(1)
		if !okID || !okVal {
			// The index-consistency scenarios index string-valued properties only;
			// a non-string value is not part of this check's contract.
			continue
		}
		ids := out[val]
		if ids == nil {
			ids = make(map[int64]struct{})
			out[val] = ids
		}
		ids[id] = struct{}{}
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// seekByValue runs the index-seek probe for one value and returns the node-id
// set the engine resolves — which flows through the index when one is present.
func (c *InvariantChecker) seekByValue(engine indexConsistencyEngine, spec IndexSpec, value string) (map[int64]struct{}, error) {
	query := fmt.Sprintf("MATCH (n:%s {%s:$v}) RETURN id(n)", spec.Label, spec.Property)
	res, err := engine.Run(context.Background(), query, map[string]any{"v": value})
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()

	out := make(map[int64]struct{})
	for res.Next() {
		if id, ok := res.ScalarInt(); ok {
			out[id] = struct{}{}
		}
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ChecksRun reports how many times [InvariantChecker.Check] has executed since
// construction. It exposes the realised invariant-check cadence so callers can
// confirm, for a given CheckEvery, that the expected number of checks ran
// (including the simulator's terminal check).
func (c *InvariantChecker) ChecksRun() int { return c.checksRun }

// HasViolations reports whether any violation has been recorded.
func (c *InvariantChecker) HasViolations() bool { return len(c.violations) > 0 }

// Violations returns all recorded violations. The returned slice aliases the
// checker's backing store and must not be mutated.
func (c *InvariantChecker) Violations() []Violation { return c.violations }
