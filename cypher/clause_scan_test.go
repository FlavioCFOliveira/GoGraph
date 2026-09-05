package cypher

import (
	"regexp"
	"strings"
	"testing"
)

// legacyWritingKeywordRE is the regexp that classified writing clauses before
// rmp #2240 replaced it with containsWritingKeyword. It survives HERE, in the
// test binary only, as the differential oracle: the replacement's contract is
// that it decides identically, and the only way to hold it to that is to keep
// asking the thing it replaced.
//
// Test-only oracle, compiled once.
var legacyWritingKeywordRE = regexp.MustCompile(`(?i)\b(CREATE|MERGE|SET|REMOVE|DELETE|DETACH)\b`)

// TestContainsWritingKeyword_AgreesWithTheRegexpItReplaced is the differential
// gate for rmp #2240. The table-driven cases in
// TestQueryHasWritingClause_IgnoresCommentsAndStrings fix the intended
// behaviour; this fixes the ACTUAL behaviour against the previous
// implementation, over a corpus wide enough to cover the boundary and folding
// rules a hand-written table will not think to include.
func TestContainsWritingKeyword_AgreesWithTheRegexpItReplaced(t *testing.T) {
	t.Parallel()

	corpus := []string{
		// Plain reads and writes.
		"MATCH (n:Person) RETURN n",
		"MATCH (n) WHERE n.age > 30 RETURN n.name ORDER BY n.age LIMIT 10",
		"CREATE (n:Person {name: 'x'})",
		"MERGE (n:Person {name: 'x'})",
		"MATCH (n) SET n.seen = true",
		"MATCH (n) REMOVE n.seen",
		"MATCH (n) DELETE n",
		"MATCH (n) DETACH DELETE n",
		"CALL db.labels() YIELD label RETURN label",
		"",
		"   ",

		// Case variants — the fold must behave exactly as (?i) did.
		"create (n)", "CrEaTe (n)", "mErGe (n)", "set", "SeT", "sEt",
		"DeTaCh dElEtE n", "rEmOvE n.p", "DELETE", "Delete", "delete",

		// Word-boundary traps: a keyword embedded in a longer word is not one.
		"PRESET", "NOMERGE", "CREATED", "CREATES", "XCREATE", "SETTING",
		"RESET", "OFFSET", "SUBSET", "DELETED", "UNDELETE", "REMOVES",
		"DETACHED", "MERGED", "_SET", "SET_", "SET1", "1SET", "a.SET", "SET.a",
		"n.createdAt", "MATCH (n) RETURN n.deleted",

		// Separators either side of a keyword.
		"MATCH(n)CREATE(m)", "a,SET,b", "(SET)", "[SET]", "{SET}", "SET;",
		"a\nSET\nb", "a\tSET\tb", "a-SET-b", "a+SET+b", "a=SET=b",

		// Non-ASCII neighbours — the boundary class is ASCII, as in the regexp.
		"ſet", "ſET", "MATCH ſet n", "café SET x", "SETé", "éSET",
		"日本語 CREATE", "CREATE日本語",

		// Comment and quote regions, which the mask removes before the scan.
		"// CREATE nothing here\nMATCH (n) RETURN n",
		"/* CREATE */ MATCH (n) RETURN n",
		"MATCH (n) WHERE n.name = 'CREATE' RETURN n",
		`MATCH (n) WHERE n.name = "DELETE" RETURN n`,
		"MATCH (n:`SET`) RETURN n",
		"MATCH (n) WHERE n.s = 'a\\'CREATE' RETURN n",
		"/* unterminated CREATE",
		"'unterminated CREATE",
		"`unterminated CREATE",
		"// trailing CREATE",
		"MATCH (n) SET n.x = 1 // CREATE",
		"/* c */ SET x",
		"'quoted' SET x",

		// Keyword-dense text that must still classify as a write.
		"MATCH (a)-[r]->(b) WHERE a.name = 'MERGE' SET r.w = 1 RETURN r",
	}

	// Widen the corpus mechanically: every entry also appears upper- and
	// lower-cased, and wrapped in surrounding text.
	cases := make([]string, 0, 6*len(corpus))
	for _, q := range corpus {
		cases = append(cases, q,
			strings.ToUpper(q), strings.ToLower(q),
			"MATCH (n) "+q, q+" RETURN n", "  "+q+"  ")
	}

	for i, q := range cases {
		want := legacyWritingKeywordRE.MatchString(maskNonClauseRegions(q))
		got := containsWritingKeyword(maskNonClauseRegions(q))
		if got != want {
			t.Errorf("case %d: containsWritingKeyword(%q) = %v, regexp oracle = %v",
				i, q, got, want)
		}
	}
	t.Logf("agreed with the regexp oracle on %d inputs", len(cases))
}

// TestQueryHasWritingClause_FastPathDoesNotAllocate is acceptance criterion 2:
// a query with no comment and no quote must classify without touching the heap.
// The regexp this replaced allocated 80 bytes per call, on every RunAny and
// RunInTxAny dispatch.
func TestQueryHasWritingClause_FastPathDoesNotAllocate(t *testing.T) {
	// Not parallel: testing.AllocsPerRun requires a quiet process.
	queries := []string{
		"MATCH (n:Person) WHERE n.age > 30 RETURN n.name ORDER BY n.age LIMIT 10",
		"MATCH (n:Person) WHERE n.age > 30 SET n.seen = true",
		"CREATE (n:Person {name: 42})",
	}
	for _, q := range queries {
		t.Run(q[:min(len(q), 28)], func(t *testing.T) {
			var sink bool
			got := testing.AllocsPerRun(200, func() {
				sink = QueryHasWritingClause(q)
			})
			_ = sink
			if got != 0 {
				t.Errorf("QueryHasWritingClause(%q) allocated %.1f objects per call, want 0", q, got)
			}
		})
	}
}

// TestIsWritingKeyword_ExactSet pins the keyword set itself, so adding a seventh
// writing clause to the engine without updating the classifier is caught here
// rather than by a query silently taking the read path.
func TestIsWritingKeyword_ExactSet(t *testing.T) {
	t.Parallel()

	for _, kw := range []string{"SET", "MERGE", "CREATE", "REMOVE", "DELETE", "DETACH"} {
		for _, form := range []string{kw, strings.ToLower(kw), swapCase(kw)} {
			if !isWritingKeyword(form) {
				t.Errorf("isWritingKeyword(%q) = false, want true", form)
			}
		}
	}
	for _, notKW := range []string{
		"", "S", "SE", "SETS", "MATCH", "RETURN", "WHERE", "WITH", "UNWIND",
		"CALL", "YIELD", "FOREACH", "LOAD", "DROP", "ALTER", "SELECT",
		"CREATED", "MERGES", "DETACHED", "REMOVED", "DELETES", "PRESET",
	} {
		if isWritingKeyword(notKW) {
			t.Errorf("isWritingKeyword(%q) = true, want false", notKW)
		}
	}
}

// swapCase inverts the ASCII case of every letter, producing a mixed-case form
// that neither ToUpper nor ToLower would generate.
func swapCase(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z':
			b[i] = c - 'a' + 'A'
		case c >= 'A' && c <= 'Z':
			b[i] = c - 'A' + 'a'
		}
	}
	return string(b)
}
