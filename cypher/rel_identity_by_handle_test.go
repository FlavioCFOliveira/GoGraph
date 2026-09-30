package cypher_test

// rel_identity_by_handle_test.go — regression tests for rmp #2939, #2940 and
// #2945: every relationship variable names exactly its own stored instance,
// by stable handle, on every pair and in both directions.
//
//   - #2939: MERGE over a pair holding parallel relationships bound every
//     matched row to the pair's FIRST relationship (its id) while reading the
//     pair's coalesced properties (a sibling's values), so ON MATCH SET, a
//     following SET or REMOVE, and the inline property map all acted on the
//     wrong relationship; the instance count came from CREATE ordinals, which
//     recovery does not rebuild, so after a reopen the MERGE matched one row.
//   - #2940: DELETE of a relationship bound by CREATE or MERGE removed the
//     pair's first relationship instead of the bound one.
//   - #2945: REMOVE of a relationship property through the mirror direction (an
//     undirected pattern walking against storage, or an undirected graph's
//     mirror slot) targeted the unstored order and removed nothing.
//   - #2950: DELETE of a path removed every relationship attached to the path's
//     nodes, not only the path's own, and a bare `DELETE p` was refused even
//     for a path whose nodes hold no other relationship.
//   - #2951: a compound MERGE returned its relationship as it stood BEFORE its
//     ON MATCH / ON CREATE actions.
//
// Every case runs on three wirings: the in-memory engine, the WAL-backed engine
// reopened gracefully (final checkpoint, recovery from the snapshot), and the
// WAL-backed engine abandoned without a close (a crash: recovery from a copy of
// the directory as the writer left it). The read-back checks run live and after
// the reopen.
//
// Controls. Some cases were already correct before the fix and are kept so the
// whole surface stays covered: merge-create-then-set, pattern-merge-create-delete
// and match-reverse-order-delete; the #2945 SET shapes, remove-with and
// merge-on-match (only the REMOVE shapes read through the raw pattern row
// failed); the rollback cases that write through a forward MATCH;
// detach-delete-path; bound-on-create-beside-parallel (the both-bound MERGE
// already applied its actions before building its row); and the two refused
// bare `DELETE p` cases, which the old code refused for the wrong reason — it
// refused every bare path DELETE, delete-isolated-path included.
//
// Layer: short. Engines, graphs and stores are local; the suite is goleak-clean.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// relIDStep is one statement and what it must return. want is compared as a
// sorted row list; a nil want is not compared. counters, when non-nil, are the
// statement's exact expected effects.
type relIDStep struct {
	counters *wantCounters
	// wantErr is a substring the statement's error must contain; the statement
	// must then fail, and want and counters are not compared.
	wantErr string
	q       string
	want    []string
}

// relIDCase is one scenario: steps run once against the live engine; checks are
// read-backs run live after the steps and again after each reopen.
type relIDCase struct {
	name       string
	steps      []relIDStep
	checks     []relIDStep
	undirected bool
}

// relIDPair is the parallel-pair seed the #2939 and #2940 cases share.
var relIDPair = []relIDStep{
	{q: "CREATE (:P {key:'a'}), (:P {key:'b'})"},
	{q: "MATCH (a:P {key:'a'}), (b:P {key:'b'}) CREATE (a)-[:R {n:1}]->(b), (a)-[:R {n:2}]->(b)"},
}

// relIDMatchCount is the MERGE that must keep matching both parallel
// relationships, with no side effect, live and after every reopen.
var relIDMatchCount = relIDStep{
	q:        "MATCH (a:P {key:'a'}), (b:P {key:'b'}) MERGE (a)-[e:R]->(b) RETURN count(*)",
	want:     []string{"2"},
	counters: &wantCounters{},
}

// relIDMergeIdentity asserts each MERGE row's id is the id MATCH reports for the
// relationship whose properties that row carries.
var relIDMergeIdentity = relIDStep{
	q: "MATCH (a:P {key:'a'}), (b:P {key:'b'}) MERGE (a)-[e:R]->(b) " +
		"WITH e.n AS n, id(e) AS i MATCH (:P {key:'a'})-[r:R {n: n}]->(:P {key:'b'}) RETURN n, i = id(r)",
	want: []string{"1|true", "2|true"},
}

var relIDPatternIdentity = relIDStep{
	q: "MERGE (a:P {key:'a'})-[e:R]->(b:P {key:'b'}) " +
		"WITH e.n AS n, id(e) AS i MATCH (:P {key:'a'})-[r:R {n: n}]->(:P {key:'b'}) RETURN n, i = id(r)",
	want: []string{"1|true", "2|true"},
}

func withSeed(steps ...relIDStep) []relIDStep {
	return append(slices.Clone(relIDPair), steps...)
}

// relIDCases2939 cover MERGE on a parallel pair, match and create, through both
// MERGE operators (both endpoints bound, and a pattern with fresh endpoints).
var relIDCases2939 = []relIDCase{
	{
		name: "merge-set-after",
		steps: withSeed(relIDStep{
			q: "MATCH (a:P {key:'a'}), (b:P {key:'b'}) MERGE (a)-[e:R]->(b) SET e.k = 1 RETURN count(*)", want: []string{"2"},
			counters: &wantCounters{propsSet: 2, containsUpdates: true},
		}),
		checks: []relIDStep{
			{q: "MATCH (:P {key:'a'})-[r:R]->(:P {key:'b'}) RETURN r.n, r.k", want: []string{"1|1", "2|1"}},
			relIDMatchCount, relIDMergeIdentity,
		},
	},
	{
		name: "merge-on-match-inline-map",
		steps: withSeed(relIDStep{
			q: "MATCH (a:P {key:'a'}), (b:P {key:'b'}) MERGE (a)-[e:R {n:2}]->(b) ON MATCH SET e.m = 5 RETURN e.n, e.m", want: []string{"2|5"},
			counters: &wantCounters{propsSet: 1, containsUpdates: true},
		}),
		checks: []relIDStep{
			{q: "MATCH (:P {key:'a'})-[r:R]->(:P {key:'b'}) RETURN r.n, r.m", want: []string{"1|null", "2|5"}},
			relIDMatchCount,
		},
	},
	{
		name: "merge-remove",
		steps: withSeed(
			relIDStep{q: "MATCH (:P {key:'a'})-[r:R]->(:P {key:'b'}) SET r.k = r.n * 10"},
			relIDStep{
				q: "MATCH (a:P {key:'a'}), (b:P {key:'b'}) MERGE (a)-[e:R {n:2}]->(b) REMOVE e.k RETURN e.n", want: []string{"2"},
				counters: &wantCounters{propsRemoved: 1, containsUpdates: true},
			},
		),
		checks: []relIDStep{
			{q: "MATCH (:P {key:'a'})-[r:R]->(:P {key:'b'}) RETURN r.n, r.k", want: []string{"1|10", "2|null"}},
			relIDMatchCount,
		},
	},
	{
		name: "merge-on-match-replace-and-mutate",
		steps: withSeed(
			relIDStep{q: "MATCH (a:P {key:'a'}), (b:P {key:'b'}) MERGE (a)-[e:R {n:1}]->(b) ON MATCH SET e = {n: 1, z: 9}"},
			relIDStep{q: "MATCH (a:P {key:'a'}), (b:P {key:'b'}) MERGE (a)-[e:R {n:2}]->(b) ON MATCH SET e += {m: 7}"},
			relIDStep{q: "MATCH (a:P {key:'a'}), (b:P {key:'b'}) MERGE (a)-[e:R {n:2}]->(b) ON MATCH SET e.q = e.n + 1"},
		),
		checks: []relIDStep{
			{q: "MATCH (:P {key:'a'})-[r:R]->(:P {key:'b'}) RETURN r.n, r.z, r.m, r.q", want: []string{"1|9|null|null", "2|null|7|3"}},
			relIDMatchCount,
		},
	},
	{
		name: "pattern-merge-set-after",
		steps: withSeed(relIDStep{
			q: "MERGE (a:P {key:'a'})-[e:R]->(b:P {key:'b'}) SET e.k = 3 RETURN e.n", want: []string{"1", "2"},
			counters: &wantCounters{propsSet: 2, containsUpdates: true},
		}),
		checks: []relIDStep{
			{q: "MATCH (:P {key:'a'})-[r:R]->(:P {key:'b'}) RETURN r.n, r.k", want: []string{"1|3", "2|3"}},
			{q: "MERGE (a:P {key:'a'})-[e:R]->(b:P {key:'b'}) RETURN count(*)", want: []string{"2"}, counters: &wantCounters{}},
			relIDPatternIdentity,
		},
	},
	{
		name: "pattern-merge-on-match-inline-map",
		steps: withSeed(relIDStep{
			q: "MERGE (a:P {key:'a'})-[e:R {n:1}]->(b:P {key:'b'}) ON MATCH SET e.m = 4 RETURN e.n, id(e)", want: nil,
			counters: &wantCounters{propsSet: 1, containsUpdates: true},
		}),
		checks: []relIDStep{
			{q: "MATCH (:P {key:'a'})-[r:R]->(:P {key:'b'}) RETURN r.n, r.m", want: []string{"1|4", "2|null"}},
			{
				q: "MERGE (a:P {key:'a'})-[e:R {n:1}]->(b:P {key:'b'}) WITH e.n AS n, id(e) AS i " +
					"MATCH (:P {key:'a'})-[r:R {n: 1}]->(:P {key:'b'}) RETURN n, i = id(r)",
				want: []string{"1|true"},
			},
		},
	},
	{
		name: "mixed-type-parallel-pair",
		steps: []relIDStep{
			{q: "CREATE (:P {key:'a'}), (:P {key:'b'})"},
			{q: "MATCH (a:P {key:'a'}), (b:P {key:'b'}) CREATE (a)-[:R {n:1}]->(b), (a)-[:T {n:3}]->(b), (a)-[:R {n:2}]->(b)"},
			{
				q: "MATCH (a:P {key:'a'}), (b:P {key:'b'}) MERGE (a)-[e:R]->(b) ON MATCH SET e.k = 1 RETURN e.n", want: []string{"1", "2"},
				counters: &wantCounters{propsSet: 2, containsUpdates: true},
			},
			{
				q: "MERGE (a:P {key:'a'})-[e:T]->(b:P {key:'b'}) ON MATCH SET e.k = 2 RETURN e.n", want: []string{"3"},
				counters: &wantCounters{propsSet: 1, containsUpdates: true},
			},
			{q: "MATCH (a:P {key:'a'}), (b:P {key:'b'}) MERGE (b)<-[e:T]-(a) RETURN e.n", want: []string{"3"}, counters: &wantCounters{}},
		},
		checks: []relIDStep{
			{q: "MATCH (:P {key:'a'})-[r]->(:P {key:'b'}) RETURN type(r), r.n, r.k", want: []string{`"R"|1|1`, `"R"|2|1`, `"T"|3|2`}},
			relIDMatchCount,
			{q: "MATCH (a:P {key:'a'}), (b:P {key:'b'}) MERGE (a)-[e:T]->(b) RETURN count(*)", want: []string{"1"}, counters: &wantCounters{}},
			{q: "MERGE (a:P {key:'a'})-[e:T]->(b:P {key:'b'}) RETURN e.n", want: []string{"3"}, counters: &wantCounters{}},
		},
	},
	{
		name: "undirected-merge-both-orders",
		steps: []relIDStep{
			{q: "CREATE (:P {key:'a'}), (:P {key:'b'})"},
			{q: "MATCH (a:P {key:'a'}), (b:P {key:'b'}) CREATE (a)-[:R {n:1}]->(b), (b)-[:R {n:2}]->(a)"},
			{
				q: "MATCH (a:P {key:'a'}), (b:P {key:'b'}) MERGE (a)-[e:R]-(b) SET e.k = e.n RETURN count(*)", want: []string{"2"},
				counters: &wantCounters{propsSet: 2, containsUpdates: true},
			},
		},
		checks: []relIDStep{
			{q: "MATCH (x:P)-[r:R]->(y:P) RETURN x.key, r.n, r.k", want: []string{`"a"|1|1`, `"b"|2|2`}},
		},
	},
	{
		name: "merge-create-then-set",
		steps: withSeed(relIDStep{
			q: "MATCH (a:P {key:'a'}), (b:P {key:'b'}) MERGE (a)-[e:R {n:3}]->(b) SET e.k = 1 RETURN e.n", want: []string{"3"},
			counters: &wantCounters{relsCreated: 1, propsSet: 2, containsUpdates: true},
		}),
		checks: []relIDStep{
			{q: "MATCH (:P {key:'a'})-[r:R]->(:P {key:'b'}) RETURN r.n, r.k", want: []string{"1|null", "2|null", "3|1"}},
		},
	},
}

// relIDPairAfterDelete is the pair's read-back when a #2940 case deleted only
// the relationship it bound.
var relIDPairAfterDelete = relIDStep{
	q:    "MATCH (:P {key:'a'})-[r]->(:P {key:'b'}) RETURN type(r), r.n",
	want: []string{`"R"|1`, `"R"|2`},
}

// relIDCases2940 cover DELETE of a relationship bound by CREATE, MERGE and
// MATCH, through each DELETE form.
var relIDCases2940 = []relIDCase{
	{
		name: "create-delete",
		steps: withSeed(relIDStep{
			q:        "MATCH (a:P {key:'a'}), (b:P {key:'b'}) CREATE (a)-[e:T]->(b) DELETE e",
			counters: &wantCounters{relsCreated: 1, relsDeleted: 1, containsUpdates: true},
		}),
		checks: []relIDStep{relIDPairAfterDelete},
	},
	{
		name: "create-with-delete",
		steps: withSeed(relIDStep{
			q:        "MATCH (a:P {key:'a'}), (b:P {key:'b'}) CREATE (a)-[e:T]->(b) WITH e DELETE e",
			counters: &wantCounters{relsCreated: 1, relsDeleted: 1, containsUpdates: true},
		}),
		checks: []relIDStep{relIDPairAfterDelete},
	},
	{
		name: "create-same-type-delete",
		steps: withSeed(relIDStep{
			q:        "MATCH (a:P {key:'a'}), (b:P {key:'b'}) CREATE (a)-[e:R {n:3}]->(b) WITH e DELETE e",
			counters: &wantCounters{relsCreated: 1, relsDeleted: 1, propsSet: 1, containsUpdates: true},
		}),
		checks: []relIDStep{relIDPairAfterDelete},
	},
	{
		name: "create-detach-delete",
		steps: withSeed(relIDStep{
			q:        "MATCH (a:P {key:'a'}), (b:P {key:'b'}) CREATE (a)-[e:T]->(b) DETACH DELETE e",
			counters: &wantCounters{relsCreated: 1, relsDeleted: 1, containsUpdates: true},
		}),
		checks: []relIDStep{relIDPairAfterDelete},
	},
	{
		name: "merge-create-delete",
		steps: withSeed(relIDStep{
			q:        "MATCH (a:P {key:'a'}), (b:P {key:'b'}) MERGE (a)-[e:T]->(b) DELETE e",
			counters: &wantCounters{relsCreated: 1, relsDeleted: 1, containsUpdates: true},
		}),
		checks: []relIDStep{relIDPairAfterDelete},
	},
	{
		name: "pattern-merge-create-delete",
		steps: withSeed(relIDStep{
			// No existing (:P {key:'a'})-[:T]->(:P {key:'b'}) path: MERGE creates the
			// WHOLE pattern, two fresh nodes included (openCypher 9), and the
			// DELETE must remove exactly the relationship it created.
			q:        "MERGE (a:P {key:'a'})-[e:T]->(b:P {key:'b'}) DELETE e",
			counters: &wantCounters{nodesCreated: 2, labelsAdded: 2, propsSet: 2, relsCreated: 1, relsDeleted: 1, containsUpdates: true},
		}),
		checks: []relIDStep{relIDPairAfterDelete},
	},
	{
		name: "merge-match-delete",
		steps: withSeed(relIDStep{
			q:        "MATCH (a:P {key:'a'}), (b:P {key:'b'}) MERGE (a)-[e:R {n:2}]->(b) DELETE e",
			counters: &wantCounters{relsDeleted: 1, containsUpdates: true},
		}),
		checks: []relIDStep{{q: "MATCH (:P {key:'a'})-[r]->(:P {key:'b'}) RETURN type(r), r.n", want: []string{`"R"|1`}}},
	},
	{
		name: "pattern-merge-match-delete",
		steps: withSeed(relIDStep{
			q:        "MERGE (a:P {key:'a'})-[e:R {n:2}]->(b:P {key:'b'}) WITH e DELETE e",
			counters: &wantCounters{relsDeleted: 1, containsUpdates: true},
		}),
		checks: []relIDStep{{q: "MATCH (:P {key:'a'})-[r]->(:P {key:'b'}) RETURN type(r), r.n", want: []string{`"R"|1`}}},
	},
	{
		name: "match-reverse-order-delete",
		steps: withSeed(relIDStep{
			q:        "MATCH (:P {key:'b'})-[r:R {n:2}]-(:P {key:'a'}) DELETE r",
			counters: &wantCounters{relsDeleted: 1, containsUpdates: true},
		}),
		checks: []relIDStep{{q: "MATCH (:P {key:'a'})-[r]->(:P {key:'b'}) RETURN type(r), r.n", want: []string{`"R"|1`}}},
	},
	{
		name: "match-reverse-order-with-delete",
		steps: withSeed(relIDStep{
			q:        "MATCH (:P {key:'b'})-[r:R {n:2}]-(:P {key:'a'}) WITH r DELETE r",
			counters: &wantCounters{relsDeleted: 1, containsUpdates: true},
		}),
		checks: []relIDStep{{q: "MATCH (:P {key:'a'})-[r]->(:P {key:'b'}) RETURN type(r), r.n", want: []string{`"R"|1`}}},
	},
}

// relIDPathSeed is a path (:A)-[:R]->(:B) whose start node also holds a
// relationship the path does not contain, (:A)-[:X]->(:C).
var relIDPathSeed = []relIDStep{{q: "CREATE (a:A)-[:R]->(:B), (a)-[:X]->(:C)"}}

// relIDPathIntact is the #2950 read-back when a DELETE of the path had to be
// refused: every relationship and node is still there.
var relIDPathIntact = []relIDStep{
	{q: "MATCH (x)-[r]->(y) RETURN labels(x), type(r), labels(y)", want: []string{`["A"]|"R"|["B"]`, `["A"]|"X"|["C"]`}},
	{q: "MATCH (n) RETURN labels(n)", want: []string{`["A"]`, `["B"]`, `["C"]`}},
}

// relIDPathOnlyC is the #2950 read-back when the path and its extra
// relationship were both deleted.
var relIDPathOnlyC = []relIDStep{
	{q: "MATCH ()-[r]->() RETURN type(r)", want: []string{}},
	{q: "MATCH (n) RETURN labels(n)", want: []string{`["C"]`}},
}

func withPathSeed(steps ...relIDStep) []relIDStep {
	return append(slices.Clone(relIDPathSeed), steps...)
}

// relIDCases2950 cover DELETE and DETACH DELETE of a path whose nodes hold a
// relationship outside the path.
var relIDCases2950 = []relIDCase{
	{
		name:   "delete-path-refused",
		steps:  withPathSeed(relIDStep{q: "MATCH p = (:A)-[:R]->(:B) DELETE p", wantErr: "cannot delete node with existing relationships"}),
		checks: relIDPathIntact,
	},
	{
		name:   "delete-path-expression-refused",
		steps:  withPathSeed(relIDStep{q: "MATCH p = (:A)-[:R]->(:B) WITH [p] AS ps DELETE ps[0]", wantErr: "cannot delete node with existing relationships"}),
		checks: relIDPathIntact,
	},
	{
		name: "delete-path-and-its-extra-relationship",
		steps: withPathSeed(relIDStep{
			q:        "MATCH p = (:A)-[:R]->(:B), (:A)-[x:X]->(:C) DELETE p, x",
			counters: &wantCounters{nodesDeleted: 2, relsDeleted: 2, containsUpdates: true},
		}),
		checks: relIDPathOnlyC,
	},
	{
		name: "detach-delete-path",
		steps: withPathSeed(relIDStep{
			q:        "MATCH p = (:A)-[:R]->(:B) DETACH DELETE p",
			counters: &wantCounters{nodesDeleted: 2, relsDeleted: 2, containsUpdates: true},
		}),
		checks: relIDPathOnlyC,
	},
	{
		name: "delete-isolated-path",
		steps: withPathSeed(
			relIDStep{q: "CREATE (:D)-[:R]->(:E)"},
			relIDStep{
				q:        "MATCH p = (:D)-[:R]->(:E) DELETE p",
				counters: &wantCounters{nodesDeleted: 2, relsDeleted: 1, containsUpdates: true},
			},
		),
		checks: relIDPathIntact,
	},
	{
		name:   "delete-path-over-parallel-pair-refused",
		steps:  withSeed(relIDStep{q: "MATCH p = (:P {key:'a'})-[:R {n:1}]->(:P {key:'b'}) DELETE p", wantErr: "cannot delete node with existing relationships"}),
		checks: []relIDStep{relIDPairAfterDelete},
	},
	{
		name: "delete-path-and-its-parallel-sibling",
		steps: withSeed(relIDStep{
			q:        "MATCH p = (:P {key:'a'})-[:R {n:1}]->(:P {key:'b'}), ()-[y:R {n:2}]->() DELETE p, y",
			counters: &wantCounters{nodesDeleted: 2, relsDeleted: 2, containsUpdates: true},
		}),
		checks: []relIDStep{{q: "MATCH (n) RETURN count(n)", want: []string{"0"}}},
	},
}

// relIDCases2951 cover the value a MERGE returns for its relationship after
// its ON MATCH / ON CREATE actions ran.
var relIDCases2951 = []relIDCase{
	{
		name: "pattern-on-match-single",
		steps: []relIDStep{
			{q: "CREATE (:P {key:'a'})-[:R {n:1}]->(:P {key:'b'})"},
			{q: "MERGE (a:P {key:'a'})-[e:R]->(b:P {key:'b'}) ON MATCH SET e.m = 4 RETURN e.n, e.m", want: []string{"1|4"}},
		},
		checks: []relIDStep{{q: "MATCH ()-[r:R]->() RETURN r.n, r.m", want: []string{"1|4"}}},
	},
	{
		name: "pattern-on-create-single",
		steps: []relIDStep{
			{q: "MERGE (a:P {key:'a'})-[e:R {n:1}]->(b:P {key:'b'}) ON CREATE SET e.m = 5 RETURN e.n, e.m", want: []string{"1|5"}},
		},
		checks: []relIDStep{{q: "MATCH ()-[r:R]->() RETURN r.n, r.m", want: []string{"1|5"}}},
	},
	{
		name: "pattern-on-match-parallel",
		steps: withSeed(
			relIDStep{q: "MERGE (a:P {key:'a'})-[e:R]->(b:P {key:'b'}) ON MATCH SET e.m = e.n * 2 RETURN e.n, e.m", want: []string{"1|2", "2|4"}},
			relIDStep{q: "MERGE (a:P {key:'a'})-[e:R]->(b:P {key:'b'}) ON MATCH SET e += {z: e.n} RETURN e.n, e.z", want: []string{"1|1", "2|2"}},
		),
		checks: []relIDStep{{q: "MATCH (:P {key:'a'})-[r:R]->(:P {key:'b'}) RETURN r.n, r.m, r.z", want: []string{"1|2|1", "2|4|2"}}},
	},
	{
		name: "pattern-on-create-beside-parallel",
		steps: withSeed(
			relIDStep{q: "MERGE (a:P {key:'a'})-[e:R {n:3}]->(b:P {key:'b'}) ON CREATE SET e.m = 9 RETURN e.n, e.m", want: []string{"3|9"}},
		),
		checks: []relIDStep{{q: "MATCH ()-[r:R {n:3}]->() RETURN r.m", want: []string{"9"}}},
	},
	{
		name: "bound-on-match-parallel",
		steps: withSeed(
			relIDStep{q: "MATCH (a:P {key:'a'}), (b:P {key:'b'}) MERGE (a)-[e:R]->(b) ON MATCH SET e.m = e.n + 10 RETURN e.n, e.m", want: []string{"1|11", "2|12"}},
		),
		checks: []relIDStep{{q: "MATCH (:P {key:'a'})-[r:R]->(:P {key:'b'}) RETURN r.n, r.m", want: []string{"1|11", "2|12"}}},
	},
	{
		name: "bound-on-create-beside-parallel",
		steps: withSeed(
			relIDStep{q: "MATCH (a:P {key:'a'}), (b:P {key:'b'}) MERGE (a)-[e:R {n:3}]->(b) ON CREATE SET e.m = 9 RETURN e.n, e.m", want: []string{"3|9"}},
		),
		checks: []relIDStep{{q: "MATCH ()-[r:R {n:3}]->() RETURN r.m", want: []string{"9"}}},
	},
}

// relIDMirrorCase builds a #2945 case: one relationship created as
// (:A)-[:R]->(:B), then stmt reaching it through the mirror direction.
func relIDMirrorCase(name, stmt string, c wantCounters, want string, undirected bool) relIDCase {
	return relIDCase{
		name:       name,
		undirected: undirected,
		steps: []relIDStep{
			{q: "CREATE (:A)-[:R {s: 1, t: 2}]->(:B)"},
			{q: stmt, counters: &c},
		},
		checks: []relIDStep{
			{q: "MATCH (:A)-[r:R]->(:B) RETURN r.s, r.t, r.u", want: []string{want}},
			{q: "MATCH (:B)-[r:R]-(:A) RETURN r.s, r.t, r.u", want: []string{want}},
		},
	}
}

// relIDCases2945 cover every relationship-property write through the mirror
// direction, on a directed and on an undirected graph.
func relIDCases2945() []relIDCase {
	type shape struct {
		name, stmt string
		c          wantCounters
		want       string
	}
	shapes := []shape{
		{"remove", "MATCH (:B)-[r:R]-(:A) REMOVE r.s", wantCounters{propsRemoved: 1, containsUpdates: true}, "null|2|null"},
		{"remove-incoming", "MATCH (:B)<-[r:R]-(:A) REMOVE r.s", wantCounters{propsRemoved: 1, containsUpdates: true}, "null|2|null"},
		{"remove-with", "MATCH (:B)-[r:R]-(:A) WITH r REMOVE r.s", wantCounters{propsRemoved: 1, containsUpdates: true}, "null|2|null"},
		{"remove-both", "MATCH (:B)-[r:R]-(:A) REMOVE r.s, r.t", wantCounters{propsRemoved: 2, containsUpdates: true}, "null|null|null"},
		{"set-null", "MATCH (:B)-[r:R]-(:A) SET r.s = null", wantCounters{propsRemoved: 1, containsUpdates: true}, "null|2|null"},
		{"set", "MATCH (:B)-[r:R]-(:A) SET r.u = 3", wantCounters{propsSet: 1, containsUpdates: true}, "1|2|3"},
		{"set-with", "MATCH (:B)-[r:R]-(:A) WITH r SET r.u = 3", wantCounters{propsSet: 1, containsUpdates: true}, "1|2|3"},
		{"set-mutate", "MATCH (:B)-[r:R]-(:A) SET r += {u: 3}", wantCounters{propsSet: 1, containsUpdates: true}, "1|2|3"},
		{"set-replace-empty", "MATCH (:B)-[r:R]-(:A) SET r = {}", wantCounters{propsRemoved: 2, containsUpdates: true}, "null|null|null"},
		{"set-replace", "MATCH (:B)-[r:R]-(:A) SET r = {u: 3}", wantCounters{propsSet: 1, propsRemoved: 2, containsUpdates: true}, "null|null|3"},
		{"set-replace-with", "MATCH (:B)-[r:R]-(:A) WITH r SET r = {u: 3}", wantCounters{propsSet: 1, propsRemoved: 2, containsUpdates: true}, "null|null|3"},
		{"merge-on-match", "MATCH (a:A), (b:B) MERGE (b)-[r:R]-(a) ON MATCH SET r.u = 3 REMOVE r.s", wantCounters{propsSet: 1, propsRemoved: 1, containsUpdates: true}, "null|2|3"},
	}
	out := make([]relIDCase, 0, 2*len(shapes))
	for _, undirected := range []bool{false, true} {
		graphKind := "directed-graph"
		if undirected {
			graphKind = "undirected-graph"
		}
		for _, s := range shapes {
			out = append(out, relIDMirrorCase(graphKind+"/"+s.name, s.stmt, s.c, s.want, undirected))
		}
	}
	return out
}

// relIDRun executes one step in its own transaction and asserts its rows and
// counters.
func relIDRun(t *testing.T, eng *cypher.Engine, s relIDStep) {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), s.q, nil)
	if s.wantErr != "" {
		if err == nil {
			for res.Next() {
			}
			err = res.Err()
			if cerr := res.Close(); err == nil {
				err = cerr
			}
		}
		if err == nil || !strings.Contains(err.Error(), s.wantErr) {
			t.Errorf("%q: error %v, want one containing %q", s.q, err, s.wantErr)
		}
		return
	}
	if err != nil {
		t.Fatalf("RunInTx(%q): %v", s.q, err)
	}
	rows := make([]string, 0, 4)
	for res.Next() {
		cols := make([]string, len(res.Columns()))
		for i := range cols {
			cols[i] = res.ValueAt(i).String()
		}
		rows = append(rows, joinCols(cols))
	}
	if rerr := res.Err(); rerr != nil {
		_ = res.Close()
		t.Fatalf("drain(%q): %v", s.q, rerr)
	}
	c := res.Counters()
	if cerr := res.Close(); cerr != nil {
		t.Fatalf("close(%q): %v", s.q, cerr)
	}
	if s.want != nil {
		slices.Sort(rows)
		want := slices.Clone(s.want)
		slices.Sort(want)
		if !slices.Equal(rows, want) {
			t.Errorf("%q:\n got  %v\n want %v", s.q, rows, want)
		}
	}
	if s.counters != nil {
		assertCounters(t, s.q, c, *s.counters)
	}
}

func joinCols(cols []string) string {
	out := ""
	for i, c := range cols {
		if i > 0 {
			out += "|"
		}
		out += c
	}
	return out
}

func relIDConfig(undirected bool) adjlist.Config {
	return adjlist.Config{Directed: !undirected, Multigraph: true}
}

// relIDDurable is a WAL-backed engine over a store directory.
type relIDDurable struct {
	g   *lpg.Graph[string, float64]
	w   *wal.Writer
	cp  *checkpoint.Checkpointer[string, float64]
	mu  *sync.Mutex
	eng *cypher.Engine
	dir string
	cfg adjlist.Config
}

// newRelIDDurable creates a store directory whose first checkpoint records the
// graph configuration, so recovery reproduces it (a fresh directory otherwise
// recovers as a directed multigraph).
func newRelIDDurable(t *testing.T, cfg adjlist.Config) *relIDDurable {
	t.Helper()
	dir := t.TempDir()
	g := lpg.New[string, float64](cfg)
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	d := &relIDDurable{g: g, w: w, mu: &sync.Mutex{}, dir: dir, cfg: cfg}
	d.cp = checkpoint.New[string, float64](checkpoint.Config{Dir: dir}, g, w, d.mu)
	if err := d.cp.RunCheckpoint(); err != nil {
		t.Fatalf("initial checkpoint: %v", err)
	}
	d.eng = cypher.NewEngineWithStore(txn.NewStoreWithOptions[string, float64](g, w, parallelEdgeStoreOpts()))
	return d
}

// closeGracefully closes the store through [store.DB] with a final checkpoint.
func (d *relIDDurable) closeGracefully(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.cp.Start(ctx)
	db := store.New(d.w, store.WithCheckpointer(d.cp), store.WithFinalCheckpoint())
	if err := db.Close(); err != nil {
		t.Fatalf("store.DB.Close: %v", err)
	}
	if st := d.cp.Stats(); st.LastError != "" {
		t.Fatalf("final checkpoint failed: %v", st.LastError)
	}
}

// crashCopy copies the store directory as the writer left it, without closing
// anything — the on-disk state a kill -9 leaves — and closes the abandoned
// writer only at test end.
func (d *relIDDurable) crashCopy(t *testing.T) string {
	t.Helper()
	t.Cleanup(func() { _ = d.w.Close() })
	dst := t.TempDir()
	err := filepath.WalkDir(d.dir, func(path string, e os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, rerr := filepath.Rel(d.dir, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if e.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		buf, rerr := os.ReadFile(path) //nolint:gosec // path under t.TempDir
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, buf, 0o600) //nolint:gosec // path under t.TempDir
	})
	if err != nil {
		t.Fatalf("copy store directory: %v", err)
	}
	return dst
}

// recoverRelID reopens dir through recovery and returns an engine over it.
func recoverRelID(t *testing.T, dir string, cfg adjlist.Config) *cypher.Engine {
	t.Helper()
	res, err := recovery.Open[string, float64](dir, parallelEdgeRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if res.Graph.AdjList().Directed() != cfg.Directed || res.Graph.AdjList().Multigraph() != cfg.Multigraph {
		t.Fatalf("recovered graph directed=%v multigraph=%v, want directed=%v multigraph=%v",
			res.Graph.AdjList().Directed(), res.Graph.AdjList().Multigraph(), cfg.Directed, cfg.Multigraph)
	}
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return cypher.NewEngineWithStore(txn.NewStoreWithOptions[string, float64](res.Graph, w, parallelEdgeStoreOpts()))
}

// runRelIDCase runs c on the in-memory, graceful-reopen and crash wirings.
func runRelIDCase(t *testing.T, c relIDCase) {
	t.Helper()
	cfg := relIDConfig(c.undirected)
	runLive := func(t *testing.T, eng *cypher.Engine) {
		t.Helper()
		for _, s := range c.steps {
			relIDRun(t, eng, s)
		}
		for _, s := range c.checks {
			relIDRun(t, eng, s)
		}
	}
	t.Run("memory", func(t *testing.T) {
		runLive(t, cypher.NewEngine(lpg.New[string, float64](cfg)))
	})
	t.Run("wal-graceful-reopen", func(t *testing.T) {
		d := newRelIDDurable(t, cfg)
		runLive(t, d.eng)
		d.closeGracefully(t)
		eng := recoverRelID(t, d.dir, cfg)
		for _, s := range c.checks {
			relIDRun(t, eng, s)
		}
	})
	t.Run("wal-crash", func(t *testing.T) {
		d := newRelIDDurable(t, cfg)
		runLive(t, d.eng)
		eng := recoverRelID(t, d.crashCopy(t), cfg)
		for _, s := range c.checks {
			relIDRun(t, eng, s)
		}
	})
}

// TestMergeParallelRelationships_BindEachInstance_2939 is the #2939 gate.
func TestMergeParallelRelationships_BindEachInstance_2939(t *testing.T) {
	for _, c := range relIDCases2939 {
		t.Run(c.name, func(t *testing.T) { runRelIDCase(t, c) })
	}
}

// TestDeleteBoundRelationship_RemovesThatInstance_2940 is the #2940 gate.
func TestDeleteBoundRelationship_RemovesThatInstance_2940(t *testing.T) {
	for _, c := range relIDCases2940 {
		t.Run(c.name, func(t *testing.T) { runRelIDCase(t, c) })
	}
}

// TestDeletePath_RemovesOnlyThePathsRelationships_2950 is the #2950 gate.
func TestDeletePath_RemovesOnlyThePathsRelationships_2950(t *testing.T) {
	for _, c := range relIDCases2950 {
		t.Run(c.name, func(t *testing.T) { runRelIDCase(t, c) })
	}
}

// TestMergeReturnsPostActionRelationship_2951 is the #2951 gate.
func TestMergeReturnsPostActionRelationship_2951(t *testing.T) {
	for _, c := range relIDCases2951 {
		t.Run(c.name, func(t *testing.T) { runRelIDCase(t, c) })
	}
}

// TestRelationshipPropertyWrite_MirrorDirection_2945 is the #2945 gate.
func TestRelationshipPropertyWrite_MirrorDirection_2945(t *testing.T) {
	for _, c := range relIDCases2945() {
		t.Run(c.name, func(t *testing.T) { runRelIDCase(t, c) })
	}
}

// relIDDump is the whole relationship state — identity, type, endpoints and
// every property the rollback statements touch — as sorted rows.
func relIDDump(t *testing.T, eng *cypher.Engine) []string {
	t.Helper()
	res, err := eng.Run(context.Background(),
		"MATCH (x)-[r]->(y) RETURN id(r), type(r), x.key, y.key, r.n, r.k, r.z, r.m", nil)
	return collectRows(t, res, err)
}

// relIDRollbackCase is one statement rolled back on a parallel pair; inTx is
// what the pair reads back INSIDE the transaction, after the statement, so the
// write is proven to have reached exactly the bound instance before the
// rollback is proven to restore it.
type relIDRollbackCase struct {
	stmt, inTx string
	inTxWant   []string
}

var relIDRollbackCases = []relIDRollbackCase{
	{
		stmt:     "MATCH (:P {key:'a'})-[r:R {n:1}]->(:P {key:'b'}) SET r.k = 9, r.z = 1",
		inTx:     "MATCH (:P {key:'a'})-[r:R]->(:P {key:'b'}) RETURN r.n, r.k, r.z",
		inTxWant: []string{"1|9|1", "2|20|null"},
	},
	{
		stmt:     "MATCH (:P {key:'a'})-[r:R {n:1}]->(:P {key:'b'}) REMOVE r.k",
		inTx:     "MATCH (:P {key:'a'})-[r:R]->(:P {key:'b'}) RETURN r.n, r.k",
		inTxWant: []string{"1|null", "2|20"},
	},
	{
		stmt:     "MATCH (:P {key:'a'})-[r:R {n:2}]->(:P {key:'b'}) SET r = {n: 2}",
		inTx:     "MATCH (:P {key:'a'})-[r:R]->(:P {key:'b'}) RETURN r.n, r.k",
		inTxWant: []string{"1|10", "2|null"},
	},
	{
		stmt:     "MATCH (:P {key:'b'})-[r:R {n:1}]-(:P {key:'a'}) REMOVE r.k",
		inTx:     "MATCH (:P {key:'a'})-[r:R]->(:P {key:'b'}) RETURN r.n, r.k",
		inTxWant: []string{"1|null", "2|20"},
	},
	{
		stmt:     "MATCH (:P {key:'b'})-[r:R {n:2}]-(:P {key:'a'}) SET r.m = 5",
		inTx:     "MATCH (:P {key:'a'})-[r:R]->(:P {key:'b'}) RETURN r.n, r.m",
		inTxWant: []string{"1|null", "2|5"},
	},
	{
		stmt:     "MATCH (a:P {key:'a'}), (b:P {key:'b'}) MERGE (a)-[e:R {n:2}]->(b) ON MATCH SET e.k = 5 REMOVE e.n",
		inTx:     "MATCH (:P {key:'a'})-[r:R]->(:P {key:'b'}) RETURN r.n, r.k",
		inTxWant: []string{"1|10", "null|5"},
	},
	{
		stmt:     "MERGE (a:P {key:'a'})-[e:R {n:1}]->(b:P {key:'b'}) ON MATCH SET e.m = 3",
		inTx:     "MATCH (:P {key:'a'})-[r:R]->(:P {key:'b'}) RETURN r.n, r.m",
		inTxWant: []string{"1|3", "2|null"},
	},
	{
		stmt:     "MATCH (a:P {key:'a'}), (b:P {key:'b'}) MERGE (a)-[e:R]->(b) SET e.m = e.n",
		inTx:     "MATCH (:P {key:'a'})-[r:R]->(:P {key:'b'}) RETURN r.n, r.m",
		inTxWant: []string{"1|1", "2|2"},
	},
	{
		stmt:     "MATCH (a:P {key:'a'}), (b:P {key:'b'}) CREATE (a)-[e:T]->(b) DELETE e",
		inTx:     "MATCH (:P {key:'a'})-[r]->(:P {key:'b'}) RETURN type(r), r.n",
		inTxWant: []string{`"R"|1`, `"R"|2`},
	},
	{
		stmt:     "MATCH (a:P {key:'a'}), (b:P {key:'b'}) MERGE (a)-[e:R {n:1}]->(b) DELETE e",
		inTx:     "MATCH (:P {key:'a'})-[r]->(:P {key:'b'}) RETURN type(r), r.n",
		inTxWant: []string{`"R"|2`},
	},
}

// assertRelIDRollback runs one case in an explicit transaction, checks the
// in-transaction read-back, rolls back and compares the dump with base.
func assertRelIDRollback(t *testing.T, eng *cypher.Engine, base []string, c relIDRollbackCase) {
	t.Helper()
	tx, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	res, err := tx.Exec(c.stmt, nil)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("Exec(%q): %v", c.stmt, err)
	}
	collectRows(t, res, nil)
	res, err = tx.Exec(c.inTx, nil)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("Exec(%q): %v", c.inTx, err)
	}
	got := collectRows(t, res, nil)
	want := slices.Clone(c.inTxWant)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("inside the transaction of %q, %q:\n got  %v\n want %v", c.stmt, c.inTx, got, want)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback(%q): %v", c.stmt, err)
	}
	if got := relIDDump(t, eng); !slices.Equal(got, base) {
		t.Errorf("after rolled-back %q the committed graph changed:\n got  %v\n want %v", c.stmt, got, base)
	}
}

// relIDRollbackSeed is a parallel pair whose instances carry distinct k.
var relIDRollbackSeed = withSeed(relIDStep{q: "MATCH (:P {key:'a'})-[r:R]->(:P {key:'b'}) SET r.k = r.n * 10"})

// TestRollback_WriteOnOneParallelInstance_2939_2945 rolls back SET, REMOVE,
// MERGE and DELETE on one instance of a parallel pair — reached forwards,
// through the mirror direction and through MERGE — and requires the committed
// graph to come back exactly, in memory and after a graceful reopen.
func TestRollback_WriteOnOneParallelInstance_2939_2945(t *testing.T) {
	for _, undirected := range []bool{false, true} {
		cfg := relIDConfig(undirected)
		name := "directed-graph"
		if undirected {
			name = "undirected-graph"
		}
		t.Run(name+"/memory", func(t *testing.T) {
			for _, c := range relIDRollbackCases {
				t.Run(c.stmt, func(t *testing.T) {
					eng := cypher.NewEngine(lpg.New[string, float64](cfg))
					for _, s := range relIDRollbackSeed {
						relIDRun(t, eng, s)
					}
					assertRelIDRollback(t, eng, relIDDump(t, eng), c)
				})
			}
		})
		t.Run(name+"/wal-graceful-reopen", func(t *testing.T) {
			d := newRelIDDurable(t, cfg)
			for _, s := range relIDRollbackSeed {
				relIDRun(t, d.eng, s)
			}
			base := relIDDump(t, d.eng)
			for _, c := range relIDRollbackCases {
				assertRelIDRollback(t, d.eng, base, c)
			}
			d.closeGracefully(t)
			if got := relIDDump(t, recoverRelID(t, d.dir, cfg)); !slices.Equal(got, base) {
				t.Fatalf("after checkpoint + reopen the recovered graph differs from the committed one:\n got  %v\n want %v", got, base)
			}
		})
	}
}
