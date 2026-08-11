package parser

import (
	"testing"
)

// TestStripLiterals_HoistsAndCollapses is the property the change exists for:
// two queries differing ONLY in a hoisted literal must produce the same
// rewritten text, because that text is the plan-cache key.
func TestStripLiterals_HoistsAndCollapses(t *testing.T) {
	a, pa, oka := StripLiterals(`MATCH (p:Person {sid: 'p00000042'}) RETURN p.name AS name`)
	b, pb, okb := StripLiterals(`MATCH (p:Person {sid: 'p00000099'}) RETURN p.name AS name`)

	if !oka || !okb {
		t.Fatalf("expected both to be hoisted, got ok=%v and %v", oka, okb)
	}
	if a != b {
		t.Fatalf("rewritten texts differ, so they would not share a cache entry:\n  %q\n  %q", a, b)
	}
	if pa[AutoParamPrefix+"0"] != "p00000042" || pb[AutoParamPrefix+"0"] != "p00000099" {
		t.Fatalf("values not carried through: %v / %v", pa, pb)
	}
	if !IsAutoParam(AutoParamPrefix + "0") {
		t.Error("IsAutoParam does not recognise a name StripLiterals produced")
	}
	if IsAutoParam("sid") {
		t.Error("IsAutoParam claims an ordinary user parameter is an auto one")
	}
}

// TestStripLiterals_RewrittenTextParses is the safety net, over the shapes the
// scanner is most likely to get wrong. A rewrite that does not parse is caught
// at runtime and discarded, but it would silently cost the optimisation on
// every execution.
func TestStripLiterals_RewrittenTextParses(t *testing.T) {
	for _, q := range []string{
		`MATCH (p:Person {sid: 'x'}) RETURN p.name AS name`,
		`MATCH (n) WHERE n.a = 'x' AND n.b = "y" RETURN n`,
		`MATCH (n) WHERE n.s = 'it\'s' RETURN n`,
		`MATCH (n) WHERE n.s = "a\"b" RETURN n`,
		`MATCH (n) WHERE n.s = 'a' RETURN n // trailing 'comment'`,
		`MATCH (n) WHERE n.s = 'a' /* a 'block' comment */ RETURN n`,
		"MATCH (n) WHERE n.`odd prop` = 'a' RETURN n",
		`MATCH (n) WHERE n.s IN ['a', 'b', 'c'] RETURN n`,
		`MATCH (n) WHERE n.s = '' RETURN n`,
		`MATCH (n) WHERE n.s = 'unicode é' RETURN n`,
		`MATCH (a:P {k: 'x'})-[:R]->(b:P {k: 'y'}) RETURN a.k AS k`,
	} {
		stripped, params, ok := StripLiterals(q)
		if !ok {
			continue // nothing hoisted is always safe
		}
		if _, err := Parse(stripped); err != nil {
			t.Errorf("rewritten text does not parse\n  original: %s\n  rewritten: %s\n  params: %v\n  err: %v",
				q, stripped, params, err)
		}
	}
}

// TestStripLiterals_LeavesUnsafePositionsAlone pins every position the doc
// comment promises to skip. Each would change what the query means or what its
// result columns are called; the CALL and SET cases are the two that regressed
// TCK scenarios when an earlier version hoisted them.
func TestStripLiterals_LeavesUnsafePositionsAlone(t *testing.T) {
	for _, tc := range []struct{ name, q string }{
		{"projection names the column after its text", `RETURN 'x'`},
		{"projection after WITH", `MATCH (n) WITH 'x' AS v RETURN v`},
		{"procedure argument", `CALL test.my.proc('Stefan', 1) YIELD c RETURN c`},
		{"map literal fed to SET +=", `MATCH (n) SET n += {name: 'bar'} RETURN n`},
		{"ON CREATE SET", `MERGE (n:L {k: 'a'}) ON CREATE SET n.v = 'b'`},
		{"CREATE", `CREATE (n:L {k: 'a'})`},
		{"a string inside a comment is not a literal", `MATCH (n) RETURN n // 'not a literal'`},
		{"a string inside a block comment is not a literal", `MATCH (n) /* 'no' */ RETURN n`},
		{"a backtick identifier is not a string", "MATCH (n) RETURN n.`a 'quoted' name` AS x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if stripped, params, ok := StripLiterals(tc.q); ok {
				t.Errorf("hoisted a literal it must leave alone:\n  %s\n  -> %s\n  params %v",
					tc.q, stripped, params)
			}
		})
	}
}

// TestStripLiterals_NumbersAreNeverHoisted pins the deliberate restriction to
// strings: each of these is a position where a parameter is invalid or would
// specialise a shared plan to the first value seen.
func TestStripLiterals_NumbersAreNeverHoisted(t *testing.T) {
	for _, q := range []string{
		`MATCH (n) WHERE n.age > 40 RETURN count(n) AS c`,
		`MATCH (a)-[r*1..3]->(b) RETURN count(r) AS c`,
		`MATCH (n) RETURN n SKIP 5 LIMIT 10`,
		`MATCH (n) WHERE n.f = 1.5 RETURN n`,
		`MATCH (n) WHERE n.h = 0x1f RETURN n`,
	} {
		if stripped, params, ok := StripLiterals(q); ok {
			t.Errorf("hoisted a number:\n  %s\n  -> %s\n  params %v", q, stripped, params)
		}
	}
}

// TestStripLiterals_UnterminatedStringIsRefused covers the one input where the
// scanner cannot know where a literal ends.
func TestStripLiterals_UnterminatedStringIsRefused(t *testing.T) {
	q := `MATCH (n) WHERE n.s = 'unterminated RETURN n`
	stripped, _, ok := StripLiterals(q)
	if ok {
		t.Fatalf("rewrote a query with an unterminated string: %s", stripped)
	}
	if stripped != q {
		t.Fatalf("returned a modified query while refusing: %q", stripped)
	}
}

// TestStripLiterals_PreservesEverythingElseByteForByte proves the rewrite only
// replaces the spans it identified. Whitespace, case and comments must survive
// untouched, or two queries that should collapse would not.
func TestStripLiterals_PreservesEverythingElseByteForByte(t *testing.T) {
	q := "match  (n)\n  where n.a = 'v'  // keep 'this'\n  return n"
	stripped, _, ok := StripLiterals(q)
	if !ok {
		t.Fatal("expected the literal to be hoisted")
	}
	want := "match  (n)\n  where n.a = $`" + AutoParamPrefix + "0`  // keep 'this'\n  return n"
	if stripped != want {
		t.Fatalf("surrounding text was not preserved:\n got %q\nwant %q", stripped, want)
	}
}

// TestStripLiterals_MultipleLiteralsGetDistinctNames guards the obvious way to
// break a multi-literal query: reusing one parameter name for two values.
func TestStripLiterals_MultipleLiteralsGetDistinctNames(t *testing.T) {
	q := `MATCH (n) WHERE n.a = 'first' AND n.b = 'second' RETURN n`
	stripped, params, ok := StripLiterals(q)
	if !ok {
		t.Fatal("expected hoisting")
	}
	if len(params) != 2 {
		t.Fatalf("got %d parameters, want 2: %v", len(params), params)
	}
	if params[AutoParamPrefix+"0"] != "first" || params[AutoParamPrefix+"1"] != "second" {
		t.Fatalf("values bound to the wrong names: %v", params)
	}
	if _, err := Parse(stripped); err != nil {
		t.Fatalf("rewritten text does not parse: %v (%s)", err, stripped)
	}
}

// TestStripLiterals_QuoteFreeQueryIsRejectedFast pins the fast path that keeps a
// fully parameterised query from paying for a scan it cannot benefit from.
func TestStripLiterals_QuoteFreeQueryIsRejectedFast(t *testing.T) {
	q := `MATCH (p:Person {sid: $s}) RETURN p.name AS name`
	stripped, params, ok := StripLiterals(q)
	if ok || params != nil || stripped != q {
		t.Fatalf("a quote-free query must be returned unchanged: ok=%v stripped=%q", ok, stripped)
	}
}
