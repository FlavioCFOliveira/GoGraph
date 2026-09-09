package main

import (
	"strings"
	"testing"
)

// newTestInventory builds an inventory the way buildInventory would, so the
// helpers under test see the same shape they see in production.
func newTestInventory(decls ...decl) *inventory {
	inv := &inventory{byName: map[string][]int{}, files: map[string]struct{}{}}
	for i := range decls {
		d := decls[i]
		inv.files[d.File] = struct{}{}
		inv.goFiles++
		inv.add(&d)
	}
	return inv
}

// TestSymbolLabelPredicateBracketsItsOrChain is the regression test for the
// worst defect this gate has produced.
//
// Cypher binds AND tighter than OR. An unbracketed `x:A OR x:B OR x:C`, spliced
// into `WHERE <chain> AND x.name = d.n`, reassociates to
// `x:A OR x:B OR (x:C AND x.name = d.n)`: every node bearing any label but the
// last satisfies the predicate regardless of the identity test. A SET behind
// that predicate, driven by UNWIND, applied the LAST row's value to every such
// node — measured on the live graph as 11652 of 12677 symbol nodes having their
// `pkg` overwritten with a single package path by one statement.
//
// Reproduced on the engine before the fix, on a three-node fixture: the
// unbracketed form wrote 2 properties where 1 was intended, and a Function node
// whose name did not match took the value.
func TestSymbolLabelPredicateBracketsItsOrChain(t *testing.T) {
	got := symbolLabelPredicate("x")
	if !strings.Contains(got, " OR ") {
		t.Fatalf("predicate %q has no OR chain; this test can no longer detect reassociation", got)
	}
	if !strings.HasPrefix(got, "(") || !strings.HasSuffix(got, ")") {
		t.Fatalf("symbolLabelPredicate returned %q, which is not bracketed: conjoining it "+
			"with an identity test reassociates and matches every node but the last label's", got)
	}
	// The bracket must enclose the WHOLE chain, not merely appear at the ends.
	// "(a OR b) OR (c OR d)" also starts and ends with a bracket while leaving
	// the final term exposed once conjoined.
	depth := 0
	for i, r := range got {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && i != len(got)-1 {
			t.Fatalf("predicate %q closes its outer bracket at byte %d, before the end; "+
				"the trailing terms are outside it and will reassociate", got, i)
		}
	}
}

// TestRepairStatementConjoinsABracketedLabelGroup checks the composition site
// rather than the helper, because that is where the defect actually bit: the
// helper was used correctly by every existing caller, all of which made it the
// entire WHERE clause, and the first caller to AND it with anything inherited
// the reassociation.
func TestRepairStatementConjoinsABracketedLabelGroup(t *testing.T) {
	inv := newTestInventory(
		decl{Kind: kindFunction, Name: "Alpha", File: "pkga/a.go", Dir: "pkga", PkgName: "pkga", ImportPath: "m/pkga"},
	)
	// A node whose pkg is wrong: it matches the declaration by key, so the
	// repair pass must re-point it at the declaration's import path.
	nodes := []symNode{{Label: "Function", Name: "Alpha", Pkg: "m/wrong", File: "pkga/a.go"}}

	repair, _ := buildSync(inv, nodes, "deadbeef", "2026-09-08")
	if len(repair) == 0 {
		t.Fatal("buildSync emitted no repair statement for a node whose pkg contradicts the tree")
	}
	stmt := repair[0]

	// The expectation is spelled out here rather than taken from
	// symbolLabelPredicate. Deriving it from the function under test would move
	// both sides of the comparison together, and the assertion could never fail
	// — which is what the first version of this test did.
	const wantOpen = "WHERE (x:"
	if !strings.Contains(stmt, wantOpen) {
		t.Fatalf("repair statement does not open its WHERE with a bracketed label group "+
			"(want %q):\n%s", wantOpen, stmt)
	}
	where := stmt[strings.Index(stmt, "WHERE ")+len("WHERE "):]
	close := strings.Index(where, ")")
	and := strings.Index(where, " AND ")
	if close < 0 || and < 0 {
		t.Fatalf("repair statement has no bracketed group followed by a conjunct:\n%s", stmt)
	}
	if close > and {
		t.Fatalf("the label group's closing bracket falls AFTER the first AND, so the OR "+
			"chain is not closed before the identity test and will reassociate:\n%s", stmt)
	}
	if strings.Contains(where[:close], " AND ") {
		t.Fatalf("an identity test sits inside the label group:\n%s", stmt)
	}
}

// TestBuildSyncKeysMethodsOnTheirReceiver guards the other way a single
// statement can silently lose data. Two methods of the same name on different
// receivers share a file legitimately — 531 such pairs exist in this tree — so
// a MERGE keyed on (name, pkg, file) binds the second to the first and writes
// one node where two are owed.
func TestBuildSyncKeysMethodsOnTheirReceiver(t *testing.T) {
	inv := newTestInventory(
		decl{Kind: kindType, Name: "Anchor", File: "pkga/a.go", Dir: "pkga", PkgName: "pkga", ImportPath: "m/pkga"},
		decl{Kind: kindMethod, Name: "Close", Recv: "Reader", File: "pkga/a.go", Dir: "pkga", PkgName: "pkga", ImportPath: "m/pkga"},
		decl{Kind: kindMethod, Name: "Close", Recv: "Writer", File: "pkga/a.go", Dir: "pkga", PkgName: "pkga", ImportPath: "m/pkga"},
	)
	// One node, so the package counts as modelled and the create pass runs.
	nodes := []symNode{{Label: "Type", Name: "Anchor", Pkg: "m/pkga", File: "pkga/a.go"}}

	_, create := buildSync(inv, nodes, "deadbeef", "2026-09-08")
	var methodStmt string
	for _, s := range create {
		if strings.Contains(s, "MERGE (x:Method ") {
			methodStmt = s
		}
	}
	if methodStmt == "" {
		t.Fatal("buildSync emitted no Method create statement for two un-noded methods")
	}
	if !strings.Contains(methodStmt, "recv:d.r") {
		t.Errorf("Method MERGE key omits the receiver, so two methods sharing a name and "+
			"file collapse to one node: %s", methodStmt)
	}
	if got := strings.Count(methodStmt, "{n:'Close'"); got != 2 {
		t.Errorf("Method statement carries %d Close rows, want 2 (one per receiver): %s", got, methodStmt)
	}
}

// TestBuildSyncSkipsPackagesTheGraphDoesNotModel holds the sync to rmp #2719's
// stated scope, which excludes modelling packages the graph does not cover.
func TestBuildSyncSkipsPackagesTheGraphDoesNotModel(t *testing.T) {
	inv := newTestInventory(
		decl{Kind: kindFunction, Name: "Modelled", File: "pkga/a.go", Dir: "pkga", PkgName: "pkga", ImportPath: "m/pkga"},
		decl{Kind: kindFunction, Name: "Anchor", File: "pkga/a.go", Dir: "pkga", PkgName: "pkga", ImportPath: "m/pkga"},
		decl{Kind: kindFunction, Name: "Untouched", File: "pkgb/b.go", Dir: "pkgb", PkgName: "pkgb", ImportPath: "m/pkgb"},
	)
	nodes := []symNode{{Label: "Function", Name: "Anchor", Pkg: "m/pkga", File: "pkga/a.go"}}

	_, create := buildSync(inv, nodes, "deadbeef", "2026-09-08")
	all := strings.Join(create, "\n")
	if !strings.Contains(all, "'Modelled'") {
		t.Error("a declaration in a modelled package was not emitted")
	}
	if strings.Contains(all, "'Untouched'") {
		t.Error("a declaration in an unmodelled package was emitted; rmp #2719 puts that out of scope")
	}
}

// TestCypherStringEscapesBackslashBeforeQuote pins the escape order. Escaping
// the quote first would leave the backslash it introduces to be escaped by the
// backslash pass, doubling it and changing the stored value.
func TestCypherStringEscapesBackslashBeforeQuote(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`plain`, `'plain'`},
		{`it's`, `'it\'s'`},
		{`a\b`, `'a\\b'`},
		{`a\'b`, `'a\\\'b'`},
	} {
		if got := cypherString(tc.in); got != tc.want {
			t.Errorf("cypherString(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// TestDeclKeySeparatesMethodsSharingFileAndName is the unit half of the
// coverage-accounting fix: keyed on (file, name) alone, two such methods score
// as one covered declaration and a package reports parity it has not reached.
func TestDeclKeySeparatesMethodsSharingFileAndName(t *testing.T) {
	a := decl{Kind: kindMethod, Name: "Close", Recv: "Reader", File: "pkga/a.go"}
	b := decl{Kind: kindMethod, Name: "Close", Recv: "Writer", File: "pkga/a.go"}
	if a.key() == b.key() {
		t.Fatalf("two methods sharing a file and name collide on key %q", a.key())
	}
	n := symNode{Label: "Method", Name: "Close", Recv: "Reader", File: "pkga/a.go"}
	if n.key() != a.key() {
		t.Errorf("node key %q does not match the declaration it names, %q", n.key(), a.key())
	}
}
