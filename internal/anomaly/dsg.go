package anomaly

import (
	"fmt"
	"sort"
)

// Dep is the kind of dependency an edge carries. Adya's three, and only his
// three: everything the checker concludes is a statement about paths made of
// these.
type Dep uint8

const (
	// WW is a write-dependency: Ti installs a version of x, and Tj installs the
	// version of x that immediately follows it in the version order.
	WW Dep = iota
	// WR is a read-dependency: Ti installs a version of x, and Tj reads it.
	WR
	// RW is an anti-dependency: Ti reads a version of x, and Tj installs the
	// version of x that immediately follows the one Ti read.
	RW
)

func (d Dep) String() string {
	switch d {
	case WW:
		return "ww"
	case WR:
		return "wr"
	default:
		return "rw"
	}
}

// Edge is one dependency between two committed transactions.
type Edge struct {
	Key  string
	From TxID
	To   TxID
	Dep  Dep
}

func (e Edge) String() string { return fmt.Sprintf("%d -%s(%s)-> %d", e.From, e.Dep, e.Key, e.To) }

// DSG is Adya's direct serialization graph over the COMMITTED transactions of a
// history.
//
// Aborted transactions are deliberately absent: Adya's DSG is defined over
// committed transactions, and a read of an aborted transaction's write is not an
// edge but the separate phenomenon G1a. Including them would turn every aborted
// write into a spurious cycle.
type DSG struct {
	// Out[t] are the edges leaving transaction t.
	Out map[TxID][]Edge
	// Nodes are the committed transaction ids, sorted, so every traversal of the
	// graph is deterministic and two runs classify a history identically.
	Nodes []TxID
}

// BuildDSG constructs the dependency graph, and reports the G1a and G1b
// phenomena it necessarily discovers on the way.
//
// G1a and G1b are found HERE rather than by a cycle search because neither is a
// cycle: G1a is a read of a version an aborted transaction wrote, and G1b is a
// read of a version its writer later superseded within the same transaction.
// Both are properties of a single read, and both are invisible to any amount of
// graph traversal.
func BuildDSG(h *History) (*DSG, []Anomaly) {
	var found []Anomaly

	// writerOf maps each version of each key to the transaction that installed
	// it, and finalOf records that transaction's LAST write of that key — which
	// is the whole of the G1b definition.
	writerOf := make(map[verKey]*Txn, len(h.Txns)*2)
	finalOf := make(map[verKey]bool, len(h.Txns)*2)
	versions := make(map[string][]Version, 8)
	for i := range h.Txns {
		t := &h.Txns[i]
		lastWrite := make(map[string]Version, len(t.Ops))
		for _, op := range t.Ops {
			if op.Kind != Write {
				continue
			}
			writerOf[verKey{op.Key, op.Ver}] = t
			versions[op.Key] = append(versions[op.Key], op.Ver)
			lastWrite[op.Key] = op.Ver
		}
		for key, ver := range lastWrite {
			finalOf[verKey{key, ver}] = true
		}
	}
	// The version order per key. Numeric, because a version IS a commit instant.
	for key := range versions {
		v := versions[key]
		sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
		versions[key] = v
	}

	committed := make(map[TxID]struct{}, len(h.Txns))
	nodes := make([]TxID, 0, len(h.Txns))
	for i := range h.Txns {
		if t := &h.Txns[i]; !t.Aborted {
			committed[t.ID] = struct{}{}
			nodes = append(nodes, t.ID)
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i] < nodes[j] })

	g := &DSG{Out: make(map[TxID][]Edge, len(nodes)), Nodes: nodes}
	add := func(e Edge) {
		if e.From == e.To {
			// A transaction depends on itself only through its own operations,
			// which Adya's DSG excludes: a self-edge would make every
			// read-your-own-write a cycle.
			return
		}
		if _, ok := committed[e.From]; !ok {
			return
		}
		if _, ok := committed[e.To]; !ok {
			return
		}
		g.Out[e.From] = append(g.Out[e.From], e)
	}

	// ww: consecutive versions of the same key.
	for key, vs := range versions {
		for i := 1; i < len(vs); i++ {
			prev, next := writerOf[verKey{key, vs[i-1]}], writerOf[verKey{key, vs[i]}]
			if prev == nil || next == nil {
				continue
			}
			add(Edge{From: prev.ID, To: next.ID, Dep: WW, Key: key})
		}
	}

	// wr and rw, plus G1a and G1b, all from the reads.
	for i := range h.Txns {
		t := &h.Txns[i]
		for _, op := range t.Ops {
			if op.Kind != Read || op.Ver == InitVersion {
				continue
			}
			w := writerOf[verKey{op.Key, op.Ver}]
			if w == nil {
				// A version nobody in this history wrote. Not classifiable as a
				// phenomenon — it means the history is incomplete — so it is
				// reported as such rather than silently skipped.
				found = append(found, Anomaly{
					Type: Unwritten,
					Txns: []TxID{t.ID},
					Detail: fmt.Sprintf("transaction %d read %s=%d, a version no transaction in this history wrote",
						t.ID, op.Key, op.Ver),
				})
				continue
			}
			if w.Aborted && w.ID != t.ID {
				found = append(found, Anomaly{
					Type: G1a,
					Txns: []TxID{w.ID, t.ID},
					Detail: fmt.Sprintf("transaction %d read %s=%d, written by transaction %d, which ABORTED",
						t.ID, op.Key, op.Ver, w.ID),
				})
				continue
			}
			if w.ID != t.ID && !finalOf[verKey{op.Key, op.Ver}] {
				found = append(found, Anomaly{
					Type: G1b,
					Txns: []TxID{w.ID, t.ID},
					Detail: fmt.Sprintf("transaction %d read %s=%d, an INTERMEDIATE version: transaction %d "+
						"overwrote it before committing", t.ID, op.Key, op.Ver, w.ID),
				})
				continue
			}
			add(Edge{From: w.ID, To: t.ID, Dep: WR, Key: op.Key})

			// rw: whoever installed the next version of this key after the one
			// read. Anti-dependency is what makes a reader ORDERED BEFORE a later
			// writer, and it is the edge every G2 verdict turns on.
			if next, ok := successor(versions[op.Key], op.Ver); ok {
				if nw := writerOf[verKey{op.Key, next}]; nw != nil && !nw.Aborted {
					add(Edge{From: t.ID, To: nw.ID, Dep: RW, Key: op.Key})
				}
			}
		}
	}
	for id := range g.Out {
		e := g.Out[id]
		sort.Slice(e, func(i, j int) bool {
			if e[i].To != e[j].To {
				return e[i].To < e[j].To
			}
			if e[i].Dep != e[j].Dep {
				return e[i].Dep < e[j].Dep
			}
			return e[i].Key < e[j].Key
		})
		g.Out[id] = e
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].Type != found[j].Type {
			return found[i].Type < found[j].Type
		}
		return found[i].Detail < found[j].Detail
	})
	return g, found
}

// successor returns the version immediately after v in a key's version order.
func successor(vs []Version, v Version) (Version, bool) {
	i := sort.Search(len(vs), func(i int) bool { return vs[i] > v })
	if i == len(vs) {
		return 0, false
	}
	return vs[i], true
}
