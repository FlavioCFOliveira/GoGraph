package server_test

// token_length_wire_2956_test.go — regression gate for rmp #2956 and #2748 over
// a real Bolt socket.
//
// #2956: on a WAL-backed engine, CREATE with a relationship type of 65536 bytes
// or more was ACKNOWLEDGED, and recovery read the relationship back with no
// type. The WAL adapter discarded the staging refusal, so nothing reached the
// log while the client was told the statement succeeded.
//
// #2748: a store-less engine accepted the same names the WAL-backed one
// refused, so a client could not know the limit from the engine it talked to.
//
// Each statement below must fail with Neo.ClientError.Schema.TokenLengthError on
// BOTH engines, carrying the engine's own diagnostic and nothing internal.

import (
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestTokenLength_WireCodeOnEveryEngine_2956(t *testing.T) {
	t.Parallel()
	overLong := strings.Repeat("T", lpg.MaxTokenLen+1)
	statements := []struct{ name, query string }{
		{"relationship type", "CREATE (:A)-[:" + overLong + "]->(:B)"},
		{"node label", "CREATE (n:" + overLong + ")"},
		{"node property key", "CREATE (n:Ok {`" + overLong + "`: 1})"},
		{"relationship property key", "CREATE (:A)-[:R {`" + overLong + "`: 1}]->(:B)"},
		{"SET label", "CREATE (n:S) SET n:" + overLong},
	}
	engines := []struct {
		name string
		mk   func(t *testing.T) *cypher.Engine
	}{
		{"wal", newWALEngine},
		{"in-memory", func(*testing.T) *cypher.Engine {
			return cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
		}},
	}
	for _, e := range engines {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			addr := startTestServerWithEngine(t, e.mk(t), server.Options{})
			for _, st := range statements {
				t.Run(st.name, func(t *testing.T) {
					f := runForFailure(t, addr, st.query)
					if f.Code != "Neo.ClientError.Schema.TokenLengthError" {
						t.Errorf("code = %q, want Neo.ClientError.Schema.TokenLengthError\n  message: %.200q", f.Code, f.Message)
					}
					if !strings.Contains(f.Message, "token too long") ||
						!strings.Contains(f.Message, "is 65536 bytes, maximum 65535") {
						t.Errorf("message does not carry the engine's own diagnostic: %.200q", f.Message)
					}
					assertDisclosesNothingInternal(t, st.name, f.Message)
				})
			}
		})
	}
}
