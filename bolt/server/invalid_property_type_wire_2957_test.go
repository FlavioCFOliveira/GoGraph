package server_test

// invalid_property_type_wire_2957_test.go — regression gate for rmp #2957.
//
// Every InvalidPropertyType refusal — the exec sentinels (a nested collection,
// a list with a null element, an entity) and the plain errors cypher and exec
// build around the TCK detail — matched no FailureCode rule. A Bolt client
// storing `[{num: 1}]` (TCK Set1 [10]: "TypeError: InvalidPropertyType")
// received Neo.DatabaseError.General.UnknownError and the masked
// "An internal error occurred" text: a server fault, for a refusal of the
// client's own value.
//
// Over a real Bolt socket, on the in-memory and on the WAL-backed engine, each
// refusal must now arrive as Neo.ClientError.Statement.TypeError, carrying the
// engine's InvalidPropertyType diagnostic and disclosing nothing internal.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestInvalidPropertyType_ReachesTheClientAsTypeError_2957(t *testing.T) {
	engines := map[string]func(t *testing.T) *cypher.Engine{
		"memory": func(_ *testing.T) *cypher.Engine {
			return cypher.NewEngine(lpg.New[string, float64](adjlist.Config{Directed: true}))
		},
		"wal": newWALEngine,
	}
	cases := []struct {
		name         string
		query        string
		wantFragment string
	}{
		{"Set1 [10] list of maps", `CREATE (a) SET a.maplist = [{num: 1}]`, "InvalidPropertyType"},
		{"map property", `CREATE (a) SET a.m = {num: 1}`, "InvalidPropertyType"},
		{"list with a null element", `CREATE (a) SET a.l = [1, null]`, "a list containing null is not a valid property value"},
		{"CREATE literal null element", `CREATE (:C {l: [null, 'x']})`, "a list containing null is not a valid property value"},
		{"nested list in a SET map", `CREATE (a) SET a += {l: [[1]]}`, "a nested list or map is not a valid property value"},
		{"node in a SET map", `CREATE (a), (b) SET a += {u: b}`, "a node, relationship or path is not a valid property value"},
		{"node as a property", `CREATE (a), (b) SET a.u = b`, "a node, relationship or path is not a valid property value"},
		{"MERGE action map", `MERGE (x:M {id: 1}) ON CREATE SET x += {u: {a: 1}}`, "InvalidPropertyType"},
	}
	for engName, mk := range engines {
		addr := startTestServerWithEngine(t, mk(t), server.Options{})
		for _, c := range cases {
			t.Run(engName+"/"+c.name, func(t *testing.T) {
				f := runForFailure(t, addr, c.query)
				if f.Code != "Neo.ClientError.Statement.TypeError" {
					t.Errorf("%q: code %q, want Neo.ClientError.Statement.TypeError (message %q)", c.query, f.Code, f.Message)
				}
				if !strings.Contains(f.Message, c.wantFragment) {
					t.Errorf("%q: message %q does not carry %q", c.query, f.Message, c.wantFragment)
				}
				assertDisclosesNothingInternal(t, c.query, f.Message)
			})
		}
	}
}

// TestFailureCode_InvalidPropertyTypeFamily_2957 pins the classification for
// every shape the family reaches the server in: each exec sentinel, bare and
// wrapped the way the operators wrap it, and the plain-text form.
func TestFailureCode_InvalidPropertyTypeFamily_2957(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		"nested":             exec.ErrNestedPropertyValue,
		"null element":       fmt.Errorf("exec: Merge: ON MATCH: SET x: value for key %q: %w", "u", exec.ErrNullListElement),
		"entity":             fmt.Errorf("cypher: build plan: %w", exec.ErrEntityPropertyValue),
		"unsupported":        fmt.Errorf("exec: SET u: %w", exec.ErrUnsupportedPropertyValue),
		"plain text":         errors.New("exec: SET p: InvalidPropertyType: maps cannot be stored as property values"),
		"plain text, entity": errors.New("exec: property u: InvalidPropertyType: a node, relationship or path is not a valid property value"),
	} {
		if got := server.FailureCode(err); got != "Neo.ClientError.Statement.TypeError" {
			t.Errorf("%s: FailureCode = %q, want Neo.ClientError.Statement.TypeError", name, got)
		}
	}
	if got := server.FailureCode(errors.New("exec: an unrelated internal fault")); got != "Neo.DatabaseError.General.UnknownError" {
		t.Errorf("control: FailureCode = %q, want the masked UnknownError", got)
	}
}
