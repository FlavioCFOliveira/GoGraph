package exec_test

// write_operators_test.go — unit tests for write operators (tasks-268 through 275).
//
// Each test creates a small in-memory stub that implements GraphMutator and
// verifies the operator's side effects independently of the lpg package.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"gograph/cypher/exec"
	"gograph/cypher/expr"
	"gograph/graph"
	"gograph/graph/lpg"
)

// mustAddNode wraps stubMutator.AddNode for tests that expect the
// call to succeed (the stub never returns errors).
func mustAddNode(t *testing.T, s *stubMutator, n string) graph.NodeID {
	t.Helper()
	id, err := s.AddNode(n)
	if err != nil {
		t.Fatalf("AddNode(%q): %v", n, err)
	}
	return id
}

// mustAddEdge wraps stubMutator.AddEdge for tests that expect the
// call to succeed.
func mustAddEdge(t *testing.T, s *stubMutator, src, dst string, w float64) (graph.NodeID, graph.NodeID) {
	t.Helper()
	sid, did, err := s.AddEdge(src, dst, w)
	if err != nil {
		t.Fatalf("AddEdge(%q,%q): %v", src, dst, err)
	}
	return sid, did
}

// ─────────────────────────────────────────────────────────────────────────────
// stubMutator — in-process GraphMutator for tests
// ─────────────────────────────────────────────────────────────────────────────

type stubMutator struct {
	mu         sync.Mutex
	nodes      map[string]graph.NodeID // key → ID
	nextID     graph.NodeID
	labels     map[string]map[string]bool // key → label set
	props      map[string]map[string]lpg.PropertyValue
	edges      map[string]map[string]bool              // src → dst set (directed)
	edgeLabels map[string]map[string]bool              // "src|dst" → label set
	edgeProps  map[string]map[string]lpg.PropertyValue // "src|dst" → prop map
	tombstones map[graph.NodeID]struct{}               // RemoveNode'd ids
}

func newStubMutator() *stubMutator {
	return &stubMutator{
		nodes:      make(map[string]graph.NodeID),
		labels:     make(map[string]map[string]bool),
		props:      make(map[string]map[string]lpg.PropertyValue),
		edges:      make(map[string]map[string]bool),
		edgeLabels: make(map[string]map[string]bool),
		edgeProps:  make(map[string]map[string]lpg.PropertyValue),
	}
}

func (s *stubMutator) AddNode(n string) (graph.NodeID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.nodes[n]; ok {
		return id, nil
	}
	id := s.nextID
	s.nextID++
	s.nodes[n] = id
	return id, nil
}

func (s *stubMutator) AddEdge(src, dst string, _ float64) (srcID, dstID graph.NodeID, err error) {
	srcID, err = s.AddNode(src)
	if err != nil {
		return 0, 0, err
	}
	dstID, err = s.AddNode(dst)
	if err != nil {
		return 0, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.edges[src] == nil {
		s.edges[src] = make(map[string]bool)
	}
	s.edges[src][dst] = true
	return
}

func (s *stubMutator) RemoveEdge(src, dst string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.edges[src]; ok {
		delete(m, dst)
	}
}

func (s *stubMutator) SetNodeLabel(n, label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.labels[n] == nil {
		s.labels[n] = make(map[string]bool)
	}
	s.labels[n][label] = true
	return nil
}

func (s *stubMutator) RemoveNodeLabel(n, label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.labels[n] != nil {
		delete(s.labels[n], label)
	}
}

func (s *stubMutator) SetNodeProperty(n, key string, value lpg.PropertyValue) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.props[n] == nil {
		s.props[n] = make(map[string]lpg.PropertyValue)
	}
	s.props[n][key] = value
	return nil
}

func (s *stubMutator) DelNodeProperty(n, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.props[n] != nil {
		delete(s.props[n], key)
	}
}

func (s *stubMutator) NodeProperties(n string) map[string]lpg.PropertyValue {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.props[n]
	if src == nil {
		return nil
	}
	cp := make(map[string]lpg.PropertyValue, len(src))
	for k, v := range src {
		cp[k] = v
	}
	return cp
}

func (s *stubMutator) NodeLabels(n string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.labels[n]
	if set == nil {
		return nil
	}
	out := make([]string, 0, len(set))
	for lbl := range set {
		out = append(out, lbl)
	}
	return out
}

func (s *stubMutator) HasEdge(src, dst string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.edges[src] != nil && s.edges[src][dst]
}

func (s *stubMutator) SetEdgeLabel(src, dst, label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := src + "|" + dst
	if s.edgeLabels[k] == nil {
		s.edgeLabels[k] = make(map[string]bool)
	}
	s.edgeLabels[k][label] = true
}

func (s *stubMutator) SetEdgeProperty(src, dst, key string, value lpg.PropertyValue) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := src + "|" + dst
	if s.edgeProps == nil {
		s.edgeProps = make(map[string]map[string]lpg.PropertyValue)
	}
	if s.edgeProps[k] == nil {
		s.edgeProps[k] = make(map[string]lpg.PropertyValue)
	}
	s.edgeProps[k][key] = value
	return nil
}

func (s *stubMutator) DelEdgeProperty(src, dst, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := src + "|" + dst
	if s.edgeProps != nil && s.edgeProps[k] != nil {
		delete(s.edgeProps[k], key)
	}
}

// EdgeProperties returns a snapshot of the property map for the directed edge
// (src, dst). The result is a fresh map; callers may mutate it freely.
func (s *stubMutator) EdgeProperties(src, dst string) map[string]lpg.PropertyValue {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := src + "|" + dst
	if s.edgeProps == nil || s.edgeProps[k] == nil {
		return map[string]lpg.PropertyValue{}
	}
	out := make(map[string]lpg.PropertyValue, len(s.edgeProps[k]))
	for kk, vv := range s.edgeProps[k] {
		out[kk] = vv
	}
	return out
}

// IncEdgeCreateCount, EdgeCreateCount, DecEdgeCreateCount and the
// per-instance metadata stubs are inert: the write-operator tests do
// not exercise multi-edge MERGE / parallel-CREATE semantics.
func (s *stubMutator) IncEdgeCreateCount(string, string) int64      { return 0 }
func (s *stubMutator) EdgeCreateCount(string, string) int64         { return 0 }
func (s *stubMutator) DecEdgeCreateCount(string, string)            {}
func (s *stubMutator) SetEdgeLabelAt(string, string, int64, string) {}
func (s *stubMutator) EdgeLabelsAt(string, string, int64) []string  { return nil }
func (s *stubMutator) SetEdgePropertyAt(string, string, int64, string, lpg.PropertyValue) {
}
func (s *stubMutator) EdgePropertiesAt(string, string, int64) map[string]lpg.PropertyValue {
	return nil
}
func (s *stubMutator) RemoveEdgeInstance(string, string, int64) {}

// EdgeLabels returns the edge labels for (src, dst). Reads from the
// edgeLabel set populated by SetEdgeLabel; returns nil when absent.
func (s *stubMutator) EdgeLabels(src, dst string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := src + "|" + dst
	labels := s.edgeLabels[k]
	if len(labels) == 0 {
		return nil
	}
	out := make([]string, 0, len(labels))
	for l := range labels {
		out = append(out, l)
	}
	return out
}

// getEdgeProp returns the edge property value for edge (src,dst) under key.
func (s *stubMutator) getEdgeProp(src, dst, key string) (lpg.PropertyValue, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := src + "|" + dst
	if s.edgeProps == nil || s.edgeProps[k] == nil {
		return lpg.PropertyValue{}, false
	}
	v, ok := s.edgeProps[k][key]
	return v, ok
}

func (s *stubMutator) OutNeighbours(n string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.edges[n]
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m))
	for dst := range m {
		out = append(out, dst)
	}
	return out
}

func (s *stubMutator) InNeighbours(n string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []string
	for src, dsts := range s.edges {
		if dsts[n] {
			result = append(result, src)
		}
	}
	return result
}

func (s *stubMutator) OutDegree(n string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.edges[n])
}

func (s *stubMutator) ResolveNodeID(n string) (graph.NodeID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.nodes[n]
	return id, ok
}

func (s *stubMutator) ResolveNodeLabel(id graph.NodeID) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.nodes {
		if v == id {
			return k, true
		}
	}
	return "", false
}

func (s *stubMutator) WalkNodeIDs(fn func(graph.NodeID) bool) {
	s.mu.Lock()
	ids := make([]graph.NodeID, 0, len(s.nodes))
	for _, id := range s.nodes {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		if !fn(id) {
			return
		}
	}
}

// RemoveNode tombstones n in the test stub.
func (s *stubMutator) RemoveNode(n string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tombstones == nil {
		s.tombstones = make(map[graph.NodeID]struct{})
	}
	if id, ok := s.nodes[n]; ok {
		s.tombstones[id] = struct{}{}
	}
}

// IsTombstoned reports whether id has been tombstoned.
func (s *stubMutator) IsTombstoned(id graph.NodeID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tombstones == nil {
		return false
	}
	_, ok := s.tombstones[id]
	return ok
}

// nodeCount returns the number of interned nodes.
func (s *stubMutator) nodeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.nodes)
}

// hasLabel reports whether node n has label lbl.
func (s *stubMutator) hasLabel(n, lbl string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.labels[n] != nil && s.labels[n][lbl]
}

// getProp returns the property value for node n under key.
func (s *stubMutator) getProp(n, key string) (lpg.PropertyValue, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.props[n] == nil {
		return lpg.PropertyValue{}, false
	}
	v, ok := s.props[n][key]
	return v, ok
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 269 — CreateNode
// ─────────────────────────────────────────────────────────────────────────────

// TestCreateNode_SingleRow verifies that one input row creates exactly one
// node with the specified labels and properties.
func TestCreateNode_SingleRow(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()

	src := newSliceOperator(exec.Row{expr.IntegerValue(0)}) // one driving row
	op, err := exec.NewCreateNode(
		"n",
		[]string{"Person"},
		`{name: "Alice"}`,
		src,
		mut,
	)
	if err != nil {
		t.Fatalf("NewCreateNode: %v", err)
	}

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}

	// NodeID must be in the emitted row.
	if len(rows[0]) < 2 {
		t.Fatalf("expected at least 2 columns in row, got %d", len(rows[0]))
	}
	nodeIDVal, ok := rows[0][1].(expr.IntegerValue)
	if !ok {
		t.Fatalf("second column must be IntegerValue (NodeID), got %T", rows[0][1])
	}

	nodeID := graph.NodeID(nodeIDVal)
	nodeKey, resolved := mut.ResolveNodeLabel(nodeID)
	if !resolved {
		t.Fatal("created NodeID not resolvable")
	}
	if !mut.hasLabel(nodeKey, "Person") {
		t.Error("node should carry label Person")
	}
	pv, hasProp := mut.getProp(nodeKey, "name")
	if !hasProp {
		t.Fatal("property 'name' not set")
	}
	s, ok := pv.String()
	if !ok || s != "Alice" {
		t.Errorf("property 'name' = %v, want Alice", pv)
	}
}

// TestCreateNode_NRows verifies that N driving rows produce N nodes.
func TestCreateNode_NRows(t *testing.T) {
	t.Parallel()
	const n = 5
	mut := newStubMutator()

	rows := make([]exec.Row, n)
	for i := range rows {
		rows[i] = exec.Row{expr.IntegerValue(int64(i))}
	}
	src := newSliceOperator(rows...)
	op, err := exec.NewCreateNode("n", []string{"X"}, "", src, mut)
	if err != nil {
		t.Fatal(err)
	}

	out, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(out) != n {
		t.Fatalf("want %d rows, got %d", n, len(out))
	}
	if mut.nodeCount() != n {
		t.Errorf("want %d nodes in graph, got %d", n, mut.nodeCount())
	}
}

// TestCreateNode_NoVar verifies that a CREATE with no variable binding
// still creates the node and passes the row through unchanged.
func TestCreateNode_NoVar(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	src := newSliceOperator(exec.Row{expr.StringValue("x")})
	op, err := exec.NewCreateNode("", []string{"Label"}, "", src, mut)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 row, got %d", len(out))
	}
	// Row should be unchanged (no extra column).
	if len(out[0]) != 1 {
		t.Errorf("row should have 1 column (no var), got %d", len(out[0]))
	}
	if mut.nodeCount() != 1 {
		t.Errorf("expected 1 node created, got %d", mut.nodeCount())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 270 — CreateRelationship
// ─────────────────────────────────────────────────────────────────────────────

// TestCreateRelationship_Basic creates two nodes then creates a relationship.
func TestCreateRelationship_Basic(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()

	// Pre-intern two nodes.
	srcID := mustAddNode(t, mut, "alice")
	dstID := mustAddNode(t, mut, "bob")

	schema := map[string]int{"a": 0, "b": 1}
	row := exec.Row{
		expr.IntegerValue(int64(srcID)),
		expr.IntegerValue(int64(dstID)),
	}
	src := newSliceOperator(row)
	op, err := exec.NewCreateRelationship("a", "b", "r", "KNOWS", "", schema, src, mut)
	if err != nil {
		t.Fatal(err)
	}

	out, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 row, got %d", len(out))
	}
	if !mut.HasEdge("alice", "bob") {
		t.Error("edge alice→bob not created")
	}
	// The relationship variable column should be a RelationshipValue.
	if len(out[0]) < 3 {
		t.Fatalf("expected ≥ 3 columns (a, b, r), got %d", len(out[0]))
	}
	relVal, ok := out[0][2].(expr.RelationshipValue)
	if !ok {
		t.Fatalf("column 2 should be RelationshipValue, got %T", out[0][2])
	}
	if relVal.Type != "KNOWS" {
		t.Errorf("relationship type = %q, want KNOWS", relVal.Type)
	}
}

// TestCreateRelationship_MissingEndpoint verifies that an unbound endpoint
// produces an error.
func TestCreateRelationship_MissingEndpoint(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	// Only one column (src) in the row; dst is missing.
	srcID := mustAddNode(t, mut, "x")
	schema := map[string]int{"a": 0}
	row := exec.Row{expr.IntegerValue(int64(srcID))}
	src := newSliceOperator(row)
	op, err := exec.NewCreateRelationship("a", "b", "", "REL", "", schema, src, mut)
	if err != nil {
		t.Fatal(err)
	}
	_, err = exec.Drain(context.Background(), op)
	if err == nil {
		t.Fatal("expected error for missing endpoint")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 271 — SetProperty / SetLabels
// ─────────────────────────────────────────────────────────────────────────────

// TestSetProperty_SingleProperty sets a single property on a node.
func TestSetProperty_SingleProperty(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	nID := mustAddNode(t, mut, "alice")

	schema := map[string]int{"n": 0}
	row := exec.Row{expr.IntegerValue(int64(nID))}
	src := newSliceOperator(row)
	op, err := exec.NewSetProperty("n", "name", `"Alice"`, schema, src, mut)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	pv, ok := mut.getProp("alice", "name")
	if !ok {
		t.Fatal("property 'name' not set")
	}
	s, _ := pv.String()
	if s != "Alice" {
		t.Errorf("property 'name' = %q, want Alice", s)
	}
}

// TestSetProperty_ReplaceAll verifies SET n = {…} replaces all properties.
func TestSetProperty_ReplaceAll(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	nID := mustAddNode(t, mut, "alice")
	if err := mut.SetNodeProperty("alice", "old", lpg.StringValue("gone")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	schema := map[string]int{"n": 0}
	row := exec.Row{expr.IntegerValue(int64(nID))}
	src := newSliceOperator(row)
	op, err := exec.NewSetProperty("n", "", `{name: "Alice"}`, schema, src, mut)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if _, ok := mut.getProp("alice", "old"); ok {
		t.Error("old property should have been removed in replace mode")
	}
	pv, ok := mut.getProp("alice", "name")
	if !ok {
		t.Fatal("new property 'name' not set")
	}
	s, _ := pv.String()
	if s != "Alice" {
		t.Errorf("property 'name' = %q, want Alice", s)
	}
}

// TestSetProperty_Merge verifies SET n += {…} merges without removing existing.
func TestSetProperty_Merge(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	nID := mustAddNode(t, mut, "alice")
	if err := mut.SetNodeProperty("alice", "existing", lpg.StringValue("keep")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	schema := map[string]int{"n": 0}
	row := exec.Row{expr.IntegerValue(int64(nID))}
	src := newSliceOperator(row)
	op, err := exec.NewSetProperty("n", "", `+= {name: "Alice"}`, schema, src, mut)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if _, ok := mut.getProp("alice", "existing"); !ok {
		t.Error("existing property should be preserved in merge mode")
	}
	pv, ok := mut.getProp("alice", "name")
	if !ok {
		t.Fatal("new property 'name' not set in merge mode")
	}
	s, _ := pv.String()
	if s != "Alice" {
		t.Errorf("property 'name' = %q, want Alice", s)
	}
}

// TestSetLabels adds labels to a node.
func TestSetLabels(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	nID := mustAddNode(t, mut, "n1")

	schema := map[string]int{"n": 0}
	row := exec.Row{expr.IntegerValue(int64(nID))}
	src := newSliceOperator(row)
	op := exec.NewSetLabels("n", []string{"Person", "Employee"}, schema, src, mut)

	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !mut.hasLabel("n1", "Person") {
		t.Error("label Person not added")
	}
	if !mut.hasLabel("n1", "Employee") {
		t.Error("label Employee not added")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 272 — RemoveProperty / RemoveLabels
// ─────────────────────────────────────────────────────────────────────────────

// TestRemoveProperty removes a property from a node.
func TestRemoveProperty(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	nID := mustAddNode(t, mut, "alice")
	if err := mut.SetNodeProperty("alice", "age", lpg.Int64Value(30)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	schema := map[string]int{"n": 0}
	row := exec.Row{expr.IntegerValue(int64(nID))}
	src := newSliceOperator(row)
	op := exec.NewRemoveProperty("n", "age", schema, src, mut)

	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if _, ok := mut.getProp("alice", "age"); ok {
		t.Error("property 'age' should have been removed")
	}
}

// TestRemoveLabels removes one or more labels from a node.
func TestRemoveLabels(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	nID := mustAddNode(t, mut, "bob")
	if err := mut.SetNodeLabel("bob", "Person"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := mut.SetNodeLabel("bob", "Employee"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}

	schema := map[string]int{"n": 0}
	row := exec.Row{expr.IntegerValue(int64(nID))}
	src := newSliceOperator(row)
	op := exec.NewRemoveLabels("n", []string{"Employee"}, schema, src, mut)

	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if mut.hasLabel("bob", "Employee") {
		t.Error("label Employee should have been removed")
	}
	if !mut.hasLabel("bob", "Person") {
		t.Error("label Person should remain")
	}
}

// TestRemoveProperty_FiveScenarios covers the 5 AC scenarios.
func TestRemoveProperty_FiveScenarios(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		setupProp   map[string]lpg.PropertyValue
		removeKey   string
		wantPresent bool
		wantRemoved bool
	}{
		{"existing property removed", map[string]lpg.PropertyValue{"k": lpg.StringValue("v")}, "k", false, true},
		{"absent property no-op", nil, "missing", false, false},
		{"integer property removed", map[string]lpg.PropertyValue{"age": lpg.Int64Value(42)}, "age", false, true},
		{"float property removed", map[string]lpg.PropertyValue{"score": lpg.Float64Value(3.14)}, "score", false, true},
		{"bool property removed", map[string]lpg.PropertyValue{"active": lpg.BoolValue(true)}, "active", false, true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mut := newStubMutator()
			nID := mustAddNode(t, mut, "node")
			for k, v := range tc.setupProp {
				if err := mut.SetNodeProperty("node", k, v); err != nil {
					t.Fatalf("SetNodeProperty: %v", err)
				}
			}
			schema := map[string]int{"n": 0}
			row := exec.Row{expr.IntegerValue(int64(nID))}
			src := newSliceOperator(row)
			op := exec.NewRemoveProperty("n", tc.removeKey, schema, src, mut)
			if _, err := exec.Drain(context.Background(), op); err != nil {
				t.Fatalf("Drain: %v", err)
			}
			_, present := mut.getProp("node", tc.removeKey)
			if present != tc.wantPresent {
				t.Errorf("property %q present=%v, want %v", tc.removeKey, present, tc.wantPresent)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 273 — DeleteNode / DeleteRelationship
// ─────────────────────────────────────────────────────────────────────────────

// TestDeleteNode_Connected verifies that DELETE on a connected node errors.
func TestDeleteNode_Connected(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	nID := mustAddNode(t, mut, "alice")
	mustAddNode(t, mut, "bob")
	mustAddEdge(t, mut, "alice", "bob", 0)

	schema := map[string]int{"n": 0}
	row := exec.Row{expr.IntegerValue(int64(nID))}
	src := newSliceOperator(row)
	op := exec.NewDeleteNode("n", schema, src, mut)

	_, err := exec.Drain(context.Background(), op)
	if !errors.Is(err, exec.ErrDeleteNodeHasRelationships) {
		t.Errorf("expected ErrDeleteNodeHasRelationships, got %v", err)
	}
}

// TestDeleteNode_Isolated deletes a node with no relationships.
func TestDeleteNode_Isolated(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	nID := mustAddNode(t, mut, "alone")
	if err := mut.SetNodeLabel("alone", "X"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := mut.SetNodeProperty("alone", "k", lpg.StringValue("v")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	schema := map[string]int{"n": 0}
	row := exec.Row{expr.IntegerValue(int64(nID))}
	src := newSliceOperator(row)
	op := exec.NewDeleteNode("n", schema, src, mut)

	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if mut.hasLabel("alone", "X") {
		t.Error("label X should have been removed after delete")
	}
	if _, ok := mut.getProp("alone", "k"); ok {
		t.Error("property k should have been removed after delete")
	}
}

// TestDeleteRelationship removes an edge.
func TestDeleteRelationship(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	aID := mustAddNode(t, mut, "a")
	bID := mustAddNode(t, mut, "b")
	mustAddEdge(t, mut, "a", "b", 0)

	schema := map[string]int{"r": 0}
	rel := expr.RelationshipValue{
		ID:      0,
		StartID: uint64(aID),
		EndID:   uint64(bID),
		Type:    "REL",
	}
	row := exec.Row{rel}
	src := newSliceOperator(row)
	op := exec.NewDeleteRelationship("r", schema, src, mut)

	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if mut.HasEdge("a", "b") {
		t.Error("edge a→b should have been removed")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 274 — DetachDelete
// ─────────────────────────────────────────────────────────────────────────────

// TestDetachDelete_Connected deletes a node together with its incident edges.
func TestDetachDelete_Connected(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	aID := mustAddNode(t, mut, "a")
	mustAddNode(t, mut, "b")
	mustAddNode(t, mut, "c")
	mustAddEdge(t, mut, "a", "b", 0) // outgoing
	mustAddEdge(t, mut, "c", "a", 0) // incoming
	if err := mut.SetNodeLabel("a", "L"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := mut.SetNodeProperty("a", "p", lpg.StringValue("v")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	schema := map[string]int{"n": 0}
	row := exec.Row{expr.IntegerValue(int64(aID))}
	src := newSliceOperator(row)
	op := exec.NewDetachDelete("n", schema, src, mut)

	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if mut.HasEdge("a", "b") {
		t.Error("outgoing edge a→b should have been removed")
	}
	if mut.HasEdge("c", "a") {
		t.Error("incoming edge c→a should have been removed")
	}
	if mut.hasLabel("a", "L") {
		t.Error("label L should have been stripped")
	}
	if _, ok := mut.getProp("a", "p"); ok {
		t.Error("property p should have been stripped")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 275 — Merge
// ─────────────────────────────────────────────────────────────────────────────

// TestMerge_NoMatch creates a node when pattern is not found.
func TestMerge_NoMatch(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()

	noMatchFn := func(_ context.Context) ([]exec.Row, error) { return nil, nil }
	schema := map[string]int{}
	src := newSliceOperator() // empty child (no driving rows needed for Merge)
	op, err := exec.NewMerge(
		"n",
		[]string{"Person"},
		`{name: "Bob"}`,
		nil, nil,
		noMatchFn,
		schema,
		src,
		mut,
	)
	if err != nil {
		t.Fatal(err)
	}

	rows, drainErr := exec.Drain(context.Background(), op)
	if drainErr != nil {
		t.Fatalf("Drain: %v", drainErr)
	}
	if len(rows) != 1 {
		t.Fatalf("Merge(ON CREATE) should emit 1 row, got %d", len(rows))
	}
	if mut.nodeCount() != 1 {
		t.Errorf("expected 1 node created, got %d", mut.nodeCount())
	}
}

// TestMerge_Match uses ON MATCH when pattern is found; no new node created.
func TestMerge_Match(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	existingID := mustAddNode(t, mut, "existing")
	existingRow := exec.Row{expr.IntegerValue(int64(existingID))}

	matchFn := func(_ context.Context) ([]exec.Row, error) {
		return []exec.Row{existingRow}, nil
	}
	schema := map[string]int{"n": 0}
	src := newSliceOperator()
	op, err := exec.NewMerge(
		"n",
		[]string{"Person"},
		"",
		nil, nil,
		matchFn,
		schema,
		src,
		mut,
	)
	if err != nil {
		t.Fatal(err)
	}

	rows, drainErr := exec.Drain(context.Background(), op)
	if drainErr != nil {
		t.Fatalf("Drain: %v", drainErr)
	}
	if len(rows) != 1 {
		t.Fatalf("Merge(ON MATCH) should emit 1 row (matched), got %d", len(rows))
	}
	// Exactly one node was present before; no new node should have been created.
	if mut.nodeCount() != 1 {
		t.Errorf("expected 1 node total (no creation), got %d", mut.nodeCount())
	}
}

// TestMerge_NoMatchOnceUnderConcurrency verifies that even when Merge.Init is
// called from multiple goroutines (each with its own operator instance, as
// required by the single-operator-tree-per-goroutine contract), each
// independent Merge creates exactly one node.
func TestMerge_NoMatchOnceUnderConcurrency(t *testing.T) {
	t.Parallel()
	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)

	results := make([]*stubMutator, n)
	for i := range n {
		i := i
		go func() {
			defer wg.Done()
			mut := newStubMutator()
			results[i] = mut
			noMatchFn := func(_ context.Context) ([]exec.Row, error) { return nil, nil }
			src := newSliceOperator()
			op, err := exec.NewMerge("n", []string{"X"}, "", nil, nil, noMatchFn,
				map[string]int{}, src, mut)
			if err != nil {
				t.Errorf("goroutine %d: NewMerge: %v", i, err)
				return
			}
			if _, err := exec.Drain(context.Background(), op); err != nil {
				t.Errorf("goroutine %d: Drain: %v", i, err)
			}
		}()
	}
	wg.Wait()
	for i, m := range results {
		if m == nil {
			continue
		}
		if m.nodeCount() != 1 {
			t.Errorf("goroutine %d: expected 1 node, got %d", i, m.nodeCount())
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// T959 regression — SetProperty / RemoveProperty with NodeValue and
// RelationshipValue column bindings
// ─────────────────────────────────────────────────────────────────────────────

// TestSetProperty_NodeValueBinding verifies that SetProperty resolves a node
// when the row carries an expr.NodeValue (as emitted by MATCH scan operators)
// rather than a bare IntegerValue. This is the root cause of T959.
func TestSetProperty_NodeValueBinding(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	nID := mustAddNode(t, mut, "alice")

	// Simulate a MATCH scan row: column holds NodeValue, not IntegerValue.
	schema := map[string]int{"n": 0}
	row := exec.Row{expr.NodeValue{ID: uint64(nID)}}
	src := newSliceOperator(row)
	op, err := exec.NewSetProperty("n", "status", `"active"`, schema, src, mut)
	if err != nil {
		t.Fatalf("NewSetProperty: %v", err)
	}

	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	pv, ok := mut.getProp("alice", "status")
	if !ok {
		t.Fatal("property 'status' not set")
	}
	s, ok := pv.String()
	if !ok || s != "active" {
		t.Errorf("property 'status' = %v, want 'active'", pv)
	}
}

// TestSetProperty_RelationshipBinding verifies that SetProperty dispatches to
// SetEdgeProperty when the row carries an expr.RelationshipValue.
func TestSetProperty_RelationshipBinding(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	srcID, dstID := mustAddEdge(t, mut, "a", "b", 1.0)

	schema := map[string]int{"r": 0}
	row := exec.Row{expr.RelationshipValue{
		ID:      1,
		StartID: uint64(srcID),
		EndID:   uint64(dstID),
		Type:    "KNOWS",
	}}
	src := newSliceOperator(row)
	op, err := exec.NewSetProperty("r", "since", `2024`, schema, src, mut)
	if err != nil {
		t.Fatalf("NewSetProperty: %v", err)
	}

	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	pv, ok := mut.getEdgeProp("a", "b", "since")
	if !ok {
		t.Fatal("edge property 'since' not set")
	}
	n, ok := pv.Int64()
	if !ok || n != 2024 {
		t.Errorf("edge property 'since' = %v, want 2024", pv)
	}
}

// TestRemoveProperty_NodeValueBinding verifies that RemoveProperty resolves a
// node when the row carries an expr.NodeValue.
func TestRemoveProperty_NodeValueBinding(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	nID := mustAddNode(t, mut, "bob")
	if err := mut.SetNodeProperty("bob", "age", lpg.Int64Value(30)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	schema := map[string]int{"n": 0}
	row := exec.Row{expr.NodeValue{ID: uint64(nID)}}
	src := newSliceOperator(row)
	op := exec.NewRemoveProperty("n", "age", schema, src, mut)

	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if _, ok := mut.getProp("bob", "age"); ok {
		t.Error("property 'age' should have been removed")
	}
}

// TestRemoveProperty_RelationshipBinding verifies that RemoveProperty dispatches
// to DelEdgeProperty when the row carries an expr.RelationshipValue.
func TestRemoveProperty_RelationshipBinding(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	srcID, dstID := mustAddEdge(t, mut, "x", "y", 0)
	if err := mut.SetEdgeProperty("x", "y", "weight", lpg.Float64Value(3.14)); err != nil {
		t.Fatalf("SetEdgeProperty: %v", err)
	}

	schema := map[string]int{"r": 0}
	row := exec.Row{expr.RelationshipValue{
		ID:      2,
		StartID: uint64(srcID),
		EndID:   uint64(dstID),
		Type:    "LINK",
	}}
	src := newSliceOperator(row)
	op := exec.NewRemoveProperty("r", "weight", schema, src, mut)

	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if _, ok := mut.getEdgeProp("x", "y", "weight"); ok {
		t.Error("edge property 'weight' should have been removed")
	}
}
