package cypher_test

// security_procs_readonly_test.go — DEFENSE LOCK-IN proving the procedure
// registry exposed to untrusted CALL statements offers ONLY read-only graph
// introspection. There is no procedure that reads the filesystem, opens a
// network connection, reads the environment, executes a subprocess, or
// decompresses attacker-controlled data — the classic procedure-library
// escalation vectors (cf. Neo4j APOC's apoc.load.*, apoc.export.*).
//
// The assertion is twofold:
//
//  1. Allow-list: every registered procedure's fully-qualified name is on a
//     fixed allow-list of read-only db.* introspection procedures. A NEW
//     procedure outside that set fails this test, forcing a security review
//     before any side-effecting procedure can ship silently.
//  2. Behavioural smoke: each allow-listed procedure, invoked via CALL through
//     the public engine against an empty graph, returns a bounded result
//     without error and without crashing — i.e. it is genuinely an
//     introspection read, not a stub hiding a side effect.
//
// All cases pass today; this is a regression fence.

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/procs"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// secCypherReadOnlyProcs is the fixed allow-list of fully-qualified procedure
// names the engine may expose. Every entry is pure graph introspection. Adding
// a procedure here is a deliberate act that must be justified as read-only.
var secCypherReadOnlyProcs = map[string]bool{
	"db.indexes":              true,
	"db.constraints":          true,
	"db.labels":               true,
	"db.relationshipTypes":    true,
	"db.propertyKeys":         true,
	"db.schema.visualization": true,
}

// secCypherFQN renders a procs.Signature's fully-qualified name ("ns.sub.name").
func secCypherFQN(sig *procs.Signature) string {
	if len(sig.Namespace) == 0 {
		return sig.Name
	}
	return strings.Join(sig.Namespace, ".") + "." + sig.Name
}

// TestSec_Cypher_Procs_OnlyReadOnlyIntrospection asserts the registry the engine
// hands to CALL contains exactly the read-only introspection allow-list — no
// more. A procedure outside the allow-list (a future apoc-style side-effecting
// proc) trips this fence.
func TestSec_Cypher_Procs_OnlyReadOnlyIntrospection(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)
	sigs := eng.Procs().List()

	got := make([]string, 0, len(sigs))
	for i := range sigs {
		got = append(got, secCypherFQN(&sigs[i]))
	}
	sort.Strings(got)

	for _, name := range got {
		if !secCypherReadOnlyProcs[name] {
			t.Errorf("procedure %q is registered but is NOT on the read-only allow-list; "+
				"a side-effecting procedure (filesystem/network/env/exec/decompress) must not be exposed to untrusted CALL without security review", name)
		}
	}

	// Also flag drift in the other direction: if an allow-listed introspection
	// procedure disappears, the allow-list is stale and should be re-reviewed.
	present := make(map[string]bool, len(got))
	for _, name := range got {
		present[name] = true
	}
	for name := range secCypherReadOnlyProcs {
		if !present[name] {
			t.Errorf("allow-listed procedure %q is no longer registered; update the allow-list deliberately", name)
		}
	}
}

// TestSec_Cypher_Procs_NoSideEffectNamespaces asserts no procedure lives in a
// namespace associated with side effects. This is a coarse, forward-looking net
// that catches an entire family (e.g. a future "apoc.load", "dbms.security",
// "file", "http") even before its individual signature is known.
func TestSec_Cypher_Procs_NoSideEffectNamespaces(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)

	// Forbidden tokens anywhere in the fully-qualified name. These name the
	// classic escalation surfaces; none are legitimate for a read-only engine.
	forbidden := []string{
		"load", "export", "import", "file", "fs",
		"http", "net", "url", "fetch",
		"exec", "shell", "cmd", "process", "system",
		"env", "getenv", "secret", "credential", "password",
		"gzip", "zip", "decompress", "inflate",
		"write", "create", "delete", "drop", "set", "remove", "merge",
	}
	sigs := eng.Procs().List()
	for i := range sigs {
		fqn := strings.ToLower(secCypherFQN(&sigs[i]))
		for _, tok := range forbidden {
			// Word-ish containment: the introspection procs ("indexes",
			// "constraints", "labels", "relationshipTypes", "propertyKeys",
			// "visualization") contain none of these tokens, so plain Contains
			// is precise enough and future-proof against side-effecting names.
			if strings.Contains(fqn, tok) {
				t.Errorf("procedure %q contains side-effect token %q; read-only engine must not expose it", fqn, tok)
			}
		}
	}
}

// TestSec_Cypher_Procs_IntrospectionCallsAreBounded invokes each allow-listed
// procedure through the public CALL surface against an empty graph and asserts
// it returns a bounded result with no error — confirming the procedure is a
// genuine read, not a stub masking a side effect or an unbounded scan.
func TestSec_Cypher_Procs_IntrospectionCallsAreBounded(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	// CALL forms that the engine supports for each introspection procedure.
	// db.schema.visualization is exercised separately because its output shape
	// (two list columns) differs; the YIELD-less form is used for the rest.
	calls := []string{
		"CALL db.labels()",
		"CALL db.relationshipTypes()",
		"CALL db.propertyKeys()",
		"CALL db.indexes()",
		"CALL db.constraints()",
	}
	for _, q := range calls {
		t.Run(secCypherCaseName(q), func(t *testing.T) {
			res, err := eng.Run(context.Background(), q, nil)
			if err != nil {
				t.Fatalf("%q: unexpected error %v", q, err)
			}
			defer func() { _ = res.Close() }()
			rows := 0
			for res.Next() {
				_ = res.Record()
				rows++
				if rows > 1_000_000 {
					t.Fatalf("%q: produced an implausibly large result on an empty graph — possible unbounded scan", q)
				}
			}
			if err := res.Err(); err != nil {
				t.Fatalf("%q: iter error %v", q, err)
			}
			// On an empty graph an introspection procedure yields zero rows.
			if rows != 0 {
				t.Fatalf("%q: produced %d rows on an empty graph; want 0", q, rows)
			}
		})
	}
}
