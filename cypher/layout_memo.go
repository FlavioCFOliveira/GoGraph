package cypher

import (
	"sync"
	"sync/atomic"
)

// layoutMemoMaxEntries bounds each site map of a [layoutMemo]. A cached plan
// reaches a fixed number of build sites; a key that is fresh on every execution
// (a node synthesised by a per-execution rewrite) fills the memo to this
// ceiling and is then answered by a fresh snapshot, which is the behaviour that
// preceded the memo.
const layoutMemoMaxEntries = 256

// layoutMemo caches, for one [planCacheEntry], the row-layout snapshots the read
// build takes of its schema (rmp #3097): the [rowSchema] a Filter predicate binds
// against, and the input-schema copy and general-path [rowSchema] a Projection
// binds against. Without it every execution of a cached plan copied the schema
// map and rebuilt its column walk at each of those sites — 5.9 copies, 765 B per
// execution of an indexed point read (docs/design-plan-pure-separation.md §1).
//
// # Why a cached snapshot is the one the build would take
//
// A snapshot is a pure function of the schema's CONTENT at the site, and a
// stored snapshot is returned only when the live schema has exactly the same
// entries ([sameLayout]); any other content gets a fresh snapshot, as before. The
// memo therefore never has to know which gates shaped the layout. It is used only
// on executions where no reorder or anchor swap fired (see
// [Engine.buildReadPhysical]); content equality is what makes it exact.
//
// The one observable difference is the ORDER of names that share a column in
// the walk (secondary expression keys): [newSchemaWalk] sorts by column with an
// unstable sort over map iteration order, so that order was already arbitrary
// per build; a cached walk fixes one of the orders a fresh build could produce.
//
// # Concurrency
//
// Safe for concurrent use by every execution of the entry. Each key is stored
// at most once (LoadOrStore) and never overwritten or deleted; a stored
// [rowSchema] and its map are never written after publication — every consumer
// of a snapshot only reads it (rowBindPlan, the typed predicate parser, the
// projection fast paths). Executions therefore share only read-only state.
type layoutMemo struct {
	filter    sync.Map // ast.Expression (pointer) -> *rowSchema
	projInput sync.Map // *ir.ProjectionItem -> *rowSchema (cols only is read)
	projGen   sync.Map // *ir.ProjectionItem -> *rowSchema

	entries atomic.Int64

	// hits and misses let a test prove the memo is consulted.
	hits, misses atomic.Int64
}

// rowSchemaAt returns a snapshot of schema for the build site (m, key): the
// stored one when its content equals schema, otherwise a fresh one, stored when
// the key is new and the ceiling allows. A nil memo or key always yields a fresh
// snapshot.
func (lm *layoutMemo) rowSchemaAt(m *sync.Map, key any, schema map[string]int) rowSchema {
	if lm == nil || key == nil {
		return newRowSchema(copySchema(schema))
	}
	if v, ok := m.Load(key); ok {
		//nolint:forcetypeassert // the layout memo is unexported and only ever stores *rowSchema
		rs := v.(*rowSchema)
		if sameLayout(rs.cols, schema) {
			lm.hits.Add(1)
			return *rs
		}
		lm.misses.Add(1)
		return newRowSchema(copySchema(schema))
	}
	lm.misses.Add(1)
	rs := newRowSchema(copySchema(schema))
	if lm.entries.Load() >= layoutMemoMaxEntries {
		return rs
	}
	if _, loaded := m.LoadOrStore(key, &rs); !loaded {
		lm.entries.Add(1)
	}
	return rs
}

// sameLayout reports whether a and b hold exactly the same entries.
func sameLayout(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// filterRowSchema is the [rowSchema] a Filter predicate binds against: from the
// entry's layout memo when the build carries one, otherwise a fresh snapshot.
func (b *buildOpts) filterRowSchema(pred any, schema map[string]int) rowSchema {
	if b == nil || b.layoutMemo == nil {
		return newRowSchema(copySchema(schema))
	}
	return b.layoutMemo.rowSchemaAt(&b.layoutMemo.filter, pred, schema)
}

// projInputSchema is the read-only copy of a Projection's input schema; key is
// the projection's first item.
func (b *buildOpts) projInputSchema(key any, schema map[string]int) map[string]int {
	if b == nil || b.layoutMemo == nil {
		return copySchema(schema)
	}
	return b.layoutMemo.rowSchemaAt(&b.layoutMemo.projInput, key, schema).cols
}

// projGeneralRowSchema is the [rowSchema] a Projection's general-path items bind
// against; key is the projection's first item.
func (b *buildOpts) projGeneralRowSchema(key any, schema map[string]int) rowSchema {
	if b == nil || b.layoutMemo == nil {
		return newRowSchema(copySchema(schema))
	}
	return b.layoutMemo.rowSchemaAt(&b.layoutMemo.projGen, key, schema)
}
