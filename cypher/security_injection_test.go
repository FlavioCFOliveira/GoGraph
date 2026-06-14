package cypher_test

// security_injection_test.go — DEFENSE LOCK-IN proving query parameters are
// treated strictly as DATA and can never inject query STRUCTURE.
//
// A parameter value is bound at execution time into an already-parsed plan; it
// is never re-lexed or re-parsed as Cypher. A value that LOOKS like Cypher
// (e.g. "1) DELETE n //", "X:Admin {evil:1})") must therefore land verbatim as
// a scalar property / returned value and must NOT cause any additional clause
// (a DELETE, a second label, a SET) to execute.
//
// The tests prove the negative empirically: they run an injection-laden write,
// then independently count nodes / inspect labels to confirm the embedded
// "attack" had zero structural effect — the value stayed data.
//
// All cases pass today; this is a regression fence.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// secCypherInjectionStrings are payloads crafted to break out of a string
// context and append destructive Cypher if the engine were to re-parse them.
var secCypherInjectionStrings = []string{
	`1) DELETE n //`,
	`') DELETE n RETURN ('`,
	`x"}) DETACH DELETE n //`,
	`X:Admin {evil:1}) DELETE n //`,
	`'; MATCH (m) DETACH DELETE m; //`,
	"\x00) DELETE n //",
}

// secCypherDrain runs q (a write) on eng and drains the result, failing on any
// error. It is used to apply the injection-laden statement.
func secCypherDrain(t *testing.T, eng *cypher.Engine, q string, params map[string]expr.Value) {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), q, params)
	if err != nil {
		t.Fatalf("RunInTx(%q): %v", q, err)
	}
	for res.Next() {
		_ = res.Record()
	}
	if err := res.Err(); err != nil {
		t.Fatalf("RunInTx(%q) iter: %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("RunInTx(%q) close: %v", q, err)
	}
}

// secCypherCountNodes returns the total node count via a read query.
func secCypherCountNodes(t *testing.T, eng *cypher.Engine) int64 {
	t.Helper()
	res, err := eng.Run(context.Background(), `MATCH (n) RETURN count(n) AS c`, nil)
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	defer func() { _ = res.Close() }()
	var c int64 = -1
	for res.Next() {
		if iv, ok := res.Record()["c"].(expr.IntegerValue); ok {
			c = int64(iv)
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("count iter: %v", err)
	}
	return c
}

// TestSec_Cypher_ParamPropertyValue_StaysData asserts a parameter bound into a
// property map is stored verbatim and triggers no extra clause. After creating
// exactly one node per payload, the value round-trips unchanged and the total
// node count equals the number of CREATE statements (no DELETE fired, no extra
// node materialised).
func TestSec_Cypher_ParamPropertyValue_StaysData(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	for i, payload := range secCypherInjectionStrings {
		secCypherDrain(t, eng, `CREATE (n:Item {name: $name}) RETURN n.name AS nm`,
			map[string]expr.Value{"name": expr.StringValue(payload)})

		// Read the value straight back and assert it is byte-for-byte the payload.
		res, err := eng.Run(context.Background(),
			`MATCH (n:Item {name: $name}) RETURN n.name AS nm`,
			map[string]expr.Value{"name": expr.StringValue(payload)})
		if err != nil {
			t.Fatalf("readback %d: %v", i, err)
		}
		var got string
		found := 0
		for res.Next() {
			if sv, ok := res.Record()["nm"].(expr.StringValue); ok {
				got = string(sv)
			}
			found++
		}
		_ = res.Close()
		if found != 1 {
			t.Fatalf("payload %d: expected exactly 1 matching node, found %d", i, found)
		}
		if got != payload {
			t.Fatalf("payload %d: stored value = %q; want verbatim %q (the parameter was re-interpreted)", i, got, payload)
		}
	}

	// The total node count must equal the number of CREATE statements: if any
	// embedded "DELETE n" had executed, the count would be lower; if any extra
	// structure had been injected, it would be higher.
	if got, want := secCypherCountNodes(t, eng), int64(len(secCypherInjectionStrings)); got != want {
		t.Fatalf("node count = %d; want %d — an injected clause altered the graph", got, want)
	}
}

// TestSec_Cypher_ParamCannotInjectLabel asserts a parameter cannot become a
// second label. The payload is bound as the name property; labels(n) must be
// exactly ["Person"], proving the "X:Admin" fragment did not create an Admin
// label.
func TestSec_Cypher_ParamCannotInjectLabel(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	payload := `Admin {pwned:true}) DELETE n //`
	res, err := eng.RunInTx(context.Background(),
		`CREATE (n:Person {name: $name}) RETURN labels(n) AS lbls`,
		map[string]expr.Value{"name": expr.StringValue(payload)})
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}
	var labels expr.ListValue
	rows := 0
	for res.Next() {
		if lv, ok := res.Record()["lbls"].(expr.ListValue); ok {
			labels = lv
		}
		rows++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iter: %v", err)
	}
	_ = res.Close()

	if rows != 1 {
		t.Fatalf("expected 1 row, got %d", rows)
	}
	if len(labels) != 1 {
		t.Fatalf("labels(n) = %v; want exactly one label (parameter injected a label)", labels)
	}
	if sv, ok := labels[0].(expr.StringValue); !ok || string(sv) != "Person" {
		t.Fatalf("labels(n)[0] = %#v; want \"Person\"", labels[0])
	}
	// And the embedded DELETE must not have run.
	if got := secCypherCountNodes(t, eng); got != 1 {
		t.Fatalf("node count = %d; want 1 — the embedded DELETE in the parameter executed", got)
	}
}

// TestSec_Cypher_ParamScalarRoundTrip asserts a hostile parameter returned
// directly (no graph write) round-trips as the identical scalar value, never as
// parsed structure.
func TestSec_Cypher_ParamScalarRoundTrip(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)
	for i, payload := range secCypherInjectionStrings {
		res, err := eng.Run(context.Background(), `RETURN $p AS p`,
			map[string]expr.Value{"p": expr.StringValue(payload)})
		if err != nil {
			t.Fatalf("payload %d: %v", i, err)
		}
		rows := 0
		var got string
		for res.Next() {
			if sv, ok := res.Record()["p"].(expr.StringValue); ok {
				got = string(sv)
			}
			rows++
		}
		_ = res.Close()
		if rows != 1 {
			t.Fatalf("payload %d: got %d rows; want exactly 1", i, rows)
		}
		if got != payload {
			t.Fatalf("payload %d: returned %q; want verbatim %q", i, got, payload)
		}
	}
}
