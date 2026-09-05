package procs

// builtin_db.go — built-in db.* procedure registrations (task-300).
//
// RegisterBuiltins registers the standard db.* introspection procedures into
// reg. mgr and every closure in BuiltinSources may be nil; procedures that
// depend on a nil source return empty result sets in that case.
//
// Registered procedures:
//
//   - db.indexes()           → name string, type string
//   - db.constraints()       → name string, type string, label string, property string
//   - db.labels()            → label string (labels in use on live nodes)
//   - db.relationshipTypes() → relationshipType string
//   - db.propertyKeys()      → propertyKey string
//   - db.schema.visualization() → nodes list, relationships list

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// numericCompanionSuffix is the reserved name suffix of the internal numeric
// btree companion that a btree CREATE INDEX registers alongside the user-named
// string btree (#1652, cypher.numericBTreeName). db.indexes() filters names
// carrying it so the user sees exactly the index they created. It is duplicated
// here rather than imported because procs must not depend on the cypher package
// (cypher imports procs).
const numericCompanionSuffix = "_btree_num"

// BuiltinSources bundles the data-source callbacks the built-in db.* procedures
// query at invocation time. It decouples the procs package from the concrete
// graph type: callers (typically the engine) supply pure closures that read
// live graph state, and procs never imports the graph layer.
//
// Every field is optional. A nil closure makes its corresponding procedure
// return an empty result set, mirroring the nil-index-manager behaviour of
// db.indexes().
type BuiltinSources struct {
	// ListConstraints is invoked by db.constraints() to obtain the current
	// constraint rows (each row is [name, type, label, property]).
	ListConstraints func() [][]expr.Value
	// Labels is invoked by db.labels() to obtain the distinct node labels
	// currently attached to live nodes, one per returned name.
	Labels func() []string
	// RelationshipTypes is invoked by db.relationshipTypes() to obtain the
	// distinct relationship types in use, one per returned name.
	RelationshipTypes func() []string
	// PropertyKeys is invoked by db.propertyKeys() to obtain the distinct
	// property keys in use, one per returned name.
	PropertyKeys func() []string
	// RefreshStatistics rebuilds the planner statistics and publishes a fresh
	// snapshot, backing db.stats.refresh(). It is the engine's own
	// Engine.RefreshStatistics, threaded here so the maintenance entry point is
	// reachable from Cypher — and therefore from Bolt — instead of only from Go
	// (#2196). May be nil, in which case the procedure reports that statistics are
	// unavailable rather than silently succeeding.
	RefreshStatistics func(context.Context) error
}

// RegisterBuiltins registers all built-in db.* procedures into reg.
//
// mgr is the index manager used by db.indexes(); it may be nil (the procedure
// returns an empty result).
//
// src carries the enumeration closures backing db.constraints(), db.labels(),
// db.relationshipTypes() and db.propertyKeys(); see [BuiltinSources]. Each
// closure may be nil, in which case its procedure returns an empty set.
//
// hugeParam: BuiltinSources is small and passed by value intentionally
func RegisterBuiltins(reg *Registry, mgr *index.Manager, src BuiltinSources) {
	mustRegister(reg, dbIndexes(mgr))
	mustRegister(reg, dbConstraints(src.ListConstraints))
	mustRegister(reg, dbLabels(src.Labels))
	mustRegister(reg, dbRelationshipTypes(src.RelationshipTypes))
	mustRegister(reg, dbPropertyKeys(src.PropertyKeys))
	mustRegister(reg, dbSchemaVisualization())
	mustRegister(reg, dbStatsRefresh(src.RefreshStatistics, &statsRefreshLimiter{}))
}

// statsRefreshMinInterval bounds how often db.stats.refresh() will actually rebuild.
//
// The rebuild is an O(nodes x properties) scan, and the procedure is reachable by any
// Bolt client. Unbounded, that is an amplification vector: a caller could drive repeated
// full scans on an otherwise idle server. The interval turns the capability into one that
// cannot be used to burn CPU — a refresh inside the window is REFUSED as a no-op with the
// remaining wait reported, rather than queued (queueing would just move the amplification
// into memory).
//
// Chosen at 30 seconds because statistics are explicitly best-effort and advisory: a plan
// built from statistics up to 30 s stale is the normal case anyway, since nothing refreshes
// them automatically. A caller that genuinely needs a rebuild sooner has the Go entry
// point, Engine.RefreshStatistics, which is not rate-limited because an embedded caller is
// already inside the trust boundary.
const statsRefreshMinInterval = 30 * time.Second

// statsRefreshLimiter bounds the rate at which a Cypher caller may drive a statistics
// rebuild. It is safe for concurrent use: the whole check-and-stamp is one critical
// section, so two simultaneous callers cannot both pass.
type statsRefreshLimiter struct {
	mu   sync.Mutex
	last time.Time
	now  func() time.Time // injectable for tests; nil means time.Now
}

// allow reports whether a rebuild may proceed now, and when it may not, how long remains.
// A permitted call stamps the clock immediately, so a concurrent second caller is refused
// even if the rebuild it permitted has not finished.
func (l *statsRefreshLimiter) allow() (bool, time.Duration) {
	nowFn := l.now
	if nowFn == nil {
		nowFn = time.Now
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := nowFn()
	if !l.last.IsZero() {
		if elapsed := now.Sub(l.last); elapsed < statsRefreshMinInterval {
			return false, statsRefreshMinInterval - elapsed
		}
	}
	l.last = now
	return true, 0
}

// dbStatsRefresh backs `CALL db.stats.refresh()`, the maintenance entry point that
// rebuilds the planner statistics and publishes a fresh snapshot (#2196).
//
// Statistics are deliberately never maintained by a background goroutine — they are
// best-effort and caller-driven — so something has to drive the rebuild.
// Engine.RefreshStatistics has existed since #2098 but was reachable only from Go, which
// left every Bolt client unable to refresh them at all.
//
// It is RATE-LIMITED (see [statsRefreshMinInterval]). Exposing an O(nodes x properties)
// scan to untrusted CALL without a bound would be an amplification vector, which is why
// the read-only procedure fence refused the unbounded form; the limit is what makes the
// capability safe to expose rather than merely useful.
//
// It returns one row reporting the outcome rather than an empty result, so a client can
// tell a completed rebuild from a refusal: `ok` is false, with a reason, when the engine
// has no statistics support or when the call arrived inside the rate-limit window. A
// rebuild that FAILS returns the error, because a caller that asked for a refresh and did
// not get one must be told — fail-stop, not a silent partial.
//
// The rebuild honours the caller's context: cancelling the query cancels the scan, which
// returns without publishing a partial snapshot.
func dbStatsRefresh(refresh func(context.Context) error, limiter *statsRefreshLimiter) ProcEntry {
	return ProcEntry{
		Sig: Signature{
			Namespace: []string{"db", "stats"},
			Name:      "refresh",
			Inputs:    nil,
			Outputs: []NamedType{
				{Name: "ok", Kind: expr.KindBool},
				{Name: "detail", Kind: expr.KindString},
			},
		},
		Impl: func(ctx context.Context, _ []expr.Value) ([][]expr.Value, error) {
			row := func(ok bool, detail string) [][]expr.Value {
				return [][]expr.Value{{expr.BoolValue(ok), expr.StringValue(detail)}}
			}
			if refresh == nil {
				return row(false, "planner statistics are not available on this engine"), nil
			}
			if limiter != nil {
				if allowed, wait := limiter.allow(); !allowed {
					return row(false, fmt.Sprintf(
						"refused: a statistics rebuild is rate-limited to one per %s; retry in %s",
						statsRefreshMinInterval, wait.Round(time.Second))), nil
				}
			}
			if err := refresh(ctx); err != nil {
				return nil, err
			}
			return row(true, "planner statistics rebuilt"), nil
		},
	}
}

// mustRegister panics when Register returns an error. It is only called for
// built-in procedures that are known to have no duplicates among themselves;
// user code should call reg.Register directly and handle the error.
//
//nolint:gocritic // hugeParam: ProcEntry is passed by value intentionally; callers own the struct
func mustRegister(reg *Registry, entry ProcEntry) {
	if err := reg.Register(entry.Sig, entry.Impl); err != nil {
		panic("procs: RegisterBuiltins: " + err.Error())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// db.indexes()
// ─────────────────────────────────────────────────────────────────────────────

func dbIndexes(mgr *index.Manager) ProcEntry {
	return ProcEntry{
		Sig: Signature{
			Namespace: []string{"db"},
			Name:      "indexes",
			Inputs:    nil,
			Outputs: []NamedType{
				{Name: "name", Kind: expr.KindString},
				{Name: "type", Kind: expr.KindString},
			},
		},
		Impl: func(_ context.Context, _ []expr.Value) ([][]expr.Value, error) {
			return CollectIndexRows(mgr), nil
		},
	}
}

// CollectIndexRows enumerates every user-visible index registered on mgr as a
// [][]expr.Value where each inner slice is [name, type]: the index name and its
// kind ("hash" or "btree"). Names are returned in deterministic (ascending)
// order so the row set is stable across calls.
//
// It is the single source of index enumeration shared by db.indexes() and the
// SHOW INDEXES statement (#1922) — both project from the identical row set, so
// the two views can never diverge on which indexes exist or on their reported
// type. The internal numeric-companion btree (a "<label>_<prop>_btree_num"
// index a btree CREATE INDEX registers alongside the user-named string btree,
// #1652) is filtered out here, so neither view exposes it. A nil mgr yields nil.
//
// CollectIndexRows is safe for concurrent use (it only issues concurrent-safe
// reads on mgr).
func CollectIndexRows(mgr *index.Manager) [][]expr.Value {
	if mgr == nil {
		return nil
	}
	names := mgr.ListIndexes()
	sort.Strings(names)
	rows := make([][]expr.Value, 0, len(names))
	for _, name := range names {
		// Hide the internal numeric-companion btree (#1652): a btree CREATE
		// INDEX registers a "<label>_<prop>_btree_num" companion alongside the
		// user-named string btree so a numeric range seek can find it, but the
		// user must see exactly the one index they created. The suffix is
		// reserved (a user index name carries no such suffix from the DDL
		// parser).
		if strings.HasSuffix(name, numericCompanionSuffix) {
			continue
		}
		sub, err := mgr.GetIndex(name)
		if err != nil {
			continue
		}
		rows = append(rows, []expr.Value{
			expr.StringValue(name),
			expr.StringValue(sub.Kind()),
		})
	}
	return rows
}

// ─────────────────────────────────────────────────────────────────────────────
// db.constraints()
// ─────────────────────────────────────────────────────────────────────────────

func dbConstraints(listConstraints func() [][]expr.Value) ProcEntry {
	return ProcEntry{
		Sig: Signature{
			Namespace: []string{"db"},
			Name:      "constraints",
			Inputs:    nil,
			Outputs: []NamedType{
				{Name: "name", Kind: expr.KindString},
				{Name: "type", Kind: expr.KindString},
				{Name: "label", Kind: expr.KindString},
				{Name: "property", Kind: expr.KindString},
			},
		},
		Impl: func(_ context.Context, _ []expr.Value) ([][]expr.Value, error) {
			if listConstraints == nil {
				return nil, nil
			}
			return listConstraints(), nil
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// db.labels()
// ─────────────────────────────────────────────────────────────────────────────

// dbLabels builds the db.labels() procedure entry. It yields a single column,
// label (a string), with one row per name returned by listLabels, in the order
// listLabels produces them. A nil listLabels closure yields an empty result
// set, mirroring the nil-source behaviour of the other built-in db.*
// procedures.
//
// Semantics. db.labels() returns the labels currently attached to live nodes
// (in use): the listLabels closure is backed by lpg.Graph.NodeLabelsInUse,
// which reports tombstone-filtered node labels. A label is therefore listed
// regardless of whether an index exists for it, and is dropped once the last
// node bearing it is deleted. The db.* introspection procedures are not covered
// by the openCypher TCK, so this is a deliberate, TCK-neutral behaviour.
func dbLabels(listLabels func() []string) ProcEntry {
	return ProcEntry{
		Sig: Signature{
			Namespace: []string{"db"},
			Name:      "labels",
			Inputs:    nil,
			Outputs: []NamedType{
				{Name: "label", Kind: expr.KindString},
			},
		},
		Impl: func(_ context.Context, _ []expr.Value) ([][]expr.Value, error) {
			if listLabels == nil {
				return nil, nil
			}
			names := listLabels()
			rows := make([][]expr.Value, 0, len(names))
			for _, name := range names {
				rows = append(rows, []expr.Value{expr.StringValue(name)})
			}
			return rows, nil
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// db.relationshipTypes()
// ─────────────────────────────────────────────────────────────────────────────

func dbRelationshipTypes(listTypes func() []string) ProcEntry {
	return ProcEntry{
		Sig: Signature{
			Namespace: []string{"db"},
			Name:      "relationshipTypes",
			Inputs:    nil,
			Outputs: []NamedType{
				{Name: "relationshipType", Kind: expr.KindString},
			},
		},
		Impl: func(_ context.Context, _ []expr.Value) ([][]expr.Value, error) {
			if listTypes == nil {
				return nil, nil
			}
			names := listTypes()
			rows := make([][]expr.Value, 0, len(names))
			for _, name := range names {
				rows = append(rows, []expr.Value{expr.StringValue(name)})
			}
			return rows, nil
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// db.propertyKeys()
// ─────────────────────────────────────────────────────────────────────────────

// dbPropertyKeys builds the db.propertyKeys() procedure entry. It yields a
// single column, propertyKey (a string), with one row per name returned by
// listKeys, in the order listKeys produces them. A nil listKeys closure yields
// an empty result set, mirroring the nil-source behaviour of the other built-in
// db.* procedures.
//
// Divergence from Neo4j (deliberate, openCypher-conformant). Neo4j's
// db.propertyKeys() returns the property-key tokens held in the token store,
// which includes keys no longer borne by any node or relationship: property-key
// tokens are interned on first use and are never garbage-collected, so a key
// survives in the listing after the last element using it is deleted. GoGraph
// instead returns only the property keys currently in use — the listKeys
// closure is backed by lpg.Graph.PropertyKeysInUse, which reports live,
// tombstone-filtered property keys. This difference is observable but is not an
// openCypher-conformance violation: the db.* introspection procedures are not
// covered by the openCypher TCK.
func dbPropertyKeys(listKeys func() []string) ProcEntry {
	return ProcEntry{
		Sig: Signature{
			Namespace: []string{"db"},
			Name:      "propertyKeys",
			Inputs:    nil,
			Outputs: []NamedType{
				{Name: "propertyKey", Kind: expr.KindString},
			},
		},
		Impl: func(_ context.Context, _ []expr.Value) ([][]expr.Value, error) {
			if listKeys == nil {
				return nil, nil
			}
			names := listKeys()
			rows := make([][]expr.Value, 0, len(names))
			for _, name := range names {
				rows = append(rows, []expr.Value{expr.StringValue(name)})
			}
			return rows, nil
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// db.schema.visualization()
// ─────────────────────────────────────────────────────────────────────────────

func dbSchemaVisualization() ProcEntry {
	return ProcEntry{
		Sig: Signature{
			Namespace: []string{"db", "schema"},
			Name:      "visualization",
			Inputs:    nil,
			Outputs: []NamedType{
				{Name: "nodes", Kind: expr.KindList},
				{Name: "relationships", Kind: expr.KindList},
			},
		},
		Impl: func(_ context.Context, _ []expr.Value) ([][]expr.Value, error) {
			return nil, nil
		},
	}
}
