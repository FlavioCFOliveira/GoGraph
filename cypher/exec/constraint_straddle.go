package exec

// constraint_straddle.go — commit-time enforcement of a constraint registered
// while an explicit transaction was open (rmp #2936).
//
// # The defect
//
// UNIQUE and NOT NULL are enforced on every write, at the instant the write is
// made. A constraint registered AFTER a transaction's write never saw that write:
// CREATE CONSTRAINT validates and seeds from committed state, which cannot see an
// uncommitted write, so a value the transaction wrote before the constraint
// existed was never checked or reserved. Measured, one goroutine and no race —
// and at HEAD 43c69dbe as well:
//
//	BEGIN; CREATE (:L {s: 'v'})
//	CREATE CONSTRAINT FOR (n:L) REQUIRE n.s IS UNIQUE
//	COMMIT
//	CREATE (:L {s: 'v'})                 ← accepted: two nodes, one UNIQUE value
//
// # The model
//
// A transaction T that straddles constraint K on (label L, property p) commits
// correctly when, afterwards, (1) the committed graph satisfies K and (2) K's
// value-set V equals the set of values the live committed :L nodes hold under p.
// Every other writer maintains (2) on its own. T must therefore turn V from the
// state before its commit to the state after it — no more and no less.
//
// For T's touched nodes — every node whose labels, properties or life T wrote —
// read the value each holds under K in two states:
//
//   - B, the LATEST COMMITTED state WITHOUT T's writes: what V describes for
//     those nodes right now;
//   - A, the latest committed state WITH T's writes: what T's commit leaves.
//
// Then T's commit needs exactly: the values in A distinct (no two of T's nodes
// share one); every value of A\B reserved, refused if another writer holds it;
// every value of B\A released at commit; and nothing done for A∩B. The last
// clause is what nets a value moving between T's own nodes — freed by one and
// taken by another — which a check that reserved before counting T's releases
// refused (audit R4-2). NOT NULL needs every node of A carrying L to carry p.
//
// "Another writer holds it" means the value is in V and T did not insert it
// there. V holds, besides the committed values, every pending reservation,
// T's included; a value of A\B that T's own reservation INSERTED into this V
// is T's and passes, and it is released at commit when T no longer holds it.
// A reservation that found the value already present inserted nothing and
// claims nothing. A write takes such a value when T released it, but T judged
// that release from its snapshot, and a peer that committed before K existed
// may since have moved the value to a node T never touched: V then holds it
// for that node, the value is outside B, and T must be refused and must not
// release it (audit R5-1). Each reservation mark therefore records whether it
// inserted.
//
// # Why the LATEST committed state and not T's snapshot
//
// T's snapshot cannot see a commit made after T began. Conflicts here are per
// SUBSTORE — labels and properties are versioned separately — so a peer that
// changed the property of a node while T changed its label, and committed
// before the constraint existed, did not collide with T and stamped nothing,
// having no constraint to stamp for. T's snapshot then showed the node with its
// old value, and the value-set was left with a stale value and without the
// real one (audit R4-1: two nodes with one UNIQUE value). The latest committed
// state shows the peer's value; merged with T's label, it is what the graph
// holds after T commits.
//
// The latest committed state must not change underneath the validation. It
// cannot, for a touched node: before validating, T stamps every touched node's
// constraint slot ([lpg.WriteView.NoteConstraintTouchByID]), and a peer that
// commits a change to that node afterwards stamps it too — at its write when it
// began after the registration, at its own commit when it straddles the
// constraint as well — and collides with T's stamp, pending or committed after
// its start. A peer that stamped before T is refused to T by the same collision.
//
// # Why not node-level first-committer-wins
//
// Refusing T whenever any touched node's label or property head was committed
// after T's start would also be correct, and it is what both reference engines
// give for free: Memgraph keeps one delta chain per vertex and refuses a write
// whose head delta is not committed before the transaction's start
// (memgraph/memgraph 309c286, src/storage/v2/mvcc.hpp, PrepareForWrite), and
// PostgreSQL versions the whole row, so a concurrent update is TM_Updated
// (postgres/postgres REL_17_STABLE 104bec3, src/backend/access/heap/heapam.c,
// heap_update). This engine versions labels and properties in separate
// substores and deliberately admits write skew across them; refusing T for a
// concurrent write the merged state proves harmless would refuse transactions
// the rest of the engine accepts. Validating the merged latest state is the
// minimal rule that is correct, and the stamp is the only exclusion it needs.
//
// # Why a reservation is bound to its value-set's generation
//
// A DROP and re-CREATE of a constraint — or a DROP whose durable commit failed
// and was rewound — builds a NEW value-set. A reservation T took in the old one
// is absent from it, and the inverse that withdraws that reservation on
// rollback must not delete the same value from the new one, where a peer may
// now hold it (audit R4-3). Every reservation therefore carries the generation
// its constraint was registered at, and so does every inverse; an inverse whose
// generation no longer matches does nothing.
//
// # Ordering against the constraint build
//
// T runs this inside its commit-decision bracket ([index.Manager.EnterCommit]),
// and a constraint is built holding that bracket exclusively
// ([index.Manager.HoldCommitDecisions]) from before its validation scan until
// after its registration, so a validation either completes before the build's
// scan — then T has published and the scan sees its nodes — or starts after the
// registration, seed and generation step are visible. Two validations serialise
// on the registry's lock, which covers both the check and the reservation, so
// of two carrying the same value exactly one reserves it.
//
// # Prior art
//
// Validating the transaction's final state at commit is what Neo4j does through
// the commit's TxStateVisitor (neo4j/neo4j 54a7dcf, community/kernel/.../api/
// KernelTransactionImplementation.java, enforceConstraints) and Memgraph does
// under its engine lock (src/storage/v2/inmemory/storage.cpp,
// UniqueConstraintsViolation). Both also keep a transaction from straddling
// constraint DDL at all — Memgraph builds a constraint under a READ_ONLY or
// UNIQUE storage accessor — which this engine does not.

import (
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// StraddleReader describes, for [ConstraintRegistry.ValidateStraddler], the
// nodes a committing transaction touched and their state before and after its
// commit.
type StraddleReader interface {
	// Touched returns every node whose labels, properties or life the
	// transaction changed, each once.
	Touched() []graph.NodeID
	// Before reports whether id carries label in the latest committed state
	// without the transaction's writes, and its value of prop there (the zero
	// value when absent). A node that does not exist carries no label.
	Before(id graph.NodeID, label, prop string) (hasLabel bool, value lpg.PropertyValue)
	// After is Before with the transaction's writes.
	After(id graph.NodeID, label, prop string) (hasLabel bool, value lpg.PropertyValue)
}

// Generation returns the constraint catalogue's generation: it advances each
// time a UNIQUE or NOT NULL constraint is registered. A nil registry reports 0.
//
// Safe for concurrent use.
func (r *ConstraintRegistry) Generation() uint64 {
	if r == nil {
		return 0
	}
	return r.gen.Load()
}

// releaseInGeneration deletes val from the value-set of key when that set is
// still the one of generation gen, and does nothing otherwise.
func (r *ConstraintRegistry) releaseInGeneration(key ckey, val string, gen uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if gen == 0 || r.uniqueSince[key] != gen {
		return
	}
	if vs := r.valueSets[key]; vs != nil {
		delete(vs, val)
	}
}

// straddledConstraint is one constraint registered after a generation.
type straddledConstraint struct {
	key    ckey
	name   string
	unique bool
}

// straddled returns the constraints registered after generation since.
func (r *ConstraintRegistry) straddled(since uint64) []straddledConstraint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []straddledConstraint
	for key, at := range r.uniqueSince {
		if at > since {
			out = append(out, straddledConstraint{key: key, name: r.uniqueNames[key], unique: true})
		}
	}
	for key, at := range r.notNullSince {
		if at > since {
			out = append(out, straddledConstraint{key: key, name: r.notNullNames[key]})
		}
	}
	return out
}

// straddleViolation wraps a violation of the named constraint.
func straddleViolation(c straddledConstraint, kind, detail string) error {
	return fmt.Errorf("constraint %q: %w", c.name, &ConstraintViolationError{
		Label: c.key.label, Property: c.key.prop, Kind: kind, Detail: detail,
	})
}

// ValidateStraddler enforces, over a committing transaction's writes merged into
// the latest committed state, every UNIQUE and NOT NULL constraint registered
// after generation since; see the file comment for the model. It returns nil at
// once when none has been. ct is the transaction's constraint contribution and
// journal records the inverse of every change this makes, so a rollback
// withdraws it; journal must not be nil.
//
// It returns an error wrapping a [*ConstraintViolationError] and naming the
// constraint on the first violation. A violation found after some changes were
// made leaves them to the caller's rollback, which the journal undoes.
//
// The caller must have stamped every touched node's constraint slot first.
//
// Safe for concurrent use; ct belongs to the calling transaction.
func (r *ConstraintRegistry) ValidateStraddler(ct *ConstraintTxn, since uint64, rd StraddleReader, journal func(inv func())) error {
	if r == nil || r.gen.Load() == since {
		return nil
	}
	cs := r.straddled(since)
	if len(cs) == 0 {
		return nil
	}
	nodes := rd.Touched()
	for _, c := range cs {
		if c.unique {
			continue
		}
		for _, id := range nodes {
			if has, v := rd.After(id, c.key.label, c.key.prop); has && v.Kind() == 0 {
				return straddleViolation(c, "NOT NULL", "value is null")
			}
		}
	}
	for _, c := range cs {
		if c.unique {
			if err := r.validateStraddledUnique(ct, c, nodes, rd, journal); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateStraddledUnique is ValidateStraddler for one UNIQUE constraint: the
// graph is read first, and the value-set is then checked and changed in one
// critical section.
func (r *ConstraintRegistry) validateStraddledUnique(
	ct *ConstraintTxn, c straddledConstraint, nodes []graph.NodeID, rd StraddleReader, journal func(inv func()),
) error {
	before := make(map[string]struct{}, len(nodes)) // B
	after := make(map[string]lpg.PropertyValue, len(nodes))
	holder := make(map[string]graph.NodeID, len(nodes))
	for _, id := range nodes {
		if had, v := rd.Before(id, c.key.label, c.key.prop); had {
			if s, ok := propertyValueToString(v); ok {
				before[s] = struct{}{}
			}
		}
		has, v := rd.After(id, c.key.label, c.key.prop)
		if !has {
			continue
		}
		s, ok := propertyValueToString(v)
		if !ok {
			continue // null: UNIQUE does not constrain it
		}
		if other, dup := holder[s]; dup && other != id {
			return straddleViolation(c, "UNIQUE", fmt.Sprintf("value %s already exists", humanConstraintValue(v)))
		}
		holder[s], after[s] = id, v
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	vs := r.valueSets[c.key]
	if vs == nil {
		return nil // dropped again since: nothing to enforce
	}
	gen := r.uniqueSince[c.key]
	anyMark := r.straddleAnyMarkForTest
	// A\B must be free of every other holder. A value this transaction itself
	// INSERTED in THIS value-set is its own; B, which it releases, cannot
	// contain a value it still needs, because A∩B is left alone.
	//
	// A reservation that inserted nothing is no claim on the value. A write may
	// take a value its transaction released, and the release was judged from
	// the transaction's snapshot: a peer that committed before the constraint
	// existed may since have moved the value to a node this transaction never
	// touched, and no conflict reports it, the label and the property being
	// separate substores. The value is then in the set, held by that node, and
	// must refuse this transaction (audit R5-1).
	var take []string
	for s := range after {
		if _, held := before[s]; held {
			continue // A∩B: the value stays where the graph already has it
		}
		if ct.insertedIn(c.key, s, gen, anyMark) {
			continue
		}
		if _, exists := vs[s]; exists {
			return straddleViolation(c, "UNIQUE", fmt.Sprintf("value %s already exists", humanConstraintValue(after[s])))
		}
		take = append(take, s)
	}
	for _, s := range take {
		vs[s] = struct{}{}
		ct.markReserved(c.key, s, gen|reservationInserted)
		key, val := c.key, s
		journal(func() {
			r.releaseInGeneration(key, val, gen)
			ct.unmarkReserved(key, val)
		})
	}
	// The releases at commit are exactly B\A, plus any value this transaction
	// inserted in this value-set and no longer holds. A value it reserved
	// without inserting is left alone: it was in the set before, for another
	// holder, and releasing it would free a value that holder still has (audit
	// R5-1, the reservation given up before commit). Whatever the statements
	// marked for this key before is replaced: they reasoned over the
	// transaction's snapshot, not over what its commit leaves.
	var prior []string
	ct.forEachReleased(func(kv ckeyval) {
		if kv.key == c.key {
			prior = append(prior, kv.val)
		}
	})
	for _, s := range prior {
		ct.unmarkReleased(c.key, s)
	}
	var marked []string
	for s := range before {
		if _, kept := after[s]; !kept {
			ct.markReleased(c.key, s)
			marked = append(marked, s)
		}
	}
	ct.forEachInsertedIn(c.key, gen, anyMark, func(s string) {
		if _, kept := after[s]; kept {
			return
		}
		if _, isBefore := before[s]; isBefore {
			return
		}
		ct.markReleased(c.key, s)
		marked = append(marked, s)
	})
	key := c.key
	journal(func() {
		for _, s := range marked {
			ct.unmarkReleased(key, s)
		}
		for _, s := range prior {
			ct.markReleased(key, s)
		}
	})
	return nil
}

// straddlesUnique reports whether the UNIQUE constraint on (label, prop) was
// registered after the transaction owning ct began, so the transaction's
// writes may predate it. False when ct tracks nothing.
func (r *ConstraintRegistry) straddlesUnique(ct *ConstraintTxn, label, prop string) bool {
	if ct == nil || !ct.tracking {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	at, ok := r.uniqueSince[constraintKey(label, prop)]
	return ok && at > ct.trackFrom
}

// adoptStraddledRelease recognises, for a UNIQUE refusal err of a write of
// value by the transaction owning ct, a release the transaction made BEFORE the
// refusing constraint existed (rmp #2948).
//
// Such a release recorded no mark — there was no value-set to mark it in — so
// the statement-time check finds the value held by the very node the
// transaction freed, and refused a transaction whose final state may be valid:
//
//	BEGIN; MATCH (n:L {s: 'a'}) REMOVE n:L
//	CREATE CONSTRAINT FOR (n:L) REQUIRE n.s IS UNIQUE
//	CREATE (:L {s: 'a'})                 ← refused, although T alone holds 'a'
//
// When the constraint was registered after the transaction began, and a node
// the transaction touched holds the value in the latest committed state without
// its writes and no longer holds it with them, the release is marked now and
// true is returned, so the caller retries the reservation. The decision is then
// the commit's ([ConstraintRegistry.ValidateStraddler]), which validates the
// final state against the latest committed one and replaces every release mark
// the statements recorded for the constraint. A value held by any other writer
// — a committed node the transaction never touched, or a live peer's
// reservation — was not released by the transaction, and the refusal stands.
//
// The reader reads the graph, so this runs outside the registry's lock.
func (r *ConstraintRegistry) adoptStraddledRelease(ct *ConstraintTxn, err error, value lpg.PropertyValue) bool {
	if ct == nil || ct.straddleState == nil {
		return false
	}
	var cv *ConstraintViolationError
	if !errors.As(err, &cv) || cv.Kind != "UNIQUE" {
		return false
	}
	val, ok := propertyValueToString(value)
	if !ok {
		return false
	}
	key := constraintKey(cv.Label, cv.Property)
	if ct.releasedHere(key, val) || !r.straddlesUnique(ct, cv.Label, cv.Property) {
		return false
	}
	rd := ct.straddleState()
	released := false
	for _, id := range rd.Touched() {
		had, bv := rd.Before(id, cv.Label, cv.Property)
		if !had {
			continue
		}
		if bs, ok := propertyValueToString(bv); !ok || bs != val {
			continue
		}
		if has, av := rd.After(id, cv.Label, cv.Property); has {
			if as, ok := propertyValueToString(av); ok && as == val {
				continue // the node still holds it: no release
			}
		}
		released = true
		break
	}
	if !released {
		return false
	}
	ct.markReleased(key, val)
	return true
}
