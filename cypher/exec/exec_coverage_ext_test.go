//go:build soak

package exec_test

// exec_coverage_ext_test.go — targeted tests for low-coverage branches in:
//   - mergeProps (create_node.go ~13%)
//   - lpgPropToExprBinding (create_relationship.go ~22%)
//   - decodeTemporalBinding (create_relationship.go ~29%)
//   - detachDeletePath (detach_delete.go ~0%)
//   - DetachDelete.Next (detach_delete.go ~42%)
//   - CreateRelationship.Next (create_relationship.go ~65%)

import (
	"context"
	"testing"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"gograph/cypher/exec"
	"gograph/cypher/expr"
	"gograph/graph"
	"gograph/graph/index/label"
	"gograph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// mergeProps — exercises via CreateNode.WithPropsEvalFn
//
// mergeProps is called when propsExprFn != nil. We set up a PropsEvalFn that
// returns dynamic entries and confirm they override static ones and that
// the static-only entries survive for non-overridden keys.
// ─────────────────────────────────────────────────────────────────────────────

// TestCreateNode_MergeProps_DynamicOverridesStatic exercises the
// dynamic-wins branch in mergeProps: a static `{name: "Alice"}` is
// overridden by a dynamic entry `{name: "Bob"}`, while the static
// `{age: 30}` key survives because the dynamic evaluator does not
// include it.
func TestCreateNode_MergeProps_DynamicOverridesStatic(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()

	src := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op, err := exec.NewCreateNode(
		"n",
		[]string{"Person"},
		`{name: "Alice", age: 30}`,
		src,
		mut,
	)
	if err != nil {
		t.Fatalf("NewCreateNode: %v", err)
	}

	// Attach a PropsEvalFn that returns {name: "Bob"} — must override "Alice".
	op.WithPropsEvalFn(func(_ exec.Row) []exec.PropEntry {
		return []exec.PropEntry{
			{Key: "name", Value: lpg.StringValue("Bob")},
		}
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}

	nodeIDVal, ok := rows[0][1].(expr.IntegerValue)
	if !ok {
		t.Fatalf("column 1 must be IntegerValue, got %T", rows[0][1])
	}
	nodeKey, resolved := mut.ResolveNodeLabel(graph.NodeID(nodeIDVal))
	if !resolved {
		t.Fatal("created NodeID not resolvable")
	}

	// Dynamic entry wins.
	pv, ok := mut.getProp(nodeKey, "name")
	if !ok {
		t.Fatal("property 'name' not set")
	}
	if s, _ := pv.String(); s != "Bob" {
		t.Errorf("name = %q, want Bob (dynamic override)", s)
	}
	// Static entry that was NOT overridden must still be present.
	pv, ok = mut.getProp(nodeKey, "age")
	if !ok {
		t.Fatal("property 'age' should survive (not overridden by dynamic entry)")
	}
	if i, _ := pv.Int64(); i != 30 {
		t.Errorf("age = %d, want 30", i)
	}
}

// TestCreateNode_MergeProps_EmptyDynamic confirms that when the PropsEvalFn
// returns an empty slice, the static properties are used unchanged (no
// allocation, no override).
func TestCreateNode_MergeProps_EmptyDynamic(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()

	src := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op, err := exec.NewCreateNode(
		"n",
		nil,
		`{x: 99}`,
		src,
		mut,
	)
	if err != nil {
		t.Fatalf("NewCreateNode: %v", err)
	}
	op.WithPropsEvalFn(func(_ exec.Row) []exec.PropEntry {
		return nil // empty dynamic — static path is returned unchanged
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	nodeIDVal := rows[0][1].(expr.IntegerValue)
	nodeKey, _ := mut.ResolveNodeLabel(graph.NodeID(nodeIDVal))
	pv, ok := mut.getProp(nodeKey, "x")
	if !ok {
		t.Fatal("property 'x' should be set from static props")
	}
	if i, _ := pv.Int64(); i != 99 {
		t.Errorf("x = %d, want 99", i)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// lpgPropToExprBinding and decodeTemporalBinding
//
// These are exercised via CreateRelationship.Next when the operator has
// properties. The relationship variable is emitted as a RelationshipValue;
// its Properties map is populated by lpgPropToExprBinding for each property.
// ─────────────────────────────────────────────────────────────────────────────

// TestCreateRelationship_PropBinding_ScalarTypes exercises
// lpgPropToExprBinding for the four non-temporal scalar kinds (string, int,
// float, bool). The resulting RelationshipValue.Properties map must carry the
// correctly-typed expr.Value for each key.
func TestCreateRelationship_PropBinding_ScalarTypes(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	srcID := mustAddNode(t, mut, "alice")
	dstID := mustAddNode(t, mut, "bob")

	schema := map[string]int{"a": 0, "b": 1}
	row := exec.Row{
		expr.IntegerValue(int64(srcID)),
		expr.IntegerValue(int64(dstID)),
	}

	// A PropsEvalFn that provides various scalar types so that
	// lpgPropToExprBinding exercises all its switch arms.
	op, err := exec.NewCreateRelationship("a", "b", "r", "KNOWS", "", schema,
		newSliceOperator(row), mut)
	if err != nil {
		t.Fatalf("NewCreateRelationship: %v", err)
	}
	op.WithPropsEvalFn(func(_ exec.Row) []exec.PropEntry {
		return []exec.PropEntry{
			{Key: "name", Value: lpg.StringValue("edge-name")},
			{Key: "count", Value: lpg.Int64Value(7)},
			{Key: "score", Value: lpg.Float64Value(3.14)},
			{Key: "active", Value: lpg.BoolValue(true)},
		}
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	relVal, ok := rows[0][2].(expr.RelationshipValue)
	if !ok {
		t.Fatalf("column 2 must be RelationshipValue, got %T", rows[0][2])
	}
	if sv, ok2 := relVal.Properties["name"].(expr.StringValue); !ok2 || string(sv) != "edge-name" {
		t.Errorf("name = %v, want StringValue(\"edge-name\")", relVal.Properties["name"])
	}
	if iv, ok2 := relVal.Properties["count"].(expr.IntegerValue); !ok2 || int64(iv) != 7 {
		t.Errorf("count = %v, want IntegerValue(7)", relVal.Properties["count"])
	}
	if fv, ok2 := relVal.Properties["score"].(expr.FloatValue); !ok2 || float64(fv) != 3.14 {
		t.Errorf("score = %v, want FloatValue(3.14)", relVal.Properties["score"])
	}
	if bv, ok2 := relVal.Properties["active"].(expr.BoolValue); !ok2 || !bool(bv) {
		t.Errorf("active = %v, want BoolValue(true)", relVal.Properties["active"])
	}
}

// TestCreateRelationship_PropBinding_ListType exercises the PropList arm of
// lpgPropToExprBinding: a list property must produce an expr.ListValue.
func TestCreateRelationship_PropBinding_ListType(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	srcID := mustAddNode(t, mut, "a")
	dstID := mustAddNode(t, mut, "b")

	schema := map[string]int{"a": 0, "b": 1}
	row := exec.Row{
		expr.IntegerValue(int64(srcID)),
		expr.IntegerValue(int64(dstID)),
	}
	op, err := exec.NewCreateRelationship("a", "b", "r", "T", "", schema,
		newSliceOperator(row), mut)
	if err != nil {
		t.Fatalf("NewCreateRelationship: %v", err)
	}
	op.WithPropsEvalFn(func(_ exec.Row) []exec.PropEntry {
		return []exec.PropEntry{
			{Key: "tags", Value: lpg.ListValue([]lpg.PropertyValue{
				lpg.StringValue("x"),
				lpg.StringValue("y"),
			})},
		}
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	relVal := rows[0][2].(expr.RelationshipValue)
	lv, ok := relVal.Properties["tags"].(expr.ListValue)
	if !ok {
		t.Fatalf("tags = %T, want ListValue", relVal.Properties["tags"])
	}
	if len(lv) != 2 {
		t.Errorf("list len = %d, want 2", len(lv))
	}
}

// TestCreateRelationship_PropBinding_TemporalDate exercises decodeTemporalBinding
// for a SOH+0x01-prefixed date string. The RelationshipValue.Properties["d"]
// must carry a DateValue, not a StringValue.
func TestCreateRelationship_PropBinding_TemporalDate(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	srcID := mustAddNode(t, mut, "p")
	dstID := mustAddNode(t, mut, "q")

	schema := map[string]int{"a": 0, "b": 1}
	row := exec.Row{
		expr.IntegerValue(int64(srcID)),
		expr.IntegerValue(int64(dstID)),
	}
	op, err := exec.NewCreateRelationship("a", "b", "r", "T", "", schema,
		newSliceOperator(row), mut)
	if err != nil {
		t.Fatalf("NewCreateRelationship: %v", err)
	}

	// Encode a date as PropString with SOH prefix 0x01 ("date" tag).
	dateStr := "\x01" + "2024-03-15"
	op.WithPropsEvalFn(func(_ exec.Row) []exec.PropEntry {
		return []exec.PropEntry{
			{Key: "d", Value: lpg.StringValue(dateStr)},
		}
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	relVal := rows[0][2].(expr.RelationshipValue)
	_, ok := relVal.Properties["d"].(expr.DateValue)
	if !ok {
		t.Errorf("d = %T %v, want DateValue", relVal.Properties["d"], relVal.Properties["d"])
	}
}

// TestCreateRelationship_PropBinding_TemporalDuration exercises the Duration
// arm (tag 0x06) of decodeTemporalBinding.
func TestCreateRelationship_PropBinding_TemporalDuration(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	srcID := mustAddNode(t, mut, "u")
	dstID := mustAddNode(t, mut, "v")

	schema := map[string]int{"a": 0, "b": 1}
	row := exec.Row{
		expr.IntegerValue(int64(srcID)),
		expr.IntegerValue(int64(dstID)),
	}
	op, err := exec.NewCreateRelationship("a", "b", "r", "T", "", schema,
		newSliceOperator(row), mut)
	if err != nil {
		t.Fatalf("NewCreateRelationship: %v", err)
	}

	// Duration tag is 0x06, body is ISO 8601 duration.
	durationStr := "\x06" + "P1Y2M3DT4H5M6S"
	op.WithPropsEvalFn(func(_ exec.Row) []exec.PropEntry {
		return []exec.PropEntry{
			{Key: "dur", Value: lpg.StringValue(durationStr)},
		}
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	relVal := rows[0][2].(expr.RelationshipValue)
	_, ok := relVal.Properties["dur"].(expr.DurationValue)
	if !ok {
		t.Errorf("dur = %T %v, want DurationValue", relVal.Properties["dur"], relVal.Properties["dur"])
	}
}

// TestCreateRelationship_PropBinding_TemporalLocalDateTime exercises tag 0x02.
func TestCreateRelationship_PropBinding_TemporalLocalDateTime(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	srcID := mustAddNode(t, mut, "e")
	dstID := mustAddNode(t, mut, "f")

	schema := map[string]int{"a": 0, "b": 1}
	row := exec.Row{
		expr.IntegerValue(int64(srcID)),
		expr.IntegerValue(int64(dstID)),
	}
	op, err := exec.NewCreateRelationship("a", "b", "r", "T", "", schema,
		newSliceOperator(row), mut)
	if err != nil {
		t.Fatalf("NewCreateRelationship: %v", err)
	}
	ldtStr := "\x02" + "2024-03-15T10:20:30"
	op.WithPropsEvalFn(func(_ exec.Row) []exec.PropEntry {
		return []exec.PropEntry{
			{Key: "ldt", Value: lpg.StringValue(ldtStr)},
		}
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	relVal := rows[0][2].(expr.RelationshipValue)
	if _, ok := relVal.Properties["ldt"].(expr.LocalDateTimeValue); !ok {
		t.Errorf("ldt = %T, want LocalDateTimeValue", relVal.Properties["ldt"])
	}
}

// TestCreateRelationship_PropBinding_TemporalDateTime exercises tag 0x03.
func TestCreateRelationship_PropBinding_TemporalDateTime(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	srcID := mustAddNode(t, mut, "g")
	dstID := mustAddNode(t, mut, "h")

	schema := map[string]int{"a": 0, "b": 1}
	row := exec.Row{
		expr.IntegerValue(int64(srcID)),
		expr.IntegerValue(int64(dstID)),
	}
	op, err := exec.NewCreateRelationship("a", "b", "r", "T", "", schema,
		newSliceOperator(row), mut)
	if err != nil {
		t.Fatalf("NewCreateRelationship: %v", err)
	}
	dtStr := "\x03" + "2024-03-15T10:20:30+01:00"
	op.WithPropsEvalFn(func(_ exec.Row) []exec.PropEntry {
		return []exec.PropEntry{
			{Key: "dt", Value: lpg.StringValue(dtStr)},
		}
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	relVal := rows[0][2].(expr.RelationshipValue)
	if _, ok := relVal.Properties["dt"].(expr.DateTimeValue); !ok {
		t.Errorf("dt = %T, want DateTimeValue", relVal.Properties["dt"])
	}
}

// TestCreateRelationship_PropBinding_TemporalLocalTime exercises tag 0x04.
func TestCreateRelationship_PropBinding_TemporalLocalTime(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	srcID := mustAddNode(t, mut, "i")
	dstID := mustAddNode(t, mut, "j")

	schema := map[string]int{"a": 0, "b": 1}
	row := exec.Row{
		expr.IntegerValue(int64(srcID)),
		expr.IntegerValue(int64(dstID)),
	}
	op, err := exec.NewCreateRelationship("a", "b", "r", "T", "", schema,
		newSliceOperator(row), mut)
	if err != nil {
		t.Fatalf("NewCreateRelationship: %v", err)
	}
	ltStr := "\x04" + "10:20:30"
	op.WithPropsEvalFn(func(_ exec.Row) []exec.PropEntry {
		return []exec.PropEntry{
			{Key: "lt", Value: lpg.StringValue(ltStr)},
		}
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	relVal := rows[0][2].(expr.RelationshipValue)
	if _, ok := relVal.Properties["lt"].(expr.LocalTimeValue); !ok {
		t.Errorf("lt = %T, want LocalTimeValue", relVal.Properties["lt"])
	}
}

// TestCreateRelationship_PropBinding_TemporalTime exercises tag 0x05.
func TestCreateRelationship_PropBinding_TemporalTime(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	srcID := mustAddNode(t, mut, "k")
	dstID := mustAddNode(t, mut, "l")

	schema := map[string]int{"a": 0, "b": 1}
	row := exec.Row{
		expr.IntegerValue(int64(srcID)),
		expr.IntegerValue(int64(dstID)),
	}
	op, err := exec.NewCreateRelationship("a", "b", "r", "T", "", schema,
		newSliceOperator(row), mut)
	if err != nil {
		t.Fatalf("NewCreateRelationship: %v", err)
	}
	tStr := "\x05" + "10:20:30+01:00"
	op.WithPropsEvalFn(func(_ exec.Row) []exec.PropEntry {
		return []exec.PropEntry{
			{Key: "t", Value: lpg.StringValue(tStr)},
		}
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	relVal := rows[0][2].(expr.RelationshipValue)
	if _, ok := relVal.Properties["t"].(expr.TimeValue); !ok {
		t.Errorf("t = %T, want TimeValue", relVal.Properties["t"])
	}
}

// TestCreateRelationship_NullEndpointWithVar exercises the null-endpoint path
// when a relationship variable is set: the row should be extended with a NULL
// column for the relationship.
func TestCreateRelationship_NullEndpointWithVar(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	srcID := mustAddNode(t, mut, "x")

	schema := map[string]int{"a": 0, "b": 1}
	// b column holds NULL — should trigger the nullRowWithRel path.
	row := exec.Row{
		expr.IntegerValue(int64(srcID)),
		expr.Null,
	}
	op, err := exec.NewCreateRelationship("a", "b", "r", "T", "", schema,
		newSliceOperator(row), mut)
	if err != nil {
		t.Fatalf("NewCreateRelationship: %v", err)
	}

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row (null endpoint), got %d", len(rows))
	}
	// The relVar column must be NULL.
	if len(rows[0]) < 3 {
		t.Fatalf("expected ≥ 3 columns, got %d", len(rows[0]))
	}
	if !expr.IsNull(rows[0][2]) {
		t.Errorf("relVar column = %T %v, want NULL", rows[0][2], rows[0][2])
	}
}

// TestCreateRelationship_NullStartEndpoint confirms null src → NULL relVar.
func TestCreateRelationship_NullStartEndpoint(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()

	schema := map[string]int{"a": 0, "b": 1}
	row := exec.Row{
		expr.Null, // src is null
		expr.IntegerValue(0),
	}
	op, err := exec.NewCreateRelationship("a", "b", "r", "T", "", schema,
		newSliceOperator(row), mut)
	if err != nil {
		t.Fatalf("NewCreateRelationship: %v", err)
	}

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row (null start), got %d", len(rows))
	}
	if !expr.IsNull(rows[0][2]) {
		t.Errorf("relVar column = %T, want NULL for null start", rows[0][2])
	}
}

// TestCreateRelationship_NoRelVar confirms that when relVar is empty the row
// passes through without extension.
func TestCreateRelationship_NoRelVar(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	srcID := mustAddNode(t, mut, "m")
	dstID := mustAddNode(t, mut, "n")

	schema := map[string]int{"a": 0, "b": 1}
	row := exec.Row{
		expr.IntegerValue(int64(srcID)),
		expr.IntegerValue(int64(dstID)),
	}
	op, err := exec.NewCreateRelationship("a", "b", "", "T", "", schema,
		newSliceOperator(row), mut)
	if err != nil {
		t.Fatalf("NewCreateRelationship: %v", err)
	}

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	// No extra column since relVar is empty.
	if len(rows[0]) != 2 {
		t.Errorf("row len = %d, want 2 (no relVar extension)", len(rows[0]))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DetachDelete.Next — additional branches
// ─────────────────────────────────────────────────────────────────────────────

// TestDetachDelete_WithRelationshipValue exercises the RelationshipValue branch
// of the schema-direct path: DETACH DELETE applied to a relationship variable
// should remove the underlying edge and pass the row through.
func TestDetachDelete_WithRelationshipValue(t *testing.T) {
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
		Type:    "T",
	}
	row := exec.Row{rel}
	src := newSliceOperator(row)
	op := exec.NewDetachDelete("r", schema, src, mut)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if mut.HasEdge("a", "b") {
		t.Error("edge a→b should have been removed")
	}
}

// TestDetachDelete_WithTargetEvalFn_Node exercises DetachDelete.Next via the
// targetEvalFn path with a NodeValue target. The node's incident edges and
// labels are stripped.
func TestDetachDelete_WithTargetEvalFn_Node(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	aID := mustAddNode(t, mut, "a")
	bID := mustAddNode(t, mut, "b")
	mustAddEdge(t, mut, "a", "b", 0)
	if err := mut.SetNodeLabel("a", "X"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}

	row := exec.Row{expr.IntegerValue(int64(aID))} // driving row
	src := newSliceOperator(row)
	op := exec.NewDetachDelete("n", map[string]int{}, src, mut)
	op.WithTargetEvalFn(func(_ exec.Row) (expr.Value, error) {
		return expr.NodeValue{ID: uint64(aID)}, nil
	})

	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if mut.HasEdge("a", "b") {
		t.Error("outgoing edge a→b should have been removed")
	}
	if mut.hasLabel("a", "X") {
		t.Error("label X should have been stripped")
	}
	_ = bID // used only to create the edge
}

// TestDetachDelete_WithTargetEvalFn_NullTarget exercises the null-target
// branch: when the targetEvalFn returns Null, the operator passes the row
// through unchanged.
func TestDetachDelete_WithTargetEvalFn_NullTarget(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()

	row := exec.Row{expr.IntegerValue(0)}
	src := newSliceOperator(row)
	op := exec.NewDetachDelete("n", map[string]int{}, src, mut)
	op.WithTargetEvalFn(func(_ exec.Row) (expr.Value, error) {
		return expr.Null, nil
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 pass-through row, got %d", len(rows))
	}
}

// TestDetachDelete_WithTargetEvalFn_RelationshipValue exercises the
// RelationshipValue branch of the targetEvalFn path: the edge must be removed.
func TestDetachDelete_WithTargetEvalFn_RelationshipValue(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	aID := mustAddNode(t, mut, "a")
	bID := mustAddNode(t, mut, "b")
	mustAddEdge(t, mut, "a", "b", 0)

	row := exec.Row{expr.IntegerValue(0)}
	src := newSliceOperator(row)
	op := exec.NewDetachDelete("r", map[string]int{}, src, mut)
	op.WithTargetEvalFn(func(_ exec.Row) (expr.Value, error) {
		return expr.RelationshipValue{StartID: uint64(aID), EndID: uint64(bID)}, nil
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if mut.HasEdge("a", "b") {
		t.Error("edge a→b should have been removed via RelationshipValue target")
	}
}

// TestDetachDelete_WithTargetEvalFn_PathValue exercises the detachDeletePath
// helper (currently 0% own-package coverage) via a PathValue target containing
// two nodes. Both nodes' incident edges and labels must be stripped.
func TestDetachDelete_WithTargetEvalFn_PathValue(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	aID := mustAddNode(t, mut, "a")
	bID := mustAddNode(t, mut, "b")
	cID := mustAddNode(t, mut, "c")
	mustAddEdge(t, mut, "a", "b", 0)
	mustAddEdge(t, mut, "b", "c", 0)
	if err := mut.SetNodeLabel("a", "LA"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := mut.SetNodeLabel("b", "LB"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}

	row := exec.Row{expr.IntegerValue(0)}
	src := newSliceOperator(row)
	op := exec.NewDetachDelete("p", map[string]int{}, src, mut)
	op.WithTargetEvalFn(func(_ exec.Row) (expr.Value, error) {
		return expr.PathValue{
			Nodes: []expr.NodeValue{
				{ID: uint64(aID)},
				{ID: uint64(bID)},
			},
		}, nil
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	// Edges must be gone.
	if mut.HasEdge("a", "b") {
		t.Error("edge a→b should have been removed")
	}
	if mut.HasEdge("b", "c") {
		t.Error("edge b→c should have been removed")
	}
	// Labels must be stripped.
	if mut.hasLabel("a", "LA") {
		t.Error("label LA on a should have been stripped")
	}
	if mut.hasLabel("b", "LB") {
		t.Error("label LB on b should have been stripped")
	}
	_ = cID
}

// TestDetachDelete_SchemaPath_PathValue exercises the schema-direct
// PathValue branch (detachDeletePath called from the schema path, not
// the targetEvalFn path).
func TestDetachDelete_SchemaPath_PathValue(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	aID := mustAddNode(t, mut, "a")
	bID := mustAddNode(t, mut, "b")
	mustAddEdge(t, mut, "a", "b", 0)

	schema := map[string]int{"p": 0}
	pv := expr.PathValue{
		Nodes: []expr.NodeValue{
			{ID: uint64(aID)},
			{ID: uint64(bID)},
		},
	}
	row := exec.Row{pv}
	src := newSliceOperator(row)
	op := exec.NewDetachDelete("p", schema, src, mut)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if mut.HasEdge("a", "b") {
		t.Error("edge a→b should have been removed by schema-path detachDeletePath")
	}
}

// TestDetachDelete_UnresolvedNodeID exercises the branch where the nodeID
// resolves from the schema but the mapper does not know it — the row must
// pass through without error.
func TestDetachDelete_UnresolvedNodeID(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()

	schema := map[string]int{"n": 0}
	// NodeID 9999 was never interned in the mutator.
	row := exec.Row{expr.IntegerValue(9999)}
	src := newSliceOperator(row)
	op := exec.NewDetachDelete("n", schema, src, mut)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row (unresolved NodeID pass-through), got %d", len(rows))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// WithParams setters — CreateRelationship, Merge, Set, DeleteNode (setters)
// ─────────────────────────────────────────────────────────────────────────────

// TestCreateRelationship_WithParams exercises WithParams on CreateRelationship.
func TestCreateRelationship_WithParams(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	srcID := mustAddNode(t, mut, "s")
	dstID := mustAddNode(t, mut, "d")

	schema := map[string]int{"a": 0, "b": 1}
	row := exec.Row{expr.IntegerValue(int64(srcID)), expr.IntegerValue(int64(dstID))}
	op, err := exec.NewCreateRelationship("a", "b", "r", "T", `{w: $weight}`, schema,
		newSliceOperator(row), mut)
	if err != nil {
		t.Fatalf("NewCreateRelationship: %v", err)
	}
	op2, err := op.WithParams(map[string]expr.Value{"weight": expr.FloatValue(1.5)})
	if err != nil {
		t.Fatalf("WithParams: %v", err)
	}

	rows, err := exec.Drain(context.Background(), op2)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
}

// TestMerge_WithParams exercises WithParams on Merge.
func TestMerge_WithParams(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	noMatchFn := func(_ context.Context) ([]exec.Row, error) { return nil, nil }
	schema := map[string]int{}
	src := newSliceOperator()
	op, err := exec.NewMerge("n", []string{"T"}, `{k: $val}`, nil, nil, noMatchFn, schema, src, mut)
	if err != nil {
		t.Fatalf("NewMerge: %v", err)
	}
	op2, err := op.WithParams(map[string]expr.Value{"val": expr.IntegerValue(42)})
	if err != nil {
		t.Fatalf("WithParams: %v", err)
	}

	rows, err := exec.Drain(context.Background(), op2)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
}

// TestDeleteNode_WithTargetEvalFn exercises the WithTargetEvalFn setter on
// DeleteNode (different from DetachDelete). The function must work as a
// pass-through setter — the operator should use the fn on the next Next call.
func TestDeleteNode_WithTargetEvalFn(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	nID := mustAddNode(t, mut, "x")
	schema := map[string]int{}
	src := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewDeleteNode("n", schema, src, mut)
	op.WithTargetEvalFn(func(_ exec.Row) (expr.Value, error) {
		return expr.NodeValue{ID: uint64(nID)}, nil
	})

	_, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
}

// TestSetAllProperties_WithSourceRelCols exercises WithSourceRelCols — a
// one-liner setter. Just verifies it returns the operator (non-nil) and
// doesn't panic.
func TestSetAllProperties_WithSourceRelCols(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	schema := map[string]int{"n": 0}
	src := newSliceOperator()
	op := exec.NewSetAllPropertiesFromEntity("n", "m", false, schema, src, mut)
	op2 := op.WithSourceRelCols(exec.RelCols{SrcCol: 0, DstCol: 1})
	if op2 == nil {
		t.Fatal("WithSourceRelCols returned nil")
	}
}

// TestRemoveProperty_WithRelCols exercises the WithRelCols setter on
// RemoveProperty: the operator must function normally after setting rel cols.
func TestRemoveProperty_WithRelCols(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	aID := mustAddNode(t, mut, "a")
	bID := mustAddNode(t, mut, "b")
	mustAddEdge(t, mut, "a", "b", 0)
	if err := mut.SetEdgeProperty("a", "b", "w", lpg.Int64Value(5)); err != nil {
		t.Fatalf("SetEdgeProperty: %v", err)
	}

	schema := map[string]int{"r": 0}
	rel := exec.RelCols{SrcCol: 1, DstCol: 2}
	row := exec.Row{
		expr.IntegerValue(0), // rel column (ignored since relCols provided)
		expr.IntegerValue(int64(aID)),
		expr.IntegerValue(int64(bID)),
	}
	src := newSliceOperator(row)
	op := exec.NewRemoveProperty("r", "w", schema, src, mut)
	op.WithRelCols(rel)

	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Index adapter constructors — NewInt64RangeIndex, NewStringRangeIndex,
// NewInt64HashIndex
// ─────────────────────────────────────────────────────────────────────────────

// int64RangeAdapter is a minimal test double for btree.Index[int64].
type int64RangeAdapter struct{}

func (a *int64RangeAdapter) Range(lo, hi int64) *roaring64.Bitmap {
	bm := roaring64.New()
	if lo <= 10 && 10 <= hi {
		bm.Add(10)
	}
	return bm
}

// stringRangeAdapter is a minimal test double for btree.Index[string].
type stringRangeAdapter struct{}

func (a *stringRangeAdapter) Range(lo, hi string) *roaring64.Bitmap {
	bm := roaring64.New()
	if lo <= "k" && "k" <= hi {
		bm.Add(1)
	}
	return bm
}

// int64HashAdapter is a minimal test double for hash.Index[int64].
type int64HashAdapter struct{}

func (a *int64HashAdapter) Lookup(value int64) *roaring64.Bitmap {
	bm := roaring64.New()
	if value == 42 {
		bm.Add(42)
	}
	return bm
}

// TestInt64RangeIndex_RangeBitmap exercises NewInt64RangeIndex + RangeBitmap.
func TestInt64RangeIndex_RangeBitmap(t *testing.T) {
	t.Parallel()
	idx := exec.NewInt64RangeIndex(&int64RangeAdapter{})

	// Normal range containing the test value 10.
	bm := idx.RangeBitmap(expr.IntegerValue(5), expr.IntegerValue(15))
	if bm == nil || !bm.Contains(10) {
		t.Errorf("RangeBitmap(5, 15) should contain 10")
	}

	// Null bounds default to min/max int64.
	bm2 := idx.RangeBitmap(nil, nil)
	if bm2 == nil || !bm2.Contains(10) {
		t.Errorf("RangeBitmap(nil, nil) should contain 10")
	}
}

// TestStringRangeIndex_RangeBitmap exercises NewStringRangeIndex + RangeBitmap.
func TestStringRangeIndex_RangeBitmap(t *testing.T) {
	t.Parallel()
	idx := exec.NewStringRangeIndex(&stringRangeAdapter{})

	// Range that includes "k".
	bm := idx.RangeBitmap(expr.StringValue("a"), expr.StringValue("z"))
	if bm == nil || !bm.Contains(1) {
		t.Errorf("RangeBitmap(a, z) should contain 1")
	}

	// Null bounds default to "" / maxStr.
	bm2 := idx.RangeBitmap(nil, nil)
	if bm2 == nil || !bm2.Contains(1) {
		t.Errorf("RangeBitmap(nil, nil) should contain 1")
	}
}

// TestInt64HashIndex_LookupBitmap exercises NewInt64HashIndex + LookupBitmap.
func TestInt64HashIndex_LookupBitmap(t *testing.T) {
	t.Parallel()
	idx := exec.NewInt64HashIndex(&int64HashAdapter{})

	// Correct type — IntegerValue lookup.
	bm, err := idx.LookupBitmap(expr.IntegerValue(42))
	if err != nil {
		t.Fatalf("LookupBitmap(42): %v", err)
	}
	if !bm.Contains(42) {
		t.Errorf("bitmap should contain 42")
	}

	// Wrong type must return ErrIndexTypeMismatch.
	_, err = idx.LookupBitmap(expr.StringValue("not-int"))
	if err == nil {
		t.Fatal("LookupBitmap on wrong type must return an error")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CreateNode.WithParams — exercises parsePropLiteralWithParams
// ─────────────────────────────────────────────────────────────────────────────

// TestCreateNode_WithParams exercises the WithParams path of CreateNode:
// a property map containing a $param reference is re-parsed with the supplied
// parameter map and the resulting node carries the resolved value.
func TestCreateNode_WithParams(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	src := newSliceOperator(exec.Row{expr.IntegerValue(0)})

	op, err := exec.NewCreateNode("n", []string{"X"}, `{name: $nm, age: 25}`, src, mut)
	if err != nil {
		t.Fatalf("NewCreateNode: %v", err)
	}
	op2, err := op.WithParams(map[string]expr.Value{"nm": expr.StringValue("Charlie")})
	if err != nil {
		t.Fatalf("WithParams: %v", err)
	}

	rows, err := exec.Drain(context.Background(), op2)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	nodeID := rows[0][1].(expr.IntegerValue)
	nodeKey, _ := mut.ResolveNodeLabel(graph.NodeID(nodeID))
	pv, ok := mut.getProp(nodeKey, "name")
	if !ok {
		t.Fatal("property 'name' not set")
	}
	if s, _ := pv.String(); s != "Charlie" {
		t.Errorf("name = %q, want Charlie", s)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Merge.WithParams and Merge.WithPropsEvalFn — exercises the merge params path
// ─────────────────────────────────────────────────────────────────────────────

// TestMerge_WithPropsEvalFn exercises the WithPropsEvalFn path of Merge:
// the dynamic property evaluator is called per row and its values are used
// for the ON CREATE property assignment.
func TestMerge_WithPropsEvalFn(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()

	noMatchFn := func(_ context.Context) ([]exec.Row, error) { return nil, nil }
	schema := map[string]int{}
	src := newSliceOperator()
	op, err := exec.NewMerge("n", []string{"T"}, `{}`, nil, nil, noMatchFn, schema, src, mut)
	if err != nil {
		t.Fatalf("NewMerge: %v", err)
	}

	// Attach a dynamic property evaluator.
	op.WithPropsEvalFn(func(_ exec.Row) []exec.PropEntry {
		return []exec.PropEntry{
			{Key: "dynamic", Value: lpg.Int64Value(999)},
		}
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if mut.nodeCount() != 1 {
		t.Errorf("expected 1 node created, got %d", mut.nodeCount())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// SetAllProperties — NewSetAllPropertiesFromParam and param-based paths
// ─────────────────────────────────────────────────────────────────────────────

// TestSetAllProperties_FromParam exercises NewSetAllPropertiesFromParam: the
// operator reads the named parameter as a MapValue and writes each key/value
// pair to the target node.
func TestSetAllProperties_FromParam(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	nID := mustAddNode(t, mut, "alice")

	schema := map[string]int{"n": 0}
	row := exec.Row{expr.IntegerValue(int64(nID))}
	src := newSliceOperator(row)

	op := exec.NewSetAllPropertiesFromParam("n", "props", false, schema, src, mut)
	_, err := op.WithParams(map[string]expr.Value{
		"props": expr.MapValue{
			"age":    expr.IntegerValue(30),
			"active": expr.BoolValue(true),
		},
	})
	if err != nil {
		t.Fatalf("WithParams: %v", err)
	}

	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	pv, ok := mut.getProp("alice", "age")
	if !ok {
		t.Fatal("property 'age' not set")
	}
	if i, _ := pv.Int64(); i != 30 {
		t.Errorf("age = %d, want 30", i)
	}
}

// TestSetAllProperties_FromParam_NullKey exercises the null-in-map-removes
// semantics: a null value in the parameter map must delete the corresponding
// property from the target.
func TestSetAllProperties_FromParam_NullKey(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	nID := mustAddNode(t, mut, "bob")
	if err := mut.SetNodeProperty("bob", "temp", lpg.StringValue("remove-me")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	schema := map[string]int{"n": 0}
	row := exec.Row{expr.IntegerValue(int64(nID))}
	src := newSliceOperator(row)

	// isReplace=true clears existing props, then applies the map (which has a
	// null entry), so temp should be gone and x should be set.
	op := exec.NewSetAllPropertiesFromParam("n", "p", true, schema, src, mut)
	_, err := op.WithParams(map[string]expr.Value{
		"p": expr.MapValue{
			"x":    expr.IntegerValue(7),
			"temp": expr.Null,
		},
	})
	if err != nil {
		t.Fatalf("WithParams: %v", err)
	}

	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if _, ok := mut.getProp("bob", "temp"); ok {
		t.Error("temp property should have been removed by null assignment")
	}
	pv, ok := mut.getProp("bob", "x")
	if !ok {
		t.Fatal("property 'x' not set")
	}
	if i, _ := pv.Int64(); i != 7 {
		t.Errorf("x = %d, want 7", i)
	}
}

// TestLPGLabelSource_ResolveLabelBitmap exercises NewLPGLabelSource and
// ResolveLabelBitmap: the label source must return the bitmap from the
// underlying label.Index when the label name resolves, and an empty bitmap
// when it does not.
func TestLPGLabelSource_ResolveLabelBitmap(t *testing.T) {
	t.Parallel()
	reg := label.NewIndex()

	// Intern label ID 1 and add two nodes to it.
	reg.Add(1, 10)
	reg.Add(1, 20)

	// lookupFn resolves "Person" → id=1, anything else → not found.
	lookupFn := func(name string) (uint32, bool) {
		if name == "Person" {
			return 1, true
		}
		return 0, false
	}

	src := exec.NewLPGLabelSource(reg, lookupFn)

	// Known label — bitmap must contain both nodes.
	bm := src.ResolveLabelBitmap("Person")
	if bm == nil || bm.IsEmpty() {
		t.Fatal("ResolveLabelBitmap(Person) returned empty bitmap")
	}

	// Unknown label — bitmap must be non-nil but empty.
	bm2 := src.ResolveLabelBitmap("Ghost")
	if bm2 == nil || !bm2.IsEmpty() {
		t.Errorf("ResolveLabelBitmap(Ghost) should return empty bitmap, got %v", bm2)
	}
}
