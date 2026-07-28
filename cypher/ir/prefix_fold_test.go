package ir

import (
	"strings"
	"testing"
)

// ddlPrefixes are the five prefixes IsDDL and isShowPrefix test against.
var ddlPrefixes = []string{
	"CREATE INDEX", "DROP INDEX", "CREATE CONSTRAINT", "DROP CONSTRAINT",
	"SHOW CONSTRAINT", "SHOW INDEX",
}

// TestHasPrefixFold_AgreesWithToUpper is the differential gate for rmp #2240's
// allocation fix. hasPrefixFold replaced
// `strings.HasPrefix(strings.ToUpper(s), upper)`, and its contract is that it
// decides IDENTICALLY — so the oracle is that expression itself, evaluated over
// a corpus built to attack the byte-wise fold.
//
// The dangerous inputs are the ones where Unicode uppercasing is not a per-byte
// map: 'ı' (U+0131) uppercases to ASCII 'I', 'ſ' (U+017F) to 'S', and 'ß'
// (U+00DF) to the TWO bytes "SS", which changes the string's length and so
// shifts every following byte out of alignment.
func TestHasPrefixFold_AgreesWithToUpper(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"", " ", "C", "CR", "CREATE", "CREATE ", "CREATE INDEX",
		"create index", "Create Index", "cReAtE iNdEx",
		"CREATE INDEX idx FOR (n:P) ON (n.k)",
		"DROP INDEX idx", "drop index idx",
		"CREATE CONSTRAINT c FOR (n:P) REQUIRE n.k IS UNIQUE",
		"DROP CONSTRAINT c", "SHOW CONSTRAINTS", "show indexes",
		"SHOW INDEX", "SHOW CONSTRAINT",
		"CREATE INDE", "CREATE INDEXX", "CREATEINDEX", "CREATE  INDEX",
		"MATCH (n) RETURN n", "CREATE (n:P)", "MERGE (n:P)",
		"SHOWINDEX", "SHOW", "SHOW ", "SHO",

		// Unicode that uppercases ONTO ASCII — the fold must not be fooled, and
		// must not diverge from ToUpper either way.
		"ındex", "CREATE ıNDEX", "cREATE ıNDEX",
		"ſHOW INDEX", "ſhow index", "SHOW ıNDEX",
		"CREATE İNDEX", // dotted capital I
		"ßHOW INDEX",   // sharp s: uppercases to two bytes
		"ß", "ßß", "CREATE INDEß",
		"CREATE ßNDEX",

		// Other non-ASCII in and around the prefix window.
		"café CREATE INDEX", "CREATE INDEXé", "日本語", "CREATE 日本語",
		"K", // Kelvin sign, uppercases to itself, folds to 'k'
	}

	// Widen with surrounding whitespace and trailing text.
	cases := make([]string, 0, 6*len(inputs))
	for _, s := range inputs {
		cases = append(cases, s, " "+s, s+" ", s+" trailing", strings.ToUpper(s), strings.ToLower(s))
	}

	checked := 0
	for _, s := range cases {
		for _, p := range ddlPrefixes {
			want := strings.HasPrefix(strings.ToUpper(s), p)
			got := hasPrefixFold(s, p)
			if got != want {
				t.Errorf("hasPrefixFold(%q, %q) = %v, ToUpper oracle = %v", s, p, got, want)
			}
			checked++
		}
	}
	t.Logf("agreed with the ToUpper oracle on %d (input, prefix) pairs", checked)
}

// TestIsDDL_DoesNotAllocate pins the reason hasPrefixFold exists: IsDDL runs on
// every RunAny / RunInTxAny dispatch, and uppercasing the whole query to compare
// a seventeen-byte prefix cost one heap allocation per query executed.
func TestIsDDL_DoesNotAllocate(t *testing.T) {
	// Not parallel: testing.AllocsPerRun requires a quiet process.
	queries := []string{
		"MATCH (n:Person) WHERE n.age > 30 RETURN n.name ORDER BY n.age LIMIT 10",
		"CREATE (n:Person {name: 'x'})",
		"CREATE INDEX idx FOR (n:Person) ON (n.name)",
		"SHOW INDEXES",
	}
	for _, q := range queries {
		t.Run(q[:min(len(q), 28)], func(t *testing.T) {
			var sink bool
			got := testing.AllocsPerRun(200, func() { sink = IsDDL(q) })
			_ = sink
			if got != 0 {
				t.Errorf("IsDDL(%q) allocated %.1f objects per call, want 0", q, got)
			}
		})
	}
}

// TestIsDDL_ClassificationUnchanged is the behavioural half: the statements that
// were DDL before the allocation fix are still DDL, and the ones that were not
// still are not.
func TestIsDDL_ClassificationUnchanged(t *testing.T) {
	t.Parallel()

	ddl := []string{
		"CREATE INDEX idx FOR (n:P) ON (n.k)",
		"create index idx FOR (n:P) ON (n.k)",
		"DROP INDEX idx",
		"CREATE CONSTRAINT c FOR (n:P) REQUIRE n.k IS UNIQUE",
		"DROP CONSTRAINT c",
		"SHOW INDEXES", "SHOW CONSTRAINTS", "show index", "SHOW CONSTRAINT",
		"  CREATE INDEX idx FOR (n:P) ON (n.k)",
		"// a comment\nCREATE INDEX idx FOR (n:P) ON (n.k)",
		"/* block */ SHOW INDEXES",
	}
	for _, q := range ddl {
		if !IsDDL(q) {
			t.Errorf("IsDDL(%q) = false, want true", q)
		}
	}

	notDDL := []string{
		"MATCH (n) RETURN n", "CREATE (n:P)", "MERGE (n:P)", "SET x = 1",
		"CREATEINDEX", "SHOWINDEX", "CREATE INDE", "", "   ",
		"DELETE n", "DETACH DELETE n", "CALL db.labels()",
	}
	for _, q := range notDDL {
		if IsDDL(q) {
			t.Errorf("IsDDL(%q) = true, want false", q)
		}
	}
}
