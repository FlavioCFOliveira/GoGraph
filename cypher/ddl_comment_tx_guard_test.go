package cypher_test

// ddl_comment_tx_guard_test.go — the transaction-guard blast radius of the DDL
// leading-comment fix (task #2227).
//
// ir.IsDDL and ir.IsShow now skip a leading comment, and both feed the explicit
// transaction guards in cypher/exectx.go:
//
//	read-only tx:  reject when queryHasWritingClause(q) || (IsDDL(q) && !IsShow(q))
//	write tx:      permit when IsShow(q); reject other DDL as non-transactional
//
// Changing the classifier therefore changes which statements those guards admit.
// These tests pin that a commented statement is classified exactly as its
// uncommented counterpart, so the fix cannot have widened what a read-only
// transaction accepts.
//
// Two of the four cases improved rather than merely held: a commented
// SHOW INDEXES is now permitted in a read-only transaction (it previously fell
// through to the DML parser and failed), and a commented CREATE INDEX in a write
// transaction now reports "not allowed inside an explicit transaction" instead of
// an opaque parse error.

import (
	"context"
	"strings"
	"testing"
)

// TestDDLLeadingComment_ReadOnlyTxGuard asserts that a leading comment does not
// change what a read-only transaction admits.
func TestDDLLeadingComment_ReadOnlyTxGuard(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		query     string
		wantErrIs string // "" means the statement must be accepted
	}{
		{"show", `SHOW INDEXES`, ""},
		{"show commented", "// list them\nSHOW INDEXES", ""},
		{"show block comment", "/* list them */ SHOW INDEXES", ""},
		{"schema write", `CREATE INDEX zz FOR (n:P) ON (n.x)`, "read-only transaction"},
		{"schema write commented", "// make one\nCREATE INDEX zz FOR (n:P) ON (n.x)", "read-only transaction"},
		{"drop commented", "// remove it\nDROP INDEX zz", "read-only transaction"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := newExistsGraph(t)
			tx, err := eng.BeginReadTx(context.Background())
			if err != nil {
				t.Fatalf("BeginReadTx: %v", err)
			}
			defer func() { _ = tx.Rollback() }()

			_, err = tx.Exec(tc.query, nil)
			switch {
			case tc.wantErrIs == "" && err != nil:
				t.Errorf("%q: rejected in a read-only transaction, want accepted: %v", tc.query, err)
			case tc.wantErrIs != "" && err == nil:
				t.Errorf("%q: accepted in a read-only transaction, want rejected (%s)", tc.query, tc.wantErrIs)
			case tc.wantErrIs != "" && !strings.Contains(err.Error(), tc.wantErrIs):
				t.Errorf("%q: rejected for the wrong reason; want %q, got: %v", tc.query, tc.wantErrIs, err)
			}
		})
	}
}

// TestDDLLeadingComment_WriteTxGuard asserts that a commented schema write
// inside an explicit transaction is rejected as non-transactional DDL — the
// same classification, and the same message, as the uncommented form.
func TestDDLLeadingComment_WriteTxGuard(t *testing.T) {
	t.Parallel()

	for _, q := range []string{
		`CREATE INDEX zz FOR (n:P) ON (n.x)`,
		"// make one\nCREATE INDEX zz FOR (n:P) ON (n.x)",
		"/* make one */ CREATE INDEX zz FOR (n:P) ON (n.x)",
		"// drop it\nDROP INDEX zz",
	} {
		eng := newExistsGraph(t)
		tx, err := eng.BeginTx(context.Background())
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		_, err = tx.Exec(q, nil)
		if err == nil {
			t.Errorf("%q: accepted inside an explicit transaction; DDL is not transactional", q)
		} else if !strings.Contains(err.Error(), "not allowed inside an explicit transaction") {
			t.Errorf("%q: rejected for the wrong reason: %v", q, err)
		}
		_ = tx.Rollback()
	}
}

// TestDDLLeadingComment_ShowInWriteTx asserts that SHOW — a pure read — stays
// permitted inside an explicit write transaction whether or not it carries a
// leading comment.
func TestDDLLeadingComment_ShowInWriteTx(t *testing.T) {
	t.Parallel()

	for _, q := range []string{
		`SHOW INDEXES`,
		"// list them\nSHOW INDEXES",
		`SHOW CONSTRAINTS`,
		"// list them\nSHOW CONSTRAINTS",
	} {
		eng := newExistsGraph(t)
		tx, err := eng.BeginTx(context.Background())
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		res, err := tx.Exec(q, nil)
		if err != nil {
			t.Errorf("%q: rejected inside an explicit transaction; SHOW is a pure read: %v", q, err)
		} else {
			for res.Next() {
			}
			if e := res.Err(); e != nil {
				t.Errorf("%q: iteration failed: %v", q, e)
			}
			_ = res.Close()
		}
		_ = tx.Rollback()
	}
}
