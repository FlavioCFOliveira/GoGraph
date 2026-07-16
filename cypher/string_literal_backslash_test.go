package cypher_test

// string_literal_backslash_test.go — regression tests for the production-
// readiness audit finding [S1] (rmp #2033).
//
// The AST printer (ast.StringLiteral.String) escaped only the single quote,
// never the backslash, so a string value ending in a backslash printed an
// unescaped trailing backslash ("'x\'"). The IR stringify -> reparse round trip
// used by CREATE/MERGE/SET then treated the real closing quote as escaped and
// desynced: sibling properties were silently corrupted or dropped, or the whole
// map was lost — an ACID Consistency breach reachable from untrusted query text.
//
// Each test READS THE STORED VALUE BACK and must FAIL on the pre-fix code
// (corrupted/dropped value) and PASS after (backslash-bearing values round-trip
// losslessly with all siblings intact).

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// writeMust executes a write statement via RunInTx and fatals on error.
func writeMust(t *testing.T, eng *cypher.Engine, q string) {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("RunInTx %q: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("drain %q: %v", q, err)
	}
}

// TestStringLiteral_BackslashTerminated_CreateKeepsSiblings: a value ending in
// a backslash must not swallow the following property. Pre-fix: b was dropped
// and a was corrupted.
func TestStringLiteral_BackslashTerminated_CreateKeepsSiblings(t *testing.T) {
	t.Parallel()
	eng, _ := newPlainEngine(t)
	// Cypher source 'x\\' encodes the single-backslash value "x\".
	writeMust(t, eng, `CREATE (n:A {a: 'x\\', b: 'y'})`)

	row := singleRow(t, eng, `MATCH (n:A) RETURN n.a AS a, n.b AS b`)
	wantString(t, row, "a", `x\`)
	wantString(t, row, "b", "y")
}

// TestStringLiteral_BackslashRun_RoundTrips: an even run of backslashes must
// round-trip exactly (the odd/even counting in the reparser and the single-pass
// unescape must agree with the printer).
func TestStringLiteral_BackslashRun_RoundTrips(t *testing.T) {
	t.Parallel()
	eng, _ := newPlainEngine(t)
	// 'x\\\\' encodes "x\\" (two backslashes); trailing sibling must survive.
	writeMust(t, eng, `CREATE (n:B {a: 'x\\\\', b: 'z'})`)

	row := singleRow(t, eng, `MATCH (n:B) RETURN n.a AS a, n.b AS b`)
	wantString(t, row, "a", `x\\`)
	wantString(t, row, "b", "z")
}

// TestStringLiteral_BackslashThenNumericSibling: pre-fix this dropped the whole
// map to {} silently (the desync consumed the numeric sibling too).
func TestStringLiteral_BackslashThenNumericSibling(t *testing.T) {
	t.Parallel()
	eng, _ := newPlainEngine(t)
	writeMust(t, eng, `CREATE (n:Q {a: 'p\\', b: 5})`)

	row := singleRow(t, eng, `MATCH (n:Q) RETURN n.a AS a, n.b AS b`)
	wantString(t, row, "a", `p\`)
	b, ok := row["b"].(expr.IntegerValue)
	if !ok || int64(b) != 5 {
		t.Fatalf("b = %v (want 5); the numeric sibling was dropped by the desync", row["b"])
	}
}

// TestStringLiteral_Backslash_SetReplaceKeepsSiblings: the fresh whole-entity
// SET path shares the same reparser and must round-trip too.
func TestStringLiteral_Backslash_SetReplaceKeepsSiblings(t *testing.T) {
	t.Parallel()
	eng, _ := newPlainEngine(t)
	writeMust(t, eng, `CREATE (n:S {seed: 1})`)
	writeMust(t, eng, `MATCH (n:S) SET n = {a: 'x\\', b: 'y'}`)

	row := singleRow(t, eng, `MATCH (n:S) RETURN n.a AS a, n.b AS b`)
	wantString(t, row, "a", `x\`)
	wantString(t, row, "b", "y")
}

// TestStringLiteral_Backslash_SetAppendKeepsSiblings: SET += variant.
func TestStringLiteral_Backslash_SetAppendKeepsSiblings(t *testing.T) {
	t.Parallel()
	eng, _ := newPlainEngine(t)
	writeMust(t, eng, `CREATE (n:T {seed: 1})`)
	writeMust(t, eng, `MATCH (n:T) SET n += {a: 'x\\', b: 'y'}`)

	row := singleRow(t, eng, `MATCH (n:T) RETURN n.a AS a, n.b AS b, n.seed AS seed`)
	wantString(t, row, "a", `x\`)
	wantString(t, row, "b", "y")
}

// TestStringLiteral_Backslash_EmbeddedQuote: a value with an escaped single
// quote and a trailing backslash must round-trip losslessly.
func TestStringLiteral_Backslash_EmbeddedQuote(t *testing.T) {
	t.Parallel()
	eng, _ := newPlainEngine(t)
	// 'a\'b\\' encodes the value  a'b\  (apostrophe in the middle, backslash at end).
	writeMust(t, eng, `CREATE (n:E {a: 'a\'b\\', b: 'end'})`)

	row := singleRow(t, eng, `MATCH (n:E) RETURN n.a AS a, n.b AS b`)
	wantString(t, row, "a", `a'b\`)
	wantString(t, row, "b", "end")
}

// TestStringLiteral_Backslash_MergeGuardNotFooled: MERGE on a backslash-
// terminated value must MATCH its own prior CREATE (the null-literal guard must
// not be desynced by the backslash), i.e. no duplicate is created.
func TestStringLiteral_Backslash_MergeGuardNotFooled(t *testing.T) {
	t.Parallel()
	eng, _ := newPlainEngine(t)
	writeMust(t, eng, `CREATE (n:M {p: 'x\\'})`)
	writeMust(t, eng, `MERGE (n:M {p: 'x\\'})`)

	// count must be exactly 1: MERGE matched the existing node, no duplicate.
	cnt := singleRow(t, eng, `MATCH (n:M) RETURN count(n) AS c`)
	c, ok := cnt["c"].(expr.IntegerValue)
	if !ok || int64(c) != 1 {
		t.Fatalf("count(:M) = %v, want 1 (MERGE must match, not duplicate)", cnt["c"])
	}
	// singleRow also asserts exactly one row here, and the value round-trips.
	row := singleRow(t, eng, `MATCH (n:M) RETURN n.p AS p`)
	wantString(t, row, "p", `x\`)
}
