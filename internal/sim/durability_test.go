package sim

import (
	"context"
	"os"
	"testing"
)

// fakeDurabilityEngine is a minimal [Engine] that lets a test inject a precise
// divergence from the oracle: a deliberately wrong node/edge count and a set of
// node/edge probes that report absent. It is used to prove [CheckDurability]
// actually flags an injected durability violation rather than only passing on a
// faithful engine.
type fakeDurabilityEngine struct {
	nodes int64
	edges int64
	// absentNodes/absentEdges name probes that should report zero rows (the
	// committed datum did not survive). Any probe not listed reports one row.
	absentNodes map[string]bool
	absentEdges map[string]bool // key "a->b"
}

func (e *fakeDurabilityEngine) NodeCount() (int64, error) { return e.nodes, nil }
func (e *fakeDurabilityEngine) EdgeCount() (int64, error) { return e.edges, nil }

func (e *fakeDurabilityEngine) Run(_ context.Context, query string, params map[string]any) (Result, error) {
	// The durability checker issues node-existence and edge-existence count
	// probes. Decide the row count from the injected absent sets.
	count := int64(1)
	switch {
	case params["name"] != nil:
		if e.absentNodes[params["name"].(string)] {
			count = 0
		}
	case params["a"] != nil && params["b"] != nil:
		key := params["a"].(string) + "->" + params["b"].(string)
		if e.absentEdges[key] {
			count = 0
		}
	}
	_ = query
	return &fakeCountResult{n: count}, nil
}

// fakeCountResult is a single-row scalar-count result.
type fakeCountResult struct {
	n    int64
	done bool
}

func (r *fakeCountResult) Next() bool {
	if r.done {
		return false
	}
	r.done = true
	return true
}
func (r *fakeCountResult) ScalarInt() (int64, bool) { return r.n, true }
func (r *fakeCountResult) RowCount() int            { return 1 }
func (r *fakeCountResult) Err() error               { return nil }
func (r *fakeCountResult) Close() error             { return nil }

// oracleWithNodesAndEdge builds an oracle with two named Person nodes and a
// KNOWS edge between them, returning it plus the two names.
func oracleWithNodesAndEdge(t *testing.T) (*GraphOracle, string, string) {
	t.Helper()
	o := NewGraphOracle()
	o.ApplyCreate(tmplCreatePerson, map[string]any{"name": "alice", "age": int64(1)})
	o.ApplyCreate(tmplCreatePerson, map[string]any{"name": "bob", "age": int64(2)})
	o.ApplyCreate(tmplCreateKnows, map[string]any{"a": "alice", "b": "bob"})
	if o.NodeCount() != 2 || o.EdgeCount() != 1 {
		t.Fatalf("oracle setup wrong: nodes=%d edges=%d", o.NodeCount(), o.EdgeCount())
	}
	return o, "alice", "bob"
}

// TestCheckDurability_CleanRunNoViolations verifies the durability check passes
// when the engine faithfully reflects every committed op.
func TestCheckDurability_CleanRunNoViolations(t *testing.T) {
	o, _, _ := oracleWithNodesAndEdge(t)
	eng := &fakeDurabilityEngine{nodes: 2, edges: 1}
	chk := NewInvariantChecker(NewSeed(1))

	if v := chk.CheckDurability(1, o, eng); v != nil {
		t.Fatalf("expected clean durability check, got %v", v)
	}
	if chk.HasViolations() {
		t.Fatalf("checker recorded violations on a clean run: %v", chk.Violations())
	}
}

// TestCheckDurability_DroppedCommittedNode verifies the check flags a committed
// node that did not survive recovery as an ACID_DURABILITY violation — both via
// the count shortfall and the full-scan node probe.
func TestCheckDurability_DroppedCommittedNode(t *testing.T) {
	o, alice, _ := oracleWithNodesAndEdge(t)
	// Engine lost one node: count is 1 (not 2) and alice's probe is absent.
	eng := &fakeDurabilityEngine{
		nodes:       1,
		edges:       1,
		absentNodes: map[string]bool{alice: true},
	}
	chk := NewInvariantChecker(NewSeed(1))

	v := chk.CheckDurability(1, o, eng)
	if !hasKind(v, ViolationACIDDurability) {
		t.Fatalf("expected ACID_DURABILITY for a dropped committed node, got %v", v)
	}
}

// TestCheckDurability_DroppedCommittedEdge verifies the check flags a committed
// edge that did not survive recovery as an ACID_DURABILITY violation.
func TestCheckDurability_DroppedCommittedEdge(t *testing.T) {
	o, alice, bob := oracleWithNodesAndEdge(t)
	eng := &fakeDurabilityEngine{
		nodes:       2,
		edges:       0, // edge lost.
		absentEdges: map[string]bool{alice + "->" + bob: true},
	}
	chk := NewInvariantChecker(NewSeed(1))

	v := chk.CheckDurability(1, o, eng)
	if !hasKind(v, ViolationACIDDurability) {
		t.Fatalf("expected ACID_DURABILITY for a dropped committed edge, got %v", v)
	}
}

// TestCheckDurability_LeakedUncommittedState verifies that a surplus at the
// crash boundary — the engine holding MORE than the committed set — is flagged
// as an ACID_ATOMICITY violation (uncommitted/torn state leaked in).
func TestCheckDurability_LeakedUncommittedState(t *testing.T) {
	o, _, _ := oracleWithNodesAndEdge(t)
	eng := &fakeDurabilityEngine{nodes: 3, edges: 2} // one extra node and edge.
	chk := NewInvariantChecker(NewSeed(1))

	v := chk.CheckDurability(1, o, eng)
	if !hasKind(v, ViolationACIDAtomicity) {
		t.Fatalf("expected ACID_ATOMICITY for leaked uncommitted state, got %v", v)
	}
}

// TestCheckDurability_FullScanNotSampled verifies the durability check scans the
// FULL oracle set: a single dropped node among many is detected even when it
// would fall outside the bounded sample the per-tick Check uses. The engine
// reports the correct count (so only the per-node probe can catch it), forcing
// the full scan to be the detector.
func TestCheckDurability_FullScanNotSampled(t *testing.T) {
	o := NewGraphOracle()
	const total = maxSamplesPerKind * 4 // far more than one sample.
	var dropped string
	for i := 0; i < total; i++ {
		name := itoa(i)
		o.ApplyCreate(tmplCreatePerson, map[string]any{"name": name, "age": int64(i)})
		if i == total-1 {
			dropped = name // drop the last one, unlikely to be sampled first.
		}
	}
	// Count matches (so the count check passes); only the full per-node scan can
	// catch the single absent node.
	eng := &fakeDurabilityEngine{
		nodes:       int64(total),
		edges:       0,
		absentNodes: map[string]bool{dropped: true},
	}
	chk := NewInvariantChecker(NewSeed(1))

	v := chk.CheckDurability(1, o, eng)
	if !hasKind(v, ViolationACIDDurability) {
		t.Fatalf("full-scan durability check missed a single dropped node among %d, got %v", total, v)
	}
}

// TestCheckDurability_RealWALTruncationLosesCommittedOps is the strongest,
// most realistic injected-violation test: it commits ops to a real SimDisk WAL,
// then deliberately truncates the WAL byte image so a committed suffix is lost,
// reopens via real recovery, and asserts the durability check against the
// pre-truncation oracle detects the loss. This proves the check catches a
// genuine durability fault, not only a synthetic count mismatch.
func TestCheckDurability_RealWALTruncationLosesCommittedOps(t *testing.T) {
	disk := NewSimDisk(NewSeed(1), 0)
	store, err := OpenSimStore(disk, simulatorStoreConfig())
	if err != nil {
		t.Fatalf("OpenSimStore: %v", err)
	}

	// Build an oracle in lock-step with several committed creates.
	o := NewGraphOracle()
	for i := 0; i < 12; i++ {
		name := "p" + itoa(i)
		params := map[string]any{"name": name, "age": int64(i)}
		runWrite(t, store, "CREATE (n:Person {name:'"+name+"', age:"+itoa(i)+"})")
		o.ApplyCreate(tmplCreatePerson, params)
	}
	a := NewEngineAdapter(store.Engine())
	if n, _ := a.NodeCount(); n != int64(o.NodeCount()) {
		t.Fatalf("pre-truncation engine/oracle disagree: engine=%d oracle=%d", n, o.NodeCount())
	}
	_ = store.Close()

	// Injected durability fault: truncate the durable WAL image so a committed
	// suffix is permanently lost (as a torn-but-undetected drop would).
	full := disk.Snapshot()[simWALPath]
	if len(full) < 64 {
		t.Fatalf("WAL too small to truncate meaningfully: %d bytes", len(full))
	}
	h, err := disk.OpenFile(simWALPath, os.O_RDWR)
	if err != nil {
		t.Fatalf("open WAL for truncation: %v", err)
	}
	// Drop the trailing half of the WAL: this severs committed frames. Half of a
	// 12-create WAL is guaranteed to land past the first few committed ops, so a
	// committed suffix is always lost.
	if err := h.Truncate(int64(len(full) / 2)); err != nil {
		t.Fatalf("truncate WAL: %v", err)
	}
	_ = h.Close()

	// Reopen via real recovery. Two outcomes are both correct durability
	// responses to a severed WAL, and the test accepts either: (1) recovery
	// fail-stops on the corruption (no silent loss — the strongest response), or
	// (2) recovery tolerates the cut as a torn tail and the committed ops past it
	// are gone, in which case the durability check MUST detect the loss against
	// the pre-truncation oracle. What is forbidden is a silent reopen that
	// matches the full pre-truncation count (that would mean nothing was lost,
	// contradicting the truncation) or a loss the check fails to flag.
	store2, err := OpenSimStore(disk, simulatorStoreConfig())
	if err != nil {
		t.Logf("recovery fail-stopped on the truncated WAL (a correct durability response): %v", err)
		return
	}
	defer func() { _ = store2.Close() }()

	recovered := NewEngineAdapter(store2.Engine())
	gotN, _ := recovered.NodeCount()
	if gotN == int64(o.NodeCount()) {
		t.Fatalf("WAL truncated to half size but recovered the full %d nodes; nothing was lost, which is impossible", o.NodeCount())
	}

	chk := NewInvariantChecker(NewSeed(1))
	v := chk.CheckDurability(1, o, recovered)
	if !hasKind(v, ViolationACIDDurability) {
		t.Fatalf("durability check missed a real committed-op loss after WAL truncation (recovered %d of %d nodes), got %v",
			gotN, o.NodeCount(), v)
	}
}

// hasKind reports whether any violation in v has the given kind.
func hasKind(v []Violation, kind ViolationKind) bool {
	for _, viol := range v {
		if viol.Kind == kind {
			return true
		}
	}
	return false
}

// itoa is a tiny base-10 formatter kept local to avoid pulling strconv into the
// test for a single conversion site; it handles the small non-negative integers
// the tests use.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
