package exec_test

// coverage_ext_test.go — targeted coverage tests for exec package functions
// that are not exercised by the operator integration suite.
//
// Targets:
//   - ConstraintViolationError.Error() (constraints.go:75)
//   - ConstraintRegistry.ListConstraintRows (constraints.go:268)
//   - Eager operator: Init / Next / Close (eager.go)

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// ConstraintViolationError
// ─────────────────────────────────────────────────────────────────────────────

func TestConstraintViolationError_Error(t *testing.T) {
	t.Parallel()
	err := &exec.ConstraintViolationError{
		Kind:     "UNIQUE",
		Label:    "Person",
		Property: "email",
		Detail:   "value 'alice@example.com' already exists",
	}
	msg := err.Error()
	for _, want := range []string{"UNIQUE", "Person", "email", "alice@example.com"} {
		if !containsStr(msg, want) {
			t.Errorf("Error() = %q, want it to contain %q", msg, want)
		}
	}
	if !errors.Is(err, exec.ErrConstraintViolation) {
		t.Error("errors.Is chain to ErrConstraintViolation failed")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ConstraintRegistry.ListConstraintRows
// ─────────────────────────────────────────────────────────────────────────────

func TestConstraintRegistry_ListConstraintRows_Empty(t *testing.T) {
	t.Parallel()
	reg := exec.NewConstraintRegistry()
	rows := reg.ListConstraintRows()
	if len(rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(rows))
	}
}

func TestConstraintRegistry_ListConstraintRows_UniqueAndNotNull(t *testing.T) {
	t.Parallel()
	reg := exec.NewConstraintRegistry()
	reg.RegisterUnique("Person", "email", "__uniq__Person.email")
	reg.RegisterNotNull("Post", "title")

	rows := reg.ListConstraintRows()
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}

	typesByKey := make(map[string]string)
	for _, row := range rows {
		key := string(row[0].(expr.StringValue))
		typ := string(row[1].(expr.StringValue))
		typesByKey[key] = typ
	}

	if typesByKey["Person.email"] != "UNIQUE" {
		t.Errorf("Person.email type = %q, want UNIQUE", typesByKey["Person.email"])
	}
	if typesByKey["Post.title"] != "NOT_NULL" {
		t.Errorf("Post.title type = %q, want NOT_NULL", typesByKey["Post.title"])
	}
}

func TestConstraintRegistry_ListConstraintRows_DeterministicOrder(t *testing.T) {
	t.Parallel()
	reg := exec.NewConstraintRegistry()
	reg.RegisterUnique("Z", "prop", "Z.prop")
	reg.RegisterUnique("A", "name", "A.name")
	reg.RegisterNotNull("M", "email")

	rows := reg.ListConstraintRows()
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}

	keys := make([]string, len(rows))
	for i, row := range rows {
		keys[i] = string(row[0].(expr.StringValue))
	}
	for i := 1; i < len(keys); i++ {
		if keys[i] < keys[i-1] {
			t.Errorf("rows not sorted: %q < %q at index %d", keys[i], keys[i-1], i)
		}
	}
}

func TestConstraintRegistry_ListConstraintRows_LabelOnly(t *testing.T) {
	t.Parallel()
	reg := exec.NewConstraintRegistry()
	reg.RegisterUnique("NoDot", "", "NoDot")

	rows := reg.ListConstraintRows()
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	label := string(rows[0][2].(expr.StringValue))
	prop := string(rows[0][3].(expr.StringValue))
	if label != "NoDot" {
		t.Errorf("label = %q, want NoDot", label)
	}
	if prop != "" {
		t.Errorf("prop = %q, want empty", prop)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Eager operator
// ─────────────────────────────────────────────────────────────────────────────

func TestEager_EmitsAllRows(t *testing.T) {
	t.Parallel()
	rows := []exec.Row{
		{expr.IntegerValue(1)},
		{expr.IntegerValue(2)},
		{expr.IntegerValue(3)},
	}
	child := newStaticOp(rows...)
	eager := exec.NewEager(child)

	ctx := context.Background()
	if err := eager.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	var got []exec.Row
	var row exec.Row
	for {
		ok, err := eager.Next(&row)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		cp := make(exec.Row, len(row))
		copy(cp, row)
		got = append(got, cp)
	}

	if len(got) != len(rows) {
		t.Fatalf("got %d rows, want %d", len(got), len(rows))
	}
	for i, r := range got {
		if r[0] != rows[i][0] {
			t.Errorf("row[%d] = %v, want %v", i, r[0], rows[i][0])
		}
	}
	if err := eager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestEager_EmptyChild(t *testing.T) {
	t.Parallel()
	eager := exec.NewEager(newStaticOp())
	if err := eager.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var row exec.Row
	ok, err := eager.Next(&row)
	if err != nil || ok {
		t.Errorf("Next on empty Eager: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	_ = eager.Close()
}

func TestEager_ContextCancelDuringIteration(t *testing.T) {
	t.Parallel()
	rows := make([]exec.Row, 5)
	for i := range rows {
		rows[i] = exec.Row{expr.IntegerValue(int64(i))}
	}
	eager := exec.NewEager(newStaticOp(rows...))

	ctx, cancel := context.WithCancel(context.Background())
	if err := eager.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	var row exec.Row
	ok, err := eager.Next(&row)
	if !ok || err != nil {
		t.Fatalf("first Next: ok=%v err=%v, want ok=true err=nil", ok, err)
	}

	cancel()

	_, err = eager.Next(&row)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Next after cancel: err=%v, want context.Canceled", err)
	}
	_ = eager.Close()
}

func TestEager_InitChildError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("child init boom")
	eager := exec.NewEager(&errInitOp{err: sentinel})
	err := eager.Init(context.Background())
	if !errors.Is(err, sentinel) {
		t.Errorf("Init error = %v, want %v", err, sentinel)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

type staticOp struct {
	rows []exec.Row
	idx  int
}

func newStaticOp(rows ...exec.Row) *staticOp { return &staticOp{rows: rows} }

func (s *staticOp) Init(_ context.Context) error { s.idx = 0; return nil }
func (s *staticOp) Close() error                 { return nil }
func (s *staticOp) Next(out *exec.Row) (bool, error) {
	if s.idx >= len(s.rows) {
		return false, nil
	}
	*out = s.rows[s.idx]
	s.idx++
	return true, nil
}

type errInitOp struct{ err error }

func (e *errInitOp) Init(_ context.Context) error   { return e.err }
func (e *errInitOp) Close() error                   { return nil }
func (e *errInitOp) Next(_ *exec.Row) (bool, error) { return false, nil }

func containsStr(s, sub string) bool {
	return sub == "" || (len(s) >= len(sub) && findStr(s, sub))
}

func findStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
