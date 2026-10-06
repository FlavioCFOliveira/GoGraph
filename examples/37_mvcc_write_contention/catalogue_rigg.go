package main

// catalogue_rigg.go — the referential-integrity and graph-shape rows (§1.8) and
// the frontier proposals FP01-FP03 (§5) of the deterministic MVCC scenario
// catalogue (rmp #2933, stage C2).
//
// # The referential-integrity invariant
//
// Catalogue §9 point 3: an edge to a dead node is never committed in any
// permutation. Every RI and GG row ends with the [world.dangling] probe, which
// walks every interned node at a fresh snapshot and counts the arcs whose source
// or destination is not alive there, and every RI and GG scenario carries the
// [noDanglingEdge] property, which fails the run on any such arc. The probe reads
// the adjacency through the lpg API rather than through Cypher on purpose: a
// Cypher pattern binds only live nodes, so a dangling arc would vanish from a
// MATCH instead of showing in it.
//
// # Drivers
//
// The rows whose shape exists in the lpg API — edge create, node removal with
// its incident arcs, edge property writes and edge removal by handle, label add
// and removal — also run through lpg.Graph. GG01 (variable-length traversal),
// GG03 (degree and count-store reads) and GG05 (delete everything) are Cypher
// only: the lpg API has no traversal operator, no relationship count store and no
// bulk delete, so an lpg arm would test a re-implementation in this file instead
// of the engine.
//
// # The commit hold (FP01-FP03)
//
// lpg.Graph.AllocateCommitTS reserves a transaction's commit instant without
// publishing it: it is the first half of the WAL commit path, which allocates
// before its fsync and publishes after it. Called on an in-memory graph and
// followed by a later step's EndVersionedTx, it holds a commit between timestamp
// allocation and publication at a fixed point of the interleaving — the seam the
// FP rows need. The straggler is therefore always an lpg transaction; the later
// commits and the reads that observe the frontier run through either driver.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/isolationtest"
)

// ---------------------------------------------------------------------------
// The dangling-edge probe and property.

// danglingStep is the name of the final probe every RI and GG row runs.
const danglingStep = "dangling"

// dangling reports, at a fresh snapshot, the live arcs and the arcs that touch a
// node not alive at that snapshot: an out-arc of a dead source, an out-arc into a
// dead destination, or an in-neighbour entry of a dead node.
func (w *world) dangling(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "<lpg: arcs touching a dead node, at a fresh snapshot>",
		Probe: func(context.Context) ([]string, [][]string, error) {
			g := w.g
			snap := g.BeginRead()
			defer g.EndRead(snap)
			view := g.ReadAt(snap)
			// Walk must not re-enter the Mapper: collect the ids, then read.
			var ids []graph.NodeID
			g.AdjList().Mapper().Walk(func(id graph.NodeID, _ string) bool {
				ids = append(ids, id)
				return true
			})
			var arcs, dead int
			for _, id := range ids {
				srcLive := view.Exists(id)
				for _, dst := range view.EntryView(id).Neighbours {
					if srcLive && view.Exists(dst) {
						arcs++
					} else {
						dead++
					}
				}
				if !srcLive {
					dead += len(g.InNeighbourIDsAsOf(id, snap))
				}
			}
			return []string{"live_arcs", "dangling"}, [][]string{{strconv.Itoa(arcs), strconv.Itoa(dead)}}, nil
		}}
}

// noDanglingEdge is the RI invariant as a property: the dangling probe must count
// zero in every permutation.
func noDanglingEdge() isolationtest.Observer {
	return func(o isolationtest.Observation) error {
		if o.Step != danglingStep {
			return nil
		}
		if o.Err != nil {
			return fmt.Errorf("dangling-edge probe failed: %w", o.Err)
		}
		if len(o.Rows) != 1 || len(o.Rows[0]) != 2 || o.Rows[0][1] != "0" {
			return fmt.Errorf("an edge to or from a dead node was committed: %v", o.Rows)
		}
		return nil
	}
}

// allOf runs every property on every observation and reports the first failure.
func allOf(checks ...func() isolationtest.Observer) func() isolationtest.Observer {
	return func() isolationtest.Observer {
		obs := make([]isolationtest.Observer, len(checks))
		for i, c := range checks {
			obs[i] = c()
		}
		return func(o isolationtest.Observation) error {
			for _, ob := range obs {
				if err := ob(o); err != nil {
					return err
				}
			}
			return nil
		}
	}
}

// sameRows asserts, per permutation, that each pair of steps returned identical
// rows: a pinned reader repeating a read must see the same answer.
func sameRows(pairs ...[2]string) func() isolationtest.Observer {
	return func() isolationtest.Observer {
		first := map[string]string{}
		return func(o isolationtest.Observation) error {
			got := fmt.Sprint(o.Rows)
			for _, p := range pairs {
				if o.Step != p[0] && o.Step != p[1] {
					continue
				}
				if o.Err != nil {
					return fmt.Errorf("repeated read %s failed: %w", o.Step, o.Err)
				}
				switch o.Step {
				case p[0]:
					first[o.Permutation+"/"+p[0]] = got
				case p[1]:
					if want := first[o.Permutation+"/"+p[0]]; got != want {
						return fmt.Errorf("%s returned %s, %s returned %s at the same snapshot", p[0], want, p[1], got)
					}
				}
			}
			return nil
		}
	}
}

// riFinal is the final block every RI and GG row ends with: the surviving nodes,
// the surviving edges and the dangling-edge probe.
func riFinal(w *world) []isolationtest.Step {
	return steps(
		q("final", "MATCH (n) RETURN n.name AS name ORDER BY name"),
		q("edges", "MATCH (x)-[r]->(y) RETURN x.name AS src, r.id AS id, y.name AS dst ORDER BY src, id, dst"),
		w.dangling(danglingStep))
}

// ---------------------------------------------------------------------------
// lpg driver additions for edges.

// refused is the error of a write the lpg API reported as not admitted: the
// conflict the transaction recorded on itself.
func refused(wv lpg.WriteView[string, float64], what string) error {
	if err := wv.Tx().Err(); err != nil {
		return err
	}
	return errors.New(what + " was not admitted")
}

// addEdge appends src→dst.
func (s *lpgSession) addEdge(name, src, dst string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: fmt.Sprintf("lpg: AddEdge(%s, %s)", src, dst),
		Hook: func(ctx context.Context) error {
			return s.write(ctx, func(wv lpg.WriteView[string, float64]) error { return wv.AddEdge(src, dst, 1) })
		}}
}

// detachDelete removes key with every arc incident to it, the lpg spelling of
// Cypher's DETACH DELETE: each in-arc, every out-arc, then the node.
func (s *lpgSession) detachDelete(name, key string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "lpg: detach-delete " + key + " (RemoveEdge of each in-arc, RemoveAllEdgesFrom, RemoveNode)",
		Hook: func(ctx context.Context) error {
			return s.write(ctx, func(wv lpg.WriteView[string, float64]) error { return detachDelete(wv, key) })
		}}
}

// RemoveEdge reports false only for a refusal; RemoveAllEdgesFrom also reports
// false when there was nothing to remove, so its false is a refusal only when the
// transaction recorded a conflict.
func detachDelete(wv lpg.WriteView[string, float64], key string) error {
	for _, x := range wv.Read().InNeighbours(key) {
		if !wv.RemoveEdge(x, key) {
			return refused(wv, "RemoveEdge("+x+", "+key+")")
		}
	}
	if !wv.RemoveAllEdgesFrom(key) {
		if err := wv.Tx().Err(); err != nil {
			return err
		}
	}
	if _, err := wv.RemoveNode(key); err != nil {
		return err
	}
	return nil
}

// deleteIfNoIncoming is RI04's application check through the lpg API: delete key
// only when its transaction's view shows no arc into it.
func (s *lpgSession) deleteIfNoIncoming(name, key string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "lpg: if InNeighbours(" + key + ") is empty, detach-delete " + key,
		Probe: func(ctx context.Context) ([]string, [][]string, error) {
			deleted := 0
			err := s.write(ctx, func(wv lpg.WriteView[string, float64]) error {
				if len(wv.Read().InNeighbours(key)) != 0 {
					return nil
				}
				deleted = 1
				return detachDelete(wv, key)
			})
			if err != nil {
				return nil, nil, err
			}
			return []string{"deleted"}, [][]string{{strconv.Itoa(deleted)}}, nil
		}}
}

// setEdgePropByHandle writes prop on one edge instance, named in h.
func (s *lpgSession) setEdgePropByHandle(name string, e lpgEdge, h map[string]uint64, prop string, v int64) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: fmt.Sprintf("lpg: SetEdgePropertyByHandle(%s, %s, <id %d>, %s, %d)", e.src, e.dst, e.id, prop, v),
		Hook: func(ctx context.Context) error {
			return s.write(ctx, func(wv lpg.WriteView[string, float64]) error {
				return wv.SetEdgePropertyByHandle(e.src, e.dst, h[e.name], prop, lpg.Int64Value(v))
			})
		}}
}

// removeEdgeByHandle removes one edge instance, named in h — its adjacency slot
// and its per-handle records — and reports whether a slot was removed.
// RemoveEdgeByHandle reports false both for a refusal and for no matching slot;
// only the first records a conflict on the transaction.
func (s *lpgSession) removeEdgeByHandle(name string, e lpgEdge, h map[string]uint64) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: fmt.Sprintf("lpg: RemoveEdgeByHandle(%s, %s, <id %d>)", e.src, e.dst, e.id),
		Probe: func(ctx context.Context) ([]string, [][]string, error) {
			removed := false
			err := s.write(ctx, func(wv lpg.WriteView[string, float64]) error {
				removed = wv.RemoveEdgeByHandle(e.src, e.dst, h[e.name])
				if !removed {
					return wv.Tx().Err()
				}
				return nil
			})
			if err != nil {
				return nil, nil, err
			}
			return []string{"removed"}, [][]string{{strconv.FormatBool(removed)}}, nil
		}}
}

// setLabel adds a node label.
func (s *lpgSession) setLabel(name, key, lbl string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: fmt.Sprintf("lpg: SetNodeLabel(%s, %s)", key, lbl),
		Hook: func(ctx context.Context) error {
			return s.write(ctx, func(wv lpg.WriteView[string, float64]) error { return wv.SetNodeLabel(key, lbl) })
		}}
}

// labelled returns the names of the nodes carrying lbl at the session's read
// snapshot, sorted by the Mapper walk then rendered as one count and one list.
func (s *lpgSession) labelled(name, lbl string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "lpg: live nodes labelled " + lbl + " at the pinned snapshot",
		Probe: func(context.Context) ([]string, [][]string, error) {
			g := s.w.g
			if s.snap == nil {
				return nil, nil, errors.New("lpg session holds no read snapshot")
			}
			view := g.ReadAt(s.snap)
			var ids []graph.NodeID
			g.AdjList().Mapper().Walk(func(id graph.NodeID, _ string) bool {
				ids = append(ids, id)
				return true
			})
			n := 0
			for _, id := range ids {
				if view.Exists(id) && view.HasNodeLabelByID(id, lbl) {
					n++
				}
			}
			return []string{"labelled"}, [][]string{{strconv.Itoa(n)}}, nil
		}}
}

// lpgEdge is one edge an lpg-driver graph fixture creates: src→dst, typed R and
// carrying property id, its handle recorded under name.
type lpgEdge struct {
	name     string
	src, dst string
	id       int64
}

// lpgGraphFixture builds :N nodes named by key and the edges given, through the
// lpg API, and records every edge's handle in h. It runs once per permutation, so
// h always names the handles of the graph in force.
func (w *world) lpgGraphFixture(h map[string]uint64, nodes []string, edges ...lpgEdge) isolationtest.Step {
	return isolationtest.Step{Name: "mk", Label: "<lpg fixture: nodes and typed edges>", Hook: func(context.Context) error {
		return w.g.ApplyVersioned(func(tx lpg.WriteTx) error {
			wv := w.g.Writer(tx)
			for _, n := range nodes {
				if err := wv.AddNode(n); err != nil {
					return err
				}
				if err := wv.SetNodeLabel(n, "N"); err != nil {
					return err
				}
				if err := wv.SetNodeProperty(n, "name", lpg.StringValue(n)); err != nil {
					return err
				}
			}
			for _, e := range edges {
				hd, err := wv.AddEdgeH(e.src, e.dst, 1)
				if err != nil {
					return err
				}
				h[e.name] = hd
				if err := wv.SetEdgeLabelByHandle(e.src, e.dst, hd, "R"); err != nil {
					return err
				}
				if err := wv.SetEdgePropertyByHandle(e.src, e.dst, hd, "id", lpg.Int64Value(e.id)); err != nil {
					return err
				}
			}
			return nil
		})
	}}
}

// ---------------------------------------------------------------------------
// §1.8 Referential integrity.

// riDoc closes every RI and GG Doc: the invariant the dangling probe asserts.
const riDoc = "\nThe final dangling probe must count 0 in every permutation: an edge to a dead node\n" +
	"is never committed (catalogue §9 point 3)."

// twoTx is the session shape of the two-transaction RI rows: each session BEGINs
// in its setup, so both snapshots predate every step, runs its steps and commits.
func twoTx(s1, s2 []isolationtest.Step) []*isolationtest.Session {
	return []*isolationtest.Session{
		{Name: "s1", Setup: steps(begin("s1b")), Steps: append(s1, commit("s1c"))},
		{Name: "s2", Setup: steps(begin("s2b")), Steps: append(s2, commit("s2c"))},
	}
}

// twoLPGTx is twoTx through two lpg sessions.
func twoLPGTx(a, b *lpgSession, s1, s2 []isolationtest.Step) []*isolationtest.Session {
	return []*isolationtest.Session{
		{Name: "s1", Setup: steps(a.begin("s1b")), Steps: append(s1, a.commit("s1c"))},
		{Name: "s2", Setup: steps(b.begin("s2b")), Steps: append(s2, b.commit("s2c"))},
	}
}

const (
	abSetup      = "CREATE (:N {name:'a'}), (:N {name:'b'})"
	createAB     = "MATCH (a:N {name:'a'}), (b:N {name:'b'}) CREATE (a)-[:R {id:1}]->(b) RETURN count(*) AS created"
	createBA     = "MATCH (a:N {name:'a'}), (b:N {name:'b'}) CREATE (b)-[:R {id:1}]->(a) RETURN count(*) AS created"
	detachDelB   = "MATCH (b:N {name:'b'}) DETACH DELETE b RETURN count(*) AS deleted"
	ri01Doc      = "RI01. s1 DETACH DELETEs b; s2, whose snapshot predates s1's commit, creates a->b.\n"
	ri01DocTrail = "In every order one of the two is refused: an edge create checks its destination's\n" +
		"existence and adjacency (rmp #2444), and a node removal claims the node's adjacency, so\n" +
		"an append committed after the remover's snapshot refuses the removal."
)

func ri01Cypher(w *world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name:     "ri01-edge-create-vs-endpoint-delete",
		Doc:      ri01Doc + ri01DocTrail + riDoc,
		Setup:    steps(q("mk", abSetup)),
		Sessions: twoTx(steps(q("s1d", detachDelB)), steps(q("s2e", createAB))),
		Final:    riFinal(w),
	}
}

func ri01LPG(w *world) *isolationtest.Spec {
	a, b := w.lpgSession(), w.lpgSession()
	return &isolationtest.Spec{
		Name:     "ri01-edge-create-vs-endpoint-delete-lpg",
		Doc:      ri01Doc + "Through the lpg API: detach-delete b; AddEdge(a, b).\n" + ri01DocTrail + riDoc,
		Setup:    steps(w.lpgGraphFixture(map[string]uint64{}, []string{"a", "b"})),
		Sessions: twoLPGTx(a, b, steps(a.detachDelete("s1d", "b")), steps(b.addEdge("s2e", "a", "b"))),
		Final:    riFinal(w),
	}
}

const ri02Doc = "RI02. Both transactions open before either writes: s1 DETACH DELETEs b, s2 creates\n" +
	"an edge %s b. Whichever writes second is refused while the first is open and after\n" +
	"it commits; no permutation leaves an edge to a dead node."

func ri02Cypher(name, dir, create string) func(*world) *isolationtest.Spec {
	return func(w *world) *isolationtest.Spec {
		return &isolationtest.Spec{
			Name:     name,
			Doc:      fmt.Sprintf(ri02Doc, dir) + riDoc,
			Setup:    steps(q("mk", abSetup)),
			Sessions: twoTx(steps(q("s1d", detachDelB)), steps(q("s2e", create))),
			Final:    riFinal(w),
		}
	}
}

func ri02LPG(name, dir, src, dst string) func(*world) *isolationtest.Spec {
	return func(w *world) *isolationtest.Spec {
		a, b := w.lpgSession(), w.lpgSession()
		return &isolationtest.Spec{
			Name:     name,
			Doc:      fmt.Sprintf(ri02Doc, dir) + "\nThrough the lpg API." + riDoc,
			Setup:    steps(w.lpgGraphFixture(map[string]uint64{}, []string{"a", "b"})),
			Sessions: twoLPGTx(a, b, steps(a.detachDelete("s1d", "b")), steps(b.addEdge("s2e", src, dst))),
			Final:    riFinal(w),
		}
	}
}

var (
	ri02IntoCypher  = ri02Cypher("ri02-edge-into-deleted-node", "into", createAB)
	ri02OutOfCypher = ri02Cypher("ri02-edge-out-of-deleted-node", "out of", createBA)
	ri02IntoLPG     = ri02LPG("ri02-edge-into-deleted-node-lpg", "into", "a", "b")
	ri02OutOfLPG    = ri02LPG("ri02-edge-out-of-deleted-node-lpg", "out of", "b", "a")
)

// ri03Doc is the G8 row's Doc.
const ri03Doc = "RI03 (G8). Two transactions each create one edge sharing an endpoint with the\n" +
	"other's: %s. The adjacency conflict rule (graph/lpg/mvcc_adjversion.go, rmp #2445)\n" +
	"claims BOTH endpoints of an append, so the second appender is refused while the first\n" +
	"is open and after it commits. If appends commuted, as an older comment stated, both\n" +
	"would commit in every order."

func ri03Cypher(name, arm, setup, e1, e2 string) func(*world) *isolationtest.Spec {
	return func(w *world) *isolationtest.Spec {
		return &isolationtest.Spec{
			Name:     name,
			Doc:      fmt.Sprintf(ri03Doc, arm) + riDoc,
			Setup:    steps(q("mk", setup)),
			Sessions: twoTx(steps(q("s1e", e1)), steps(q("s2e", e2))),
			Final:    riFinal(w),
		}
	}
}

func ri03LPG(name, arm string, nodes []string, e1, e2 [2]string) func(*world) *isolationtest.Spec {
	return func(w *world) *isolationtest.Spec {
		a, b := w.lpgSession(), w.lpgSession()
		return &isolationtest.Spec{
			Name:  name,
			Doc:   fmt.Sprintf(ri03Doc, arm) + "\nThrough the lpg API: AddEdge in each transaction." + riDoc,
			Setup: steps(w.lpgGraphFixture(map[string]uint64{}, nodes)),
			Sessions: twoLPGTx(a, b, steps(a.addEdge("s1e", e1[0], e1[1])),
				steps(b.addEdge("s2e", e2[0], e2[1]))),
			Final: riFinal(w),
		}
	}
}

const (
	hubSetup = "CREATE (:N {name:'x1'}), (:N {name:'x2'}), (:N {name:'hub'})"
	srcSetup = "CREATE (:N {name:'x'}), (:N {name:'t1'}), (:N {name:'t2'})"
)

var (
	ri03SameTarget = ri03Cypher("ri03-hub-same-target", "x1->hub and x2->hub (same target)", hubSetup,
		"MATCH (x:N {name:'x1'}), (h:N {name:'hub'}) CREATE (x)-[:R {id:1}]->(h) RETURN count(*) AS created",
		"MATCH (x:N {name:'x2'}), (h:N {name:'hub'}) CREATE (x)-[:R {id:2}]->(h) RETURN count(*) AS created")
	ri03SameSource = ri03Cypher("ri03-hub-same-source", "x->t1 and x->t2 (same source)", srcSetup,
		"MATCH (x:N {name:'x'}), (t:N {name:'t1'}) CREATE (x)-[:R {id:1}]->(t) RETURN count(*) AS created",
		"MATCH (x:N {name:'x'}), (t:N {name:'t2'}) CREATE (x)-[:R {id:2}]->(t) RETURN count(*) AS created")
	ri03SameTargetLPG = ri03LPG("ri03-hub-same-target-lpg", "x1->hub and x2->hub (same target)",
		[]string{"x1", "x2", "hub"}, [2]string{"x1", "hub"}, [2]string{"x2", "hub"})
	ri03SameSourceLPG = ri03LPG("ri03-hub-same-source-lpg", "x->t1 and x->t2 (same source)",
		[]string{"x", "t1", "t2"}, [2]string{"x", "t1"}, [2]string{"x", "t2"})
)

const ri04Doc = "RI04. An application-level RI check: s1 deletes b only if no edge points into it\n" +
	"(MATCH (b) WHERE NOT ()-->(b) DELETE b); s2 creates a->b. At s1's snapshot b has no\n" +
	"incoming edge, so the check passes; the native edge RI must still refuse one of the\n" +
	"two. PostgreSQL without a foreign key permits the orphan."

func ri04Cypher(w *world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name:  "ri04-application-check-vs-edge-insert",
		Doc:   ri04Doc + riDoc,
		Setup: steps(q("mk", abSetup)),
		Sessions: twoTx(
			steps(q("s1d", "MATCH (b:N {name:'b'}) WHERE NOT ()-->(b) DELETE b RETURN count(*) AS deleted")),
			steps(q("s2e", createAB))),
		Final: riFinal(w),
	}
}

func ri04LPG(w *world) *isolationtest.Spec {
	a, b := w.lpgSession(), w.lpgSession()
	return &isolationtest.Spec{
		Name:     "ri04-application-check-vs-edge-insert-lpg",
		Doc:      ri04Doc + "\nThrough the lpg API: the check reads InNeighbours in the transaction's view." + riDoc,
		Setup:    steps(w.lpgGraphFixture(map[string]uint64{}, []string{"a", "b"})),
		Sessions: twoLPGTx(a, b, steps(a.deleteIfNoIncoming("s1d", "b")), steps(b.addEdge("s2e", "a", "b"))),
		Final:    riFinal(w),
	}
}

const ri05Doc = "RI05. The foreign-key deadlock shape: s1 and s2 each create an edge to parent p,\n" +
	"then SET p.n. No step waits (F7): the second writer to p is refused at once, at the\n" +
	"edge create or at the SET, and p.n holds the committed writer's value."

const ri05Setup = "CREATE (:N {name:'p', n:0}), (:N {name:'c1'}), (:N {name:'c2'})"

func ri05Cypher(w *world) *isolationtest.Spec {
	child := func(name, c string, id int) isolationtest.Step {
		return q(name, fmt.Sprintf("MATCH (c:N {name:'%s'}), (p:N {name:'p'}) CREATE (c)-[:R {id:%d}]->(p) RETURN count(*) AS created", c, id))
	}
	set := func(name string, v int) isolationtest.Step {
		return q(name, fmt.Sprintf("MATCH (p:N {name:'p'}) SET p.n = %d RETURN p.n AS n", v))
	}
	return &isolationtest.Spec{
		Name:     "ri05-edge-to-parent-then-update-parent",
		Doc:      ri05Doc + riDoc,
		Setup:    steps(q("mk", ri05Setup)),
		Sessions: twoTx(steps(child("s1e", "c1", 1), set("s1u", 1)), steps(child("s2e", "c2", 2), set("s2u", 2))),
		Final:    append(steps(q("parent", "MATCH (p:N {name:'p'}) RETURN p.n AS n")), riFinal(w)...),
	}
}

func ri05LPG(w *world) *isolationtest.Spec {
	a, b := w.lpgSession(), w.lpgSession()
	return &isolationtest.Spec{
		Name:  "ri05-edge-to-parent-then-update-parent-lpg",
		Doc:   ri05Doc + "\nThrough the lpg API: AddEdge, then SetNodeProperty." + riDoc,
		Setup: steps(w.lpgFixture(lpgNode{key: "p", labels: []string{"N"}, props: map[string]int64{"n": 0}}, lpgNode{key: "c1", labels: []string{"N"}}, lpgNode{key: "c2", labels: []string{"N"}})),
		Sessions: twoLPGTx(a, b,
			steps(a.addEdge("s1e", "c1", "p"), a.set("s1u", "p", "n", 1)),
			steps(b.addEdge("s2e", "c2", "p"), b.set("s2u", "p", "n", 2))),
		Final: append(steps(q("parent", "MATCH (p:N {name:'p'}) RETURN p.n AS n")), riFinal(w)...),
	}
}

const ri06SameDoc = "RI06, same edge. s1 and s2 each SET r.w on the one edge a->b. The edge side store\n" +
	"refuses the second writer (WW13 found it refused at the statement, not at COMMIT)."

const ri06ParallelDoc = "RI06, parallel edges. s1 SETs r.w on edge id 1, s2 on edge id 2; both run from a\n" +
	"to b. The catalogue expects no conflict: the two are different relationships."

const ri06Setup = "CREATE (a:N {name:'a'})-[:R {id:1, w:0}]->(b:N {name:'b'})"

const ri06ParallelSetup = "CREATE (a:N {name:'a'})-[:R {id:1, w:0}]->(b:N {name:'b'}), (a)-[:R {id:2, w:0}]->(b)"

const ri06EdgeFinal = "MATCH (:N {name:'a'})-[r]->(:N {name:'b'}) RETURN r.id AS id, r.w AS w ORDER BY id"

func setW(name string, id, v int) isolationtest.Step {
	return q(name, fmt.Sprintf("MATCH (:N {name:'a'})-[r:R {id:%d}]->(:N {name:'b'}) SET r.w = %d RETURN r.w AS w", id, v))
}

func ri06Same(w *world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name:     "ri06-same-edge-property",
		Doc:      ri06SameDoc + riDoc,
		Setup:    steps(q("mk", ri06Setup)),
		Sessions: twoTx(steps(setW("s1w", 1, 1)), steps(setW("s2w", 1, 2))),
		Final:    append(steps(q("weights", ri06EdgeFinal)), riFinal(w)...),
	}
}

func ri06SameLPG(w *world) *isolationtest.Spec {
	a, b := w.lpgSession(), w.lpgSession()
	h := map[string]uint64{}
	e1 := lpgEdge{name: "e1", src: "a", dst: "b", id: 1}
	return &isolationtest.Spec{
		Name: "ri06-same-edge-property-lpg",
		Doc: ri06SameDoc + "\nThrough the lpg API: SetEdgePropertyByHandle on the one instance — the per-instance\n" +
			"store a Cypher read of r.w resolves." + riDoc,
		Setup: steps(w.lpgGraphFixture(h, []string{"a", "b"}, e1)),
		Sessions: twoLPGTx(a, b, steps(a.setEdgePropByHandle("s1w", e1, h, "w", 1)),
			steps(b.setEdgePropByHandle("s2w", e1, h, "w", 2))),
		Final: append(steps(q("weights", ri06EdgeFinal)), riFinal(w)...),
	}
}

func ri06Parallel(w *world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name:     "ri06-parallel-edges-disjoint",
		Doc:      ri06ParallelDoc + riDoc,
		Setup:    steps(q("mk", ri06ParallelSetup)),
		Sessions: twoTx(steps(setW("s1w", 1, 1)), steps(setW("s2w", 2, 2))),
		Final:    append(steps(q("weights", ri06EdgeFinal)), riFinal(w)...),
	}
}

func ri06ParallelLPG(w *world) *isolationtest.Spec {
	a, b := w.lpgSession(), w.lpgSession()
	h := map[string]uint64{}
	e1 := lpgEdge{name: "e1", src: "a", dst: "b", id: 1}
	e2 := lpgEdge{name: "e2", src: "a", dst: "b", id: 2}
	return &isolationtest.Spec{
		Name:  "ri06-parallel-edges-disjoint-lpg",
		Doc:   ri06ParallelDoc + "\nThrough the lpg API: SetEdgePropertyByHandle on each instance." + riDoc,
		Setup: steps(w.lpgGraphFixture(h, []string{"a", "b"}, e1, e2)),
		Sessions: twoLPGTx(a, b, steps(a.setEdgePropByHandle("s1w", e1, h, "w", 1)),
			steps(b.setEdgePropByHandle("s2w", e2, h, "w", 2))),
		Final: append(steps(q("weights", ri06EdgeFinal)), riFinal(w)...),
	}
}

// ---------------------------------------------------------------------------
// §1.8 Graph shapes.

const gg01Setup = "CREATE (a:N {name:'a'})-[:R {id:1}]->(b:N {name:'b'})-[:R {id:2}]->(c:N {name:'c'})" +
	"-[:R {id:3}]->(d:N {name:'d'})-[:R {id:4}]->(e:N {name:'e'}), (a)-[:R {id:5}]->(c)"

const gg01Read = "MATCH p = (:N {name:'a'})-[*1..4]->(z) RETURN count(p) AS paths"

func gg01(w *world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "gg01-traversal-at-pinned-snapshot",
		Doc: "GG01. s0 pins a read snapshot and counts the paths of length 1..4 from a, twice; s1\n" +
			"adds b->e and deletes c->d in two autocommit statements. Both of s0's counts equal\n" +
			"the snapshot's: a traversal never mixes instants." + riDoc,
		Setup: steps(q("mk", gg01Setup)),
		Sessions: []*isolationtest.Session{
			{Name: "s0", Setup: steps(beginRead("s0b")), Steps: steps(q("s0r1", gg01Read), q("s0r2", gg01Read), commit("s0c"))},
			{Name: "s1", Steps: steps(
				q("s1a", "MATCH (x:N {name:'b'}), (y:N {name:'e'}) CREATE (x)-[:R {id:6}]->(y) RETURN count(*) AS created"),
				q("s1d", "MATCH (:N {name:'c'})-[r:R]->(:N {name:'d'}) DELETE r RETURN count(*) AS deleted"))},
		},
		Final: append(steps(q("paths", gg01Read)), riFinal(w)...),
	}
}

const gg02Setup = "CREATE (h:N {name:'hub'}), (:N {name:'x1'})-[:R {id:1}]->(h), (:N {name:'x2'})-[:R {id:2}]->(h), " +
	"(:N {name:'x3'})-[:R {id:3}]->(h), (:N {name:'x'})"

const gg02Doc = "GG02. s1 DETACH DELETEs a hub with three incoming edges; s2 creates x->hub. One\n" +
	"is refused in every order: no edge into the dead hub survives, and the relationship\n" +
	"count agrees with the arcs the probe walks."

const gg02Count = "MATCH ()-[r:R]->() RETURN count(r) AS rels"

func gg02Cypher(w *world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name:  "gg02-detach-delete-hub-vs-edge-create",
		Doc:   gg02Doc + riDoc,
		Setup: steps(q("mk", gg02Setup)),
		Sessions: twoTx(
			steps(q("s1d", "MATCH (h:N {name:'hub'}) DETACH DELETE h RETURN count(*) AS deleted")),
			steps(q("s2e", "MATCH (x:N {name:'x'}), (h:N {name:'hub'}) CREATE (x)-[:R {id:4}]->(h) RETURN count(*) AS created"))),
		Final: append(steps(q("rels", gg02Count)), riFinal(w)...),
	}
}

func gg02LPG(w *world) *isolationtest.Spec {
	a, b := w.lpgSession(), w.lpgSession()
	return &isolationtest.Spec{
		Name: "gg02-detach-delete-hub-vs-edge-create-lpg",
		Doc:  gg02Doc + "\nThrough the lpg API: detach-delete hub; AddEdge(x, hub)." + riDoc,
		Setup: steps(w.lpgGraphFixture(map[string]uint64{}, []string{"hub", "x1", "x2", "x3", "x"},
			lpgEdge{name: "e1", src: "x1", dst: "hub", id: 1}, lpgEdge{name: "e2", src: "x2", dst: "hub", id: 2},
			lpgEdge{name: "e3", src: "x3", dst: "hub", id: 3})),
		Sessions: twoLPGTx(a, b, steps(a.detachDelete("s1d", "hub")), steps(b.addEdge("s2e", "x", "hub"))),
		Final:    append(steps(q("rels", gg02Count)), riFinal(w)...),
	}
}

const gg03Setup = "CREATE (h:N {name:'hub'}), (:N {name:'x1'})-[:R {id:1}]->(h), (:N {name:'x2'})-[:R {id:2}]->(h), " +
	"(h)-[:R {id:3}]->(:N {name:'y'}), (:N {name:'x'})"

const (
	gg03Degree = "MATCH (h:N {name:'hub'}) OPTIONAL MATCH (h)--(m) RETURN count(m) AS degree"
	gg03Rels   = "MATCH ()-[r:R]->() WITH count(r) AS seek OPTIONAL MATCH ()-[s]->() WHERE type(s) = 'R' RETURN seek, count(s) AS scan"
)

func gg03(w *world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "gg03-degree-and-count-at-snapshot",
		Doc: "GG03. s0 pins a read snapshot and reads the hub's degree and the :R relationship\n" +
			"count — the count-store answer (seek) beside a typed scan (scan) — twice; s1 adds an\n" +
			"edge into the hub and deletes one in two autocommit statements. s0's repeated reads\n" +
			"are equal, and seek = scan in every read." + riDoc,
		Setup: steps(q("mk", gg03Setup)),
		Sessions: []*isolationtest.Session{
			{Name: "s0", Setup: steps(beginRead("s0b")), Steps: steps(
				q("s0d1", gg03Degree), q("s0r1", gg03Rels), q("s0d2", gg03Degree), q("s0r2", gg03Rels), commit("s0c"))},
			{Name: "s1", Steps: steps(
				q("s1a", "MATCH (x:N {name:'x'}), (h:N {name:'hub'}) CREATE (x)-[:R {id:4}]->(h) RETURN count(*) AS created"),
				q("s1d", "MATCH (:N {name:'x1'})-[r:R]->(:N {name:'hub'}) DELETE r RETURN count(*) AS deleted"))},
		},
		Final: append(steps(q("degree", gg03Degree), q("rels", gg03Rels)), riFinal(w)...),
	}
}

const gg04Setup = "UNWIND range(0, 9) AS i CREATE (:N:L {name:'l' + toString(i)}) WITH count(*) AS c CREATE (:N {name:'u'})"

const gg04Doc = "GG04. s0 pins a read snapshot and counts :L twice — the label store (seek) beside a\n" +
	"label test over every node (scan); s1 adds :L to u and removes it from l0 in two\n" +
	"autocommit statements. The pinned reader's count is unchanged (10) and seek = scan."

func gg04Cypher(w *world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name:  "gg04-label-churn-at-pinned-snapshot",
		Doc:   gg04Doc + riDoc,
		Setup: steps(q("mk", gg04Setup)),
		Sessions: []*isolationtest.Session{
			{Name: "s0", Setup: steps(beginRead("s0b")), Steps: steps(q("s0c1", countL), q("s0c2", countL), commit("s0c"))},
			{Name: "s1", Steps: steps(
				q("s1a", "MATCH (n:N {name:'u'}) SET n:L RETURN count(*) AS labelled"),
				q("s1r", "MATCH (n:N {name:'l0'}) REMOVE n:L RETURN count(*) AS unlabelled"))},
		},
		Final: append(steps(q("count", countL)), riFinal(w)...),
	}
}

func gg04LPG(w *world) *isolationtest.Spec {
	r, s := w.lpgSession(), w.lpgSession()
	nodes := make([]lpgNode, 0, 11)
	for i := 0; i < 10; i++ {
		nodes = append(nodes, lpgNode{key: "l" + strconv.Itoa(i), labels: []string{"N", "L"}})
	}
	nodes = append(nodes, lpgNode{key: "u", labels: []string{"N"}})
	return &isolationtest.Spec{
		Name:  "gg04-label-churn-at-pinned-snapshot-lpg",
		Doc:   gg04Doc + "\nThrough the lpg API: Graph.BeginRead, SetNodeLabel, RemoveNodeLabel." + riDoc,
		Setup: steps(w.lpgFixture(nodes...)),
		Sessions: []*isolationtest.Session{
			{Name: "s0", Setup: steps(r.beginRead("s0b")), Steps: steps(
				r.labelled("s0c1", "L"), r.labelled("s0c2", "L"), r.endRead("s0c"))},
			{Name: "s1", Steps: steps(s.setLabel("s1a", "u", "L"), s.removeLabel("s1r", "l0", "L"))},
		},
		Final: append(steps(q("count", countL)), riFinal(w)...),
	}
}

func gg05(w *world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "gg05-delete-everything-vs-insert",
		Doc: "GG05. s1 runs MATCH (n) DETACH DELETE n; s2 creates a new node. s1's snapshot\n" +
			"predates s2's commit in every order, so s1 cannot see the new node and it survives:\n" +
			"a phantom insert is permitted, and a delete never reaches across snapshots." + riDoc,
		Setup: steps(q("mk", "CREATE (:N {name:'a'})-[:R {id:1}]->(:N {name:'b'}), (:N {name:'c'})")),
		Sessions: twoTx(
			steps(q("s1d", "MATCH (n) DETACH DELETE n RETURN count(*) AS deleted")),
			steps(q("s2n", "CREATE (n:N {name:'new'}) RETURN n.name AS name"))),
		Final: riFinal(w),
	}
}

const gg06Setup = "CREATE (a:N {name:'a'})-[:R {id:1}]->(b:N {name:'b'}), (a)-[:R {id:2}]->(b), (a)-[:R {id:3}]->(a)"

func delEdge(name string, id int) isolationtest.Step {
	return q(name, fmt.Sprintf("MATCH (:N {name:'a'})-[r:R {id:%d}]->() DELETE r RETURN count(*) AS deleted", id))
}

const gg06Doc = "GG06 (%s). Two parallel edges a->b (id 1, id 2) and a self-loop a->a (id 3).\n" +
	"s1 deletes edge id %d, s2 deletes edge id %d, each by instance. The same instance:\n" +
	"the second deleter is refused. Different instances: the catalogue expects both to\n" +
	"commit; every instance here has source a, the node the adjacency conflict rule\n" +
	"claims, so the golden pins which verdict holds."

var gg06Edges = []lpgEdge{
	{name: "e1", src: "a", dst: "b", id: 1},
	{name: "e2", src: "a", dst: "b", id: 2},
	{name: "e3", src: "a", dst: "a", id: 3},
}

func gg06Cypher(name, arm string, id1, id2 int) func(*world) *isolationtest.Spec {
	return func(w *world) *isolationtest.Spec {
		return &isolationtest.Spec{
			Name:     name,
			Doc:      fmt.Sprintf(gg06Doc, arm, id1, id2) + riDoc,
			Setup:    steps(q("mk", gg06Setup)),
			Sessions: twoTx(steps(delEdge("s1d", id1)), steps(delEdge("s2d", id2))),
			Final:    riFinal(w),
		}
	}
}

func gg06LPG(name, arm string, id1, id2 int) func(*world) *isolationtest.Spec {
	return func(w *world) *isolationtest.Spec {
		a, b := w.lpgSession(), w.lpgSession()
		h := map[string]uint64{}
		return &isolationtest.Spec{
			Name:  name,
			Doc:   fmt.Sprintf(gg06Doc, arm, id1, id2) + "\nThrough the lpg API: RemoveEdgeByHandle (slot and per-handle records)." + riDoc,
			Setup: steps(w.lpgGraphFixture(h, []string{"a", "b"}, gg06Edges...)),
			Sessions: twoLPGTx(a, b, steps(a.removeEdgeByHandle("s1d", gg06Edges[id1-1], h)),
				steps(b.removeEdgeByHandle("s2d", gg06Edges[id2-1], h))),
			Final: riFinal(w),
		}
	}
}

var (
	gg06Disjoint    = gg06Cypher("gg06-delete-disjoint-parallel-edges", "disjoint parallel edges", 1, 2)
	gg06SelfLoop    = gg06Cypher("gg06-delete-self-loop-and-parallel-edge", "self-loop and parallel edge", 3, 1)
	gg06DisjointLPG = gg06LPG("gg06-delete-disjoint-parallel-edges-lpg", "disjoint parallel edges", 1, 2)
	gg06SameLPG     = gg06LPG("gg06-delete-same-edge-instance-lpg", "same instance", 1, 1)
	gg06SelfLoopLPG = gg06LPG("gg06-delete-self-loop-and-parallel-edge-lpg", "self-loop and parallel edge", 3, 1)
)

// ---------------------------------------------------------------------------
// §5 FP01-FP03: the commit frontier behind a held straggler.

// allocate is the commit hold: it reserves the open transaction's commit instant
// without publishing it. The transaction's later commit step publishes it.
func (s *lpgSession) allocate(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "lpg: Graph.AllocateCommitTS (instant reserved, not published)",
		Hook: func(context.Context) error {
			if !s.tx.Valid() {
				return errors.New("lpg session has no open transaction")
			}
			if s.w.g.AllocateCommitTS(s.tx) == 0 {
				return errors.New("AllocateCommitTS returned no instant")
			}
			return nil
		}}
}

// await waits, bounded by d, for the frontier to reach the session's own commits.
func (s *lpgSession) await(name string, d time.Duration) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: fmt.Sprintf("lpg: Session.Await, deadline %s", d),
		Hook: func(ctx context.Context) error {
			if s.sess == nil {
				return errors.New("lpg session has made no commit")
			}
			actx, cancel := context.WithTimeout(ctx, d)
			defer cancel()
			return s.sess.Await(actx)
		}}
}

// frontier reports the frontier counters. No drain: none depends on the vacuum.
func (w *world) frontier(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "<MVCCStats: in_flight_commits, sessions_waiting>",
		Probe: func(context.Context) ([]string, [][]string, error) {
			st := w.g.MVCCStats()
			return []string{"in_flight_commits", "sessions_waiting"},
				[][]string{{strconv.FormatUint(st.InFlightCommits, 10), strconv.FormatInt(st.SessionsWaiting, 10)}}, nil
		}}
}

// fpWriters and fpCommits are FP01's fan: fpWriters goroutines, each committing
// fpCommits single-node transactions. Every commit creates its own node, so no
// commit conflicts with another — a sessionless rewrite of one node would be
// refused while the frontier hides its own previous commit (SE04's shape).
const (
	fpWriters = 8
	fpCommits = 256
)

// fanLPG commits fpWriters x fpCommits lpg transactions, sessionless, from
// fpWriters goroutines, and returns once every commit has returned. A commit that
// waited on the held straggler would hold the step past the block timeout and
// show as <waiting ...>.
func (w *world) fanLPG(name string) isolationtest.Step {
	return w.fan(name, fmt.Sprintf("<%d goroutines x %d lpg commits, each creating one :W node>", fpWriters, fpCommits),
		func(ctx context.Context, i, j int) error {
			key := fmt.Sprintf("w-%d-%d", i, j)
			return w.g.ApplyVersionedCtx(ctx, func(tx lpg.WriteTx) error {
				wv := w.g.Writer(tx)
				if err := wv.AddNode(key); err != nil {
					return err
				}
				return wv.SetNodeLabel(key, "W")
			})
		})
}

// fanCypher is fanLPG through sessionless autocommit Cypher statements.
func (w *world) fanCypher(name string) isolationtest.Step {
	return w.fan(name, fmt.Sprintf("<%d goroutines x %d autocommit CREATE (:W)>", fpWriters, fpCommits),
		func(ctx context.Context, i, j int) error {
			res, err := w.eng.RunInTx(ctx, fmt.Sprintf("CREATE (:W {g:%d, i:%d})", i, j), nil)
			if err != nil {
				return err
			}
			return errors.Join(res.Err(), res.Close())
		})
}

func (w *world) fan(name, label string, commit func(ctx context.Context, i, j int) error) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: label, Hook: func(ctx context.Context) error {
		errs := make([]error, fpWriters)
		var wg sync.WaitGroup
		for i := range fpWriters {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range fpCommits {
					if err := commit(ctx, i, j); err != nil {
						errs[i] = fmt.Errorf("writer %d commit %d: %w", i, j, err)
						return
					}
				}
			}()
		}
		wg.Wait()
		return errors.Join(errs...)
	}}
}

const fpVisible = "MATCH (n:W) RETURN count(n) AS visible"

// fpNodes lists every node of an FP02/FP03 graph: s (label S) and t1, which the
// lpg createNode step gives no label and no name.
const fpNodes = "MATCH (n) RETURN labels(n) AS labels, n.name AS name, n.v AS v ORDER BY v, size(labels(n))"

const fp01Doc = "FP01. s0 writes s and reserves its commit instant without publishing it (the commit\n" +
	"hold). s1 then commits %d single-node transactions from %d goroutines%s: every one\n" +
	"returns while s0 is held (no step <waiting ...>), and none is visible to a sessionless\n" +
	"reader, because a commit above an in-flight one is invisible until it publishes (F11).\n" +
	"When s0 publishes, the frontier catches up in one step: all of them become visible\n" +
	"together. The control interleaving runs the fan before the hold."

func fp01(name, via string, fanStep func(*world) isolationtest.Step) func(*world) *isolationtest.Spec {
	return func(w *world) *isolationtest.Spec {
		s0 := w.lpgSession()
		return &isolationtest.Spec{
			Name:  name,
			Doc:   fmt.Sprintf(fp01Doc, fpWriters*fpCommits, fpWriters, via),
			Setup: steps(w.lpgFixture(lpgNode{key: "s", labels: []string{"S"}, props: map[string]int64{"v": 0}})),
			Sessions: []*isolationtest.Session{
				{Name: "s0", Setup: steps(s0.begin("s0b")), Steps: steps(
					s0.set("s0w", "s", "v", 1), s0.allocate("s0a"), s0.commit("s0p"))},
				{Name: "s1", Steps: steps(fanStep(w), w.sessionless("s1lag", fpVisible), w.frontier("s1fr"),
					w.sessionless("s1seen", fpVisible))},
			},
			Permutations: perms(
				"s0w s0a s1fan s1lag s1fr s0p s1seen",
				"s1fan s1lag s1fr s0w s0a s0p s1seen",
			),
			Final: steps(w.frontier("ffr"), q("final", "MATCH (n:S {name:'s'}) RETURN n.v AS v")),
		}
	}
}

func fanLPGStep(w *world) isolationtest.Step    { return w.fanLPG("s1fan") }
func fanCypherStep(w *world) isolationtest.Step { return w.fanCypher("s1fan") }

var (
	fp01Cypher = fp01("fp01-later-commits-ack-behind-held-straggler", " through autocommit Cypher", fanCypherStep)
	fp01LPG    = fp01("fp01-later-commits-ack-behind-held-straggler-lpg", " through the lpg API", fanLPGStep)
)

// awaiter is a session wait running in the background, so a step can observe it
// in flight and a later step can collect what it returned.
type awaiter struct {
	done   chan error
	cancel context.CancelFunc
}

func (w *world) newAwaiter() *awaiter {
	a := &awaiter{}
	w.closers = append(w.closers, a.stop)
	return a
}

// stop ends a wait a permutation left in flight.
func (a *awaiter) stop() {
	if a.cancel != nil {
		a.cancel()
		<-a.done
		a.cancel = nil
	}
}

// awaitOpen is a generous bound for a wait that must END because a peer acts,
// not because time passes: it only keeps a defect from hanging the test.
const awaitOpen = 30 * time.Second

// start launches s's Session.Await on its own goroutine.
func (a *awaiter) start(name string, s *lpgSession) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "<start lpg: Session.Await in the background>", Hook: func(context.Context) error {
		if s.sess == nil {
			return errors.New("lpg session has made no commit")
		}
		ctx, cancel := context.WithTimeout(context.Background(), awaitOpen)
		a.done, a.cancel = make(chan error, 1), cancel
		sess := s.sess
		go func() { a.done <- sess.Await(ctx) }()
		return nil
	}}
}

// waiting reports where the background wait stands: it polls until the wait has
// either registered in MVCCStats.SessionsWaiting or returned — one of the two is
// the wait's settled state, whichever goroutine schedule ran it — and reports
// both.
func (a *awaiter) waiting(name string, w *world) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "<background wait: sessions_waiting, returned>",
		Probe: func(ctx context.Context) ([]string, [][]string, error) {
			deadline := time.Now().Add(awaitOpen)
			for w.g.MVCCStats().SessionsWaiting < 1 && len(a.done) == 0 && time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				case <-time.After(time.Millisecond):
				}
			}
			returned := len(a.done) != 0
			return []string{"sessions_waiting", "returned"},
				[][]string{{strconv.FormatInt(w.g.MVCCStats().SessionsWaiting, 10), strconv.FormatBool(returned)}}, nil
		}}
}

// result collects what the background wait returned.
func (a *awaiter) result(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "<collect the background wait's result>", Hook: func(context.Context) error {
		if a.cancel == nil {
			return errors.New("no background wait")
		}
		err := <-a.done
		a.cancel()
		a.cancel = nil
		return err
	}}
}

func fp02(w *world) *isolationtest.Spec {
	s0, s1 := w.lpgSession(), w.lpgSession()
	a := w.newAwaiter()
	return &isolationtest.Spec{
		Name: "fp02-straggler-abandons-lpg",
		Doc: "FP02. s0 writes s and reserves its commit instant (the commit hold); s1 then commits\n" +
			"t1 and waits, in the background, for its own commit to become visible: the wait is\n" +
			"parked behind s0 (sessions_waiting = 1). s0 then ABANDONS — the fsync-failure path:\n" +
			"WriteTx.Abandon, then EndVersionedTx — and the frontier passes its instant: s1's\n" +
			"wait is released with no error, t1 is visible, s's write is not, and no commit is\n" +
			"left in flight. The control interleaving abandons before s1 commits.",
		Setup: steps(w.lpgFixture(lpgNode{key: "s", labels: []string{"S"}, props: map[string]int64{"v": 0}})),
		Sessions: []*isolationtest.Session{
			{Name: "s0", Setup: steps(s0.begin("s0b")), Steps: steps(s0.set("s0w", "s", "v", 1), s0.allocate("s0a"), s0.abort("s0x"))},
			{Name: "s1", Steps: steps(s1.begin("s1b"), s1.createNode("s1w", "t1", 1), s1.commit("s1c"),
				a.start("s1aw", s1), a.waiting("s1wait", w), a.result("s1rel"),
				w.sessionless("s1r", fpNodes))},
		},
		Permutations: perms(
			"s0w s0a s1b s1w s1c s1aw s1wait s0x s1rel s1r",
			"s0w s0a s0x s1b s1w s1c s1aw s1wait s1rel s1r",
		),
		Final: steps(w.frontier("ffr")),
	}
}

// fp02Released asserts the parked wait was observed parked in the held
// interleaving: the probe saw it registered and not returned.
func fp02Released() isolationtest.Observer {
	return func(o isolationtest.Observation) error {
		if o.Step != "s1wait" || o.Permutation != "s0w s0a s1b s1w s1c s1aw s1wait s0x s1rel s1r" {
			return nil
		}
		if o.Err != nil {
			return fmt.Errorf("the wait probe failed: %w", o.Err)
		}
		if len(o.Rows) != 1 || o.Rows[0][0] != "1" || o.Rows[0][1] != "false" {
			return fmt.Errorf("the wait behind the held straggler was not parked: %v", o.Rows)
		}
		return nil
	}
}

const fp03Doc = "FP03. s0 writes s and reserves its commit instant (the commit hold); s1 commits t1%s\n" +
	"and then waits for its own commit with a 50 ms deadline: the wait returns the context\n" +
	"error instead of hanging (CLAUDE.md, context-aware blocking). After s0 publishes, the\n" +
	"same wait returns at once. The control interleaving publishes s0 before s1 commits."

// fp03Deadline is FP03's bound on a wait that cannot end before it expires.
const fp03Deadline = 50 * time.Millisecond

func fp03LPG(w *world) *isolationtest.Spec {
	s0, s1 := w.lpgSession(), w.lpgSession()
	return &isolationtest.Spec{
		Name:  "fp03-session-wait-bounded-by-context-lpg",
		Doc:   fmt.Sprintf(fp03Doc, " through a lpg.Session"),
		Setup: steps(w.lpgFixture(lpgNode{key: "s", labels: []string{"S"}, props: map[string]int64{"v": 0}})),
		Sessions: []*isolationtest.Session{
			{Name: "s0", Setup: steps(s0.begin("s0b")), Steps: steps(s0.set("s0w", "s", "v", 1), s0.allocate("s0a"), s0.commit("s0p"))},
			{Name: "s1", Steps: steps(s1.begin("s1b"), s1.createNode("s1w", "t1", 1), s1.commit("s1c"),
				s1.await("s1aw", fp03Deadline), s1.await("s1aw2", awaitOpen))},
		},
		Permutations: perms(
			"s0w s0a s1b s1w s1c s1aw s0p s1aw2",
			"s0w s0a s0p s1b s1w s1c s1aw s1aw2",
		),
		Final: steps(w.frontier("ffr"), w.sessionless("final", fpNodes)),
	}
}

// cypherSession is a cypher.Session bound to the engine of the permutation in
// force.
type cypherSession struct{ s *cypher.Session }

func (w *world) cypherSession() *cypherSession {
	cs := &cypherSession{}
	w.resets = append(w.resets, func() { cs.s = w.eng.NewSession() })
	return cs
}

// run runs query through the session, bounded by d.
func (cs *cypherSession) run(name, query string, d time.Duration) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: fmt.Sprintf("<cypher.Session, deadline %s> %s", d, query),
		Probe: func(ctx context.Context) ([]string, [][]string, error) {
			rctx, cancel := context.WithTimeout(ctx, d)
			defer cancel()
			res, err := cs.s.RunInTx(rctx, query, nil)
			if err != nil {
				return nil, nil, err
			}
			cols := res.Columns()
			var rows [][]string
			for res.Next() {
				row := make([]string, len(cols))
				for i := range cols {
					if v := res.ValueAt(i); v != nil {
						row[i] = v.String()
					} else {
						row[i] = "null"
					}
				}
				rows = append(rows, row)
			}
			return cols, rows, errors.Join(res.Err(), res.Close())
		}}
}

func fp03Cypher(w *world) *isolationtest.Spec {
	s0 := w.lpgSession()
	cs := w.cypherSession()
	const read = "MATCH (n {name:'t1'}) RETURN n.name AS name"
	return &isolationtest.Spec{
		Name:  "fp03-session-read-bounded-by-context",
		Doc:   fmt.Sprintf(fp03Doc, " through a cypher.Session (autocommit CREATE)"),
		Setup: steps(w.lpgFixture(lpgNode{key: "s", labels: []string{"S"}, props: map[string]int64{"v": 0}})),
		Sessions: []*isolationtest.Session{
			{Name: "s0", Setup: steps(s0.begin("s0b")), Steps: steps(s0.set("s0w", "s", "v", 1), s0.allocate("s0a"), s0.commit("s0p"))},
			{Name: "s1", Steps: steps(cs.run("s1w", "CREATE (n:N {name:'t1'}) RETURN n.name AS name", awaitOpen),
				cs.run("s1r", read, fp03Deadline), cs.run("s1r2", read, awaitOpen))},
		},
		Permutations: perms(
			"s0w s0a s1w s1r s0p s1r2",
			"s0w s0a s0p s1w s1r s1r2",
		),
		Final: steps(w.frontier("ffr"), w.sessionless("final", "MATCH (n) RETURN n.name AS name, n.v AS v ORDER BY name")),
	}
}
