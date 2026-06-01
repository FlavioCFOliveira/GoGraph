package plan_test

// regression_test.go — golden-plan regression tests (task-291).
//
// Each fixture feeds a ScanInput through SelectScanStrategy and captures the
// resulting ScanDecision.Kind and IndexName.  The outcomes are compared against
// a committed JSON golden file.
//
// To regenerate the golden file after an intentional plan change:
//
//	UPDATE_GOLDEN=1 go test ./cypher/plan/... -run TestPlanRegression

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/plan"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/label"
)

// planFixture describes one regression scenario.
type planFixture struct {
	Name     string
	Input    plan.ScanInput
	HasHash  bool
	HasBTree bool
	// LabelNodes is the number of nodes to add to label 1 in the estimator.
	// Zero means the estimator has no label-1 data.
	LabelNodes int
}

// planFixtures is the canonical set of 20 regression scenarios covering the
// full ScanInput × index-availability matrix.
var planFixtures = []planFixture{
	// 1. No label, no index — AllNodes.
	{Name: "no_label_no_index", Input: plan.ScanInput{}},

	// 2. Label only, no index — LabelScan.
	{Name: "label_only", Input: plan.ScanInput{LabelID: 1}, LabelNodes: 1000},

	// 3. Label + hash index → IndexSeek (hash cost < label cost).
	{Name: "label_hash", Input: plan.ScanInput{LabelID: 1, EqPropID: 2}, HasHash: true, LabelNodes: 1000},

	// 4. Label + btree index → IndexRangeScan (btree cost < label cost).
	{Name: "label_btree", Input: plan.ScanInput{LabelID: 1, RangePropID: 3}, HasBTree: true, LabelNodes: 1000},

	// 5. Eq predicate but no hash index → falls back to LabelScan.
	{Name: "eq_no_hash", Input: plan.ScanInput{LabelID: 1, EqPropID: 2}, LabelNodes: 1000},

	// 6. Range predicate but no btree index → falls back to LabelScan.
	{Name: "range_no_btree", Input: plan.ScanInput{LabelID: 1, RangePropID: 3}, LabelNodes: 1000},

	// 7. Hash index present but no label → AllNodes (label cost
	// dominates when LabelID==0, and hash needs EqPropID>0 to activate).
	{Name: "hash_no_label_id", Input: plan.ScanInput{EqPropID: 2}, HasHash: true},

	// 8. Both hash and btree available, hash cheaper → IndexSeek.
	{Name: "both_hash_wins", Input: plan.ScanInput{LabelID: 1, EqPropID: 2, RangePropID: 3}, HasHash: true, HasBTree: true, LabelNodes: 1000},

	// 9. Only btree available with range predicate → IndexRangeScan.
	{Name: "btree_only_range", Input: plan.ScanInput{LabelID: 1, RangePropID: 3}, HasBTree: true, LabelNodes: 1000},

	// 10. Empty ScanInput with both indexes → AllNodes (no label/prop IDs).
	{Name: "empty_both_indexes", Input: plan.ScanInput{}, HasHash: true, HasBTree: true},

	// 11. LabelID with no nodes recorded → LabelScan with 1-row estimate.
	{Name: "label_zero_nodes", Input: plan.ScanInput{LabelID: 1}},

	// 12. Hash index, EqPropID set, but LabelID=0 → AllNodes (EqPropID > 0
	// but reg has hash; however LabelID=0 means no label step, and hash
	// needs at least non-zero estimate — hash cost=1*0.05=0.05 < allnodes
	// cost fallback=1e9; IndexSeek wins).
	{Name: "hash_no_label", Input: plan.ScanInput{EqPropID: 2}, HasHash: true},

	// 13. Label + hash + zero eq-prop → LabelScan (EqPropID=0 disables hash).
	{Name: "label_hash_eq_zero", Input: plan.ScanInput{LabelID: 1}, HasHash: true, LabelNodes: 1000},

	// 14. Label + btree + zero range-prop → LabelScan (RangePropID=0 disables btree).
	{Name: "label_btree_range_zero", Input: plan.ScanInput{LabelID: 1}, HasBTree: true, LabelNodes: 1000},

	// 15. Large label count (10 000) with hash registered → IndexSeek still wins.
	{Name: "large_label_hash_wins", Input: plan.ScanInput{LabelID: 1, EqPropID: 5}, HasHash: true, LabelNodes: 10_000},

	// 16. Large label count, btree registered → IndexRangeScan still wins.
	{Name: "large_label_btree_wins", Input: plan.ScanInput{LabelID: 1, RangePropID: 5}, HasBTree: true, LabelNodes: 10_000},

	// 17. No indexes, LabelID=1, 10 000 nodes → LabelScan.
	{Name: "large_label_no_index", Input: plan.ScanInput{LabelID: 1}, LabelNodes: 10_000},

	// 18. All fields set, no indexes → LabelScan.
	{Name: "full_input_no_index", Input: plan.ScanInput{LabelID: 1, EqPropID: 2, RangePropID: 3}, LabelNodes: 500},

	// 19. EqPropID and RangePropID both set, only btree available → IndexRangeScan.
	{Name: "btree_no_hash_full_input", Input: plan.ScanInput{LabelID: 1, EqPropID: 2, RangePropID: 3}, HasBTree: true, LabelNodes: 1000},

	// 20. EqPropID and RangePropID both set, only hash available → IndexSeek.
	{Name: "hash_no_btree_full_input", Input: plan.ScanInput{LabelID: 1, EqPropID: 2, RangePropID: 3}, HasHash: true, LabelNodes: 1000},
}

// goldenEntry is the serialised form of one fixture's plan decision.
type goldenEntry struct {
	Name      string `json:"name"`
	Kind      int    `json:"kind"`
	IndexName string `json:"index_name"`
}

// buildFixtureRegistry creates an IndexRegistry and Estimator for a fixture.
// LabelNodes (when > 0) populates label 1 in the label index; label 0
// receives totalNodes = LabelNodes * 10 as the all-nodes sentinel.
func buildFixtureRegistry(t testing.TB, f planFixture) (*plan.IndexRegistry, plan.Estimator) {
	t.Helper()

	mgr := index.NewManager()
	if f.HasHash {
		if err := mgr.CreateIndex("hash_idx", stubSubscriber{"hash"}); err != nil {
			t.Fatalf("CreateIndex hash: %v", err)
		}
	}
	if f.HasBTree {
		if err := mgr.CreateIndex("btree_idx", stubSubscriber{"btree"}); err != nil {
			t.Fatalf("CreateIndex btree: %v", err)
		}
	}
	reg := plan.NewIndexRegistry(mgr)

	labelIdx := label.NewNodeIndex()
	if f.LabelNodes > 0 {
		total := f.LabelNodes * 10 // sentinel stored at label 0
		for i := graph.NodeID(0); i < graph.NodeID(total); i++ {
			labelIdx.Add(0, i)
		}
		for i := graph.NodeID(0); i < graph.NodeID(f.LabelNodes); i++ {
			labelIdx.Add(1, i)
		}
	}
	est := plan.NewIndexEstimator(labelIdx, mgr)
	return reg, est
}

// computeCurrent evaluates SelectScanStrategy for every fixture and returns
// the ordered list of goldenEntry results.
func computeCurrent(t testing.TB) []goldenEntry {
	t.Helper()
	current := make([]goldenEntry, len(planFixtures))
	for i, f := range planFixtures {
		reg, est := buildFixtureRegistry(t, f)
		dec := plan.SelectScanStrategy(f.Input, est, reg)
		current[i] = goldenEntry{
			Name:      f.Name,
			Kind:      int(dec.Kind),
			IndexName: dec.IndexName,
		}
	}
	return current
}

// TestPlanRegression compares the current SelectScanStrategy decisions against
// the committed golden file.  Run with UPDATE_GOLDEN=1 to regenerate.
func TestPlanRegression(t *testing.T) {
	t.Parallel()

	goldenPath := filepath.Join("testdata", "golden_plans.json")
	current := computeCurrent(t)

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		data, err := json.MarshalIndent(current, "", "  ")
		if err != nil {
			t.Fatalf("marshal golden: %v", err)
		}
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, append(data, '\n'), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("golden file written to %s (%d entries)", goldenPath, len(current))
		return
	}

	rawGolden, err := os.ReadFile(goldenPath) //nolint:gosec // path is a committed testdata file, not user input
	if err != nil {
		t.Fatalf("golden file missing — run: UPDATE_GOLDEN=1 go test ./cypher/plan/... -run TestPlanRegression\nerror: %v", err)
	}
	var golden []goldenEntry
	if err := json.Unmarshal(rawGolden, &golden); err != nil {
		t.Fatalf("parse golden %s: %v", goldenPath, err)
	}

	if len(golden) != len(current) {
		t.Fatalf("golden has %d entries, current has %d; regenerate with UPDATE_GOLDEN=1",
			len(golden), len(current))
	}

	for i, c := range current {
		g := golden[i]
		if c.Name != g.Name {
			t.Errorf("fixture[%d]: name mismatch: current %q != golden %q", i, c.Name, g.Name)
			continue
		}
		if c.Kind != g.Kind || c.IndexName != g.IndexName {
			t.Errorf("fixture %q: got kind=%d index=%q, want kind=%d index=%q",
				c.Name, c.Kind, c.IndexName, g.Kind, g.IndexName)
		}
	}
}
