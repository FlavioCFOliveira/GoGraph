// Package cypher provides the public query engine API for the GoGraph Cypher
// executor.
//
// # Usage
//
//	g := lpg.New[string, float64](adjlist.Config{})
//	// ... populate graph ...
//
//	engine := cypher.NewEngine(g)
//	result, err := engine.Run(ctx, "MATCH (n) RETURN n", nil)
//	if err != nil { ... }
//	defer result.Close()
//	for result.Next() {
//	    rec := result.Record()
//	    _ = rec
//	}
//
// # Plan cache
//
// Engine caches parsed and translated logical plans together with the
// semantic-analysis verdict in a bounded LRU keyed by the query string. The
// cached entry is a *planCacheEntry; the physical build step runs per
// Engine.Run call so that per-call executor state is fresh. Semantically
// invalid queries are also cached (with the typed error) so that repeated
// runs of the same bad query short-circuit without re-parsing.
//
// The default capacity is [DefaultPlanCacheCapacity] (1024 entries). Configure
// a different bound via [EngineOptions.PlanCacheCapacity] and the [NewEngineWithOptions]
// constructor. Eviction is least-recently-used and emits the
// cypher.plan_cache.evictions counter on the global metrics surface; hits and
// misses are reported under cypher.plan_cache.hits and
// cypher.plan_cache.misses.
//
// # Concurrency
//
// Engine is safe for concurrent use. Each Run call creates an independent
// physical operator tree. The plan cache itself serialises its structural
// updates on a single sync.Mutex; the cached *planCacheEntry is immutable
// once published, so callers operate on the returned pointer without further
// synchronisation.
//
// Write queries serialise on a single-writer lock: the backing [txn.Store]'s
// writer mutex when the engine is WAL-backed ([NewEngineWithStore]), or the
// engine's own writer mutex when it is store-less ([NewEngine]). Autocommit
// [Engine.RunInTx] holds that lock for one statement; an explicit transaction
// ([Engine.BeginTx]) holds it from BEGIN until COMMIT/ROLLBACK, so concurrent
// writers block until it finishes (write-write isolation). Reads ([Engine.Run])
// never take the writer lock. The lock order is writer-lock (outermost) → the
// graph visibility barrier (visMu, inside [lpg.Graph.ApplyAtomically]); both
// wirings share it, so no deadlock is possible across them.
//
// # Transactions
//
// [Engine.RunInTx] is autocommit: each call is its own all-or-nothing,
// durable-then-visible transaction. [Engine.BeginTx] opens an explicit,
// multi-statement transaction ([ExplicitTx]) whose statements commit or roll
// back together — the engine substrate for the Bolt BEGIN/RUN/COMMIT/ROLLBACK
// protocol. Both apply writes eagerly to the in-memory graph and roll back via
// the in-memory undo log on error; a concurrent reader can therefore observe an
// open transaction's not-yet-committed writes (read-uncommitted for readers).
// See [ExplicitTx] (exectx.go) for the full transaction and isolation contract.
package cypher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/cypher/procs"
	"github.com/FlavioCFOliveira/GoGraph/cypher/sema"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// init wires the cross-package hook that lets sema reject calls to
// non-existent functions at compile time. The hook is consulted from
// sema's *ast.FunctionInvocation check; aggregates are recognised by
// sema independently.
func init() {
	sema.IsKnownFunction = func(qualifiedLower string) bool {
		_, ok := funcs.DefaultRegistry.Resolve(qualifiedLower)
		return ok
	}
}

// buildOpts carries the query-scope, optional state threaded through
// [buildOperator] alongside the per-call positional arguments. A nil
// *buildOpts is equivalent to the legacy build path: no SubqueryEvaluator,
// background context. Each Engine.Run / Engine.RunInTx invocation allocates
// its own *buildOpts so closures created during the build observe a stable
// per-run snapshot.
// edgeVarInfo records the schema columns emitted by an Expand operator for a
// named relationship variable. The triple (srcCol, edgeCol, dstCol) allows
// buildIRProjection to reconstruct a RelationshipValue from the raw
// IntegerValue columns in the executor row.
type edgeVarInfo struct {
	srcCol        int
	edgeCol       int
	dstCol        int
	edgeType      string   // first element of RelTypes, or empty
	acceptedTypes []string // full RelTypes list; used to disambiguate when
	// the stored edge carries multiple labels (e.g. (a)-[:HATES]->(c) and
	// (a)-[:WONDERS]->(c) merge in LPG as one edge with labels {HATES,
	// WONDERS}). For a pattern `(n)-[r:KNOWS|HATES]->(x)` we record
	// [KNOWS, HATES] here, and the projection's RelationshipValue
	// reconstruction prefers a matching label from acceptedTypes over the
	// non-deterministic map-iteration first label. Closes Match2 [6] flake.
}

// pathVarInfo records the schema column that holds the flat alternating path
// list emitted by a VarLengthExpand operator for a named path variable. The
// listCol column contains an expr.ListValue of the form
//
//	[srcNodeID, edgePos0, dstNode0, edgePos1, dstNode1, ...]
//
// buildIRProjection uses this to reconstruct an expr.PathValue.
//
// For a chained-VLE path pattern (`MATCH p = (a)-[*]->(b)-[*]->(c)`),
// each VLE in the chain registers a segment in the `segments` field
// (in plan-build order, which is bottom-up, so the leftmost VLE in
// the pattern is appended FIRST). The projection iterates segments in
// reverse plan-build order to stitch the path left-to-right. When
// only one VLE contributes to the path (the common case), segments
// has length 1 — equivalent to the legacy (listCol, edgeType)
// shape — and the projection reads it identically.
type pathVarInfo struct {
	listCol  int              // first segment's listCol (legacy shape)
	edgeType string           // first segment's edgeType (legacy shape)
	segments []pathVarSegment // chained-VLE segments in plan-build order
	// leadingSteps captures fixed-length Expand hops that precede the
	// VLE within the same named path (Match6 [14]'s `MATCH p =
	// (:Start)<-[:CONNECTED_TO]-()-[:CONNECTED_TO*3..3]-(:End)` has
	// one leading Expand and one VLE — the path reconstruction must
	// prepend the leading hop's (src, edge, dst) triplet before
	// iterating the VLE list). Recorded in plan-build order with the
	// leading-most hop at index 0.
	leadingSteps []pathChainStep
}

// pathVarSegment captures one VarLengthExpand's contribution to a
// chained-VLE named path. listCol points at the flat alternating
// ListValue this VLE emits ([srcID, edgePos0, dst0, ...]); edgeType
// is the first declared RelTypes filter or empty.
type pathVarSegment struct {
	listCol  int
	edgeType string
}

// pathChainStep describes one (relationship, destination-node) hop of a
// fixed-length named path. The (srcCol, edgeCol, dstCol) triplet matches the
// layout emitted by [exec.Expand]; edgeType is the relationship-type filter
// declared in the AST and acts as a fallback when the live-graph lookup
// cannot resolve one.
type pathChainStep struct {
	srcCol   int
	edgeCol  int
	dstCol   int
	edgeType string
}

// pathChainInfo describes a named path bound by a zero- or fixed-length
// (possibly chained) pattern. leadingCol is the schema column that carries
// the IntegerValue of the path's leading node. Each step extends the path by
// one relationship and one destination node, in document order.
// buildIRProjection consumes this to reconstruct an [expr.PathValue].
type pathChainInfo struct {
	leadingCol int
	steps      []pathChainStep
}

// vleRelInfo describes the schema column and edge-type filter for a
// VarLengthExpand relationship variable. The column holds a flat
// alternating ListValue [srcID, edgePos0, dst0, edgePos1, dst1, …];
// buildIRProjection extracts each (edgePos, dst) pair and reconstructs
// a RelationshipValue per hop so `RETURN r` for variable-length r
// projects [[:T], [:T], …] instead of the raw integer list.
type vleRelInfo struct {
	listCol  int
	edgeType string
}

type buildOpts struct {
	// subEval handles [ast.ExistsSubquery] and [ast.CountSubquery] expressions
	// encountered inside Filter/Project closures. May be nil; in that case
	// subquery expressions surface as [expr.EvalError] at runtime.
	subEval expr.SubqueryEvaluator
	// patEval handles [ast.PathPattern] existential predicates inside WHERE
	// clauses. May be nil; in that case pattern predicates surface as
	// [expr.EvalError] at runtime.
	patEval expr.PatternEvaluator
	// queryCtx is the context.Context attached to the enclosing Engine.Run
	// call. It is threaded into [expr.EvalWith] so subquery drives observe
	// cancellation and deadlines from the outer query.
	queryCtx context.Context //nolint:containedctx // per-query state owned by the buildOpts holder, not a long-lived field
	// writeFallback, when non-nil, is invoked by [buildOperator]'s default
	// branch on encountering a write IR node ([ir.CreateNode],
	// [ir.SetProperty], …). [buildPlanWithMutatorFull] sets it so that a
	// read wrapper such as [ir.Projection] above a write subtree (the
	// canonical lowering for `CREATE … RETURN`) recurses through the
	// write-aware planner rather than failing with
	// "unsupported IR node *ir.CreateNode". Leaving the field nil
	// preserves the original error for read-only call sites.
	writeFallback func(ir.LogicalPlan) (exec.Operator, error)
	// edgeVarMeta maps a relationship variable name (e.g. "r" from
	// `MATCH (a)-[r:R]->(b)`) to the triplet of schema columns that the Expand
	// operator places in each output row. Used by buildIRProjection to
	// reconstruct a RelationshipValue when the variable is projected directly.
	edgeVarMeta map[string]edgeVarInfo
	// pathVarMeta maps a named path variable (e.g. "p" from `MATCH p=(a)-[*]->(b)`)
	// to the schema column that the VarLengthExpand operator populates with a flat
	// alternating ListValue. Used by buildIRProjection to reconstruct a PathValue.
	pathVarMeta map[string]pathVarInfo
	// pathVarChain maps a named path variable bound by a zero- or
	// fixed-length pattern (no variable-length expansion) to the explicit
	// alternating triplet description emitted by the underlying Expand
	// chain. Used by buildIRProjection to reconstruct an expr.PathValue
	// without the flat ListValue encoding used by VarLengthExpand.
	pathVarChain map[string]pathChainInfo
	// expandTripletSeq is the ordered list of (srcCol, edgeCol, dstCol)
	// triplets registered by each [exec.Expand] operator as its physical
	// builder runs. A [*ir.NamedPath] wrapper above an Expand subtree
	// captures the slice length before recursing into its child, then
	// consumes the triplets appended during the child build to associate
	// IR chain elements with row slots.
	expandTripletSeq []pathChainStep
	// vleRelMeta maps a VarLengthExpand relationship variable name (e.g. "r"
	// from `MATCH (a)-[r*]->(b)`) to the flat-list column it occupies plus
	// the optional edge-type filter. Used by buildIRProjection to render the
	// variable as a list of RelationshipValues rather than the raw
	// alternating ListValue emitted by the VLE operator.
	vleRelMeta map[string]vleRelInfo
	// scalarCols is the set of schema variable names whose row values must NOT be
	// upgraded from IntegerValue(NodeID) to NodeValue. Aggregate output columns
	// (e.g. the output of count(*), sum, avg) are always scalars; their integer
	// values can coincide with real NodeIDs and must pass through as-is to prevent
	// mis-upgrading a count result into a graph node. buildEagerAggregation
	// populates this set for every aggregate output name it registers in the schema.
	scalarCols map[string]struct{}
	// projAliasScalarCols mirrors scalarCols for the BUILDROWCTX / Variable
	// fast-path upgrade-bypass only. Distinct from scalarCols so the
	// colliding-alias guard in buildIRProjection still routes a
	// re-aliasing projection (`WITH a.num%3 AS x WITH a.num+a.num2 AS x`)
	// through general eval — the prior alias is here, not in scalarCols,
	// and the guard reads only scalarCols. Closes WithSkipLimit3 [3]
	// without re-breaking WithOrderBy4 [7]/[9]/[10].
	projAliasScalarCols map[string]struct{}
	// aggKeyScalarCols tracks EagerAggregation grouping-key alias names
	// whose value at the post-aggregation row slot is a scalar (the
	// grouping expression evaluated against the pre-aggregation row).
	// Read ONLY by the buildIRProjection Variable fast path's upgrade
	// guard, NOT by buildRowCtx — the pre-projection's
	// buildRowCtxFromMutator must keep upgrading the underlying bound
	// variable so the grouping expression `a.num2 % 3` can evaluate
	// against the NodeValue `a`. Closes WithOrderBy4 [12] without
	// regressing Return6 [1] / ExistentialSubquery2 [2] (which both
	// rely on the pre-projection's `a`/`n` staying a NodeValue).
	aggKeyScalarCols map[string]struct{}
	// edgeIDResolver, when non-nil, returns the storage endpoints of an
	// edge identified by its forward-CSR position. Used by the path-
	// reconstruction fast paths to determine the true storage direction
	// of a relationship when the row's (src, dst) columns reflect the
	// traversal direction (which differs from storage for undirected /
	// reverse-edge traversals). Lazily populated on first use to avoid
	// building CSR snapshots for queries that never reconstruct paths.
	edgeIDResolver func(edgeID uint64) (storageSrc, storageDst uint64, ok bool)
	// preprojectedCols is the set of schema variable names whose row column
	// already holds the projection-equivalent value (e.g. an EagerAggregation
	// grouping-key column carries the pre-evaluated grouping expression, not
	// the original variable). The colliding-alias guard in buildIRProjection
	// skips when a name is in this set because the fast path is sound — the
	// slot value is already the result the projection expression would
	// compute. Without this Return6 [1] returns NULL for the first column of
	// `RETURN n.num AS n, count(n) AS count` because the guard routes through
	// general eval which interprets `n` as the original NodeValue rather
	// than the pre-projected n.num.
	preprojectedCols map[string]struct{}
	// maxCollectItems carries the Engine's per-group element budget for
	// buffering aggregators (collect / percentileCont / percentileDisc) into
	// buildEagerAggregation. The encoding mirrors EngineOptions.MaxCollectItems:
	// 0 means "unset" (apply DefaultMaxCollectItems), a negative value is the
	// explicit opt-out (no cap), and a positive value is an active budget. Only
	// the Engine-internal build paths set it; the public BuildPlanWithMutator
	// path leaves it zero and so inherits the finite default.
	maxCollectItems int
}

// evalRow is the canonical bridge from a per-row closure to [expr.Eval] /
// [expr.EvalWith]. When bopts is non-nil and carries a SubqueryEvaluator or
// PatternEvaluator, [expr.EvalWith] is used so EXISTS/COUNT subquery dispatch
// and pattern predicate dispatch are enabled; otherwise the call degrades to
// [expr.Eval], preserving exact backward compatibility.
func evalRow(bopts *buildOpts, e ast.Expression, row expr.RowContext, params map[string]expr.Value, reg expr.FunctionRegistry) (expr.Value, error) {
	if bopts == nil || (bopts.subEval == nil && bopts.patEval == nil) {
		return expr.Eval(e, row, params, reg)
	}
	ctx := bopts.queryCtx
	if ctx == nil {
		ctx = context.Background()
	}
	return expr.EvalWith(ctx, e, row, params, reg, bopts.subEval, bopts.patEval)
}

// rollUpItemsFromBopts returns the per-list element budget a RollUpApply
// operator should enforce, in the EngineOptions.MaxCollectItems encoding
// ([exec.NewRollUpApplyN] resolves 0 → default, <0 → unlimited, >0 → verbatim).
// A nil bopts (and the public BuildPlanWithMutator path, where maxCollectItems
// is 0) yields the finite default, so a pattern comprehension is never built
// unbounded — matching how the buffering aggregators are bounded (#1294, #1298).
func rollUpItemsFromBopts(bopts *buildOpts) int {
	if bopts == nil {
		return 0 // resolves to funcs.DefaultMaxCollectItems
	}
	return bopts.maxCollectItems
}

// ─────────────────────────────────────────────────────────────────────────────
// Engine
// ─────────────────────────────────────────────────────────────────────────────

// EngineOptions configures an [Engine]. The zero value is valid: it selects
// the default function registry ([funcs.DefaultRegistry]), no WAL-backed
// store, and the default plan cache capacity ([DefaultPlanCacheCapacity]).
// Use [NewEngineWithOptions] to construct an Engine from this struct.
type EngineOptions struct {
	// Registry, when non-nil, overrides the default built-in function
	// registry used to resolve scalar function calls.
	Registry expr.FunctionRegistry

	// Store, when non-nil, binds the Engine to a WAL-enabled
	// [txn.Store]. The Engine's graph is taken from store.Graph()
	// when both Store and Graph fields are set; the explicit Graph
	// is then ignored. Run queries through [Engine.RunInTx] for
	// atomicity and WAL durability.
	Store *txn.Store[string, float64]

	// PlanCacheCapacity bounds the number of cached plans. Zero
	// selects [DefaultPlanCacheCapacity]; positive values override
	// it. A negative value is treated as misconfiguration and is
	// clamped to the default by the constructor.
	PlanCacheCapacity int

	// MaxResultRows limits the number of rows a single [Engine.Run] or
	// [Engine.RunInTx] call may materialise. If a query produces more rows than
	// the limit, the [Result] iterator returns [ErrResultRowsExceeded] from
	// [Result.Next] when the limit is hit, and [Result.Err] reports the same
	// error.
	//
	// The value is interpreted as follows:
	//
	//   - Zero (the default) selects [DefaultMaxResultRows], a finite cap that
	//     prevents an unintentional whole-graph scan or Cartesian-product query
	//     from materialising an unbounded number of rows — and holding the
	//     graph's visibility barrier — until memory is exhausted.
	//   - A positive value overrides the default. Set it to a value appropriate
	//     for the operational environment (e.g. 1_000_000 for a shared
	//     multi-tenant server).
	//   - [MaxResultRowsUnlimited] (-1) disables the cap entirely; use it only
	//     when memory is bounded by another means.
	MaxResultRows int64

	// MaxResultBytes is a coarse aggregate-BYTE budget on a single [Engine.Run]
	// or [Engine.RunInTx] result, complementing [MaxResultRows]. The row cap
	// bounds the number of rows; a small number of rows carrying very large
	// values (a node with megabyte-scale string properties) can still consume
	// large memory inside the visibility barrier under that cap. When the
	// cumulative *estimated* encoded size of the materialised rows exceeds this
	// budget, [Result.Err] reports [ErrResultBytesExceeded].
	//
	// The estimate is intentionally coarse and cheap (O(columns) per row, no
	// allocation, no serialisation): a fixed per-value overhead plus the lengths
	// of string/[]byte payloads and the element counts of lists/maps. It is a
	// guard against pathological memory use, not an exact accounting of heap
	// bytes.
	//
	// The value is interpreted as follows:
	//
	//   - Zero (the default) selects [DefaultMaxResultBytes], a finite budget.
	//   - A positive value overrides the default (a byte count).
	//   - [MaxResultBytesUnlimited] (-1) disables the budget entirely; use it
	//     only when memory is bounded by another means.
	MaxResultBytes int64

	// MaxCollectItems bounds the number of values a single buffering aggregator
	// — collect(), collect(DISTINCT …), percentileCont(), percentileDisc() —
	// retains in one group. A grouping-key-free aggregate such as
	// `RETURN collect(n)` forms exactly one group, so the group-count cap never
	// fires; without this per-aggregator budget, `MATCH (n) RETURN collect(n)`
	// would build an unbounded list inside the graph's visibility barrier.
	//
	// The value is interpreted as follows:
	//
	//   - Zero (the default) selects [funcs.DefaultMaxCollectItems], a finite cap
	//     that prevents an unbounded collect/percentile buffer from exhausting
	//     memory and holding the visibility barrier.
	//   - A positive value overrides the default.
	//   - [MaxCollectItemsUnlimited] (-1) disables the cap entirely; use it only
	//     when memory is bounded by another means.
	//
	// When the budget is exceeded the aggregator returns
	// [funcs.ErrCollectItemsExceeded], which the executor surfaces through
	// [Result.Err] (the aggregation buffers during materialisation inside the
	// barrier, so the cap trips before the whole list is built).
	MaxCollectItems int

	// RecoveredConstraints, when non-empty, are the durable schema constraints
	// recovered from disk (the [store/recovery.Result.Constraints] of the open
	// that produced Store/Graph). The constructor re-registers each one in the
	// engine's constraint registry and re-seeds every UNIQUE value-set by
	// scanning the recovered graph, so a constraint declared before a crash is
	// enforced again after recovery — without this, the registry is rebuilt
	// empty on every open and duplicates are silently accepted (audit gap H1).
	// A caller recovering a WAL-backed store from disk MUST pass these (or use
	// [NewEngineWithStoreAndConstraints]); a store-less in-memory engine leaves
	// the field nil.
	RecoveredConstraints []ConstraintDef
}

// ConstraintDef is a durable constraint definition handed to the engine on
// open so it can re-register a constraint recovered from disk. It mirrors
// [store/recovery.ConstraintRecord] without coupling callers to the recovery
// package's wire types; [ConstraintDefsFromRecovery] converts a recovery
// result into this form.
type ConstraintDef struct {
	// Unique is true for a UNIQUE constraint, false for a NOT NULL constraint.
	Unique bool
	// Label is the constrained node label.
	Label string
	// Property is the constrained property key.
	Property string
	// Name is the user-defined constraint name.
	Name string
}

// Engine is the public query engine. It binds a graph, a function registry,
// and a plan cache, and exposes a single Run method for query execution.
//
// Engine is safe for concurrent use. A single Engine may serve any number of
// concurrent [Engine.Run] readers together with concurrent [Engine.RunInTx]
// writers: each call builds its own operator tree, the plan cache is
// internally synchronised, and both the physical-plan build and execution run
// under the graph's visibility barrier ([lpg.Graph.View] for reads,
// [lpg.Graph.ApplyAtomically] for writes). A writer that grows the node space
// can therefore never tear a concurrent reader's plan build, and readers never
// observe a partially-applied write transaction (#1077, audit gap F3).
//
// Write queries remain subject to the underlying store's single-writer
// constraint: when the Engine is backed by a [txn.Store], concurrent
// [Engine.RunInTx] calls serialise on the store's writer mutex.
type Engine struct {
	g             *lpg.Graph[string, float64]
	store         *txn.Store[string, float64] // non-nil when WAL-backed
	reg           expr.FunctionRegistry
	constraintReg *exec.ConstraintRegistry
	procReg       *procs.Registry
	cache         *planCache
	maxResultRows int64 // zero means no limit; from EngineOptions.MaxResultRows
	// maxResultBytes is the aggregate-byte budget for a single result, threaded
	// to the Result drain alongside maxResultRows. Zero means no budget (the
	// convention the drain checks with maxBytes > 0); the public
	// EngineOptions.MaxResultBytes field is mapped onto it by resolveMaxResultBytes
	// (0 → DefaultMaxResultBytes, MaxResultBytesUnlimited → 0, positive verbatim).
	maxResultBytes int64
	// maxCollectItems is the per-group element budget for buffering aggregators,
	// threaded into every plan build via buildOpts. The encoding mirrors the
	// public EngineOptions.MaxCollectItems field (0 → default, <0 → no cap,
	// >0 → active) rather than the resolved internal form, because
	// buildEagerAggregation performs the final resolution at the build boundary.
	maxCollectItems int

	// writeMu is the engine-level single-writer serialisation used ONLY when
	// the engine is store-less (store == nil). A WAL-backed engine instead
	// serialises every write on the store's own single-writer mutex (taken in
	// [txn.Store.Begin] and released by Commit/Rollback), so this mutex is left
	// untouched in that wiring to avoid a redundant second lock.
	//
	// It provides write-write isolation for the store-less engine in two
	// places: (a) every autocommit [Engine.RunInTx] statement holds it for the
	// statement's duration; (b) an explicit transaction ([Engine.BeginTx])
	// holds it from BEGIN until COMMIT or ROLLBACK, so a concurrent writer
	// blocks until the transaction finishes — the same isolation the store
	// mutex gives the WAL-backed wiring. The lock order is writeMu (outermost)
	// → visMu (inside [lpg.Graph.ApplyAtomically]), matching the WAL-backed
	// store-mutex → visMu order, so the two wirings share one deadlock-free
	// ordering. readers ([Engine.Run] / [lpg.Graph.View]) never take it.
	writeMu sync.Mutex
}

// lockWriter acquires the engine's write serialisation appropriate to its
// wiring and returns the matching unlock closure. For a store-less engine it
// locks [Engine.writeMu]; for a WAL-backed engine the store mutex is taken by
// the caller's [txn.Store.Begin], so lockWriter is a no-op there and the
// returned closure does nothing. The returned unlock is idempotent-safe to
// call exactly once on the write path's single exit.
func (e *Engine) lockWriter() func() {
	if e.store != nil {
		return func() {}
	}
	e.writeMu.Lock()
	return e.writeMu.Unlock
}

// NewEngine creates an Engine backed by g. The default built-in function
// registry ([funcs.DefaultRegistry]) and the default plan cache capacity
// ([DefaultPlanCacheCapacity]) are used. Use [NewEngineWithOptions] when a
// non-default function registry or plan cache capacity is required.
//
// If g has no [index.Manager] attached yet, NewEngine installs a new empty one
// so that DDL statements (CREATE INDEX / DROP INDEX) work out of the box.
func NewEngine(g *lpg.Graph[string, float64]) *Engine {
	return NewEngineWithOptions(g, EngineOptions{})
}

// NewEngineWithRegistry creates an Engine backed by g using a custom function
// registry and the default plan cache capacity.
//
// If g has no [index.Manager] attached yet, a new empty one is installed.
func NewEngineWithRegistry(g *lpg.Graph[string, float64], reg expr.FunctionRegistry) *Engine {
	return NewEngineWithOptions(g, EngineOptions{Registry: reg})
}

// NewEngineWithStore creates an Engine backed by a WAL-enabled [txn.Store]
// using the default plan cache capacity.
//
// All write queries routed through [Engine.RunInTx] use a single [txn.Tx] for
// atomicity and WAL durability: mutations are applied eagerly to the in-memory
// graph (so reads within the same transaction see the writes) and the WAL is
// fsynced on [Result.Close] when no pipeline error occurred.
//
// The underlying graph is taken from store.Graph(). If the graph has no
// [index.Manager] attached yet, a new empty one is installed.
func NewEngineWithStore(store *txn.Store[string, float64]) *Engine {
	return NewEngineWithOptions(store.Graph(), EngineOptions{Store: store})
}

// NewEngineWithStoreAndConstraints creates a WAL-backed Engine that also
// re-registers the schema constraints recovered from disk. It is the
// recommended constructor for opening a persisted store: pass the
// store/recovery.Result.Constraints surfaced by the open that produced store,
// so a constraint declared before a crash is enforced again (audit gap H1).
// Using the plain [NewEngineWithStore] on a recovered store leaves the
// constraint registry empty and duplicates would be silently accepted.
//
// recovered is converted via [ConstraintDefsFromRecovery]; pass nil (or use
// [NewEngineWithStore]) when there are no recovered constraints.
func NewEngineWithStoreAndConstraints(store *txn.Store[string, float64], recovered []recovery.ConstraintRecord) *Engine {
	return NewEngineWithOptions(store.Graph(), EngineOptions{
		Store:                store,
		RecoveredConstraints: ConstraintDefsFromRecovery(recovered),
	})
}

// resolveMaxResultRows maps the public [EngineOptions.MaxResultRows] value to
// the Engine's internal cap, where a positive value is an active limit and zero
// disables it (the convention the [Result] drain checks with maxRows > 0):
//
//   - 0 (the zero value)        → [DefaultMaxResultRows] (a finite default cap)
//   - [MaxResultRowsUnlimited]  → 0 (unlimited, the explicit opt-out)
//   - any positive value        → that value, verbatim
//
// Centralising the policy here means every constructor that routes through
// [NewEngineWithOptions] — [NewEngine], [NewEngineWithRegistry], and the
// WAL-backed [NewEngineWithStore] — inherits the finite default, closing the
// previously-unbounded default that let a whole-graph MATCH materialise every
// row inside the visibility barrier.
func resolveMaxResultRows(opt int64) int64 {
	switch opt {
	case 0:
		return DefaultMaxResultRows
	case MaxResultRowsUnlimited:
		return 0
	default:
		return opt
	}
}

// resolveMaxResultBytes maps the public [EngineOptions.MaxResultBytes] value to
// the Engine's internal aggregate-byte budget, where a positive value is an
// active budget and zero disables it (the convention the [Result] drain checks
// with maxBytes > 0):
//
//   - 0 (the zero value)         → [DefaultMaxResultBytes] (a finite default)
//   - [MaxResultBytesUnlimited]  → 0 (unlimited, the explicit opt-out)
//   - any positive value         → that value, verbatim
//
// It mirrors [resolveMaxResultRows] so every constructor that routes through
// [NewEngineWithOptions] inherits the finite default byte budget alongside the
// finite default row cap.
func resolveMaxResultBytes(opt int64) int64 {
	switch opt {
	case 0:
		return DefaultMaxResultBytes
	case MaxResultBytesUnlimited:
		return 0
	default:
		return opt
	}
}

// NewEngineWithOptions creates an Engine backed by g with explicit options.
// Zero-valued fields are filled with their documented defaults. When
// opts.Store is non-nil, the Engine is bound to that WAL-enabled
// [txn.Store] in addition to g.
//
// If g has no [index.Manager] attached yet, a new empty one is installed.
//
//nolint:gocritic // public API: EngineOptions is passed by value to preserve every existing call site; the constructor only reads from it.
func NewEngineWithOptions(g *lpg.Graph[string, float64], opts EngineOptions) *Engine {
	ensureIndexManager(g)
	reg := opts.Registry
	if reg == nil {
		reg = funcs.DefaultRegistry
	}
	// Wrap the registry so the graph-aware startnode / endnode overrides
	// hydrate the returned NodeValue with labels and properties looked up
	// against the live graph. The default funcs implementation only sets
	// NodeValue.ID, which makes subsequent property access (`startNode(r).id`)
	// return null because the per-row schema upgrade does not fire on
	// function-produced values.
	reg = newGraphAwareRegistry(reg, g)
	e := &Engine{
		g:               g,
		store:           opts.Store,
		reg:             reg,
		constraintReg:   exec.NewConstraintRegistry(),
		procReg:         procs.NewRegistry(),
		cache:           newPlanCache(opts.PlanCacheCapacity),
		maxResultRows:   resolveMaxResultRows(opts.MaxResultRows),
		maxResultBytes:  resolveMaxResultBytes(opts.MaxResultBytes),
		maxCollectItems: opts.MaxCollectItems,
	}
	procs.RegisterBuiltins(e.procReg, g.IndexManager(), func() [][]expr.Value {
		return e.constraintReg.ListConstraintRows()
	})
	// Re-register constraints recovered from disk and re-seed each UNIQUE
	// value-set by scanning the recovered graph, so a constraint declared
	// before a crash is enforced again after recovery (audit gap H1). Without
	// this the registry is rebuilt empty on every open.
	e.registerRecoveredConstraints(opts.RecoveredConstraints)
	return e
}

// registerRecoveredConstraints re-registers each recovered constraint in the
// engine's registry, re-creating the UNIQUE backing index, recording the
// constraint name, and re-seeding the UNIQUE value-set from the recovered
// graph's existing data. It is invoked once at construction from
// [NewEngineWithOptions]. defs is the set surfaced by [store/recovery.Open];
// an empty slice is a no-op (a store-less or fresh engine).
//
// Re-seeding ignores any pre-existing duplicate that the recovered data may
// contain rather than failing the open: recovery must always succeed so the
// store is serviceable, and a duplicate that predates the constraint is a
// historical artefact the live enforcement path will still reject on the next
// write. (CREATE CONSTRAINT itself, in contrast, rejects pre-existing
// duplicates — but that is the creation path, not recovery.)
func (e *Engine) registerRecoveredConstraints(defs []ConstraintDef) {
	for i := range defs {
		d := defs[i]
		if d.Unique {
			idxName := exec.UniqueIndexName(d.Label, d.Property)
			// Best-effort backing index: ignore ErrIndexExists (a previous
			// constraint or a recovered index already created it). Any other
			// error leaves the value-set as the sole enforcement source, which
			// still rejects duplicates.
			_ = e.g.IndexManager().CreateIndex(idxName, exec.NewUniqueBackingIndex())
			e.constraintReg.RegisterUnique(d.Label, d.Property, idxName)
			e.constraintReg.SetConstraintName(true, d.Label, d.Property, d.Name)
			values, _ := e.scanLabelProperty(d.Label, d.Property)
			e.constraintReg.SeedUniqueValuesIgnoringDuplicates(d.Label, d.Property, values)
		} else {
			e.constraintReg.RegisterNotNull(d.Label, d.Property)
			e.constraintReg.SetConstraintName(false, d.Label, d.Property, d.Name)
		}
	}
}

// ConstraintDefsFromRecovery converts the durable constraint set surfaced by
// [store/recovery.Open] into the [ConstraintDef] slice the engine constructor
// accepts via [EngineOptions.RecoveredConstraints]. Pass it the
// store/recovery.Result.Constraints field. The recovery package's wire kind
// (txn.ConstraintKind: 0 = UNIQUE, 1 = NOT NULL) is mapped to the boolean
// [ConstraintDef.Unique].
func ConstraintDefsFromRecovery(recovered []recovery.ConstraintRecord) []ConstraintDef {
	if len(recovered) == 0 {
		return nil
	}
	out := make([]ConstraintDef, 0, len(recovered))
	for i := range recovered {
		r := recovered[i]
		out = append(out, ConstraintDef{
			Unique:   r.Kind == txn.ConstraintUnique,
			Label:    r.Label,
			Property: r.Property,
			Name:     r.Name,
		})
	}
	return out
}

// ResultRowCap reports the effective per-query result-row cap this Engine
// enforces, after [EngineOptions.MaxResultRows] has been resolved by the
// constructor:
//
//   - A positive value is the active cap. A single [Engine.Run] or
//     [Engine.RunInTx] call materialising more than this many rows trips
//     [ErrResultRowsExceeded] during the in-barrier drain, before the surplus
//     rows are ever handed to the caller.
//   - Zero means the cap is disabled (the engine was built with
//     [MaxResultRowsUnlimited]). Such an engine offers no upper bound on the
//     rows a single query materialises, so an embedder exposing it to untrusted
//     callers — for example behind the Bolt server — should bound memory by
//     another means.
//
// The accessor lets an embedder that receives a pre-built Engine observe its
// memory-safety posture without reaching into unexported state; the Bolt server
// uses it to warn when handed an uncapped engine.
func (e *Engine) ResultRowCap() int64 {
	return e.maxResultRows
}

// Constraints returns a structured snapshot of every schema constraint
// currently registered on the engine, in deterministic order (UNIQUE before
// NOT NULL, then by label, property, name). It is the source a checkpointer
// passes to [store/snapshot.WriteSnapshotFullWithConstraints] (via
// [ConstraintSpecsForSnapshot]) so the constraint set survives a checkpoint
// that truncates the WAL prefix which first declared a constraint (audit gap
// H1, the checkpoint-survival half).
//
// Constraints is safe for concurrent use.
func (e *Engine) Constraints() []ConstraintDef {
	infos := e.constraintReg.Constraints()
	if len(infos) == 0 {
		return nil
	}
	out := make([]ConstraintDef, 0, len(infos))
	for i := range infos {
		out = append(out, ConstraintDef{
			Unique:   infos[i].KindUnique,
			Label:    infos[i].Label,
			Property: infos[i].Property,
			Name:     infos[i].Name,
		})
	}
	return out
}

// ConstraintSpecsForSnapshot converts the engine's current constraint set into
// the [store/snapshot.ConstraintSpec] slice that
// [store/snapshot.WriteSnapshotFullWithConstraints] (and its mapper-codec
// variant) persists into a snapshot's constraints.bin component. A checkpointer
// calls e.ConstraintSpecsForSnapshot() and hands the result to the writer so a
// checkpoint + WAL truncate does not lose constraints.
func (e *Engine) ConstraintSpecsForSnapshot() []snapshot.ConstraintSpec {
	defs := e.Constraints()
	if len(defs) == 0 {
		return nil
	}
	out := make([]snapshot.ConstraintSpec, 0, len(defs))
	for i := range defs {
		kind := uint8(txn.ConstraintUnique)
		if !defs[i].Unique {
			kind = uint8(txn.ConstraintNotNull)
		}
		out = append(out, snapshot.ConstraintSpec{
			Kind:     kind,
			Label:    defs[i].Label,
			Property: defs[i].Property,
			Name:     defs[i].Name,
		})
	}
	return out
}

// graphAwareRegistry overlays a small set of graph-bound functions on top of
// a delegate FunctionRegistry. The overlay currently covers startnode and
// endnode: both look up the bound RelationshipValue's StartID / EndID in the
// graph and return a NodeValue carrying labels and properties so that
// downstream property access works.
type graphAwareRegistry struct {
	delegate expr.FunctionRegistry
	g        *lpg.Graph[string, float64]
}

// newGraphAwareRegistry wraps delegate with graph-aware startnode and
// endnode implementations. Other function lookups pass through unchanged.
func newGraphAwareRegistry(delegate expr.FunctionRegistry, g *lpg.Graph[string, float64]) expr.FunctionRegistry {
	return &graphAwareRegistry{delegate: delegate, g: g}
}

// Resolve implements [expr.FunctionRegistry].
func (r *graphAwareRegistry) Resolve(name string) (expr.BuiltinFn, bool) {
	switch name {
	case "startnode":
		fn := r.g
		return func(args []expr.Value) (expr.Value, error) {
			if len(args) != 1 {
				return nil, fmt.Errorf("funcs: startNode() takes exactly 1 argument(s), got %d", len(args))
			}
			if expr.IsNull(args[0]) {
				return expr.Null, nil
			}
			rv, ok := args[0].(expr.RelationshipValue)
			if !ok {
				return nil, fmt.Errorf("funcs: startNode() argument 0: got %s, want Relationship", args[0].Kind())
			}
			return buildNodeValueFromID(graph.NodeID(rv.StartID), fn), nil
		}, true
	case "endnode":
		fn := r.g
		return func(args []expr.Value) (expr.Value, error) {
			if len(args) != 1 {
				return nil, fmt.Errorf("funcs: endNode() takes exactly 1 argument(s), got %d", len(args))
			}
			if expr.IsNull(args[0]) {
				return expr.Null, nil
			}
			rv, ok := args[0].(expr.RelationshipValue)
			if !ok {
				return nil, fmt.Errorf("funcs: endNode() argument 0: got %s, want Relationship", args[0].Kind())
			}
			return buildNodeValueFromID(graph.NodeID(rv.EndID), fn), nil
		}, true
	}
	return r.delegate.Resolve(name)
}

// ensureIndexManager installs a new empty index.Manager on g when none is
// present, so that DDL operators have a non-nil manager to work with.
func ensureIndexManager(g *lpg.Graph[string, float64]) {
	if g.IndexManager() == nil {
		g.SetIndexManager(index.NewManager())
	}
}

// ClearPlanCache drops every cached plan and increments the
// cypher.plan_cache.invalidations counter exactly once. It is the
// operator-facing invalidation hook installed on every DDL operator
// (CREATE/DROP INDEX, CREATE/DROP CONSTRAINT) — successful schema
// mutations call it so that subsequent queries re-plan against the
// new index / constraint topology rather than reusing stale plans
// built before the schema changed.
//
// ClearPlanCache is also safe to invoke directly as a user-facing
// manual reset (e.g. from operational tooling after an out-of-band
// index swap on the underlying graph).
//
// ClearPlanCache is idempotent and safe for concurrent use; each call
// emits exactly one invalidations counter increment regardless of the
// cache's prior size.
func (e *Engine) ClearPlanCache() {
	e.cache.clear()
}

// Procs returns the engine's procedure registry so callers can register
// custom procedures alongside the built-in db.* set. The returned
// [*procs.Registry] is the live, owning registry — mutations are
// observed immediately by every subsequent CALL <ns>.<name>() in any
// query parsed by this engine.
//
// Returned registry is non-nil. Safe for concurrent use; see
// [procs.Registry] for the concurrency contract.
func (e *Engine) Procs() *procs.Registry {
	return e.procReg
}

// Run executes query against the engine's graph and returns a streaming
// [Result]. The caller must call [Result.Close] when done.
//
// A wrapped [*parser.ParseError] is returned when the query has a syntax
// error; the error includes line and column information.
//
// Semantic violations detected by the scope analyser (undefined variables,
// variable-type conflicts, scope leaks) are returned as a [*sema.SemanticError]
// before plan execution begins — see [sema.MapToBolt] for the
// ErrorKind→Bolt mapping used. Callers may use [errors.As] to recover the
// typed error and inspect Category / SubType.
//
// checkParamTypes validates the supplied params against the types inferred from
// plan. Property-vs-parameter equalities are typed from the index that backs
// the property when one exists (an int64 index proves an Integer property, a
// string index a String property); absent an index the inference defaults to
// String. It is a no-op when params is empty.
func (e *Engine) checkParamTypes(plan ir.LogicalPlan, params map[string]expr.Value) error {
	if len(params) == 0 {
		return nil
	}
	idxMgr := e.g.IndexManager()
	resolve := func(label, property string) (expr.Kind, bool) {
		return indexedPropKind(idxMgr, label, property)
	}
	return sema.CheckParams(sema.InferParamTypesWithResolver(plan, resolve), params)
}

// ErrInternalPanic wraps a recoverable panic that occurred while planning or
// executing a query on behalf of a single caller. The engine's query
// entrypoints ([Engine.Run], [Engine.RunInTx], [Engine.RunAny],
// [Engine.RunInTxAny]) install a recover boundary so that such a panic — an
// index-out-of-range on a malformed plan, a nil dereference, a future bug — is
// converted into this error and returned to the caller instead of unwinding
// past the engine and crashing the embedding process. Callers may match it
// with [errors.Is].
//
// The returned error deliberately carries only the panic value, never a stack
// trace: the full trace (via [runtime/debug.Stack]) is logged to the default
// [slog] handler so internal details are not leaked to the caller. This is
// defence-in-depth against recoverable panics; a Go fatal runtime error (an
// uncatchable stack overflow) cannot be intercepted here and is instead
// prevented upstream by the parser's length/nesting guards.
var ErrInternalPanic = errors.New("cypher: internal panic")

// recoverQueryPanic is the deferred recover boundary shared by the engine's
// read-only query entrypoints. When a recoverable panic is in flight it logs
// the panic value together with a full stack trace, increments the named
// metric counter, and writes a sanitised typed error (wrapping
// [ErrInternalPanic]) through errp — the caller's named return — so the
// embedder receives an inspectable error rather than a process crash. When no
// panic is in flight it is a no-op, so the happy path is unaffected.
//
// It must be called as `defer recoverQueryPanic(&err, "<entrypoint>", "<metric>")`
// from a function with a named error return. Write entrypoints that hold a WAL
// transaction must roll it back before delegating to [convertQueryPanic]; see
// [Engine.RunInTx].
//
// errp must be a pointer: the deferred recover writes through the caller's
// named return on the caller's stack frame, so this is structurally required,
// not the style choice gocritic's ptrToRefParam assumes.
//
//nolint:gocritic // ptrToRefParam: errp must be the caller's named-return pointer
func recoverQueryPanic(errp *error, entrypoint, counter string) {
	if r := recover(); r != nil {
		convertQueryPanic(r, errp, entrypoint, counter)
	}
}

// convertQueryPanic performs the log+count+convert half of the panic boundary.
// It is split out from [recoverQueryPanic] so that a write entrypoint can call
// recover() itself, roll back its in-flight WAL transaction (releasing the
// store's single-writer lock so future writes are not deadlocked), and only
// then funnel the recovered value through the same logging, counting, and
// typed-error conversion. The stack trace is logged, never returned, so engine
// internals are not leaked to the caller.
//
// errp must be a pointer: it is the caller's named return, written through on
// the caller's stack frame (gocritic's ptrToRefParam is a false positive here).
//
//nolint:gocritic // ptrToRefParam: errp must be the caller's named-return pointer
func convertQueryPanic(r any, errp *error, entrypoint, counter string) {
	cmetrics.IncCounter(counter, 1)
	slog.Default().Error("cypher: recovered panic during query execution",
		slog.String("entrypoint", entrypoint),
		slog.Any("panic", r),
		slog.String("stack", string(debug.Stack())))
	*errp = fmt.Errorf("%w: %v", ErrInternalPanic, r)
}

// recoverWriteQueryPanic is the deferred recover boundary for write
// entrypoints that hold an in-flight WAL transaction. It differs from
// [recoverQueryPanic] in one ACID-critical respect: on a recovered panic it
// first rolls back the transaction pointed to by *txp, releasing the store's
// single-writer mutex, and only then funnels the panic value through
// [convertQueryPanic] for logging, counting, and typed-error conversion.
//
// Without this rollback a panic raised after [txn.Store.Begin] — for example
// inside the [lpg.Graph.ApplyAtomically] closure during plan build, exec, or
// index commit — would convert to an error but leave the single-writer mutex
// held forever, deadlocking every subsequent write transaction and leaving a
// partial WAL transaction dangling: a violation of ACID atomicity and
// liveness. (The visibility barrier itself is safe: [lpg.Graph.ApplyAtomically]
// releases visMu with a deferred Unlock, so the panic unwinds past it cleanly.)
//
// Rolling back on panic is unconditionally correct here: a recovered panic
// always returns an error and never hands a [Result] back to the caller, so
// the transaction is never the caller's to Close. txp is a pointer-to-pointer
// so the deferred call observes the transaction assigned after Begin; *txp is
// nil when the engine is not WAL-backed, in which case the rollback is skipped.
// [txn.Tx.Rollback] is idempotent against an already-finished transaction, so
// it never double-unlocks.
//
// It must be called as
// `defer recoverWriteQueryPanic(&err, &walTx, "<entrypoint>", "<metric>")`
// from a function with a named error return whose walTx is declared before the
// defer registers; see [Engine.RunInTx].
//
// errp (caller's named return) and txp (pointer-to-pointer, so the deferred
// call observes the walTx assigned after Begin) are both structurally required
// pointers, not the style choice gocritic's ptrToRefParam assumes.
//
//nolint:gocritic // ptrToRefParam: errp and txp must be pointers for this defer pattern
func recoverWriteQueryPanic(errp *error, txp **txn.Tx[string, float64], entrypoint, counter string) {
	if r := recover(); r != nil {
		if txp != nil && *txp != nil {
			_ = (*txp).Rollback() //nolint:errcheck // rollback error is not actionable while converting a panic
		}
		convertQueryPanic(r, errp, entrypoint, counter)
	}
}

// checkContext returns a wrapped context error when ctx is already cancelled or
// its deadline has expired, and nil otherwise. The engine's query entrypoints
// call it once, before any synchronous parse/plan work, so a caller that has
// already given up (a cancelled context, an elapsed deadline, the Bolt
// statement-timeout) is answered promptly instead of paying for an
// expensive-to-parse-but-valid query whose worst case the parser's
// length/nesting guards only bound, not eliminate.
//
// The returned error wraps ctx.Err() with the package "cypher:" prefix, so it
// reads consistently with the engine's other errors while remaining matchable
// via errors.Is(err, context.Canceled) / errors.Is(err, context.DeadlineExceeded).
//
// It is O(1) and allocation-free on the happy path: ctx.Err() on a live context
// is a single atomic/struct read returning nil, and the fmt.Errorf branch is
// taken only when the context is already done.
func checkContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cypher: %w", err)
	}
	return nil
}

// Run parses, analyses, plans, and executes query, returning a materialised
// [Result]. The query is built and drained inside the read visibility barrier
// (Graph.View) so it observes a consistent, partial-transaction-free snapshot.
// DDL statements take a dedicated fast path. Parameters are bound from params
// and type-checked against the plan before execution.
//
// If ctx is already cancelled or its deadline has elapsed when Run is called,
// it returns promptly — before any parse, plan, or execution work — with an
// error wrapping the context error (matchable via [errors.Is] against
// [context.Canceled] / [context.DeadlineExceeded]).
//
// A recoverable panic raised while planning or executing the query is
// intercepted and returned as an error wrapping [ErrInternalPanic]; it never
// unwinds past this method to crash the embedding process.
func (e *Engine) Run(ctx context.Context, query string, params map[string]expr.Value) (res *Result, err error) {
	defer cmetrics.Time("cypher.Run")()
	defer func() {
		if err != nil {
			cmetrics.IncCounter("cypher.Run.errors", 1)
		}
	}()
	// Registered last so it runs first on unwind: a recovered panic sets err
	// before the cypher.Run.errors counter defer above observes it.
	defer recoverQueryPanic(&err, "cypher.Run", "cypher.Run.panics")
	// ── 0. Honour an already-cancelled/expired context before any synchronous
	// parse or plan work. Placed after the metrics/recover defers so a
	// cancellation is still timed and counted (cypher.Run.errors) consistently,
	// but before parseAndAnalyse so a caller that has already given up never
	// pays for the parse. O(1) and allocation-free on the happy path. ─────────
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	// ── 1. DDL fast-path ─────────────────────────────────────────────────────
	if ir.IsDDL(query) {
		return e.runDDL(ctx, query)
	}

	// ── 1. Parse, analyse, and retrieve from plan cache ──────────────────────
	entry, err := e.parseAndAnalyse(query)
	if err != nil {
		return nil, err
	}

	// ── 1a. Sema fast-path: skip planning when scope violations were found ───
	if entry.semaErr != nil {
		return nil, entry.semaErr
	}
	plan := entry.plan

	// ── 1b. Parameter type check ─────────────────────────────────────────────
	if err := e.checkParamTypes(plan, params); err != nil {
		return nil, err
	}

	// ── 2+3. Build the physical operator tree AND execute it under the read
	// visibility barrier (#1077) ─────────────────────────────────────────────
	// The physical build snapshots live mutable graph structures (the forward
	// CSR in buildEdgeTypeFilter, the per-edge label/instance lookups). Running
	// it inside Graph.View (visMu.RLock) means a concurrent writer — which grows
	// the adjacency under Graph.ApplyAtomically (visMu.Lock) — cannot tear those
	// snapshots mid-build. Draining the whole query inside the same barrier also
	// gives the read a consistent, partial-transaction-free view (audit gap F3,
	// docs/isolation-design.md); materialising releases the read lock before the
	// caller iterates, so a long-open Result can never deadlock a writer.
	//
	// build runs under visMu.RLock; nothing here may call g.View/g.ApplyAtomically
	// (visMu is non-re-entrant — see lpg.Graph.View/ApplyAtomically).
	var (
		r        *Result
		buildErr error
	)
	// Freeze a per-query "now" so all temporal constructors (date(), time(),
	// localtime(), datetime(), localdatetime()) observe the same instant within
	// this statement — openCypher requirement. The registry wrapper captures
	// the frozen time and overrides only the zero-argument forms of those five
	// functions; all other functions and all non-zero-argument calls are
	// delegated unchanged. Using a per-query registry avoids touching the
	// process-global statementNow in funcs, so concurrent Engine.Run calls
	// never race on it.
	queryReg := newNowAwareRegistry(e.reg, time.Now())
	e.g.View(func() {
		walker := &lpgNodeWalker{g: e.g}
		labelSrc := &lpgLabelResolver{g: e.g}
		// Allocate a per-run subquery evaluator so EXISTS { … } / COUNT { … }
		// expressions encountered inside Filter/Project closures can drive their
		// inner pipelines against the current outer row (task-396).
		subEval := newSubqueryEvaluator(walker, labelSrc, queryReg, e.g)
		// Allocate a per-run pattern evaluator so WHERE (a)-[:T]->(b) existential
		// predicates can be evaluated against the live graph (task-961). It
		// receives the Engine's per-query element budget so a pattern
		// comprehension over a supernode anchor cannot build an unbounded
		// result list — the same bound collect() enforces (#1294, #1298).
		patEval := newPatternEvaluator(e.g, e.maxCollectItems)
		bopts := &buildOpts{subEval: subEval, patEval: patEval, queryCtx: ctx, maxCollectItems: e.maxCollectItems}
		op, cols, err := buildPlanEngine(plan, walker, labelSrc, queryReg, params, e.g.IndexManager(), e.procReg, bopts)
		if err != nil {
			buildErr = err
			return
		}
		rs := exec.Run(ctx, op, cols)
		r = newResultWithLimit(rs, cols, nil, nil, nil, e.maxResultRows, e.maxResultBytes)
		r.materialize()
	})
	if buildErr != nil {
		return nil, fmt.Errorf("cypher: build plan: %w", buildErr)
	}
	return r, nil
}

// Explain returns a textual representation of the physical plan that would be
// chosen to execute query with the given params. The plan reflects current index
// availability: a hash index on the relevant (label, property) pair causes the
// relevant Selection+LabelScan subtree to appear as NodeByIndexSeek. No rows
// are produced; the graph is not modified.
//
// The format mirrors [ir.Explain] but annotates Selection→LabelScan pairs that
// would be rewritten to index seeks at execution time.
func (e *Engine) Explain(query string, params map[string]expr.Value) (string, error) {
	if ir.IsDDL(query) {
		return "(DDL — no query plan)", nil
	}
	plan, err := e.planFor(query)
	if err != nil {
		return "", err
	}
	return explainWithIndexes(plan, e.g.IndexManager(), params), nil
}

// explainWithIndexes walks plan and renders operator names, substituting
// Selection→{AllNodesScan|NodeByLabelScan} pairs with "NodeByIndexSeek" when
// tryBuildIndexSeekFromSelection would succeed given idxMgr and params.
func explainWithIndexes(plan ir.LogicalPlan, idxMgr *index.Manager, params map[string]expr.Value) string {
	var b strings.Builder
	explainWithIndexesNode(&b, plan, idxMgr, params, "", true, true)
	return b.String()
}

func explainWithIndexesNode(
	b *strings.Builder,
	plan ir.LogicalPlan,
	idxMgr *index.Manager,
	params map[string]expr.Value,
	prefix string,
	isRoot, isLast bool,
) {
	var connector, childCont string
	if isRoot {
		connector = ""
		childCont = ""
	} else if isLast {
		connector = "└─ "
		childCont = "   "
	} else {
		connector = "├─ "
		childCont = "│  "
	}

	// Check whether a Selection→scan rewrite would fire.
	opName := ir.OperatorName(plan)
	if sel, ok := plan.(*ir.Selection); ok && idxMgr != nil {
		schema := make(map[string]int)
		if op, fired, err := tryBuildIndexSeekFromSelection(sel, params, schema, idxMgr); err == nil && fired && op != nil {
			opName = "NodeByIndexSeek"
		}
	}

	b.WriteString(prefix)
	b.WriteString(connector)
	b.WriteString(opName)
	b.WriteByte('\n')

	// When a Selection was rewritten to an index seek, skip its scan child
	// (the child would be NodeByLabelScan which is subsumed by the seek).
	if opName == "NodeByIndexSeek" {
		return
	}

	children := plan.Children()
	nextPrefix := prefix + childCont
	for i, child := range children {
		explainWithIndexesNode(b, child, idxMgr, params, nextPrefix, false, i == len(children)-1)
	}
}

// guardDDLLength rejects an over-length DDL query with the same "query too
// large" error the DML parse path raises, restoring the byte-length cap the
// DDL route (ir.ParseDDL) bypasses (audit gap H7). It delegates to
// [parser.CheckQueryLength] so the limit and message live in one place.
func guardDDLLength(query string) error {
	if err := parser.CheckQueryLength(query); err != nil {
		return fmt.Errorf("cypher: DDL parse: %w", err)
	}
	return nil
}

// runDDL executes a DDL statement (CREATE INDEX / DROP INDEX / CREATE
// CONSTRAINT / DROP CONSTRAINT) eagerly. DDL operators emit no rows and are
// fully executed during runDDL — callers receive errors immediately rather
// than lazily during Result.Next.
//
// A CREATE/DROP CONSTRAINT on a WAL-backed engine is made durable: after the
// in-memory registry change succeeds, a typed constraint op is appended to the
// WAL and fsynced, so the schema change survives a crash and is re-registered
// by [store/recovery.Open] (audit gap H1). A CREATE CONSTRAINT also validates
// the pre-existing data and seeds the UNIQUE value-set by scanning the graph,
// so a constraint added to a non-empty dataset is enforced rather than silently
// inert (audit gap H2).
func (e *Engine) runDDL(ctx context.Context, query string) (*Result, error) {
	// Apply the same byte-length guard the DML parse path enforces: DDL routes
	// through ir.ParseDDL, which predates the parser's pre-parse guard, so an
	// oversize DDL query would otherwise bypass the cap (audit gap H7).
	if err := guardDDLLength(query); err != nil {
		return nil, err
	}
	ddlPlan, err := ir.ParseDDL(query)
	if err != nil {
		return nil, fmt.Errorf("cypher: DDL parse: %w", err)
	}
	idxMgr := e.g.IndexManager()
	switch p := ddlPlan.(type) {
	case *ir.CreateIndex:
		var kind exec.IndexKindExec
		switch p.Type {
		case ir.IndexTypeHash:
			kind = exec.ExecIndexHash
		case ir.IndexTypeBTree:
			kind = exec.ExecIndexBTree
		}
		return e.runDDLOp(ctx, exec.NewCreateIndexOp(p.Name, kind, p.IfNotExists, idxMgr, e.ClearPlanCache))
	case *ir.DropIndex:
		return e.runDDLOp(ctx, exec.NewDropIndexOp(p.Name, p.IfExists, idxMgr, e.ClearPlanCache))
	case *ir.CreateConstraint:
		return e.runCreateConstraint(ctx, p, idxMgr)
	case *ir.DropConstraint:
		return e.runDropConstraint(ctx, p, idxMgr)
	default:
		return nil, fmt.Errorf("cypher: unsupported DDL plan %T", ddlPlan)
	}
}

// runDDLOp executes a single eager DDL operator (emitting zero rows) and
// returns an empty Result. Errors surface at Run time rather than lazily
// during Result.Next.
func (e *Engine) runDDLOp(ctx context.Context, op exec.Operator) (*Result, error) {
	if err := op.Init(ctx); err != nil {
		return nil, fmt.Errorf("cypher: DDL init: %w", err)
	}
	var dummy exec.Row
	if _, err := op.Next(&dummy); err != nil {
		_ = op.Close()
		return nil, err
	}
	if err := op.Close(); err != nil {
		return nil, fmt.Errorf("cypher: DDL close: %w", err)
	}
	return newResult(exec.Run(ctx, exec.NewArgument(), nil), nil, nil, nil, nil), nil
}

// emptyDDLResult returns the canonical zero-row Result that every DDL statement
// yields once its side effect has been applied.
func emptyDDLResult(ctx context.Context) *Result {
	return newResult(exec.Run(ctx, exec.NewArgument(), nil), nil, nil, nil, nil)
}

// runCreateConstraint executes CREATE CONSTRAINT: it validates the pre-existing
// data, registers the constraint and seeds its value-set, then (on a WAL-backed
// engine) appends a durable constraint op so the schema change survives a
// crash.
func (e *Engine) runCreateConstraint(ctx context.Context, p *ir.CreateConstraint, idxMgr *index.Manager) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	kind := execConstraintKind(p.Kind)

	// IF NOT EXISTS that absorbs an already-registered constraint is a silent
	// no-op with no schema change and no WAL record — match the operator's
	// existing semantics and skip validation/durability.
	if p.IfNotExists && e.constraintAlreadyRegistered(kind, p.Label, p.Property) {
		return emptyDDLResult(ctx), nil
	}

	// Validate the pre-existing data and seed the value-set BEFORE registering,
	// so a constraint over already-violating data is rejected with nothing
	// registered (audit gap H2).
	values, anyNull := e.scanLabelProperty(p.Label, p.Property)
	if err := validatePreExisting(kind, p.Label, p.Property, values, anyNull); err != nil {
		return nil, err
	}

	// Register in memory via the operator (handles index creation + IF NOT
	// EXISTS), then seed the UNIQUE value-set from the scanned values.
	op := exec.NewCreateConstraintOp(p.Name, p.Label, p.Property, kind, p.IfNotExists, idxMgr, e.constraintReg, e.ClearPlanCache)
	if _, err := e.runDDLOp(ctx, op); err != nil {
		return nil, err
	}
	if kind == exec.ConstraintUnique {
		if err := e.constraintReg.SeedUniqueValues(p.Label, p.Property, values); err != nil {
			return nil, err
		}
	}

	// Durability: append the constraint op to the WAL and fsync it, so the
	// schema change survives a crash (audit gap H1).
	if err := e.appendConstraintOp(ctx, txn.OpCreateConstraint, kind, p.Label, p.Property, p.Name); err != nil {
		return nil, err
	}
	return emptyDDLResult(ctx), nil
}

// runDropConstraint executes DROP CONSTRAINT: it deregisters the constraint via
// the operator, then (on a WAL-backed engine) appends a durable drop op so the
// removal survives a crash.
func (e *Engine) runDropConstraint(ctx context.Context, p *ir.DropConstraint, idxMgr *index.Manager) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	kind := execConstraintKind(p.Kind)
	op := exec.NewDropConstraintOp(p.Name, p.Label, p.Property, kind, p.IfExists, idxMgr, e.constraintReg, e.ClearPlanCache)
	if _, err := e.runDDLOp(ctx, op); err != nil {
		return nil, err
	}
	if err := e.appendConstraintOp(ctx, txn.OpDropConstraint, kind, p.Label, p.Property, p.Name); err != nil {
		return nil, err
	}
	return emptyDDLResult(ctx), nil
}

// execConstraintKind maps the IR constraint kind to the exec kind.
func execConstraintKind(k ir.ConstraintKind) exec.ConstraintKind {
	if k == ir.ConstraintNotNull {
		return exec.ConstraintNotNull
	}
	return exec.ConstraintUnique
}

// constraintAlreadyRegistered reports whether a constraint of the given kind is
// already registered for (label, prop).
func (e *Engine) constraintAlreadyRegistered(kind exec.ConstraintKind, label, prop string) bool {
	if kind == exec.ConstraintNotNull {
		return e.constraintReg.HasNotNull(label, prop)
	}
	return e.constraintReg.HasUnique(label, prop)
}

// appendConstraintOp appends a durable CREATE/DROP CONSTRAINT op to the WAL on a
// WAL-backed engine and fsyncs it. It is a no-op on a store-less in-memory
// engine (no durability surface). The op carries no node endpoints; it serves
// only to re-register (or suppress) the constraint on recovery. The single-op
// transaction is committed WAL-only — the in-memory registry was already
// updated by the operator — mirroring the eager-apply + CommitWALOnly pattern
// the write path uses.
func (e *Engine) appendConstraintOp(ctx context.Context, opKind txn.OpKind, kind exec.ConstraintKind, label, prop, name string) error {
	if e.store == nil {
		return nil
	}
	ck := txn.ConstraintUnique
	if kind == exec.ConstraintNotNull {
		ck = txn.ConstraintNotNull
	}
	tx, err := e.store.BeginCtx(ctx)
	if err != nil {
		return err
	}
	switch opKind {
	case txn.OpCreateConstraint:
		err = tx.CreateConstraint(ck, label, prop, name)
	case txn.OpDropConstraint:
		err = tx.DropConstraint(ck, label, prop, name)
	default:
		err = fmt.Errorf("cypher: appendConstraintOp: unexpected op kind %d", opKind)
	}
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	// CommitWALOnly: the registry change is already applied in memory; this only
	// secures the durable record. A constraint op applies nothing to the graph,
	// so a full Commit would be equivalent, but CommitWALOnly states the intent.
	if cerr := tx.CommitWALOnly(); cerr != nil {
		return cerr
	}
	return nil
}

// scanLabelProperty walks the live (non-tombstoned) nodes carrying label and
// returns the values of their prop property and whether any such node lacks the
// property (a null for the NOT NULL check). It is used both to validate
// pre-existing data on CREATE CONSTRAINT and to re-seed a UNIQUE value-set on
// recovery. The scan is O(N) over interned nodes; CREATE CONSTRAINT is a rare
// schema operation, so the cost is acceptable and bounded by the graph size.
//
// The scan is two-phase to preserve liveness under concurrent writers (task
// #1339): [graph.Mapper.Walk] holds each shard's read lock while iterating
// that shard, and [lpg.Graph.HasNodeLabel] / [lpg.Graph.GetNodeProperty]
// re-enter [graph.Mapper.Lookup] on the very same shard the walked key lives
// in. With a concurrent writer's [graph.Mapper.Intern] queued on that shard's
// write lock, sync.RWMutex stops admitting new readers, so a nested Lookup
// issued from inside the Walk callback blocks forever — deadlocking the
// writer, the scan, and every future operation on the shard. Phase 1 therefore
// only snapshots the (id, key) pairs under the shard locks; phase 2 resolves
// tombstone, label, and property state after every shard lock is released. The
// keys are interned and immutable, so resolving them outside the walk is safe.
func (e *Engine) scanLabelProperty(label, prop string) (values []lpg.PropertyValue, anyNull bool) {
	mapper := e.g.AdjList().Mapper()

	// Phase 1 — snapshot the interned nodes. The callback must not touch any
	// other graph state (see the deadlock note above).
	type nodeRef struct {
		id  graph.NodeID
		key string
	}
	refs := make([]nodeRef, 0, mapper.Len())
	mapper.Walk(func(id graph.NodeID, key string) bool {
		refs = append(refs, nodeRef{id: id, key: key})
		return true
	})

	// Phase 2 — resolve graph state with no shard lock held.
	for i := range refs {
		r := refs[i]
		if e.g.IsTombstoned(r.id) {
			continue
		}
		if !e.g.HasNodeLabel(r.key, label) {
			continue
		}
		v, ok := e.g.GetNodeProperty(r.key, prop)
		if !ok {
			anyNull = true
			continue
		}
		values = append(values, v)
	}
	return values, anyNull
}

// validatePreExisting enforces the at-creation invariant for CREATE CONSTRAINT
// over pre-existing data (Neo4j semantics): a UNIQUE constraint is rejected
// when two existing nodes share a value; a NOT NULL constraint is rejected when
// any node with the label lacks the property. On violation it returns an error
// wrapping [exec.ErrConstraintViolation]; otherwise nil.
func validatePreExisting(kind exec.ConstraintKind, label, prop string, values []lpg.PropertyValue, anyNull bool) error {
	switch kind {
	case exec.ConstraintNotNull:
		if anyNull {
			return fmt.Errorf("cypher: cannot create NOT NULL constraint on (:%s).%s: %w: pre-existing node has a null value",
				label, prop, exec.ErrConstraintViolation)
		}
	case exec.ConstraintUnique:
		// SeedUniqueValues performs the duplicate detection against the same
		// canonical encoding the live check uses; reuse it here on a throwaway
		// registry so the rejection semantics are identical.
		probe := exec.NewConstraintRegistry()
		probe.RegisterUnique(label, prop, "")
		if err := probe.SeedUniqueValues(label, prop, values); err != nil {
			return fmt.Errorf("cypher: cannot create UNIQUE constraint on (:%s).%s: %w",
				label, prop, err)
		}
	}
	return nil
}

// RunAny executes query with params expressed as map[string]any, automatically
// converting Go native types to [expr.Value]. See [BindParams] for the
// supported conversions.
//
// RunAny auto-detects whether the query contains writing clauses (CREATE,
// MERGE, SET, REMOVE, DELETE, DETACH DELETE) and routes through
// [Engine.RunInTx] when so, or [Engine.Run] otherwise. Callers that need
// an explicit choice should invoke [Engine.Run] / [Engine.RunInTx]
// directly.
func (e *Engine) RunAny(ctx context.Context, query string, params map[string]any) (*Result, error) {
	converted, err := BindParams(params)
	if err != nil {
		return nil, err
	}
	// The panic boundary (ErrInternalPanic) is inherited transitively: this
	// method delegates to Run/RunInTx, which each install their own recover.
	if queryHasWritingClause(query) {
		return e.RunInTx(ctx, query, converted)
	}
	return e.Run(ctx, query, converted)
}

// QueryHasWritingClause reports whether the query string contains any
// writing keyword (CREATE, MERGE, SET, REMOVE, DELETE, DETACH) outside a
// DDL prefix, i.e. whether it must be routed through [Engine.RunInTx]
// rather than [Engine.Run]. This is a textual heuristic: it avoids
// triggering the plan-cache machinery on a second pass, which would
// otherwise double-count hits and misses in concurrency tests.
//
// External front-ends that classify queries as read vs write (for example,
// to serialise writers or pick a read replica) should call this rather than
// re-deriving the keyword set, so the classification stays in lockstep with
// [Engine.RunAny].
//
// The heuristic is intentionally permissive — false positives (writing
// keywords inside string literals or backtick identifiers) merely cause a
// read-only query to be routed through RunInTx, which executes identical
// semantics with the same correctness guarantees, only with the cost of
// opening and committing a write transaction.
func QueryHasWritingClause(query string) bool {
	if ir.IsDDL(query) {
		return false
	}
	return writingKeywordRE.MatchString(query)
}

// queryHasWritingClause is the internal alias for [QueryHasWritingClause],
// retained so existing call sites read naturally.
func queryHasWritingClause(query string) bool {
	return QueryHasWritingClause(query)
}

// ErrResultRowsExceeded is returned by [Result.Next] and [Result.Err] when the
// number of materialised rows exceeds [EngineOptions.MaxResultRows]. It is a
// permanent error: once set, subsequent Next calls return false.
var ErrResultRowsExceeded = errors.New("cypher: result row limit exceeded")

// DefaultMaxResultRows is the default upper bound on the number of rows a single
// [Engine.Run] or [Engine.RunInTx] call materialises when [EngineOptions.MaxResultRows]
// is left at its zero value. It bounds the worst case — an unintentional
// whole-graph scan or Cartesian product — so the engine never materialises an
// unbounded number of rows into memory inside the visibility barrier. It matches
// the sibling pipeline-breaker caps ([exec.DefaultMaxSortRows], [exec.DefaultMaxDistinct])
// and is set high enough that ordinary queries, the openCypher TCK, and all
// examples stay well below it; callers that genuinely need an unbounded result
// must opt out explicitly with [MaxResultRowsUnlimited].
const DefaultMaxResultRows int64 = 10_000_000

// MaxResultRowsUnlimited is the explicit opt-out sentinel for
// [EngineOptions.MaxResultRows]: set the field to this value to disable the
// row cap entirely and allow an unbounded result. It is distinct from the zero
// value, which selects [DefaultMaxResultRows]. Use it only when the caller can
// bound memory by another means (e.g. streaming the result and closing it
// promptly), because an unbounded MATCH then materialises every row under the
// graph's visibility barrier.
const MaxResultRowsUnlimited int64 = -1

// ErrResultBytesExceeded is returned by [Result.Err] when the cumulative
// estimated encoded size of the materialised rows exceeds
// [EngineOptions.MaxResultBytes]. It complements [ErrResultRowsExceeded]: the
// row cap bounds the *number* of rows, but a handful of rows carrying very large
// values (a node with megabyte-scale string properties) can dwarf a high row
// count, so the byte budget bounds that residual case. Like the row cap it is a
// permanent error tripped inside the visibility barrier during materialisation,
// before the surplus reaches the caller.
var ErrResultBytesExceeded = errors.New("cypher: result byte budget exceeded")

// DefaultMaxResultBytes is the default upper bound on the aggregate estimated
// encoded size of the rows a single [Engine.Run] or [Engine.RunInTx] call
// materialises when [EngineOptions.MaxResultBytes] is left at its zero value.
// It is a coarse budget against the worst case the row cap alone cannot catch —
// a result that stays under [DefaultMaxResultRows] yet carries enough bytes per
// row to exhaust memory inside the visibility barrier. The default (1 GiB) is
// set high enough that ordinary queries, the openCypher TCK, and all examples
// stay well below it; callers that genuinely need an unbounded result size must
// opt out explicitly with [MaxResultBytesUnlimited].
const DefaultMaxResultBytes int64 = 1 << 30 // 1 GiB

// MaxResultBytesUnlimited is the explicit opt-out sentinel for
// [EngineOptions.MaxResultBytes]: set the field to this value to disable the
// aggregate-byte budget entirely. It is distinct from the zero value, which
// selects [DefaultMaxResultBytes]. Use it only when memory is bounded by another
// means, because an unbounded wide-row result then materialises every byte under
// the graph's visibility barrier.
const MaxResultBytesUnlimited int64 = -1

// MaxCollectItemsUnlimited is the explicit opt-out sentinel for
// [EngineOptions.MaxCollectItems]: set the field to this value to disable the
// per-group element budget entirely and allow an unbounded buffering aggregator
// (collect / percentile). It is distinct from the zero value, which selects
// [funcs.DefaultMaxCollectItems]. Use it only when memory is bounded by another
// means, because an unbounded `collect(n)` over a whole-graph scan then
// materialises every value into one list under the graph's visibility barrier.
const MaxCollectItemsUnlimited = -1

// writingKeywordRE matches any writing-clause keyword as a standalone word.
// The pattern uses a case-insensitive flag and a word boundary anchor so
// fragments like "PRESET" or "NOMERGE" are not falsely classified.
//
//nolint:gochecknoglobals // singleton regex compiled once at init
var writingKeywordRE = regexp.MustCompile(`(?i)\b(CREATE|MERGE|SET|REMOVE|DELETE|DETACH)\b`)

// RunInTxAny executes a write query with params expressed as map[string]any,
// automatically converting Go native types to [expr.Value]. See [BindParams].
func (e *Engine) RunInTxAny(ctx context.Context, query string, params map[string]any) (*Result, error) {
	converted, err := BindParams(params)
	if err != nil {
		return nil, err
	}
	return e.RunInTx(ctx, query, converted)
}

// BindParams converts a map[string]any to map[string]expr.Value using the
// following type mapping:
//
//   - nil                       → expr.Null
//   - bool                      → expr.BoolValue
//   - int, int8, int16, int32, int64 → expr.IntegerValue
//   - uint, uint8, uint16, uint32, uint64 → expr.IntegerValue (truncated to int64)
//   - float32, float64          → expr.FloatValue
//   - string                    → expr.StringValue
//   - []any                     → expr.ListValue (recursively converted)
//   - map[string]any            → expr.MapValue (recursively converted)
//   - expr.Value                → passed through unchanged
//
// Returns an error for unsupported types.
func BindParams(params map[string]any) (map[string]expr.Value, error) {
	if len(params) == 0 {
		return nil, nil
	}
	out := make(map[string]expr.Value, len(params))
	for k, v := range params {
		converted, err := bindAny(v)
		if err != nil {
			return nil, fmt.Errorf("cypher: BindParams: key %q: %w", k, err)
		}
		out[k] = converted
	}
	return out, nil
}

// bindAny converts a single Go value to an expr.Value.
// Numeric types (int*, uint*, float*) are handled by bindNumeric.
func bindAny(v any) (expr.Value, error) {
	if v == nil {
		return expr.Null, nil
	}
	switch t := v.(type) {
	case expr.Value:
		return t, nil
	case bool:
		return expr.BoolValue(t), nil
	case string:
		return expr.StringValue(t), nil
	case []any:
		list := make(expr.ListValue, len(t))
		for i, elem := range t {
			converted, err := bindAny(elem)
			if err != nil {
				return nil, fmt.Errorf("list[%d]: %w", i, err)
			}
			list[i] = converted
		}
		return list, nil
	case map[string]any:
		m := make(expr.MapValue, len(t))
		for k, elem := range t {
			converted, err := bindAny(elem)
			if err != nil {
				return nil, fmt.Errorf("map[%q]: %w", k, err)
			}
			m[k] = converted
		}
		return m, nil
	default:
		if num, ok := bindNumeric(v); ok {
			return num, nil
		}
		return nil, fmt.Errorf("unsupported parameter type %T", v)
	}
}

// bindNumeric converts Go numeric primitives to expr.Value.
// Returns (value, true) on match, (nil, false) if v is not a numeric type.
func bindNumeric(v any) (expr.Value, bool) {
	switch t := v.(type) {
	case int:
		return expr.IntegerValue(int64(t)), true
	case int8:
		return expr.IntegerValue(int64(t)), true
	case int16:
		return expr.IntegerValue(int64(t)), true
	case int32:
		return expr.IntegerValue(int64(t)), true
	case int64:
		return expr.IntegerValue(t), true
	case uint:
		return expr.IntegerValue(int64(t)), true //nolint:gosec // intentional truncation documented
	case uint8:
		return expr.IntegerValue(int64(t)), true
	case uint16:
		return expr.IntegerValue(int64(t)), true
	case uint32:
		return expr.IntegerValue(int64(t)), true
	case uint64:
		return expr.IntegerValue(int64(t)), true //nolint:gosec // intentional truncation documented
	case float32:
		return expr.FloatValue(float64(t)), true
	case float64:
		return expr.FloatValue(t), true
	default:
		return nil, false
	}
}

// planCacheEntry is the value stored in [Engine.cache] for a successfully
// parsed query. It bundles the translated logical plan with the semantic
// analyser's verdict so that both lookups (planFor, semaCheckCached) hit a
// single cache slot.
//
// The semaErr field is non-nil when [sema.Analyse] reported violations;
// callers consult it before plan execution to short-circuit with a typed
// [*sema.SemanticError]. The logical plan is still cached even when semaErr
// is non-nil so that [Engine.Explain] can render the plan tree for
// diagnostic purposes without re-parsing.
type planCacheEntry struct {
	plan    ir.LogicalPlan
	semaErr *sema.SemanticError
}

// planFor returns the cached logical plan for query, or parses, translates,
// and caches it. Semantic-analysis failures are NOT surfaced from planFor:
// callers should invoke [Engine.semaCheckCached] separately so they can
// decide whether to fast-fail before plan execution (Run/RunInTx) or to
// still render an Explain tree.
func (e *Engine) planFor(query string) (ir.LogicalPlan, error) {
	entry, err := e.parseAndAnalyse(query)
	if err != nil {
		return nil, err
	}
	return entry.plan, nil
}

// parseAndAnalyse parses, runs the scope analyser, and translates query into
// a logical plan. The full result (plan + sema verdict) is cached so that
// subsequent calls with the same query string skip every stage above plan
// execution.
//
// A non-nil error is returned only for parse or translation failures; a
// semantically invalid (but parseable) query yields a cache entry whose
// semaErr field is set, and parseAndAnalyse returns (entry, nil).
func (e *Engine) parseAndAnalyse(query string) (*planCacheEntry, error) {
	if v, ok := e.cache.get(query); ok {
		return v, nil
	}
	astNode, err := parser.Parse(query)
	if err != nil {
		return nil, fmt.Errorf("cypher: parse: %w", err)
	}
	semaErr := sema.MapToBolt(sema.Analyse(astNode))
	plan, err := ir.FromAST(astNode)
	if err != nil {
		return nil, fmt.Errorf("cypher: translate: %w", err)
	}
	entry := &planCacheEntry{plan: plan, semaErr: semaErr}
	actual, _ := e.cache.loadOrStore(query, entry)
	return actual, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Result
// ─────────────────────────────────────────────────────────────────────────────

// Result is a forward-only streaming result set returned by [Engine.Run] /
// [Engine.RunInTx]. It wraps [exec.ResultSet] and exposes the same iterator
// contract.
//
// # Lifecycle contract
//
// Every Result returned from a successful Run/RunInTx call MUST be closed
// by the caller via [Result.Close], even if [Result.Err] is non-nil and even
// if the caller stops iterating before exhaustion. Close releases the
// physical operator tree, drains any goroutines spawned by parallel operators,
// commits or rolls back buffered index mutations for write queries, and (for
// WAL-backed engines) fsyncs the WAL or rolls the transaction back.
//
// The typical pattern is:
//
//	res, err := engine.Run(ctx, query, params)
//	if err != nil {
//	    return err
//	}
//	defer res.Close()
//	for res.Next() {
//	    rec := res.Record()
//	    // ... consume rec ...
//	}
//	return res.Err()
//
// # Safety net
//
// Result installs a [runtime.SetFinalizer] that detects callers who forget
// to Close. When the garbage collector reclaims an unclosed Result, the
// finalizer:
//
//  1. Increments the metric "cypher.result.leaked" so operators see the
//     incidence count in their monitoring; and
//  2. Best-effort closes the underlying resources to limit damage on a
//     long-running server.
//
// The finalizer is a fail-stop diagnostic, NOT a substitute for an explicit
// Close. In particular, the finalizer runs at an unpredictable time after
// the leak (it depends on the GC schedule) and CANNOT report errors back to
// the caller. For a write [Result] from [Engine.RunInTx] the WAL transaction
// is already committed (fsynced) or rolled back under the barrier before
// RunInTx returns (#1281), so the store's single-writer mutex is released at
// that point — a leaked, unclosed Result leaks only the ResultSet, not the
// write lock. Callers that need predictable resource release MUST still call
// Close themselves.
//
// Result is NOT safe for concurrent use.
type Result struct {
	rs     *exec.ResultSet
	cols   []string
	buf    *exec.IndexBuffer        // non-nil only for RunInTx results
	idxMgr *index.Manager           // non-nil only when buf != nil
	tx     *txn.Tx[string, float64] // non-nil only for WAL-backed RunInTx results

	// matRows holds the rows drained under the transaction-visibility barrier
	// (Graph.View for reads, Graph.ApplyAtomically for writes) at creation, so
	// the whole query observes/produces one atomic, partial-transaction-free
	// state (audit gap F3, docs/isolation-design.md). Once materialised the
	// Result serves these buffered rows and holds NO lock while the caller
	// iterates, so a long-open Result can never deadlock a concurrent writer —
	// the property that makes the barrier safe for the lazy executor. matOn
	// distinguishes a materialised Result (serve matRows) from a raw streaming
	// one (delegate to rs).
	matRows []exec.Record
	matIdx  int
	matOn   bool

	// bufHandled is set once the secondary-index buffer has been committed (or
	// rolled back) inside the write query's ApplyAtomically window (F3.4), so
	// closeLocked does not act on it again. Committing the index buffer under
	// the same barrier as the graph writes makes the secondary indexes flip
	// atomically with the graph, so an index-seek read never observes a
	// transaction whose graph change is visible but whose index change is not.
	bufHandled bool

	// walHandled is set once the WAL transaction has been committed (fsynced)
	// or rolled back inside the write query's ApplyAtomically window (#1281,
	// durable-then-visible). When set, closeLocked must NOT touch r.tx again —
	// the durability decision was already made and finalised under the barrier,
	// so the WAL fsync happens-before the mutations become observable to a
	// concurrent Graph.View reader. It is the WAL analogue of bufHandled. For a
	// read query, or a write whose WAL commit is still deferred to Close (none,
	// post-#1281), it stays false and closeLocked owns the commit/rollback.
	walHandled bool

	// undo holds the inverse of every in-memory mutation this write query
	// applied eagerly. On a failed drain it is replayed in reverse inside the
	// same ApplyAtomically window as the graph writes (commitUnderBarrier),
	// so the live graph is restored before any reader can observe the partial
	// transaction (Atomicity). nil for read queries. See undo.go.
	undo *undoLog
	// undoErr is set non-nil when the undo replay above itself failed (an
	// inverse panicked), so the graph may be inconsistent. Err surfaces it
	// wrapped in [ErrUndoFailed] alongside the triggering pipeline error.
	undoErr error
	// walErr is set non-nil when the in-barrier WAL fsync (CommitWALOnly)
	// failed for an otherwise-successful write (#1281, durable-then-visible).
	// The eager in-memory mutations have ALREADY been rolled back (undo
	// replayed) and the WAL transaction rolled back, both under the barrier, so
	// the failed write is neither visible nor durable. RunInTx surfaces walErr
	// to the caller instead of returning the Result; Err also reports it so a
	// caller that somehow holds the Result still learns the write did not land.
	walErr error

	// maxRows, when positive, caps the total number of rows Next() may return.
	// Set from EngineOptions.MaxResultRows at result construction time.
	maxRows  int64
	rowCount int64 // incremented by Next(); never reset
	rowsErr  error // ErrResultRowsExceeded or ErrResultBytesExceeded when a cap trips

	// maxBytes, when positive, caps the cumulative estimated encoded size of the
	// materialised rows (EngineOptions.MaxResultBytes). It complements maxRows:
	// the row cap bounds the row count, the byte budget bounds the residual
	// wide-row case a high row cap cannot catch. Both are enforced in
	// materialize() under the visibility barrier. rowsErr is shared by both caps
	// (set to ErrResultBytesExceeded when the byte budget trips) because it is the
	// "result was truncated by a bounded-resource guard" signal Next/Err already
	// honour.
	maxBytes int64

	closed atomic.Bool // tripped by Close; checked by the finalizer
}

// Next advances to the next result row. Returns true when a row is available.
// If [EngineOptions.MaxResultRows] is set and the limit is reached, Next sets
// the result's error to [ErrResultRowsExceeded] and returns false.
func (r *Result) Next() bool {
	if r.rowsErr != nil {
		return false
	}
	if r.matOn {
		if r.matIdx < len(r.matRows) {
			r.matIdx++
			return true
		}
		return false
	}
	if !r.rs.Next() {
		return false
	}
	if r.maxRows > 0 {
		r.rowCount++
		if r.rowCount > r.maxRows {
			r.rowsErr = ErrResultRowsExceeded
			return false
		}
	}
	return true
}

// Record returns the current row as a map from column name to value.
// Must only be called after a successful [Next].
func (r *Result) Record() exec.Record {
	if r.matOn {
		return r.matRows[r.matIdx-1]
	}
	return r.rs.Record()
}

// materialize drains the underlying ResultSet fully into matRows. Each row is
// retained by taking ownership of the ResultSet's per-row map (TakeRecord
// installs a fresh map for the next Next), which avoids the extra shallow copy
// that re-hashing every column into a new map would cost — the alloc count is
// unchanged (one map per retained row either way) but the per-row copy loop is
// removed. It MUST be called inside Graph.View (read queries) or
// Graph.ApplyAtomically (write queries): the whole drain — every graph read and
// every eager write — then happens under one barrier acquisition, so a
// concurrent reader observes the query's writes atomically and the query itself
// observes a consistent, partial-transaction-free snapshot. After materialize
// returns, the Result holds no lock; iteration is served from matRows. Errors
// encountered during the drain are recorded on the ResultSet and surfaced via
// Result.Err(); Close still commits/rolls back.
func (r *Result) materialize() {
	var byteCount int64
	for r.rs.Next() {
		rec := r.rs.TakeRecord()
		r.matRows = append(r.matRows, rec)
		if r.maxRows > 0 && int64(len(r.matRows)) > r.maxRows {
			r.rowsErr = ErrResultRowsExceeded
			break
		}
		// Aggregate-byte budget (#1328): the row cap bounds the *number* of rows
		// but a handful of rows carrying very large values (a node with
		// megabyte-scale string properties) can still dwarf a high row cap. A
		// coarse, allocation-free size estimate (estimateRecordSize: O(columns)
		// per row, no serialisation) accumulates here alongside the row check and
		// trips ErrResultBytesExceeded when the budget is exceeded. byteCount can
		// saturate for a pathological result, which only makes the budget trip
		// sooner — never miss — so the additions are deliberately unguarded against
		// overflow. The only per-row cost is one map lookup per column to reach the
		// value; benchstat shows that is allocation-free and within noise of the
		// un-accounted drain on a representative scalar-projection result (see
		// BenchmarkResultMaterialize* in result_bytes_cap_bench_test.go).
		if r.maxBytes > 0 {
			byteCount += estimateRecordSize(r.cols, rec)
			if byteCount > r.maxBytes {
				r.rowsErr = ErrResultBytesExceeded
				break
			}
		}
	}
	r.matOn = true
}

// perValueOverhead is the flat byte charge attributed to every value regardless
// of kind, covering the interface header, kind tag, and (for scalars) the small
// fixed payload of an integer / float / bool / temporal / point value. It is a
// coarse constant, not a measurement: the byte budget is a guard against
// pathological memory use, not an exact heap accounting.
const perValueOverhead int64 = 16

// estimateRecordSize returns a coarse, allocation-free estimate of the encoded
// size of one result row, summing estimateValueSize over the row's column
// values. It is the per-row term of the aggregate-byte budget (#1328) and is
// called once per materialised row inside the visibility barrier, so cheapness
// is load-bearing.
//
// It iterates the ordered column list (a slice) and indexes the record map by
// column name, rather than ranging the map directly. A map range pays Go's
// per-iteration iterator setup (a randomised hash-seed init) on every call; over
// a multi-row drain that fixed cost dominates the trivial per-value arithmetic
// and measurably regresses the common small-result path. Ranging the cols slice
// and doing one map lookup per column avoids that setup and keeps the accounting
// within noise of the un-accounted drain (verified by BenchmarkResultMaterialize*
// in result_bytes_cap_bench_test.go). cols always lists exactly the record's
// keys, so the two traversals are equivalent. The estimate never allocates or
// serialises.
func estimateRecordSize(cols []string, rec exec.Record) int64 {
	var total int64
	for _, k := range cols {
		total += estimateValueSize(rec[k])
	}
	return total
}

// estimateValueSize returns a coarse, allocation-free byte estimate for a single
// column value. It takes any because a materialised [exec.Record] is a
// map[string]interface{} (its values are [expr.Value] instances boxed as the
// empty interface); a type switch over the empty interface adds no allocation
// because the operand is already an interface value.
//
// The estimate is deliberately cheap: a fixed per-value overhead plus the
// lengths of variable-size payloads (string bytes, list and map element counts,
// node/relationship/path element counts). It NEVER fully encodes or serialises
// the value — String() would allocate, defeating the point of a barrier-held
// budget. The structural types (node/relationship/path) are unfolded inline
// rather than recursing through the any boundary so iterating their by-value
// element slices ([]NodeValue, []RelationshipValue) does not box each element.
// List and map elements are already [expr.Value] interface values, so recursing
// on them is a no-op conversion. Cypher values are acyclic, so the recursion is
// bounded and the whole estimate is O(total elements in the row), allocation-free.
func estimateValueSize(v any) int64 {
	switch t := v.(type) {
	case expr.StringValue:
		return perValueOverhead + int64(len(t))
	case expr.ListValue:
		// Per-element overhead (carried by each element's own estimate) plus the
		// recursive size of each element, so a long list of tiny scalars still
		// counts against the budget rather than estimating near zero.
		total := perValueOverhead
		for _, e := range t {
			total += estimateValueSize(e)
		}
		return total
	case expr.MapValue:
		return estimateMapSize(t)
	case expr.NodeValue:
		return estimateNodeSize(t)
	case expr.RelationshipValue:
		return estimateRelSize(t)
	case expr.PathValue:
		total := perValueOverhead
		for i := range t.Nodes {
			total += estimateNodeSize(t.Nodes[i])
		}
		for i := range t.Relationships {
			total += estimateRelSize(t.Relationships[i])
		}
		return total
	default:
		// Integer, Float, Bool, Null, the temporal / point scalars, and any
		// non-expr.Value a caller might place in a Record: a fixed, small
		// footprint covered entirely by the per-value overhead.
		return perValueOverhead
	}
}

// estimateMapSize sums the per-value overhead, key lengths, and recursive value
// sizes of a property map. Split out so the node/relationship paths can reuse it
// without re-boxing the map through estimateValueSize's any parameter.
func estimateMapSize(m expr.MapValue) int64 {
	total := perValueOverhead
	for k, e := range m {
		total += int64(len(k)) + estimateValueSize(e)
	}
	return total
}

// estimateNodeSize accounts a node's label byte-lengths plus its property map.
func estimateNodeSize(n expr.NodeValue) int64 {
	total := perValueOverhead
	for _, lbl := range n.Labels {
		total += int64(len(lbl))
	}
	return total + estimateMapSize(n.Properties)
}

// estimateRelSize accounts a relationship's type byte-length plus its property map.
func estimateRelSize(r expr.RelationshipValue) int64 {
	return perValueOverhead + int64(len(r.Type)) + estimateMapSize(r.Properties)
}

// commitUnderBarrier finalises a write query's transaction inside the same
// [lpg.Graph.ApplyAtomically] window in which its mutations were applied,
// enforcing the durable-then-visible ordering ACID Durability requires
// (#1281). The visibility barrier (visMu) is still held when this runs, so
// whatever decision it makes — keep the writes or roll them back — becomes
// observable to a concurrent [lpg.Graph.View] reader as a single atomic step,
// and the WAL fsync that gates "keep" happens-before that visibility flip.
//
// It is a no-op for read queries (buf == nil and tx == nil) and is idempotent
// (bufHandled / walHandled). The keep/roll-back decision uses the
// post-materialise iteration error:
//
//   - Failed write (drain error). Replay the transaction-undo log (undo.go) to
//     roll the live in-memory graph back to its pre-statement state, then roll
//     back the secondary-index buffer and the WAL transaction. The undo runs
//     before the index rollback so the indexes are dropped only after the graph
//     entries they describe are gone. Nothing was made durable. If the undo
//     replay itself fails, undoErr is recorded so [Result.Err] surfaces
//     [ErrUndoFailed].
//
//   - Truncated write (rowsErr: a bounded-resource guard — [ErrResultRowsExceeded]
//     or [ErrResultBytesExceeded] — stopped the drain early, #1338). Treated
//     exactly like a drain error: the rows pulled before the trip already
//     applied their mutations eagerly, so committing here would fsync a PARTIAL
//     transaction while the caller receives the cap sentinel — an Atomicity
//     violation. The whole statement is rolled back instead; the sentinel still
//     surfaces via [Result.Err].
//
//   - Successful write. fsync the WAL FIRST (CommitWALOnly) so durability is
//     secured before the writes are allowed to remain visible past the barrier.
//     Only when the fsync succeeds is the index buffer committed and the undo
//     log dropped — the transaction is now durable AND visible as one step. If
//     the fsync FAILS, the in-memory writes are NOT durable, so they must not
//     stay visible: the undo log is replayed (rolling the graph back), the index
//     buffer is rolled back, the WAL transaction is rolled back, and walErr is
//     recorded. RunInTx then surfaces walErr instead of the Result, so a write
//     whose durability could not be guaranteed is reported as a failure rather
//     than acknowledged as a non-durable success.
//
// Pre-#1281 the WAL fsync happened later, in [Result.Close], AFTER visMu was
// released — a window in which a concurrent reader could observe (and act on) a
// write that a crash before Close would lose. Moving the fsync in here closes
// that window. The documented trade-off (docs/isolation-design.md: the barrier
// is correctness-first) is that the fsync now runs while visMu is held,
// briefly excluding transactional readers for the duration of the disk sync;
// the lock-free per-shard snapshot is the tracked performance end-state, and
// the read/analytics CSR path does not go through this barrier.
func (r *Result) commitUnderBarrier() {
	if r.bufHandled && r.walHandled {
		return
	}
	if r.rs.Err() != nil || r.rowsErr != nil {
		r.rollbackUnderBarrier()
		return
	}
	// Success path: durability before visibility. fsync the WAL before the
	// index commit so the transaction is durable the instant its writes are
	// allowed to remain observable past the barrier.
	if r.tx != nil {
		if werr := r.tx.CommitWALOnly(); werr != nil {
			cmetrics.IncCounter("cypher.RunInTx.wal.commitErrors", 1)
			// The fsync failed: roll the not-durable write back so it never
			// stays visible, then surface the error from RunInTx.
			r.walErr = werr
			r.rollbackUnderBarrier()
			return
		}
		r.walHandled = true
	}
	if r.buf != nil {
		r.buf.Commit(r.idxMgr)
	}
	r.bufHandled = true
	// Mark the WAL handled even when tx == nil (a store-less engine has no WAL to
	// commit) so the idempotency guard above trips on a second call.
	r.walHandled = true
	// Success: the transaction is keeping its writes; drop the undo log so its
	// closures (and their captured pre-images) are released for GC.
	r.undo = nil
}

// rollbackUnderBarrier undoes a write query's eager in-memory mutations,
// secondary-index buffer, and WAL transaction, all while the visibility barrier
// is still held, so the rolled-back transaction never becomes observable to a
// concurrent [lpg.Graph.View] reader (#1282 for the in-memory undo, #1281 for
// the WAL rollback). It is shared by the drain-error and fsync-failure branches
// of commitUnderBarrier. The undo runs first so the secondary indexes are
// dropped only after the graph entries they describe are gone; the WAL
// transaction is rolled back last (it holds no in-memory state). [txn.Tx.Rollback]
// is idempotent against an already-finished transaction, so a tx whose
// CommitWALOnly already ran (and failed) is not double-released.
func (r *Result) rollbackUnderBarrier() {
	if r.undo != nil && !r.undo.replay() {
		r.undoErr = wrapUndoFailure(nil)
	}
	if r.buf != nil {
		r.buf.Rollback()
	}
	if r.tx != nil {
		_ = r.tx.Rollback() // release store mutex; in-memory state already restored
	}
	r.bufHandled = true
	r.walHandled = true
}

// Err returns the first error encountered during iteration, or nil.
//
// When a bounded-resource guard truncated the result during materialisation, Err
// returns the guard's sentinel: [ErrResultRowsExceeded] when the row cap
// ([EngineOptions.MaxResultRows]) was hit, or [ErrResultBytesExceeded] when the
// aggregate-byte budget ([EngineOptions.MaxResultBytes]) was hit. Either is
// matchable with [errors.Is]. For an autocommit write statement a tripped guard
// is a failed statement: its eager mutations were rolled back atomically inside
// the barrier and nothing was made durable (#1338).
//
// When the query was a write that failed and the subsequent in-memory undo
// replay ALSO failed (an inverse panicked, [ErrUndoFailed]), Err returns the
// pipeline error wrapped together with ErrUndoFailed so the caller learns both
// that the statement failed and that the rollback could not fully restore the
// graph; either is matchable with [errors.Is].
//
// When the in-barrier WAL fsync failed (#1281), Err returns that error. RunInTx
// already surfaces it directly and does not hand such a Result back, so this is
// a defensive backstop for any caller that nonetheless holds the Result: it
// reports that the write did not become durable (and was therefore rolled back).
func (r *Result) Err() error {
	if r.rowsErr != nil {
		return r.rowsErr
	}
	if r.walErr != nil {
		return r.walErr
	}
	pipeErr := r.rs.Err()
	if r.undoErr != nil {
		return wrapUndoFailure(pipeErr)
	}
	return pipeErr
}

// Columns returns the ordered list of output column names.
func (r *Result) Columns() []string { return r.cols }

// IsClosed reports whether Close has been called on this Result.
func (r *Result) IsClosed() bool { return r.closed.Load() }

// Close releases all resources held by the result set.
//
// For a write [Result] created by [Engine.RunInTx], the buffered index changes
// and the WAL transaction were already committed (durably, fsync first) or
// rolled back inside the write query's [lpg.Graph.ApplyAtomically] window
// (commitUnderBarrier, #1281). Close therefore only releases the underlying
// ResultSet for such a result — the durability and visibility decision is made
// and finalised before RunInTx returns, never deferred to Close. The
// commit/rollback branches below survive only as a fallback for a Result that
// reached Close without that in-barrier finalisation (e.g. one that was never
// materialised), preserving the historical contract for that path.
//
// Close is idempotent: a second invocation returns nil without re-entering
// the underlying ResultSet. The finalizer safety net also relies on this
// idempotence — see the type-level documentation.
func (r *Result) Close() error {
	if !r.closed.CompareAndSwap(false, true) {
		return nil
	}
	// Disarm the finalizer: we are about to release the resources ourselves
	// and there is no point in the GC calling us back later.
	runtime.SetFinalizer(r, nil)
	return r.closeLocked()
}

// closeLocked performs the actual resource release. Callers must hold the
// closed flag (set via CompareAndSwap by [Result.Close] or by the finalizer)
// to ensure exactly-once semantics across both call sites.
func (r *Result) closeLocked() error {
	err := r.rs.Close()
	if r.buf != nil && !r.bufHandled {
		// Fallback path: the index buffer was not flipped under the barrier
		// (e.g. a Result that was never materialised). Commit/roll back here as
		// before. Materialised write queries flip it inside ApplyAtomically via
		// commitUnderBarrier (bufHandled), so this branch is skipped. A tripped
		// bounded-resource guard (rowsErr) counts as a failure, mirroring
		// commitUnderBarrier (#1338): a truncated write must never commit.
		if err != nil || r.rs.Err() != nil || r.rowsErr != nil {
			r.buf.Rollback()
		} else {
			r.buf.Commit(r.idxMgr)
		}
	}
	if r.tx != nil && !r.walHandled {
		// Fallback path only: a materialised write query has already fsynced
		// (durable-then-visible) or rolled back its WAL transaction under the
		// barrier (walHandled, #1281), so this branch is skipped for it and the
		// WAL fsync never happens after visMu is released. It survives for a
		// Result that reached Close without in-barrier finalisation. As above,
		// rowsErr is failure: a partial transaction must not be made durable.
		if err != nil || r.rs.Err() != nil || r.rowsErr != nil {
			_ = r.tx.Rollback() // release store mutex; in-memory state already dirty
		} else {
			if werr := r.tx.CommitWALOnly(); werr != nil {
				err = werr
			}
		}
	}
	return err
}

// newResult wraps the construction of every Result returned by Run/RunInTx,
// installing the leak-detection finalizer on the freshly built value. The
// finalizer is the only safety net against callers that forget Close; it
// emits cypher.result.leaked and performs a best-effort release.
//
// When cols is empty the query has no RETURN clause and the caller cares
// only about side effects (CREATE/SET/DELETE/MERGE/REMOVE without a
// trailing projection). In that case newResult drains the underlying
// [exec.ResultSet] eagerly so the writes execute and the iterator becomes
// immediately exhausted — TCK-conformant write-only semantics.
func newResultWithLimit(rs *exec.ResultSet, cols []string, buf *exec.IndexBuffer, idxMgr *index.Manager, tx *txn.Tx[string, float64], maxRows, maxBytes int64) *Result {
	r := &Result{rs: rs, cols: cols, buf: buf, idxMgr: idxMgr, tx: tx, maxRows: maxRows, maxBytes: maxBytes}
	if len(cols) == 0 {
		for rs.Next() {
			// discard the row; write side effects execute as a side effect
		}
	}
	runtime.SetFinalizer(r, finalizeResult)
	return r
}

func newResult(rs *exec.ResultSet, cols []string, buf *exec.IndexBuffer, idxMgr *index.Manager, tx *txn.Tx[string, float64]) *Result {
	return newResultWithLimit(rs, cols, buf, idxMgr, tx, 0, 0)
}

// finalizeResult is the runtime.SetFinalizer callback invoked by the GC
// when an unclosed Result becomes unreachable. It increments the leak
// counter and runs the same close path Close() would, ignoring its error
// (the caller is no longer there to receive it). See [Result] for the full
// contract.
func finalizeResult(r *Result) {
	if !r.closed.CompareAndSwap(false, true) {
		return
	}
	cmetrics.IncCounter("cypher.result.leaked", 1)
	_ = r.closeLocked()
}

// ─────────────────────────────────────────────────────────────────────────────
// Graph adapters
// ─────────────────────────────────────────────────────────────────────────────

// lpgNodeWalker adapts *lpg.Graph[string, float64] to the exec.nodeWalker
// interface expected by [exec.AllNodesScan].
type lpgNodeWalker struct {
	g *lpg.Graph[string, float64]
}

// WalkNodeIDs implements nodeWalkerIface. Tombstoned node IDs (those
// removed via the GraphMutator's RemoveNode) are skipped so
// AllNodesScan, count(*), and downstream scans treat deleted nodes
// as absent.
func (w *lpgNodeWalker) WalkNodeIDs(fn func(graph.NodeID) bool) {
	w.g.AdjList().Mapper().Walk(func(id graph.NodeID, _ string) bool {
		if w.g.IsTombstoned(id) {
			return true // skip but continue iteration
		}
		return fn(id)
	})
}

// lpgLabelResolver adapts *lpg.Graph[string, float64] to the exec.labelResolver
// interface expected by [exec.NodeByLabelScan].
type lpgLabelResolver struct {
	g *lpg.Graph[string, float64]
}

// ResolveLabelBitmap implements exec.labelResolver.
func (s *lpgLabelResolver) ResolveLabelBitmap(name string) *roaring64.Bitmap {
	lid, ok := s.g.Registry().Lookup(name)
	if !ok {
		return roaring64.New()
	}
	return s.g.NodeIndex().Intersect(uint32(lid))
}

// ─────────────────────────────────────────────────────────────────────────────
// BuildPlan — IR → physical operator tree
// ─────────────────────────────────────────────────────────────────────────────

// nodeWalkerIface is the minimal interface needed from a node source.
type nodeWalkerIface interface {
	WalkNodeIDs(fn func(graph.NodeID) bool)
}

// labelResolverIface is the interface for label bitmap resolution, matching
// exec.labelResolver without importing the unexported type.
type labelResolverIface interface {
	ResolveLabelBitmap(name string) *roaring64.Bitmap
}

// BuildPlanWithMutator converts an IR [ir.LogicalPlan] tree into a physical
// [exec.Operator] tree, supporting both read and write IR operators. The
// mutator provides the write surface for CREATE, SET, REMOVE, DELETE, and
// MERGE operators.
//
// For read-only plans the behaviour is identical to [BuildPlan]; the mutator
// is only invoked when a write IR node is encountered.
func BuildPlanWithMutator(
	plan ir.LogicalPlan,
	walker nodeWalkerIface,
	labelSrc labelResolverIface,
	reg expr.FunctionRegistry,
	params map[string]expr.Value,
	mutator exec.GraphMutator,
) (op exec.Operator, cols []string, err error) {
	// The public entry point applies the finite default per-group element budget
	// (maxCollectItems == 0 → DefaultMaxCollectItems in buildEagerAggregation) so
	// a collect on this path is never unbounded either.
	return buildPlanWithMutatorFull(plan, walker, labelSrc, reg, params, mutator, nil, nil, 0)
}

// buildPlanWithMutatorFull is the engine-internal variant of
// BuildPlanWithMutator that also threads constraint enforcement through write
// operators. constraintReg and idxMgr may both be nil (no enforcement).
//
// maxCollectItems carries the Engine's per-group element budget for buffering
// aggregators into the write-path build, using the EngineOptions.MaxCollectItems
// encoding (0 → default, <0 → no cap, >0 → active).
func buildPlanWithMutatorFull(
	plan ir.LogicalPlan,
	walker nodeWalkerIface,
	labelSrc labelResolverIface,
	reg expr.FunctionRegistry,
	params map[string]expr.Value,
	mutator exec.GraphMutator,
	constraintReg *exec.ConstraintRegistry,
	idxMgr *index.Manager,
	maxCollectItems int,
) (op exec.Operator, cols []string, err error) {
	schema := make(map[string]int)
	argByTag := make(map[uint32]*exec.Argument)

	// bopts carries the writeFallback closure that lets read-side operator
	// builders (Projection / Selection / Sort / Limit / EagerAggregation
	// over a write subtree) recurse back into [buildOperatorWrite] when
	// they encounter a write IR node. Without this, the canonical lowering
	// of `CREATE (n) RETURN n.x` — ProduceResults → Projection → CreateNode
	// — falls through to [buildOperator]'s default branch and errors with
	// "unsupported IR node *ir.CreateNode".
	bopts := &buildOpts{maxCollectItems: maxCollectItems}
	bopts.writeFallback = func(child ir.LogicalPlan) (exec.Operator, error) {
		return buildOperatorWrite(child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
	}

	// When the IR root is a ProduceResults, use its declared columns; otherwise
	// treat the plan as a write-only query with no output columns. A CREATE
	// without RETURN has a write operator as root.
	if pr, ok := plan.(*ir.ProduceResults); ok {
		cols = pr.Columns
		child, buildErr := buildOperatorWrite(pr.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if buildErr != nil {
			return nil, nil, buildErr
		}
		items := make([]exec.ProjectionItem, len(cols))
		for i, col := range cols {
			if colIdx, exists := schema[col]; exists {
				idx := colIdx
				items[i] = exec.ProjectionItem{
					Alias: col,
					Eval: func(row exec.Row) (expr.Value, error) {
						if idx < len(row) {
							return row[idx], nil
						}
						return expr.Null, nil
					},
				}
			} else {
				items[i] = exec.ProjectionItem{
					Alias: col,
					Eval:  func(_ exec.Row) (expr.Value, error) { return expr.Null, nil },
				}
			}
		}
		proj, projErr := exec.NewProject(child, items)
		if projErr != nil {
			return nil, nil, fmt.Errorf("cypher: build final projection: %w", projErr)
		}
		return proj, cols, nil
	}

	// Write-only query (no RETURN clause): build the write operator tree
	// directly and wrap in a single pass-through projection so the result
	// set can be drained to trigger side effects.
	child, buildErr := buildOperatorWrite(plan, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
	if buildErr != nil {
		return nil, nil, buildErr
	}
	// Emit a single synthetic "__write__" column so the result set is non-empty
	// and can be drained. Callers that only care about side effects can ignore
	// the column.
	cols = nil // no output columns for write-only queries
	return child, cols, nil
}

// buildOperatorWrite extends buildOperator with handling for write IR nodes.
// When mutator is nil, write nodes fall through to the default error case.
// constraintReg and idxMgr are optional (nil means no enforcement).
//
// argByTag is forwarded to buildOperator for [*ir.Argument] resolution; pass
// nil when no Apply-family operator is in scope.
//
//nolint:gocyclo // large switch — one case per write IR node, no hidden branches
func buildOperatorWrite(
	plan ir.LogicalPlan,
	walker nodeWalkerIface,
	labelSrc labelResolverIface,
	reg expr.FunctionRegistry,
	params map[string]expr.Value,
	schema map[string]int,
	mutator exec.GraphMutator,
	constraintReg *exec.ConstraintRegistry,
	idxMgr *index.Manager,
	argByTag map[uint32]*exec.Argument,
	bopts *buildOpts,
) (exec.Operator, error) {
	if plan == nil {
		// A nil plan arises when a write clause has no driving subplan (e.g.
		// CREATE without a leading MATCH). Return a single-row operator that
		// drives the write operator exactly once.
		return exec.NewSingleRowOperator(), nil
	}

	switch p := plan.(type) {

	case *ir.CreateNode:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		// Snapshot schema before adding NodeVar: the propsExprFn must see the
		// input row layout (which does not include the newly created node).
		propsSchema := copySchema(schema)
		if p.NodeVar != "" {
			schema[p.NodeVar] = schemaWidth(schema)
		}
		cn, buildErr := exec.NewCreateNode(p.NodeVar, p.Labels, p.Properties, child, mutator)
		if buildErr != nil {
			return nil, buildErr
		}
		if len(params) > 0 {
			if cn, buildErr = cn.WithParams(params); buildErr != nil {
				return nil, buildErr
			}
		}
		if p.PropertiesExpr != nil {
			if ml, ok := p.PropertiesExpr.(*ast.MapLiteral); ok {
				if fn := buildPropsEvalFn(ml, propsSchema, params, reg, mutator, bopts); fn != nil {
					cn.WithPropsEvalFn(fn)
				}
			}
		}
		if constraintReg != nil {
			cn.WithConstraints(constraintReg, idxMgr)
		}
		return cn, nil

	case *ir.CreateRelationship:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		if p.RelVar != "" {
			schema[p.RelVar] = schemaWidth(schema)
		}
		// Pass a copy of schema so the operator captures the current state.
		schemaCopy := make(map[string]int, len(schema))
		for k, v := range schema {
			schemaCopy[k] = v
		}
		cr, buildErr := exec.NewCreateRelationship(p.StartVar, p.EndVar, p.RelVar, p.RelType, p.Properties, schemaCopy, child, mutator)
		if buildErr != nil {
			return nil, buildErr
		}
		if len(params) > 0 {
			if cr, buildErr = cr.WithParams(params); buildErr != nil {
				return nil, buildErr
			}
		}
		if p.PropertiesExpr != nil {
			if ml, ok := p.PropertiesExpr.(*ast.MapLiteral); ok {
				// Use schemaCopy (which captures relationship endpoints) for
				// property expression evaluation.
				relPropsSchema := copySchema(schemaCopy)
				if p.RelVar != "" {
					delete(relPropsSchema, p.RelVar)
				}
				if fn := buildPropsEvalFn(ml, relPropsSchema, params, reg, mutator, bopts); fn != nil {
					cr.WithPropsEvalFn(fn)
				}
			}
		}
		return cr, nil

	case *ir.SetProperty:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		sp, buildErr := exec.NewSetProperty(p.EntityVar, p.PropertyKey, p.Value, schemaCopy, child, mutator)
		if buildErr != nil {
			return nil, buildErr
		}
		if len(params) > 0 {
			sp.WithParams(params)
		}
		if constraintReg != nil {
			sp.WithConstraints(constraintReg, idxMgr)
		}
		if bopts != nil {
			if info, isRel := bopts.edgeVarMeta[p.EntityVar]; isRel {
				sp.WithRelCols(exec.RelCols{SrcCol: info.srcCol, DstCol: info.dstCol})
			}
		}
		// AST-eval path for non-literal RHS expressions (SET n.num = n.num + 1,
		// SET n.name = a.name, etc.). When the IR carries a parsed expression,
		// build a per-row closure that evaluates it and converts the result to
		// an lpg.PropertyValue. The closure short-circuits to ('no value') for
		// expr.Eval errors or unsupported value kinds so the operator skips
		// the write without aborting the pipeline.
		if p.ValueExpr != nil && p.PropertyKey != "" {
			schemaSnap := schemaCopy
			capturedExpr := p.ValueExpr
			capturedParams := params
			capturedReg := reg
			capturedBopts := bopts
			var capturedG *lpg.Graph[string, float64]
			if lw, ok := walker.(*lpgNodeWalker); ok {
				capturedG = lw.g
			}
			sp.WithValueEvalFn(func(row exec.Row) (lpg.PropertyValue, bool, bool, error) {
				rowCtx := buildRowCtx(row, schemaSnap, capturedG, capturedBopts)
				v, evalErr := evalRow(capturedBopts, capturedExpr, rowCtx, capturedParams, capturedReg)
				if evalErr != nil {
					return lpg.PropertyValue{}, false, false, nil // surface as no-op
				}
				if v == nil || expr.IsNull(v) {
					return lpg.PropertyValue{}, true, false, nil
				}
				// Reject property values whose shape is not storable: a Map,
				// a List containing a Map, or any deeper nesting of either.
				// Per openCypher these surface as InvalidPropertyType at
				// runtime (Set1 [10]).
				if !isStorableProperty(v) {
					return lpg.PropertyValue{}, false, false, fmt.Errorf("exec: SET %s: InvalidPropertyType: maps cannot be stored as property values", p.PropertyKey)
				}
				pv, ok := exprValueToLPGProp(v)
				if !ok {
					return lpg.PropertyValue{}, false, false, nil
				}
				return pv, false, true, nil
			})
		}
		return sp, nil

	case *ir.SetAllProperties:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		var sap *exec.SetAllProperties
		switch {
		case p.SourceVar != "":
			sap = exec.NewSetAllPropertiesFromEntity(p.EntityVar, p.SourceVar, p.IsReplace, schemaCopy, child, mutator)
		case p.ParamName != "":
			sap = exec.NewSetAllPropertiesFromParam(p.EntityVar, p.ParamName, p.IsReplace, schemaCopy, child, mutator)
		default:
			var bErr error
			sap, bErr = exec.NewSetAllPropertiesFromMap(p.EntityVar, p.MapLiteral, p.IsReplace, schemaCopy, child, mutator)
			if bErr != nil {
				return nil, bErr
			}
		}
		if len(params) > 0 {
			var pErr error
			sap, pErr = sap.WithParams(params)
			if pErr != nil {
				return nil, pErr
			}
		}
		if constraintReg != nil {
			sap.WithConstraints(constraintReg, idxMgr)
		}
		if bopts != nil {
			if info, isRel := bopts.edgeVarMeta[p.EntityVar]; isRel {
				sap.WithRelCols(exec.RelCols{SrcCol: info.srcCol, DstCol: info.dstCol})
			}
			if p.SourceVar != "" {
				if info, isRel := bopts.edgeVarMeta[p.SourceVar]; isRel {
					sap.WithSourceRelCols(exec.RelCols{SrcCol: info.srcCol, DstCol: info.dstCol})
				}
			}
		}
		return sap, nil

	case *ir.SetLabels:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewSetLabels(p.NodeVar, p.Labels, schemaCopy, child, mutator), nil

	case *ir.RemoveProperty:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		rp := exec.NewRemoveProperty(p.EntityVar, p.PropertyKey, schemaCopy, child, mutator)
		if bopts != nil {
			if info, isRel := bopts.edgeVarMeta[p.EntityVar]; isRel {
				rp.WithRelCols(exec.RelCols{SrcCol: info.srcCol, DstCol: info.dstCol})
			}
		}
		return rp, nil

	case *ir.RemoveLabels:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewRemoveLabels(p.NodeVar, p.Labels, schemaCopy, child, mutator), nil

	case *ir.DeleteNode:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		// Redirect: if the bare-variable target names a relationship variable
		// emitted by an upstream Expand, install an edge-ID → endpoint
		// lookup so DeleteNode's schema-direct path dispatches to the
		// edge-removal branch instead of treating the IntegerValue edge
		// id as a node id. Without this `DELETE r` would either fail
		// (ErrDeleteNodeHasRelationships when the colliding node still
		// has incident edges) or silently delete the wrong entity.
		var deleteRelEndpoints func(row exec.Row) (uint64, uint64, bool)
		if p.TargetExpr != nil {
			if v, isVar := p.TargetExpr.(*ast.Variable); isVar && bopts != nil && bopts.edgeVarMeta != nil {
				if meta, isRel := bopts.edgeVarMeta[v.Name]; isRel {
					srcCol, dstCol := meta.srcCol, meta.dstCol
					deleteRelEndpoints = func(row exec.Row) (uint64, uint64, bool) {
						if srcCol >= len(row) || dstCol >= len(row) {
							return 0, 0, false
						}
						srcID, srcOk := nodeIDOrNodeValue(row[srcCol])
						dstID, dstOk := nodeIDOrNodeValue(row[dstCol])
						if !srcOk || !dstOk {
							return 0, 0, false
						}
						return srcID, dstID, true
					}
				}
			}
		}
		dn := exec.NewDeleteNode(p.NodeVar, schemaCopy, child, mutator)
		if deleteRelEndpoints != nil {
			dn.WithRelEndpoints(deleteRelEndpoints)
		}
		if p.TargetExpr != nil {
			if _, isVar := p.TargetExpr.(*ast.Variable); !isVar {
				schemaSnap := schemaCopy
				capturedExpr := p.TargetExpr
				capturedParams := params
				capturedReg := reg
				capturedBopts := bopts
				var capturedG *lpg.Graph[string, float64]
				if lw, ok := walker.(*lpgNodeWalker); ok {
					capturedG = lw.g
				}
				dn.WithTargetEvalFn(func(row exec.Row) (expr.Value, error) {
					rowCtx := buildRowCtx(row, schemaSnap, capturedG, capturedBopts)
					return evalRow(capturedBopts, capturedExpr, rowCtx, capturedParams, capturedReg)
				})
			}
		}
		return dn, nil

	case *ir.DeleteRelationship:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewDeleteRelationship(p.RelVar, schemaCopy, child, mutator), nil

	case *ir.DetachDelete:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		dd := exec.NewDetachDelete(p.NodeVar, schemaCopy, child, mutator)
		needTargetEval := false
		if p.TargetExpr != nil {
			if _, isVar := p.TargetExpr.(*ast.Variable); !isVar {
				needTargetEval = true
			} else if bopts != nil {
				// Bare-Variable target that names a path: the row slot
				// carries the leading node id, not a PathValue, so the
				// schema-direct branch in DetachDelete would only
				// delete the leading node. Route through buildRowCtx
				// instead so the evaluator returns a reconstructed
				// PathValue that the operator walks node-by-node.
				if _, isChainPath := bopts.pathVarChain[p.NodeVar]; isChainPath {
					needTargetEval = true
				} else if _, isVLEPath := bopts.pathVarMeta[p.NodeVar]; isVLEPath {
					needTargetEval = true
				}
			}
		}
		if needTargetEval {
			schemaSnap := schemaCopy
			capturedExpr := p.TargetExpr
			capturedParams := params
			capturedReg := reg
			capturedBopts := bopts
			var capturedG *lpg.Graph[string, float64]
			if lw, ok := walker.(*lpgNodeWalker); ok {
				capturedG = lw.g
			}
			dd.WithTargetEvalFn(func(row exec.Row) (expr.Value, error) {
				rowCtx := buildRowCtx(row, schemaSnap, capturedG, capturedBopts)
				return evalRow(capturedBopts, capturedExpr, rowCtx, capturedParams, capturedReg)
			})
		}
		return dd, nil

	case *ir.MergeRelationship:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		srcCol, srcOk := schema[p.SrcVar]
		dstCol, dstOk := schema[p.DstVar]
		if !srcOk || !dstOk {
			return nil, fmt.Errorf("cypher: MergeRelationship: src=%q (in schema=%v) dst=%q (in schema=%v) unresolved",
				p.SrcVar, srcOk, p.DstVar, dstOk)
		}
		// openCypher rejects MERGE patterns whose property maps contain a
		// null literal — null comparisons are tri-valued false, so the
		// pattern can never match its own write and MergeReadOwnWrites is
		// the canonical compile-time/runtime error. The general Merge
		// branch above already enforces this; mirror the check here so
		// the single-relationship MergeRelationship fast path does not
		// silently accept `(a)-[r:X {num: null}]->(b)`. Closes
		// Merge5 [29].
		if p.RelProps != "" && exec.PropMapContainsNullLiteral(p.RelProps) {
			return nil, fmt.Errorf("cypher: SemanticError.MergeReadOwnWrites: MERGE pattern contains a null property literal")
		}
		op := exec.NewMergeRelationship(child, srcCol, dstCol, p.RelType, mutator)
		if p.RelProps != "" {
			op = op.WithRelProperties(p.RelProps)
		}
		if p.Undirected {
			op = op.WithUndirected(true)
		}
		// Allocate a schema column for the relationship variable so
		// downstream operators (RETURN r, count(r), …) see the bound
		// edge. Anonymous relationships still get a slot so a NamedPath
		// wrapper above can reconstruct the edge id when reconstructing
		// the path — but the slot must be reserved in the schema map so
		// subsequent operators do not allocate over it (Create3 [12]
		// regression: a CREATE following an anonymous-rel MERGE picked
		// the same column for its new node and read the
		// RelationshipValue back as the bound node).
		relCol := schemaWidth(schema)
		relKey := p.RelVar
		if relKey == "" {
			relKey = fmt.Sprintf("__anon_merge_rel_%d", relCol)
		}
		schema[relKey] = relCol
		op = op.WithRelColumn(relCol).WithSchema(copySchema(schema))
		// Register the (srcCol, edgeCol, dstCol) triplet so a NamedPath
		// wrapper above the MergeRelationship can reconstruct a PathValue
		// for `MERGE p = (a)-[:R]->(b) RETURN p`. Without this hook the
		// projection fast-path only sees the leading-node column and emits
		// a single-node path (Merge5 [10]).
		if bopts != nil {
			step := pathChainStep{srcCol: srcCol, edgeCol: relCol, dstCol: dstCol, edgeType: p.RelType}
			bopts.expandTripletSeq = append(bopts.expandTripletSeq, step)
		}
		if len(p.OnCreate) > 0 {
			actions := make([]exec.MergeRelAction, 0, len(p.OnCreate))
			for _, kv := range p.OnCreate {
				actions = append(actions, exec.MergeRelActionFromKV(kv.Key, kv.Value))
			}
			op = op.WithOnCreate(p.RelVar, actions)
		}
		if len(p.OnMatch) > 0 {
			actions := make([]exec.MergeRelAction, 0, len(p.OnMatch))
			for _, kv := range p.OnMatch {
				actions = append(actions, exec.MergeRelActionFromKV(kv.Key, kv.Value))
			}
			op = op.WithOnMatch(p.RelVar, actions)
		}
		return op, nil

	case *ir.Merge:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		// Extract labels and properties from the pattern string. For the
		// current IR the pattern is an opaque string; we surface the bound vars
		// as the output schema columns.
		for _, v := range p.BoundVars {
			if v != "" {
				schema[v] = schemaWidth(schema)
			}
		}
		schemaCopy := copySchema(schema)
		labels, props := parseNodePatternStr(p.Pattern)
		// MERGE with a null property literal can never match its own write —
		// null property comparisons are tri-valued false — so the spec
		// rejects the pattern as MergeReadOwnWrites at runtime. The check
		// scans the full pattern string so relationship-property maps in
		// shapes like (a)-[r:T {k: null}]->(b) are also caught.
		if exec.PropMapContainsNullLiteral(p.Pattern) {
			return nil, fmt.Errorf("cypher: SemanticError.MergeReadOwnWrites: MERGE pattern contains a null property literal")
		}
		// Real MERGE search (T930): scan the live graph for a node whose
		// labels are a superset of `labels` AND whose properties equal every
		// (key, value) parsed from `props`. When matches are found the ON
		// MATCH branch fires; only zero matches drives the ON CREATE branch.
		// Single-writer serialisation in the engine keeps concurrent MERGE
		// callers from racing to a phantom zero-match result.
		searchFn, sfErr := exec.NewMergeSearchFnFromPattern(labels, props, params, mutator)
		if sfErr != nil {
			return nil, sfErr
		}
		m, buildErr := exec.NewMerge(
			firstVar(p.BoundVars),
			labels,
			props,
			p.OnCreate, p.OnMatch,
			searchFn,
			schemaCopy,
			child,
			mutator,
		)
		if buildErr != nil {
			return nil, buildErr
		}
		if len(params) > 0 {
			if m, buildErr = m.WithParams(params); buildErr != nil {
				return nil, buildErr
			}
		}
		if constraintReg != nil {
			m.WithConstraints(constraintReg, idxMgr)
		}
		// Row-aware property map: when the IR carried a *ast.MapLiteral whose
		// values include non-literal expressions (variable references,
		// property accesses, parameter forms outside `$name`), install a
		// PropsEvalFn that resolves them per row. The closure draws its
		// schema from the snapshot taken right after the boundvars were
		// added (schemaCopy), which mirrors the row layout the Merge
		// operator sees at runtime.
		if ml, isMap := p.NodePropsAST.(*ast.MapLiteral); isMap && ml != nil {
			if mapLiteralHasNonLiteralValue(ml) {
				if fn := buildPropsEvalFn(ml, schemaCopy, params, reg, mutator, bopts); fn != nil {
					m.WithPropsEvalFn(fn)
				}
			}
		}
		return m, nil

	default:
		// Fall through to the read-operator builder.
		// procReg is nil here because buildOperatorWrite is only called from the
		// write path (buildPlanWithMutatorFull) which does not thread procReg.
		return buildOperator(plan, walker, labelSrc, reg, params, schema, idxMgr, nil, argByTag, bopts)
	}
}

// setSnap returns the set of keys present in m as a struct{} map. Used by
// the plain-Apply builder to remember which metadata entries existed
// before the inner-side build so newly-added entries can be offset by
// outerWidth post-merge.
func setSnap[V any](m map[string]V) map[string]struct{} {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

// copySchema returns a shallow copy of the schema map.
func copySchema(schema map[string]int) map[string]int {
	cp := make(map[string]int, len(schema))
	for k, v := range schema {
		cp[k] = v
	}
	return cp
}

// schemaWidth returns the actual row width implied by schema: the maximum
// column index present plus one. This is the correct "next available column
// index" to use when appending a new column to the row.
//
// Using len(schema) directly is incorrect when buildIRProjection has registered
// secondary expression-string keys (e.g. schema["[1,2,3]"] == schema["lst"] ==
// 0 for "WITH [1,2,3] AS lst"), because those secondary entries inflate
// len(schema) without adding real row columns.
func schemaWidth(schema map[string]int) int {
	maxIdx := -1
	for _, idx := range schema {
		if idx > maxIdx {
			maxIdx = idx
		}
	}
	return maxIdx + 1
}

// mapLiteralHasNonLiteralValue reports whether ml carries at least one value
// whose evaluation requires a row context — anything other than a primitive
// literal (Int / Float / String / Bool / Null) or a parameter reference
// (`$name`). Used by the MERGE physical builder to decide whether to install
// a per-row [exec.PropsEvalFn].
func mapLiteralHasNonLiteralValue(ml *ast.MapLiteral) bool {
	if ml == nil {
		return false
	}
	for _, v := range ml.Values {
		switch v.(type) {
		case *ast.IntLiteral, *ast.FloatLiteral, *ast.StringLiteral,
			*ast.BoolLiteral, *ast.NullLiteral, *ast.Parameter:
			continue
		}
		return true
	}
	return false
}

// buildPropsEvalFn constructs a [exec.PropsEvalFn] closure that evaluates the
// key→expression pairs in ml against each incoming row at runtime. It is used
// when the property map for a CreateNode or CreateRelationship contains
// non-literal values (variable references, property accesses, arithmetic) that
// cannot be resolved at plan-construction time.
//
// The closure:
//  1. Builds an [expr.RowContext] from the current row using the captured schema
//     and mutator (for upgrading IntegerValue(NodeID) → NodeValue with properties).
//  2. Calls [expr.Eval] on each value expression in ml.
//  3. Converts the resulting [expr.Value] to [lpg.PropertyValue]; entries that
//     evaluate to Null or to an unsupported type are silently omitted.
//
// A nil ml produces a nil closure (no-op).
func buildPropsEvalFn(
	ml *ast.MapLiteral,
	schemaCopy map[string]int,
	params map[string]expr.Value,
	reg expr.FunctionRegistry,
	mutator exec.GraphMutator,
	bopts *buildOpts,
) exec.PropsEvalFn {
	if ml == nil {
		return nil
	}
	// Snapshot keys and value expressions so the closure is self-contained.
	keys := make([]string, len(ml.Keys))
	copy(keys, ml.Keys)
	vals := make([]ast.Expression, len(ml.Values))
	copy(vals, ml.Values)
	// Snapshot the scalar-column set: any column that flows from an UNWIND
	// element variable or an aggregate output is numeric and must NOT be
	// upgraded to a NodeValue when it numerically coincides with an internal
	// node id. Without this guard a CREATE that consumes the unwound element
	// as a property value (e.g. `UNWIND range(0, 15) AS i CREATE ({count:
	// i})`) intermittently stores a NodeValue (which reads back as null) for
	// whichever element happens to match a freshly-allocated node id.
	var scalarSnap map[string]struct{}
	if bopts != nil {
		nScalar := len(bopts.scalarCols)
		nAlias := len(bopts.projAliasScalarCols)
		if nScalar+nAlias > 0 {
			scalarSnap = make(map[string]struct{}, nScalar+nAlias)
			for k := range bopts.scalarCols {
				scalarSnap[k] = struct{}{}
			}
			// projAliasScalarCols also covers integer-typed projection
			// aliases (e.g. `WITH foo.x AS x`) whose integer value can
			// numerically coincide with a real NodeID. Without folding
			// it into scalarSnap, the closure's buildRowCtxFromMutator
			// upgrades `x` to a NodeValue and the downstream property
			// evaluation (`{x: x}` in a MERGE pattern) drops the entry
			// because exprValueToLPGProp rejects NodeValue — leaving
			// the MERGE search predicate under-filtered and matching
			// every :N node regardless of `x`. Closes Merge1 [9] flake.
			for k := range bopts.projAliasScalarCols {
				scalarSnap[k] = struct{}{}
			}
		}
	}

	return func(row exec.Row) []exec.PropEntry {
		// Build a RowContext that can resolve variable bindings and node
		// property accesses from the current row.
		rowCtx := buildRowCtxFromMutator(row, schemaCopy, mutator, scalarSnap)

		var out []exec.PropEntry
		for i, k := range keys {
			v, evalErr := expr.Eval(vals[i], rowCtx, params, reg)
			if evalErr != nil || v == nil {
				continue // expression error or nil: skip
			}
			if expr.IsNull(v) {
				continue // openCypher: assigning null to a property is a no-op
			}
			pv, ok := exprValueToLPGProp(v)
			if !ok {
				continue // unsupported type (e.g. NodeValue): skip
			}
			out = append(out, exec.PropEntry{Key: k, Value: pv})
		}
		return out
	}
}

// buildRowCtxFromMutator builds an [expr.RowContext] from a row using the
// captured schema. For each column that holds an IntegerValue, it attempts to
// resolve the corresponding node from the mutator and upgrade it to a NodeValue
// carrying properties — enabling property-access expressions like `a.id` when
// `a` is a bound node variable in the row.
//
// When no mutator is available, or when the integer cannot be resolved to a
// node, the raw IntegerValue is kept.
//
// scalarCols, when non-nil, lists variable names whose row values must pass
// through unchanged: UNWIND element variables and EagerAggregation outputs
// are scalar by construction and may numerically coincide with internal node
// ids — upgrading them would silently corrupt downstream CREATE/SET property
// writes.
func buildRowCtxFromMutator(row exec.Row, schema map[string]int, mutator exec.GraphMutator, scalarCols map[string]struct{}) expr.RowContext {
	ctx := make(expr.RowContext, len(schema))
	for varName, colIdx := range schema {
		if colIdx >= len(row) || row[colIdx] == nil {
			continue
		}
		v := row[colIdx]
		if scalarCols != nil {
			if _, isScalar := scalarCols[varName]; isScalar {
				ctx[varName] = v
				continue
			}
		}
		if mutator != nil {
			if iv, ok := v.(expr.IntegerValue); ok {
				nodeID := graph.NodeID(iv)
				if key, resolved := mutator.ResolveNodeLabel(nodeID); resolved {
					rawProps := mutator.NodeProperties(key)
					props := make(expr.MapValue, len(rawProps))
					for k, pv := range rawProps {
						props[k] = lpgPropToExpr(pv)
					}
					labels := mutator.NodeLabels(key)
					ctx[varName] = expr.NodeValue{
						ID:         uint64(nodeID),
						Labels:     labels,
						Properties: props,
					}
					continue
				}
			}
		}
		ctx[varName] = v
	}
	return ctx
}

// exprValueToLPGProp converts an [expr.Value] to an [lpg.PropertyValue].
// Returns (zero, false) when the value type has no natural property encoding
// (e.g. NodeValue, RelationshipValue, PathValue).
//
// Temporal values (Date, LocalDateTime, DateTime, LocalTime, Time,
// Duration) are encoded as PropString with a SOH-range tag byte
// (0x01..0x06) followed by the canonical openCypher textual form, the
// same scheme used by [cypher.exec.parseTemporalLiteral] on the
// literal-string write path. The decoder is [decodeTemporalString].
// isStorableProperty reports whether v can be stored as a node or
// relationship property value. Maps and lists-containing-maps are
// rejected; openCypher classifies an attempt to set such a value as
// InvalidPropertyType at runtime.
func isStorableProperty(v expr.Value) bool {
	switch val := v.(type) {
	case expr.MapValue:
		return false
	case expr.ListValue:
		for _, el := range val {
			if !isStorableProperty(el) {
				return false
			}
		}
	}
	return true
}

func exprValueToLPGProp(v expr.Value) (lpg.PropertyValue, bool) {
	switch val := v.(type) {
	case expr.StringValue:
		return lpg.StringValue(string(val)), true
	case expr.IntegerValue:
		return lpg.Int64Value(int64(val)), true
	case expr.FloatValue:
		return lpg.Float64Value(float64(val)), true
	case expr.BoolValue:
		return lpg.BoolValue(bool(val)), true
	case expr.DateValue:
		return lpg.StringValue("\x01" + val.String()), true
	case expr.LocalDateTimeValue:
		return lpg.StringValue("\x02" + val.String()), true
	case expr.DateTimeValue:
		return lpg.StringValue("\x03" + val.String()), true
	case expr.LocalTimeValue:
		return lpg.StringValue("\x04" + val.String()), true
	case expr.TimeValue:
		return lpg.StringValue("\x05" + val.String()), true
	case expr.DurationValue:
		return lpg.StringValue("\x06" + val.String()), true
	case expr.ListValue:
		elems := make([]lpg.PropertyValue, 0, len(val))
		for _, el := range val {
			pv, ok := exprValueToLPGProp(el)
			if !ok {
				continue
			}
			elems = append(elems, pv)
		}
		return lpg.ListValue(elems), true
	default:
		return lpg.PropertyValue{}, false
	}
}

// firstVar returns the first non-empty string from vars, or empty string.
func firstVar(vars []string) string {
	for _, v := range vars {
		if v != "" {
			return v
		}
	}
	return ""
}

// parseNodePatternStr extracts labels and property string from an opaque IR
// node-pattern string of the form "(var:Label1:Label2 {key:'val',...})".
// Both outputs may be empty when the pattern is absent or unparseable.
//
// When the source pattern is a relationship MERGE such as
// "(a)-[r:T {k:'v'}]->(b)", the head-node prefix has no labels or
// properties, the leading "(...)" closes before the relationship body,
// and the embedded "{...}" belongs to the relationship — not to the
// head node. Both cases are handled defensively: only the head "(...)"
// segment is scanned, and the property map is extracted by a
// brace-balanced cursor so trailing pattern syntax never bleeds into
// the props string.
func parseNodePatternStr(pattern string) (labels []string, props string) {
	s := strings.TrimSpace(pattern)
	s = leadingParenSegment(s)
	if len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		s = s[1 : len(s)-1]
	}
	s, props = extractBracedSuffix(s)

	// Remaining: "var:Label1:Label2" — split on ':' and discard first token (var).
	parts := strings.Split(s, ":")
	for _, p := range parts[1:] {
		lbl := strings.TrimSpace(p)
		if lbl != "" {
			labels = append(labels, lbl)
		}
	}
	return
}

// leadingParenSegment returns the leading "(...)" segment of s using
// paren-depth tracking so a relationship pattern "(a)-[...]->(b)"
// resolves to "(a)" instead of consuming the entire string. Strings
// that do not start with '(' or have unbalanced parens are returned
// unchanged.
func leadingParenSegment(s string) string {
	if s == "" || s[0] != '(' {
		return s
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[:i+1]
			}
		}
	}
	return s
}

// extractBracedSuffix splits the first balanced "{...}" suffix out of
// s. Returns the head (everything before the opening brace) and the
// balanced "{...}" substring. When s contains no '{', or the braces
// are unbalanced, the head is s unchanged and props is empty.
func extractBracedSuffix(s string) (head, props string) {
	braceIdx := strings.IndexByte(s, '{')
	if braceIdx < 0 {
		return s, ""
	}
	depth := 0
	end := -1
	for i := braceIdx; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end > braceIdx {
		props = strings.TrimSpace(s[braceIdx : end+1])
	}
	return s[:braceIdx], props
}

// BuildPlan converts an IR [ir.LogicalPlan] tree into a physical [exec.Operator]
// tree together with the ordered output column names.
//
// walker provides node enumeration; labelSrc provides label-filtered scans;
// reg provides the built-in function registry; params are the query parameters.
//
// Sprint 25 support matrix:
//   - [ir.AllNodesScan]
//   - [ir.NodeByLabelScan]
//   - [ir.Selection] (predicate is an always-true stub)
//   - [ir.Projection]
//   - [ir.ProduceResults] (required as root)
//   - [ir.Expand] (stub; child rows pass through, rel/dst vars bound to NULL)
func BuildPlan(
	plan ir.LogicalPlan,
	walker nodeWalkerIface,
	labelSrc labelResolverIface,
	reg expr.FunctionRegistry,
	params map[string]expr.Value,
) (op exec.Operator, cols []string, err error) {
	pr, ok := plan.(*ir.ProduceResults)
	if !ok {
		return nil, nil, fmt.Errorf("cypher: plan root must be ProduceResults, got %T", plan)
	}
	cols = pr.Columns

	// schema maps variable name → column index in the current row.
	schema := make(map[string]int)
	argByTag := make(map[uint32]*exec.Argument)
	child, err := buildOperator(pr.Child, walker, labelSrc, reg, params, schema, nil, nil, argByTag, nil)
	if err != nil {
		return nil, nil, err
	}

	// Wrap in a final projection that maps the schema to the output column order.
	items := make([]exec.ProjectionItem, len(cols))
	for i, col := range cols {
		if colIdx, exists := schema[col]; exists {
			idx := colIdx
			items[i] = exec.ProjectionItem{
				Alias: col,
				Eval: func(row exec.Row) (expr.Value, error) {
					if idx < len(row) {
						return row[idx], nil
					}
					return expr.Null, nil
				},
			}
		} else {
			items[i] = exec.ProjectionItem{
				Alias: col,
				Eval:  func(_ exec.Row) (expr.Value, error) { return expr.Null, nil },
			}
		}
	}

	proj, err := exec.NewProject(child, items)
	if err != nil {
		return nil, nil, fmt.Errorf("cypher: build final projection: %w", err)
	}
	return proj, cols, nil
}

// buildPlanEngine is the Engine-internal variant of BuildPlan that threads the
// index manager and procedure registry through so that NodeByIndexSeek and
// ProcedureCall IR nodes can be resolved at build time. idxMgr and procReg
// may both be nil. bopts carries the query-scope SubqueryEvaluator and
// queryCtx for [ast.ExistsSubquery] / [ast.CountSubquery] expressions; pass
// nil when no subquery support is needed.
func buildPlanEngine(
	plan ir.LogicalPlan,
	walker nodeWalkerIface,
	labelSrc labelResolverIface,
	reg expr.FunctionRegistry,
	params map[string]expr.Value,
	idxMgr *index.Manager,
	procReg *procs.Registry,
	bopts *buildOpts,
) (op exec.Operator, cols []string, err error) {
	// Standalone CALL (root is *ir.ProcedureCall): treat YieldVars as columns.
	// When YieldVars is empty (no explicit YIELD), openCypher specifies the
	// procedure's declared output columns become the result columns.
	if p, ok := plan.(*ir.ProcedureCall); ok {
		schema := make(map[string]int)
		argByTag := make(map[uint32]*exec.Argument)
		child, buildErr := buildOperator(p, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if buildErr != nil {
			return nil, nil, buildErr
		}
		outCols := p.YieldVars
		if len(outCols) == 0 && procReg != nil {
			if entry, lookupErr := procReg.Lookup(p.Namespace, p.Name); lookupErr == nil {
				outCols = make([]string, len(entry.Sig.Outputs))
				for i, out := range entry.Sig.Outputs {
					outCols[i] = out.Name
				}
			}
		}
		return child, outCols, nil
	}

	// UNION / UNION ALL: each branch is itself a top-level plan (typically
	// a ProduceResults). Recursively build each side, then concatenate via
	// exec.NewUnionAll (preserves duplicates) or exec.NewUnion (with a
	// Distinct cap to deduplicate). The left branch's column names are
	// returned as the union's output schema — openCypher requires every
	// branch of a UNION to expose the same column names in the same order.
	if u, ok := plan.(*ir.UnionAll); ok {
		leftOp, leftCols, lerr := buildPlanEngine(u.Left, walker, labelSrc, reg, params, idxMgr, procReg, bopts)
		if lerr != nil {
			return nil, nil, lerr
		}
		rightOp, _, rerr := buildPlanEngine(u.Right, walker, labelSrc, reg, params, idxMgr, procReg, bopts)
		if rerr != nil {
			return nil, nil, rerr
		}
		return exec.NewUnionAll(leftOp, rightOp), leftCols, nil
	}
	if u, ok := plan.(*ir.Union); ok {
		leftOp, leftCols, lerr := buildPlanEngine(u.Left, walker, labelSrc, reg, params, idxMgr, procReg, bopts)
		if lerr != nil {
			return nil, nil, lerr
		}
		rightOp, _, rerr := buildPlanEngine(u.Right, walker, labelSrc, reg, params, idxMgr, procReg, bopts)
		if rerr != nil {
			return nil, nil, rerr
		}
		return exec.NewUnion(leftOp, rightOp, 0), leftCols, nil
	}

	pr, ok := plan.(*ir.ProduceResults)
	if !ok {
		return nil, nil, fmt.Errorf("cypher: plan root must be ProduceResults, got %T", plan)
	}
	cols = pr.Columns
	schema := make(map[string]int)
	argByTag := make(map[uint32]*exec.Argument)
	child, err := buildOperator(pr.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
	if err != nil {
		return nil, nil, err
	}
	items := make([]exec.ProjectionItem, len(cols))
	for i, col := range cols {
		if colIdx, exists := schema[col]; exists {
			idx := colIdx
			items[i] = exec.ProjectionItem{
				Alias: col,
				Eval: func(row exec.Row) (expr.Value, error) {
					if idx < len(row) {
						return row[idx], nil
					}
					return expr.Null, nil
				},
			}
		} else {
			items[i] = exec.ProjectionItem{
				Alias: col,
				Eval:  func(_ exec.Row) (expr.Value, error) { return expr.Null, nil },
			}
		}
	}
	proj, err := exec.NewProject(child, items)
	if err != nil {
		return nil, nil, fmt.Errorf("cypher: build final projection: %w", err)
	}
	return proj, cols, nil
}

// buildOperator recursively converts an IR plan node to a physical operator.
// schema accumulates variable→column-index bindings as operators are visited
// top-down (left-to-right for children). idxMgr and procReg may both be nil.
//
// argByTag routes [*ir.Argument] nodes to a specific [*exec.Argument] instance
// pre-allocated by an enclosing Apply-family operator (CorrelatedApply or
// OptionalApply). It may be nil when no Apply is in scope; missing tags fall
// back to a fresh exec.Argument so isolated Argument nodes still work.
//
//nolint:gocyclo // large switch — one case per read IR node type; no hidden branches
func buildOperator(
	plan ir.LogicalPlan,
	walker nodeWalkerIface,
	labelSrc labelResolverIface,
	reg expr.FunctionRegistry,
	params map[string]expr.Value,
	schema map[string]int,
	idxMgr *index.Manager,
	procReg *procs.Registry,
	argByTag map[uint32]*exec.Argument,
	bopts *buildOpts,
) (exec.Operator, error) {
	if plan == nil {
		// A nil read plan drives a single-row empty operator (e.g. bare RETURN
		// without a preceding MATCH).
		return exec.NewSingleRowOperator(), nil
	}
	switch p := plan.(type) {

	case *ir.AllNodesScan:
		schema[p.NodeVar] = schemaWidth(schema)
		return exec.NewAllNodesScan(walker), nil

	case *ir.NodeByLabelScan:
		schema[p.NodeVar] = schemaWidth(schema)
		// Adapt lpgLabelResolver to exec.labelResolver; both have the same
		// method signature so a direct wrapper suffices.
		src := &execLabelAdapter{labelSrc: labelSrc}
		return exec.NewNodeByLabelScan(p.Label, src), nil

	case *ir.NodeByIndexSeek:
		return buildIndexSeekOperator(p, params, schema, idxMgr)

	case *ir.Selection:
		// Opportunistic index-seek rewrite: if the predicate is a simple equality
		// n.prop = $name and a hash index is available, produce NodeByIndexSeek
		// directly without first building the scan child.
		if idxMgr != nil {
			if op, ok, err := tryBuildIndexSeekFromSelection(p, params, schema, idxMgr); err != nil {
				return nil, err
			} else if ok {
				return op, nil
			}
		}
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		if p.PredicateExpr != nil {
			var selG *lpg.Graph[string, float64]
			if lw, ok := walker.(*lpgNodeWalker); ok {
				selG = lw.g
			}
			if selG != nil {
				schemaSnap := copySchema(schema)
				predExpr := p.PredicateExpr
				capturedG := selG
				capturedParams := params
				capturedReg := reg
				capturedBopts := bopts
				return exec.NewFilter(child, func(row exec.Row) (expr.Value, error) {
					rowCtx := buildRowCtx(row, schemaSnap, capturedG, capturedBopts)
					return evalRow(capturedBopts, predExpr, rowCtx, capturedParams, capturedReg)
				}), nil
			}
		}
		// Fallback: no AST or no graph available — pass-through filter.
		return exec.NewFilter(child, func(_ exec.Row) (expr.Value, error) {
			return expr.BoolValue(true), nil
		}), nil

	case *ir.Projection:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		var projG *lpg.Graph[string, float64]
		if lw, ok := walker.(*lpgNodeWalker); ok {
			projG = lw.g
		}
		return buildIRProjection(p.Items, child, schema, projG, params, reg, bopts)

	case *ir.Expand:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}

		// Record the column index of the source var BEFORE we add new columns so
		// that inputCol correctly points at the source node in the child row.
		fromCol := 0
		if col, ok := schema[p.FromVar]; ok {
			fromCol = col
		}

		// Expand.buildRow emits: inputRow... || srcID || edgeID || dstID
		// The srcID duplicate occupies one slot with no variable bound (the source
		// is already in the schema via FromVar); RelVar maps to the edgeID slot;
		// ToVar maps to the dstID slot.
		//
		// We advance the schema counter by 3 for the (srcID, edgeID, dstID)
		// triplet emitted by Expand. Each slot gets a stable key in the schema
		// map even when no user-visible variable is bound, so schemaWidth(schema)
		// matches the actual row width — critical for downstream operators (e.g.
		// a sibling AllNodesScan inside an Apply) that allocate schema slots
		// based on the row width.
		schemaBase := schemaWidth(schema)
		schema[p.FromVar+"__dup"] = schemaBase // srcID dup — anonymous internal slot
		relKey := p.RelVar
		if relKey == "" {
			relKey = fmt.Sprintf("__anon_rel_%d", schemaBase+1)
		}
		schema[relKey] = schemaBase + 1
		toKey := p.ToVar
		if toKey == "" {
			toKey = fmt.Sprintf("__anon_to_%d", schemaBase+2)
		}
		schema[toKey] = schemaBase + 2

		// Record the triplet in chain order so a *ir.NamedPath wrapper above
		// this subtree can map its IR chain elements to the slots emitted by
		// this Expand. Done for both named and anonymous relationships — the
		// chain may include either.
		if bopts != nil {
			step := pathChainStep{
				srcCol:  schemaBase,
				edgeCol: schemaBase + 1,
				dstCol:  schemaBase + 2,
			}
			if len(p.RelTypes) > 0 {
				step.edgeType = p.RelTypes[0]
			}
			bopts.expandTripletSeq = append(bopts.expandTripletSeq, step)
			// When this Expand participates in a named path that also
			// has a VLE (set by the IR's setPathVarOnVLE tagging),
			// record the triplet as a leading hop in pathVarMeta so
			// buildPathValueFromVLEMeta can prepend it to the
			// VLE-emitted node list (Match6 [14]).
			if p.PathVar != "" {
				if bopts.pathVarMeta == nil {
					bopts.pathVarMeta = make(map[string]pathVarInfo)
				}
				info := bopts.pathVarMeta[p.PathVar]
				info.leadingSteps = append(info.leadingSteps, step)
				bopts.pathVarMeta[p.PathVar] = info
			}
		}

		// Record edge variable metadata so buildIRProjection can reconstruct
		// a RelationshipValue when the variable is projected directly.
		if p.RelVar != "" && bopts != nil {
			if bopts.edgeVarMeta == nil {
				bopts.edgeVarMeta = make(map[string]edgeVarInfo)
			}
			info := edgeVarInfo{
				srcCol:  schemaBase,     // srcID dup column
				edgeCol: schemaBase + 1, // edgeID column (= schema[relKey])
				dstCol:  schemaBase + 2, // dstID column  (= schema[toKey])
			}
			if len(p.RelTypes) > 0 {
				info.edgeType = p.RelTypes[0]
				info.acceptedTypes = append([]string(nil), p.RelTypes...)
			}
			bopts.edgeVarMeta[p.RelVar] = info
		}

		var g *lpg.Graph[string, float64]
		if lw, ok := walker.(*lpgNodeWalker); ok {
			g = lw.g
		}
		if g == nil {
			// No graph available (e.g. schema-only planning) — return child with
			// NULL columns so downstream projections can reference the vars.
			return child, nil
		}

		fwd, rev := csrPairFromGraph(g)
		dir := irDirToExec(p.Direction)

		cfg := exec.ExpandConfig{
			Direction: dir,
			InputCol:  fromCol,
		}
		if len(p.RelTypes) > 0 {
			cfg.EdgeType = p.RelTypes[0]
			cfg.EdgeTypeFilter = buildEdgeTypeFilter(g, p.RelTypes)
		}
		// Cyphermorphism: pass the schema columns of every sibling
		// relationship variable already bound in this MATCH pattern so
		// the Expand operator excludes those edges from its emissions.
		// Resolving names here keeps the IR free of schema-index
		// concerns; SiblingRelVars carries the names attached by
		// matchPathPattern when the chain was built.
		for _, name := range p.SiblingRelVars {
			if col, ok := schema[name]; ok {
				cfg.RelCols = append(cfg.RelCols, col)
			}
		}
		return exec.NewExpand(child, fwd, rev, cfg), nil

	case *ir.Apply:
		// Build the outer plan first so its vars enter the schema.
		outer, err := buildOperator(p.Outer, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		// The inner subtree of a plain (non-correlated) Apply runs in
		// isolation — it does not consume the outer row, so inner rows are
		// inner-columns-only. Build the inner with a FRESH schema so its
		// operators index columns from 0. After the build, merge the inner
		// schema back into the shared schema with each column offset by the
		// outer's width so the post-Apply combined row layout (outer||inner)
		// stays addressable downstream.
		outerWidth := schemaWidth(schema)
		innerSchema := map[string]int{}
		// Snapshot bopts state that records inner-relative column positions
		// (edgeVarMeta / pathVarMeta / vleRelMeta / pathVarChain /
		// expandTripletSeq). The inner build adds new entries indexed
		// against innerSchema's 0-based positions; once the inner schema
		// is merged with the outer offset those entries become stale, so
		// we shift every metadata column by outerWidth post-merge. Closes
		// Match8 [3] (`MATCH ()-->() WITH 1 AS x MATCH ()-[r1]->()<--()
		// RETURN sum(r1.times)` returned NULL because edgeVarMeta[r1]
		// still pointed at the inner-only triplet positions, which after
		// the outer-side offset belonged to outer-or-other columns).
		var preEdgeKeys, prePathChainKeys, prePathMetaKeys, preVLEKeys map[string]struct{}
		var preTripletLen int
		if bopts != nil {
			preEdgeKeys = setSnap(bopts.edgeVarMeta)
			prePathChainKeys = setSnap(bopts.pathVarChain)
			prePathMetaKeys = setSnap(bopts.pathVarMeta)
			preVLEKeys = setSnap(bopts.vleRelMeta)
			preTripletLen = len(bopts.expandTripletSeq)
		}
		arg := exec.NewArgument()
		inner, err := buildOperator(p.Inner, walker, labelSrc, reg, params, innerSchema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		for k, v := range innerSchema {
			schema[k] = v + outerWidth
		}
		if bopts != nil {
			for name, info := range bopts.edgeVarMeta {
				if _, was := preEdgeKeys[name]; was {
					continue
				}
				info.srcCol += outerWidth
				info.edgeCol += outerWidth
				info.dstCol += outerWidth
				bopts.edgeVarMeta[name] = info
			}
			for name, info := range bopts.pathVarChain {
				if _, was := prePathChainKeys[name]; was {
					continue
				}
				info.leadingCol += outerWidth
				for i := range info.steps {
					info.steps[i].srcCol += outerWidth
					info.steps[i].edgeCol += outerWidth
					info.steps[i].dstCol += outerWidth
				}
				bopts.pathVarChain[name] = info
			}
			for name, info := range bopts.pathVarMeta {
				if _, was := prePathMetaKeys[name]; was {
					continue
				}
				info.listCol += outerWidth
				bopts.pathVarMeta[name] = info
			}
			for name, info := range bopts.vleRelMeta {
				if _, was := preVLEKeys[name]; was {
					continue
				}
				info.listCol += outerWidth
				bopts.vleRelMeta[name] = info
			}
			for i := preTripletLen; i < len(bopts.expandTripletSeq); i++ {
				bopts.expandTripletSeq[i].srcCol += outerWidth
				bopts.expandTripletSeq[i].edgeCol += outerWidth
				bopts.expandTripletSeq[i].dstCol += outerWidth
			}
		}
		return exec.NewApply(outer, inner, arg), nil

	case *ir.CorrelatedApply:
		// Build outer first; its vars enter the schema and define the outer width.
		outer, err := buildOperator(p.Outer, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		// Pre-allocate the exec.Argument and register it under the IR Argument's
		// tag so the inner subtree's matching Argument leaf resolves to this
		// instance.
		arg := exec.NewArgument()
		if argByTag != nil {
			argByTag[p.ArgTag] = arg
		}
		inner, err := buildOperator(p.Inner, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		if argByTag != nil {
			delete(argByTag, p.ArgTag)
		}
		return exec.NewCorrelatedApply(outer, inner, arg), nil

	case *ir.OptionalApply:
		// Build outer first; record its width so the NULL-extension row has the
		// correct padding when the inner pipeline emits zero rows.
		outer, err := buildOperator(p.Outer, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		outerWidth := schemaWidth(schema)
		arg := exec.NewArgument()
		if argByTag != nil {
			argByTag[p.ArgTag] = arg
		}
		inner, err := buildOperator(p.Inner, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		if argByTag != nil {
			delete(argByTag, p.ArgTag)
		}
		// paddedWidth = outerWidth + (inner-added columns). schemaWidth captures
		// the real row width (max index + 1) so secondary expression-string keys
		// (e.g. schema["date({…})"] sharing schema["x"]'s slot) do not inflate
		// the count.
		paddedWidth := schemaWidth(schema)
		if paddedWidth < outerWidth {
			paddedWidth = outerWidth
		}
		return exec.NewOptionalApply(outer, inner, arg, paddedWidth), nil

	case *ir.SemiApply:
		outer, err := buildOperator(p.Outer, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		// Pre-allocate the exec.Argument and register it under the IR
		// SemiApply's ArgTag so the inner subtree's matching Argument leaf
		// resolves to this instance and receives the outer row per iteration.
		arg := exec.NewArgument()
		if argByTag != nil {
			argByTag[p.ArgTag] = arg
		}
		inner, err := buildOperator(p.Inner, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		if argByTag != nil {
			delete(argByTag, p.ArgTag)
		}
		return exec.NewSemiApply(outer, inner, arg), nil

	case *ir.AntiSemiApply:
		outer, err := buildOperator(p.Outer, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		arg := exec.NewArgument()
		if argByTag != nil {
			argByTag[p.ArgTag] = arg
		}
		inner, err := buildOperator(p.Inner, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		if argByTag != nil {
			delete(argByTag, p.ArgTag)
		}
		return exec.NewAntiSemiApply(outer, inner, arg), nil

	case *ir.RollUpApply:
		// Pattern-comprehension execution. Build outer first; its
		// columns enter the schema and define the outer width that
		// the RollUpApply output preserves.
		outer, err := buildOperator(p.Outer, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		// Snapshot the outer schema verbatim so we can restore it after
		// the inner build. The inner subplan ends with a Projection that
		// wipes-and-rewrites the shared schema map (buildIRProjection's
		// post-projection schema reset) — any outer entry at idx < outerWidth
		// would otherwise be lost or overwritten by an inner-only column
		// name. Without this snapshot, downstream lookups for outer
		// variables (n, b, …) miss the schema and return NULL.
		outerSchemaSnap := copySchema(schema)
		outerWidth := schemaWidth(schema)
		// Pre-allocate the exec.Argument and register it under the IR
		// RollUpApply's ArgTag so the inner subtree's matching
		// Argument leaf resolves to this instance and receives the
		// outer row per iteration.
		arg := exec.NewArgument()
		if argByTag != nil {
			argByTag[p.ArgTag] = arg
		}
		inner, err := buildOperator(p.Inner, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		if argByTag != nil {
			delete(argByTag, p.ArgTag)
		}
		// The RollUpApply output is (outer columns…, collected list).
		// Restore the outer schema verbatim, then register CollectVar at
		// outerWidth so the final-projection lookup resolves to the list
		// column. Inner-built variables (and any names the inner Projection
		// rebound to outer slots) are dropped — only outer columns and the
		// collected list survive downstream.
		for k := range schema {
			delete(schema, k)
		}
		for k, v := range outerSchemaSnap {
			schema[k] = v
		}
		schema[p.CollectVar] = outerWidth
		// listEval is left nil — the inner subplan ends with a
		// Projection that puts the comprehension's projected value at
		// the row's first column, which is the default collection
		// extractor. The collected list shares the buffering-aggregator
		// element budget (EngineOptions.MaxCollectItems) so a pattern
		// comprehension over a supernode anchor cannot build an unbounded
		// in-memory list — the same bound collect() enforces (#1294, #1298).
		return exec.NewRollUpApplyN(outer, inner, arg, nil, rollUpItemsFromBopts(bopts)), nil

	case *ir.Argument:
		// Argument is the leaf of an Apply-family inner plan. At runtime the exec
		// Argument operator re-emits the outer row that was injected by the Apply
		// loop. The IR vars are already in schema from the outer build; no new
		// column registrations are needed here.
		//
		// When p.Tag is registered in argByTag (by an enclosing CorrelatedApply
		// or OptionalApply), reuse the same exec.Argument instance so the Apply
		// driver and the leaf share state. Otherwise allocate a fresh, unbound
		// Argument for standalone use.
		if argByTag != nil {
			if a, ok := argByTag[p.Tag]; ok {
				return a, nil
			}
		}
		return exec.NewArgument(), nil

	case *ir.EagerAggregation:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		var aggG *lpg.Graph[string, float64]
		if lw, ok := walker.(*lpgNodeWalker); ok {
			aggG = lw.g
		}
		return buildEagerAggregation(p, child, schema, aggG, params, reg, bopts)

	case *ir.Sort:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		var sortG *lpg.Graph[string, float64]
		if lw, ok := walker.(*lpgNodeWalker); ok {
			sortG = lw.g
		}
		keys := irSortKeys(p.SortItems, schema, sortG, params, reg, bopts)
		if len(keys) == 0 {
			// No resolvable sort keys — pass through without sorting.
			return child, nil
		}
		return exec.NewSort(child, keys, 0)

	case *ir.Top:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		var topG *lpg.Graph[string, float64]
		if lw, ok := walker.(*lpgNodeWalker); ok {
			topG = lw.g
		}
		keys := irSortKeys(p.SortItems, schema, topG, params, reg, bopts)
		if len(keys) == 0 || p.Limit <= 0 {
			// Degenerate: no sort keys or zero limit — return child unchanged.
			return child, nil
		}
		// exec.NewTop requires n ≥ 1 (int); ir.Top.Limit is int64.
		n := int(p.Limit)
		if int64(n) != p.Limit {
			// Limit overflows int — clamp to a large safe value.
			n = int(^uint(0) >> 1) // math.MaxInt
		}
		return exec.NewTop(child, keys, n)

	case *ir.Eager:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		return exec.NewEager(child, 0), nil

	case *ir.Limit:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		count := p.Count
		if p.CountExpr != nil {
			n, rerr := resolveCountExpr(p.CountExpr, params, reg, "LIMIT")
			if rerr != nil {
				return nil, rerr
			}
			count = n
		}
		// LIMIT over a write subtree: drain the child via an Eager
		// barrier so the write operators below run to completion
		// before LIMIT short-circuits the output stream. openCypher
		// 9 §3.6.2 requires the write side effects to occur
		// regardless of how many rows the projection finally returns
		// — `UNWIND $list AS x CREATE (...) RETURN ... LIMIT 2` must
		// still create one node/relationship per UNWIND element even
		// though only 2 rows make it past LIMIT. The Eager wrapper
		// materialises every input row (firing every write) before
		// LIMIT begins consuming. Closes Create6 [10] and similar
		// SKIP/LIMIT-truncated CREATE scenarios.
		if ir.ContainsWrite(p.Child) {
			return exec.NewLimit(exec.NewEager(child, 0), count)
		}
		return exec.NewLimit(child, count)

	case *ir.Skip:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		count := p.Count
		if p.CountExpr != nil {
			n, rerr := resolveCountExpr(p.CountExpr, params, reg, "SKIP")
			if rerr != nil {
				return nil, rerr
			}
			count = n
		}
		return exec.NewSkip(child, count)

	case *ir.Unwind:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		// Use schemaWidth (max column index + 1) rather than len(schema) to
		// determine the element column index.  buildIRProjection registers
		// secondary expression-string keys that share an existing column index
		// (e.g. schema["[1,2,3]"] == schema["lst"] == 0), which inflates
		// len(schema) without widening the actual row.
		schema[p.ElementVar] = schemaWidth(schema)
		// Tag the UNWIND element as a scalar column so buildIRProjection's
		// Variable fast path does NOT upgrade an IntegerValue element into
		// a NodeValue when the integer numerically equals a real NodeID.
		// Without this, `UNWIND range(0, 20) AS i` would project i=14 as
		// node#14 whenever a node with internal id 14 happened to exist —
		// breaking downstream `list[i]` (Match4 [4] setup query).
		if bopts != nil && p.ElementVar != "" {
			if bopts.scalarCols == nil {
				bopts.scalarCols = make(map[string]struct{})
			}
			bopts.scalarCols[p.ElementVar] = struct{}{}
		}
		return buildUnwindOperator(p, child, schema, walker, params, reg, bopts)

	case *ir.Distinct:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		return exec.NewDistinct(child, 0), nil

	case *ir.OptionalExpand:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}

		fromCol := 0
		if col, ok := schema[p.FromVar]; ok {
			fromCol = col
		}

		// Same output layout as Expand: inputRow... || srcID || edgeID || dstID.
		// Always advance the schema by 3 (including anonymous slots) so that
		// schemaWidth(schema) tracks the actual row width — see the Expand case
		// for rationale.
		schemaBase := schemaWidth(schema)
		schema[p.FromVar+"__opt_dup"] = schemaBase
		relKey := p.RelVar
		if relKey == "" {
			relKey = fmt.Sprintf("__anon_opt_rel_%d", schemaBase+1)
		}
		schema[relKey] = schemaBase + 1
		toKey := p.ToVar
		if toKey == "" {
			toKey = fmt.Sprintf("__anon_opt_to_%d", schemaBase+2)
		}
		schema[toKey] = schemaBase + 2

		// Mirror the Expand triplet bookkeeping so a *ir.NamedPath wrapper can
		// reconstruct a PathValue across OPTIONAL hops as well. The PathValue
		// closure tolerates NULL slots (returns Null when any expected column
		// is missing).
		if bopts != nil {
			step := pathChainStep{
				srcCol:  schemaBase,
				edgeCol: schemaBase + 1,
				dstCol:  schemaBase + 2,
			}
			if len(p.RelTypes) > 0 {
				step.edgeType = p.RelTypes[0]
			}
			bopts.expandTripletSeq = append(bopts.expandTripletSeq, step)
		}

		var g *lpg.Graph[string, float64]
		if lw, ok := walker.(*lpgNodeWalker); ok {
			g = lw.g
		}
		if g == nil {
			return child, nil
		}

		fwd, rev := csrPairFromGraph(g)
		dir := irDirToExec(p.Direction)

		cfg := exec.ExpandConfig{
			Direction: dir,
			InputCol:  fromCol,
		}
		if len(p.RelTypes) > 0 {
			cfg.EdgeType = p.RelTypes[0]
			cfg.EdgeTypeFilter = buildEdgeTypeFilter(g, p.RelTypes)
		}
		return exec.NewOptionalExpand(child, fwd, rev, cfg), nil

	case *ir.VarLengthExpand:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}

		fromCol := 0
		if col, ok := schema[p.FromVar]; ok {
			fromCol = col
		}

		// VarLengthExpand emits: inputRow... || pathList || dstNodeID.
		// pathList is a flat alternating ListValue: [srcID, edgePos0, dst0, ...].
		// Always advance schema by 2 — anonymous slots receive synthetic keys so
		// schemaWidth(schema) matches the actual row width.
		schemaBaseVLE := schemaWidth(schema)
		relKey := p.RelVar
		if relKey == "" {
			relKey = fmt.Sprintf("__anon_vlrel_%d", schemaBaseVLE)
		}
		schema[relKey] = schemaBaseVLE
		toKey := p.ToVar
		if toKey == "" {
			toKey = fmt.Sprintf("__anon_vlto_%d", schemaBaseVLE+1)
		}
		schema[toKey] = schemaBaseVLE + 1

		// Record path variable metadata for buildIRProjection. For a
		// chained-VLE pattern (multiple VLEs sharing the same PathVar)
		// each VLE registers a segment in plan-build order; the
		// projection iterates segments to stitch the full path. The
		// single-VLE case is unchanged: segments has length 1 and the
		// legacy listCol/edgeType fields mirror the first segment.
		if p.PathVar != "" && bopts != nil {
			if bopts.pathVarMeta == nil {
				bopts.pathVarMeta = make(map[string]pathVarInfo)
			}
			seg := pathVarSegment{listCol: schemaBaseVLE}
			if len(p.RelTypes) > 0 {
				seg.edgeType = p.RelTypes[0]
			}
			info, exists := bopts.pathVarMeta[p.PathVar]
			if !exists {
				info = pathVarInfo{listCol: seg.listCol, edgeType: seg.edgeType}
			}
			info.segments = append(info.segments, seg)
			bopts.pathVarMeta[p.PathVar] = info
			// Also register in schema so variable is accessible.
			if !exists {
				schema[p.PathVar] = schemaBaseVLE
			}
		}
		// Record the VLE relationship variable so the projection renders it
		// as a list of RelationshipValues instead of the raw flat list.
		if p.RelVar != "" && bopts != nil {
			if bopts.vleRelMeta == nil {
				bopts.vleRelMeta = make(map[string]vleRelInfo)
			}
			info := vleRelInfo{listCol: schemaBaseVLE}
			if len(p.RelTypes) > 0 {
				info.edgeType = p.RelTypes[0]
			}
			bopts.vleRelMeta[p.RelVar] = info
		}

		var g *lpg.Graph[string, float64]
		if lw, ok := walker.(*lpgNodeWalker); ok {
			g = lw.g
		}
		if g == nil {
			return child, nil
		}

		fwd, rev := csrPairFromGraph(g)
		dir := irDirToExec(p.Direction)
		minHops := p.MinDepth
		maxHops := p.MaxDepth
		// ir.VarLengthExpand.MaxDepth carries math.MaxInt for an unbounded
		// upper bound and a real integer otherwise (including 0 for the
		// degenerate "*0" quantifier). The exec.VarLengthExpand operator
		// honours MaxHops==0 as "no expansion beyond the source", which is
		// exactly the desired semantics for *0 / *0..0 / *N..M (N>M).

		var etFilter map[uint64]string
		edgeType := ""
		if len(p.RelTypes) > 0 {
			edgeType = p.RelTypes[0]
			etFilter = buildEdgeTypeFilter(g, p.RelTypes)
		}

		// Resolve excluded rel-variable names to row column indices via
		// the current schema. Variables not in schema (not yet bound at
		// this point in the plan) are silently dropped — the exec op
		// reads the row column at runtime and skips non-edge values, so
		// passing a wrong col is also a soft no-op.
		var excludedCols []int
		for _, v := range p.ExcludedRelVars {
			if col, ok := schema[v]; ok {
				excludedCols = append(excludedCols, col)
			}
		}

		cfg := exec.VarLengthConfig{
			Direction:       dir,
			EdgeType:        edgeType,
			EdgeTypeFilter:  etFilter,
			InputCol:        fromCol,
			MinHops:         minHops,
			MaxHops:         maxHops,
			ExcludedRelCols: excludedCols,
		}
		return exec.NewVarLengthExpand(child, fwd, rev, &cfg), nil

	case *ir.NamedPath:
		// NamedPath is a pure pass-through: build the child, then register
		// the alternating-chain metadata so that buildIRProjection can
		// reconstruct an expr.PathValue for this path variable.
		//
		// Approach: each non-leading IR chain element corresponds to one
		// Expand operator emitted by the child subtree. Expand registers its
		// (srcCol, edgeCol, dstCol) triplet into bopts.expandTripletSeq in
		// build order, so we capture the slice length before recursing and
		// consume the entries appended during the child build.
		var startIdx int
		if bopts != nil {
			startIdx = len(bopts.expandTripletSeq)
		}
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		if bopts == nil || p.PathName == "" {
			return child, nil
		}

		// Map IR chain to triplets. The leading element's NodeVar must be
		// resolvable in the current schema (an anonymous leading node would
		// have been registered under its synthetic key by AllNodesScan /
		// NodeByLabelScan); when the lookup fails we fall back to column 0,
		// which matches the legacy behaviour of the VLE pathway when the
		// FromVar lookup fails.
		var info pathChainInfo
		if leadingChain := p.Chain; len(leadingChain) > 0 && leadingChain[0].IsLeading {
			if col, ok := schema[leadingChain[0].NodeVar]; ok {
				info.leadingCol = col
			}
		}
		// Collect the triplets registered during the child build.
		added := bopts.expandTripletSeq[startIdx:]
		// Count non-leading IR chain elements to bound the iteration.
		nSteps := 0
		for i := range p.Chain {
			if !p.Chain[i].IsLeading {
				nSteps++
			}
		}
		if nSteps > len(added) {
			nSteps = len(added)
		}
		info.steps = make([]pathChainStep, 0, nSteps)
		chainIdx := 0
		for i := 0; i < nSteps && chainIdx < len(p.Chain); {
			if p.Chain[chainIdx].IsLeading {
				chainIdx++
				continue
			}
			step := added[i]
			// Prefer the AST-declared rel type when present; the live-graph
			// lookup in buildIRProjection takes precedence at row time.
			if len(p.Chain[chainIdx].RelTypes) > 0 && step.edgeType == "" {
				step.edgeType = p.Chain[chainIdx].RelTypes[0]
			}
			info.steps = append(info.steps, step)
			chainIdx++
			i++
		}

		if bopts.pathVarChain == nil {
			bopts.pathVarChain = make(map[string]pathChainInfo)
		}
		bopts.pathVarChain[p.PathName] = info
		// Register the path variable in the schema so downstream operators
		// that resolve "p" via column lookup (e.g. after a projection that
		// emits PathValue into a slot) keep working. We map it to the
		// leading-node column as a stable, harmless placeholder: the
		// projection fast path keys off pathVarChain before falling back to
		// schema, and any post-projection read of "p" looks up the slot
		// allocated by buildIRProjection itself rather than this one.
		if _, exists := schema[p.PathName]; !exists {
			schema[p.PathName] = info.leadingCol
		}
		return child, nil

	case *ir.ProcedureCall:
		return buildProcedureCallOperator(p, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)

	default:
		// When the engine is running inside a transactional write call
		// ([Engine.RunInTx]), buildPlanWithMutatorFull installs a
		// writeFallback closure on bopts so that write IR nodes
		// encountered as children of read wrappers (Projection over
		// CreateNode, Sort over SetProperty, …) recurse through the
		// write-aware planner instead of failing here. Read-only
		// callers leave the field nil and fall through to the original
		// "unsupported IR node" error.
		if bopts != nil && bopts.writeFallback != nil {
			return bopts.writeFallback(plan)
		}
		return nil, fmt.Errorf("cypher: unsupported IR node %T", plan)
	}
}

// buildEagerAggregation builds the physical EagerAggregation operator from the
// IR node. It wraps the child in a pre-projection so that the exec operator
// sees rows in the expected layout: [groupByKeys..., aggArgs...].
//
// Each pre-projection slot resolves its source value with the following
// priority order:
//
//  1. Parsed AST expression (when carried by the IR via GroupByExprs /
//     AggregateExpr.ArgumentExpr) — evaluated through [expr.Eval] against a
//     loaded RowContext so property accesses (n.prop), function calls, and
//     parameter references resolve correctly.
//  2. Schema lookup keyed on the textual group-by variable name or aggregate
//     argument string — preserves the legacy fast path for bare-variable
//     groupings (e.g. WITH n, count(*)).
//  3. Constant NULL — last-resort fallback so the pipeline keeps emitting
//     rows for malformed but non-fatal inputs.
//
// The Null fallback is openCypher-safe: count(NULL) does not increment,
// sum/avg/min/max ignore NULL, and collect skips NULL.
func buildEagerAggregation(
	p *ir.EagerAggregation,
	child exec.Operator,
	schema map[string]int,
	g *lpg.Graph[string, float64],
	params map[string]expr.Value,
	reg expr.FunctionRegistry,
	bopts *buildOpts,
) (exec.Operator, error) {
	// Snapshot the inbound schema once: every pre-projection eval needs the
	// pre-aggregation column layout, not the post-aggregation one (which we
	// overwrite below).
	schemaSnap := copySchema(schema)
	capturedG := g
	capturedParams := params
	capturedReg := reg
	capturedBopts := bopts

	// Per-group element budget for buffering aggregators (collect / percentile).
	// The Engine threads its budget decision through bopts.maxCollectItems using
	// the same encoding as EngineOptions.MaxCollectItems:
	//
	//   0  → unset; apply the finite DefaultMaxCollectItems (also the value seen
	//        by the public BuildPlanWithMutator path, so collect is never
	//        unbounded there either)
	//   <0 → the explicit opt-out; pass 0 to the factory so the cap is disabled
	//   >0 → an active budget, used verbatim
	maxCollectItems := funcs.DefaultMaxCollectItems
	if bopts != nil {
		switch {
		case bopts.maxCollectItems < 0:
			maxCollectItems = 0 // funcs convention: 0 disables the cap
		case bopts.maxCollectItems > 0:
			maxCollectItems = bopts.maxCollectItems
		}
	}

	// Build pre-projection items:
	//   positions 0..len(GroupBy)-1  → group-by key columns
	//   positions len(GroupBy)..end  → aggregate argument columns
	items := make([]exec.ProjectionItem, 0, len(p.GroupBy)+len(p.Aggregates))

	// Group-by key projections.
	keyCols := make([]int, len(p.GroupBy))
	for i, varName := range p.GroupBy {
		keyCols[i] = i // after pre-projection, key i is at position i

		var astExpr ast.Expression
		if i < len(p.GroupByExprs) {
			astExpr = p.GroupByExprs[i]
		}
		items = append(items, exec.ProjectionItem{
			Alias: varName,
			Eval:  newAggregationEval(astExpr, varName, schemaSnap, capturedG, capturedParams, capturedReg, capturedBopts),
		})
	}

	// Aggregate argument projections.
	aggFactories := make([]funcs.AggregatorFactory, 0, len(p.Aggregates))
	for _, aggExpr := range p.Aggregates {
		// Two-arg aggregates (percentileCont, percentileDisc) carry the
		// percentile parameter in SecondArgExpr. Evaluate it once at
		// build time so the factory bakes the value in. Single-arg
		// aggregates pass expr.Null which is ignored downstream. The
		// openCypher TCK (Aggregation6 [5]) treats a row-dependent
		// percentile (e.g. a bare Variable that resolves only at row
		// time) as ArgumentError: NumberOutOfRange — propagate that
		// distinction by leaving secondArg as Null when the
		// build-time eval cannot bind every leaf to a constant, then
		// rely on aggregateFactory's strict numeric check below to
		// surface the typed error.
		var secondArg = expr.Null
		var secondArgIsRowDependent bool
		if aggExpr.SecondArgExpr != nil {
			if exprContainsRowDependency(aggExpr.SecondArgExpr) {
				secondArgIsRowDependent = true
			} else {
				v, evErr := expr.Eval(aggExpr.SecondArgExpr, expr.RowContext{}, params, reg)
				if evErr == nil {
					secondArg = v
				}
			}
		}
		_ = secondArgIsRowDependent
		if secondArgIsRowDependent {
			return nil, fmt.Errorf("cypher: ArgumentError.NumberOutOfRange: percentile argument of %s must be a constant in [0.0, 1.0], got a row-dependent expression", aggExpr.Function)
		}
		factory, ferr := aggregateFactory(aggExpr.Function, aggExpr.Argument, secondArg, maxCollectItems)
		if ferr != nil {
			return nil, fmt.Errorf("cypher: %w", ferr)
		}
		// DISTINCT inside aggregation: wrap each per-group instance with a
		// "seen-values" set so repeated identical inputs are skipped.
		// Equality uses [expr.Value.Hash] + per-value Equal (via
		// IsTruthy(a.Equal(b))), so list/map values with the same shape
		// dedup correctly.
		if aggExpr.Distinct {
			inner := factory
			factory = func() funcs.Aggregator {
				return newDistinctAggregator(inner())
			}
		}
		aggFactories = append(aggFactories, factory)

		// count(*) — argument is irrelevant; emit a constant non-null sentinel so
		// the aggregator's Step always increments. exec.NewCountStarAgg treats any
		// non-null value as a tick.
		if aggExpr.Argument == "" {
			items = append(items, exec.ProjectionItem{
				Alias: aggExpr.OutputName,
				Eval:  func(_ exec.Row) (expr.Value, error) { return expr.BoolValue(true), nil },
			})
			continue
		}

		items = append(items, exec.ProjectionItem{
			Alias: aggExpr.OutputName,
			Eval: newAggregationEval(
				aggExpr.ArgumentExpr, aggExpr.Argument,
				schemaSnap, capturedG, capturedParams, capturedReg, capturedBopts,
			),
		})
	}

	// Wrap child with the pre-projection.
	preProj, err := exec.NewProject(child, items)
	if err != nil {
		return nil, fmt.Errorf("cypher: EagerAggregation pre-projection: %w", err)
	}

	op, err := exec.NewEagerAggregation(preProj, keyCols, aggFactories, 0)
	if err != nil {
		return nil, fmt.Errorf("cypher: NewEagerAggregation: %w", err)
	}

	// When there are no group-by keys, openCypher semantics require a single
	// output row even when the input is empty — e.g.
	//
	//	MATCH (n:NeverExists) RETURN count(*)   --> 0
	//
	// EagerAggregation as a multiset operator emits zero rows in that case, so
	// wrap it in an adapter that synthesises the "empty-input → one-row of
	// neutral results" row.
	var topOp exec.Operator = op
	if len(p.GroupBy) == 0 {
		topOp = exec.NewGlobalAggregateAdapter(op, aggFactories)
	}

	// Replace schema with EagerAggregation output schema:
	//   positions 0..len(GroupBy)-1            → group-by variable names
	//   positions len(GroupBy)..len(Aggs)-1    → aggregate output names
	for k := range schema {
		delete(schema, k)
	}
	for i, varName := range p.GroupBy {
		schema[varName] = i
	}
	for i, aggExpr := range p.Aggregates {
		schema[aggExpr.OutputName] = len(p.GroupBy) + i
	}

	// Mark every aggregate output column as scalar so that buildIRProjection's
	// Variable fast-path does not mis-upgrade an integer count/sum/avg result into
	// a NodeValue when the integer coincides with a real NodeID.
	// Mark every grouping-key column as preprojected so the colliding-alias
	// guard in buildIRProjection's schema-name fast path keeps the fast path
	// instead of routing through general eval (which would re-interpret the
	// variable as the original pre-aggregation value).
	if bopts != nil {
		if bopts.scalarCols == nil {
			bopts.scalarCols = make(map[string]struct{})
		}
		for _, aggExpr := range p.Aggregates {
			// Skip adding when OutputName shadows a variable that was in the
			// PRE-aggregation schema (e.g. `count(n) AS n` keeps OutputName
			// "n" but Selection operators below the aggregation still hold
			// a closure over bopts.scalarCols and would interpret the
			// existing pre-aggregation "n" column as scalar, breaking
			// property predicates like `{matched: true}` on the bound node.
			// The projection-fast-path that consults scalarCols for the
			// post-aggregation read is harmless to skip — without the tag
			// it falls through to upgradeNodeIDToValue, which only fires
			// when the integer actually resolves to a node; aggregate
			// integers that happen to coincide with a node id remain a
			// known sharp edge but are vanishingly rare compared with the
			// systematic alias-shadow breakage that the unguarded write
			// causes.
			if _, shadowsInput := schemaSnap[aggExpr.OutputName]; shadowsInput {
				continue
			}
			bopts.scalarCols[aggExpr.OutputName] = struct{}{}
		}
		if bopts.preprojectedCols == nil {
			bopts.preprojectedCols = make(map[string]struct{})
		}
		for _, varName := range p.GroupBy {
			bopts.preprojectedCols[varName] = struct{}{}
		}
		// Computed (non-Variable) grouping keys evaluate to a scalar at
		// the EagerAggregation's pre-projection — e.g. `WITH a.num2 % 3
		// AS mod` stores the integer mod value in the post-aggregation
		// row slot. Downstream Variable fast-path reads of `mod` must
		// NOT upgrade the integer to a NodeValue when it numerically
		// coincides with an interned NodeID (WithOrderBy4 [12] flake:
		// `RETURN mod, sum` surfaced `(node#2)` instead of `2`). Track
		// these in a dedicated set rather than projAliasScalarCols
		// because the latter is also read by buildRowCtx — which the
		// pre-projection closure invokes when evaluating the grouping
		// expression. Adding `mod` to projAliasScalarCols would suppress
		// the upgrade of `a` (the only variable in scope at the
		// pre-projection); we only want the suppression in the POST-
		// aggregation Variable read.
		if bopts.aggKeyScalarCols == nil {
			bopts.aggKeyScalarCols = make(map[string]struct{})
		}
		for i, varName := range p.GroupBy {
			if i >= len(p.GroupByExprs) {
				break
			}
			if _, isVar := p.GroupByExprs[i].(*ast.Variable); isVar {
				continue
			}
			bopts.aggKeyScalarCols[varName] = struct{}{}
		}
	}

	return topOp, nil
}

// distinctAggregator wraps a [funcs.Aggregator] with a "seen-values" set
// so the inner Step receives only the first occurrence of each distinct
// value within the same group. The hash bucket holds full Values so
// list/map equality (which falls outside a usable Hash collision) still
// resolves correctly via [expr.Value.Equal]. NULL is silently skipped
// per openCypher aggregation semantics.
//
// distinctAggregator is NOT safe for concurrent use.
type distinctAggregator struct {
	inner funcs.Aggregator
	seen  map[uint64][]expr.Value
}

func newDistinctAggregator(inner funcs.Aggregator) *distinctAggregator {
	return &distinctAggregator{inner: inner, seen: map[uint64][]expr.Value{}}
}

func (d *distinctAggregator) Init() {
	d.inner.Init()
	d.seen = map[uint64][]expr.Value{}
}

func (d *distinctAggregator) Step(v expr.Value) error {
	if expr.IsNull(v) {
		// NULL is filtered by every standard aggregator's Step; preserve
		// that behaviour and skip the dedup bookkeeping too.
		return d.inner.Step(v)
	}
	h := v.Hash()
	for _, prev := range d.seen[h] {
		if expr.IsTruthy(prev.Equal(v)) {
			return nil
		}
	}
	// Forward to the inner aggregator first; only record the value as "seen"
	// once it has been accepted. When the inner aggregator rejects the value
	// (e.g. its per-group element budget is exceeded), the typed error
	// propagates and the dedup set is left unchanged.
	if err := d.inner.Step(v); err != nil {
		return err
	}
	d.seen[h] = append(d.seen[h], v)
	return nil
}

func (d *distinctAggregator) Result() expr.Value { return d.inner.Result() }

// newAggregationEval returns a row-evaluator function suitable for an
// EagerAggregation pre-projection slot. The evaluator's resolution order is:
//
//  1. When astExpr is non-nil, evaluate it via [expr.Eval] against a RowContext
//     built from schemaSnap (which always reflects the pre-aggregation column
//     layout).
//  2. Otherwise, attempt a direct schema lookup keyed on varName.
//  3. Otherwise return [expr.Null].
func newAggregationEval(
	astExpr ast.Expression,
	varName string,
	schemaSnap map[string]int,
	g *lpg.Graph[string, float64],
	params map[string]expr.Value,
	reg expr.FunctionRegistry,
	bopts *buildOpts,
) func(exec.Row) (expr.Value, error) {
	// AST path — always preferred when present.
	if astExpr != nil {
		return func(row exec.Row) (expr.Value, error) {
			rowCtx := buildRowCtx(row, schemaSnap, g, bopts)
			return evalRow(bopts, astExpr, rowCtx, params, reg)
		}
	}
	// Legacy schema-lookup path.
	if col, ok := schemaSnap[varName]; ok {
		capturedCol := col
		return func(row exec.Row) (expr.Value, error) {
			if capturedCol < len(row) {
				return row[capturedCol], nil
			}
			return expr.Null, nil
		}
	}
	return func(_ exec.Row) (expr.Value, error) { return expr.Null, nil }
}

// aggregateFactory maps an IR aggregate function name and argument to a
// [funcs.AggregatorFactory]. An empty argument string means count(*).
// secondArg is the evaluated percentile parameter for two-arg
// aggregates; pass NULL for single-arg aggregates.
//
// maxItems is the resolved per-group element budget for the buffering
// aggregators (collect, percentileCont, percentileDisc): a positive value is an
// active cap and zero disables it. The streaming aggregators ignore it.
func aggregateFactory(fn, argument string, secondArg expr.Value, maxItems int) (funcs.AggregatorFactory, error) {
	lower := strings.ToLower(fn)
	switch lower {
	case "count":
		if argument == "" {
			return funcs.NewCountStarAgg(), nil
		}
		return funcs.NewCountAgg(), nil
	case "sum":
		return funcs.NewSumAgg(), nil
	case "avg":
		return funcs.NewAvgAgg(), nil
	case "min":
		return funcs.NewMinAgg(), nil
	case "max":
		return funcs.NewMaxAgg(), nil
	case "collect":
		return funcs.NewCollectAggN(maxItems), nil
	case "stdev":
		return funcs.NewStdDevAgg(), nil
	case "stdevp":
		return funcs.NewStdDevPAgg(), nil
	case "percentilecont":
		p, err := validPercentileParam(secondArg)
		if err != nil {
			return nil, err
		}
		return funcs.NewPercentileContAggN(p, maxItems), nil
	case "percentiledisc":
		p, err := validPercentileParam(secondArg)
		if err != nil {
			return nil, err
		}
		return funcs.NewPercentileDiscAggN(p, maxItems), nil
	default:
		return nil, fmt.Errorf("unknown aggregate function %q", fn)
	}
}

// exprContainsRowDependency reports whether e references at least one
// row-dependent value — any *ast.Variable (which only binds at row
// time) or any non-parameter expression that walks down to one.
// Parameters and literals are NOT row-dependent. Used by the
// percentile-aggregate builder to reject row-varying percentile
// arguments at plan-build time (Aggregation6 [5]).
func exprContainsRowDependency(e ast.Expression) bool {
	if e == nil {
		return false
	}
	switch n := e.(type) {
	case *ast.Variable:
		return true
	case *ast.Property:
		return exprContainsRowDependency(n.Receiver)
	case *ast.BinaryOp:
		return exprContainsRowDependency(n.Left) || exprContainsRowDependency(n.Right)
	case *ast.UnaryOp:
		return exprContainsRowDependency(n.Operand)
	case *ast.FunctionInvocation:
		for _, arg := range n.Args {
			if exprContainsRowDependency(arg) {
				return true
			}
		}
		return false
	case *ast.SubscriptExpr:
		return exprContainsRowDependency(n.Expr) || exprContainsRowDependency(n.Index)
	case *ast.SliceExpr:
		return exprContainsRowDependency(n.Expr) || exprContainsRowDependency(n.From) || exprContainsRowDependency(n.To)
	case *ast.ListLiteral:
		for _, el := range n.Elements {
			if exprContainsRowDependency(el) {
				return true
			}
		}
		return false
	}
	return false
}

// validPercentileParam coerces and validates the second argument of a
// percentile aggregate. Per openCypher, the percentile must be a number
// in [0.0, 1.0] (inclusive); values outside this range raise an
// ArgumentError(NumberOutOfRange) at plan-build time so the engine can
// surface it as a runtime error to the caller.
func validPercentileParam(v expr.Value) (float64, error) {
	switch n := v.(type) {
	case expr.FloatValue:
		f := float64(n)
		if f < 0.0 || f > 1.0 {
			return 0, fmt.Errorf("ArgumentError.NumberOutOfRange: percentile must be in [0.0, 1.0], got %g", f)
		}
		return f, nil
	case expr.IntegerValue:
		f := float64(int64(n))
		if f < 0.0 || f > 1.0 {
			return 0, fmt.Errorf("ArgumentError.NumberOutOfRange: percentile must be in [0.0, 1.0], got %g", f)
		}
		return f, nil
	}
	// Unset or non-numeric: default to median (0.5) for the non-failing
	// happy paths.
	return 0.5, nil
}

// irSortKeys converts a slice of ir.SortItem to exec.SortKey values.
//
// Resolution strategy (per item):
//  1. Direct schema lookup by expression string — covers the common case where
//     the sort key is a projected column (name or alias matches).
//  2. If (1) fails and the item carries an AST expression (si.Expr != nil),
//     compile an expression evaluator that derives the sort value from the row
//     context at runtime. This handles ORDER BY on expressions that are not
//     direct projection outputs (e.g. ORDER BY n.age after RETURN n).
//  3. If both fail, the item is skipped (callers treat empty result as
//     "no sort needed").
//
// The g, params, reg, and bopts arguments are used only when compiling an
// expression evaluator (case 2). Pass nil/zero values when the caller does
// not have access to them; in that case only direct schema lookups succeed.
func irSortKeys(
	items []ir.SortItem,
	schema map[string]int,
	g *lpg.Graph[string, float64],
	params map[string]expr.Value,
	reg expr.FunctionRegistry,
	bopts *buildOpts,
) []exec.SortKey {
	keys := make([]exec.SortKey, 0, len(items))
	for _, si := range items {
		// Case 1: direct lookup by expression string.
		if col, ok := schema[si.Expression]; ok {
			keys = append(keys, exec.SortKey{
				ColIdx:    col,
				Ascending: !si.Descending,
			})
			continue
		}

		// Case 2: compile an expression evaluator from the stored AST node.
		if si.Expr != nil {
			// Snapshot the schema and capture all dependencies so the closure
			// evaluates correctly against the row layout produced by the child.
			schemaCopy := make(map[string]int, len(schema))
			for k, v := range schema {
				schemaCopy[k] = v
			}
			capturedExpr := si.Expr
			capturedG := g
			capturedParams := params
			capturedReg := reg
			capturedBopts := bopts
			ascending := !si.Descending
			keys = append(keys, exec.SortKey{
				Ascending: ascending,
				Eval: func(row exec.Row) (expr.Value, error) {
					rowCtx := buildRowCtx(row, schemaCopy, capturedG, capturedBopts)
					return evalRow(capturedBopts, capturedExpr, rowCtx, capturedParams, capturedReg)
				},
			})
			continue
		}

		// Case 3: unresolvable — skip.
	}
	return keys
}

// buildProcedureCallOperator builds a ProcedureCallOp from an *ir.ProcedureCall node.
// When procReg is nil, it falls back to an empty registry which will return
// ErrProcNotFound at runtime.
func buildProcedureCallOperator(
	p *ir.ProcedureCall,
	walker nodeWalkerIface,
	labelSrc labelResolverIface,
	reg expr.FunctionRegistry,
	params map[string]expr.Value,
	schema map[string]int,
	idxMgr *index.Manager,
	procReg *procs.Registry,
	argByTag map[uint32]*exec.Argument,
	bopts *buildOpts,
) (exec.Operator, error) {
	// Build child if present.
	var child exec.Operator
	if p.Child != nil {
		var err error
		child, err = buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
	}

	// Resolve effective registry (never nil at runtime to avoid nil deref).
	effectiveProcReg := procReg
	if effectiveProcReg == nil {
		effectiveProcReg = procs.NewRegistry()
	}

	// Look up the procedure signature to determine YIELD columns.
	entry, err := effectiveProcReg.Lookup(p.Namespace, p.Name)
	if err != nil {
		return nil, fmt.Errorf("cypher: ProcedureCall %q: %w", p.Name, err)
	}

	// Build argument evaluators. Each argument string is either a
	// variable reference resolved via the current schema, or a literal
	// (quoted string, integer, float, boolean, null) materialised once
	// at plan-build time. The latter is the common case for TCK
	// fixtures and any hand-written CALL with explicit constants.
	//
	// When the declared input kind is FLOAT and the evaluator yields an
	// IntegerValue, the value is widened to FloatValue at runtime so the
	// procedure receives the kind it expects (openCypher numeric widening
	// per the TCK Call3 scenarios).
	var argEvals []func(exec.Row) (expr.Value, error)
	if len(p.Arguments) == 0 && len(entry.Sig.Inputs) > 0 && len(entry.Sig.InputNames) == len(entry.Sig.Inputs) {
		// Implicit-argument form: bind each declared input from the
		// query parameter whose name matches the declared input name.
		// openCypher restricts implicit argument passing to STANDALONE
		// CALL — `CALL proc` with no argument list and no YIELD. An
		// in-query CALL (one that drives a downstream YIELD/RETURN, i.e.
		// has YieldVars populated by the translator) must pass arguments
		// explicitly. Surfaces SyntaxError(InvalidArgumentPassingMode)
		// per Call2 [4].
		if len(p.YieldVars) > 0 {
			return nil, fmt.Errorf(
				"cypher: SyntaxError.InvalidArgumentPassingMode: in-query CALL %q with YIELD must pass arguments explicitly",
				p.Name,
			)
		}
		argEvals = make([]func(exec.Row) (expr.Value, error), len(entry.Sig.Inputs))
		for i, paramName := range entry.Sig.InputNames {
			v, ok := params[paramName]
			if !ok {
				// openCypher: implicit-argument CALL must find every
				// declared input as a query parameter. A missing
				// parameter is ParameterMissing: MissingParameter at
				// compile time (Call1 [11]). We surface the error here
				// — the closest the engine has to a "compile time" gate
				// — so the result drainage propagates it before any
				// rows are emitted.
				return nil, fmt.Errorf(
					"cypher: ParameterMissing: MissingParameter: procedure %q implicit argument %q has no matching $%s parameter",
					p.Name, paramName, paramName,
				)
			}
			if entry.Sig.Inputs[i] == expr.KindFloat {
				if iv, isInt := v.(expr.IntegerValue); isInt {
					v = expr.FloatValue(float64(iv))
				}
			}
			captured := v
			argEvals[i] = func(_ exec.Row) (expr.Value, error) { return captured, nil }
		}
	} else {
		argEvals = make([]func(exec.Row) (expr.Value, error), len(p.Arguments))
		for i, argStr := range p.Arguments {
			baseEval := buildProcArgEvaluator(argStr, schema)
			if i < len(entry.Sig.Inputs) && entry.Sig.Inputs[i] == expr.KindFloat {
				inner := baseEval
				argEvals[i] = func(row exec.Row) (expr.Value, error) {
					v, err := inner(row)
					if err != nil {
						return v, err
					}
					if iv, ok := v.(expr.IntegerValue); ok {
						return expr.FloatValue(float64(iv)), nil
					}
					return v, nil
				}
			} else {
				argEvals[i] = baseEval
			}
		}
	}

	// Compile-time arity validation. The procedure declares N inputs;
	// the call must supply exactly N arguments OR zero (the "implicit"
	// form, where openCypher binds inputs from query parameters whose
	// names match the declared input names). Surfaces
	// SyntaxError(InvalidNumberOfArguments) per openCypher CALL
	// semantics for the partial / overflow cases.
	if len(p.Arguments) != 0 && len(p.Arguments) != len(entry.Sig.Inputs) {
		return nil, fmt.Errorf(
			"cypher: SyntaxError.InvalidNumberOfArguments: procedure %q expects %d argument(s), got %d",
			p.Name, len(entry.Sig.Inputs), len(p.Arguments),
		)
	}

	// Compile-time argument-type validation. For every positional
	// argument that is a known primitive literal (string, integer,
	// float, boolean, null), check it against the declared input kind
	// from entry.Sig.Inputs. Mismatches surface as a typed compile-time
	// error that the engine maps to SyntaxError(InvalidArgumentType).
	// expr.KindNull is treated as a wildcard (NUMBER / ANY / unknown
	// declared kinds map to KindNull in our TCK proc-decl parser). A
	// null literal arg is always accepted (procedures whose inputs are
	// declared nullable are the common case).
	for i, argStr := range p.Arguments {
		if i >= len(entry.Sig.Inputs) {
			break
		}
		want := entry.Sig.Inputs[i]
		if want == expr.KindNull {
			continue
		}
		lit, ok := parseProcArgLiteral(argStr)
		if !ok || lit == expr.Null {
			continue
		}
		got := lit.Kind()
		if got == want {
			continue
		}
		// Numeric widening: INTEGER is accepted wherever FLOAT is
		// declared. openCypher TCK Call3 specifies that an integer
		// literal value is coercible to FLOAT (the procedure receives
		// a FloatValue at runtime). We do NOT promote in the other
		// direction or across boolean/string/number boundaries.
		if got == expr.KindInteger && want == expr.KindFloat {
			continue
		}
		return nil, fmt.Errorf(
			"cypher: SyntaxError.InvalidArgumentType: procedure %q argument %d expects %s, got %s",
			p.Name, i, want, got,
		)
	}

	// Determine yield variables: explicit YIELD wins; otherwise emit all output columns.
	yieldVars := p.YieldVars
	sourceNames := p.YieldSourceNames
	if len(yieldVars) == 0 {
		yieldVars = make([]string, len(entry.Sig.Outputs))
		sourceNames = make([]string, len(entry.Sig.Outputs))
		for i, out := range entry.Sig.Outputs {
			yieldVars[i] = out.Name
			sourceNames[i] = out.Name
		}
	} else if len(sourceNames) == 0 {
		// IR built before YieldSourceNames was added falls back to assuming
		// yield variables match declared output names (no AS rename).
		sourceNames = yieldVars
	}

	// Register output columns in the schema. ProcedureCallOp emits every
	// row in the procedure's declared output order (entry.Sig.Outputs);
	// YIELD may reorder, subset, or rename those columns. Map each yield
	// variable to the declared output column with the matching SOURCE name
	// so a downstream RETURN/WITH that references `a` reads the procedure's
	// `a` column even when YIELD listed `b, a` (Call5 [3]) or `a AS x, b AS y`
	// (Call5 [4]).
	declaredIdx := make(map[string]int, len(entry.Sig.Outputs))
	for i, out := range entry.Sig.Outputs {
		declaredIdx[out.Name] = i
	}
	base := schemaWidth(schema)
	emitSlots := make([]int, 0, len(entry.Sig.Outputs))
	for i, v := range yieldVars {
		src := sourceNames[i]
		if di, ok := declaredIdx[src]; ok {
			schema[v] = base + di
			emitSlots = append(emitSlots, di)
		} else {
			// Fallback: source not declared. Allocate the next available
			// slot so the schema still has unique indices, even though the
			// procedure cannot supply a matching value.
			schema[v] = schemaWidth(schema)
		}
	}
	_ = emitSlots // reserved for future column subsetting; today ProcedureCallOp emits all declared columns

	return exec.NewProcedureCallOp(p.Namespace, p.Name, argEvals, yieldVars, child, effectiveProcReg), nil
}

// buildProcArgEvaluator returns a row-evaluator for a single CALL
// argument string. Variable references that resolve in schema return
// row[idx]; primitive literals (quoted strings, integers, floats,
// booleans, null) are decoded once at plan-build time and the resulting
// [expr.Value] is captured by the closure. Unrecognised forms fall
// through to expr.Null so the procedure impl can still surface a
// typed-error message if the value is critical.
func buildProcArgEvaluator(argStr string, schema map[string]int) func(exec.Row) (expr.Value, error) {
	if colIdx, ok := schema[argStr]; ok {
		idx := colIdx
		return func(row exec.Row) (expr.Value, error) {
			if idx < len(row) {
				return row[idx], nil
			}
			return expr.Null, nil
		}
	}
	if lit, ok := parseProcArgLiteral(argStr); ok {
		return func(_ exec.Row) (expr.Value, error) { return lit, nil }
	}
	return func(_ exec.Row) (expr.Value, error) { return expr.Null, nil }
}

// parseProcArgLiteral recognises the primitive Cypher literal forms
// that may appear as a CALL argument: quoted single-/double-quoted
// strings, decimal integers, decimal floats, the boolean keywords and
// the null keyword. Returns (value, true) on recognition; (zero, false)
// when the string is not a primitive literal — the caller falls back
// to a variable lookup or a Null placeholder.
func parseProcArgLiteral(s string) (expr.Value, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	switch s {
	case "null", "NULL":
		return expr.Null, true
	case "true", "TRUE":
		return expr.BoolValue(true), true
	case "false", "FALSE":
		return expr.BoolValue(false), true
	}
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		return expr.StringValue(s[1 : len(s)-1]), true
	}
	if strings.ContainsAny(s, ".eE") {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return expr.FloatValue(f), true
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return expr.IntegerValue(n), true
	}
	return nil, false
}

// buildIndexSeekOperator builds an exec.NodeByIndexSeek from an IR NodeByIndexSeek node.
// The Value field may be a parameter reference ($name) or a literal string/integer.
func buildIndexSeekOperator(
	p *ir.NodeByIndexSeek,
	params map[string]expr.Value,
	schema map[string]int,
	idxMgr *index.Manager,
) (exec.Operator, error) {
	seekVal, err := resolveSeekValue(p.Value, params)
	if err != nil {
		return nil, fmt.Errorf("cypher: NodeByIndexSeek %q: %w", p.NodeVar, err)
	}
	if idxMgr == nil {
		return nil, fmt.Errorf("cypher: NodeByIndexSeek requires an index manager")
	}
	names := idxMgr.ListIndexes()
	for _, name := range names {
		sub, err := idxMgr.GetIndex(name)
		if err != nil || sub.Kind() != "hash" {
			continue
		}
		if op, ok := tryNewHashSeek(sub, seekVal); ok {
			schema[p.NodeVar] = schemaWidth(schema)
			return op, nil
		}
	}
	return nil, fmt.Errorf("cypher: no hash index found for NodeByIndexSeek on %q.%q", p.Label, p.Property)
}

// tryBuildIndexSeekFromSelection inspects a Selection predicate for the pattern
// "n.prop = $name" or "$name = n.prop" over an AllNodesScan or NodeByLabelScan
// child, and when a hash index is available returns a NodeByIndexSeek operator.
// ok is false when the rewrite does not apply.
func tryBuildIndexSeekFromSelection(
	sel *ir.Selection,
	params map[string]expr.Value,
	schema map[string]int,
	idxMgr *index.Manager,
) (exec.Operator, bool, error) {
	nodeVar, label, ok := scanLeafNodeVar(sel.Child)
	if !ok {
		return nil, false, nil
	}
	seekVal, propKey, err := extractSeekFromSelection(sel, params, nodeVar)
	if err != nil || seekVal == nil {
		return nil, false, err
	}
	if op, ok := tryNamedHashSeek(idxMgr, label, propKey, seekVal); ok {
		schema[nodeVar] = schemaWidth(schema)
		return op, true, nil
	}
	if op, ok := tryAnyHashSeek(idxMgr, seekVal); ok {
		schema[nodeVar] = schemaWidth(schema)
		return op, true, nil
	}
	return nil, false, nil
}

// scanLeafNodeVar returns (nodeVar, label, true) when child is a
// bare scan leaf (AllNodesScan or NodeByLabelScan); (_, _, false)
// for any other operator type. Label is "" for the unlabelled
// AllNodesScan case.
func scanLeafNodeVar(child ir.LogicalPlan) (nodeVar, label string, ok bool) {
	switch c := child.(type) {
	case *ir.AllNodesScan:
		return c.NodeVar, "", true
	case *ir.NodeByLabelScan:
		return c.NodeVar, c.Label, true
	default:
		return "", "", false
	}
}

// extractSeekFromSelection walks the Selection's predicate in two
// passes — the parameterised n.prop = $param shorthand first, then
// a general AST-based extraction — and returns (seekVal, propKey,
// nil) on the first success. The seek value is nil when no
// suitable equality predicate is present; the caller treats that
// as "this Selection is not index-seekable" and falls back to the
// child scan.
func extractSeekFromSelection(
	sel *ir.Selection,
	params map[string]expr.Value,
	nodeVar string,
) (expr.Value, string, error) {
	if paramName, pk := extractEqParamFromPredicate(sel.Predicate, nodeVar); paramName != "" && pk != "" {
		sv, err := resolveSeekValue("$"+paramName, params)
		if err != nil {
			return nil, "", err
		}
		return sv, pk, nil
	}
	if sel.PredicateExpr != nil {
		if pk, sv, ok := extractEqFromAST(sel.PredicateExpr, nodeVar, params); ok {
			return sv, pk, nil
		}
	}
	return nil, "", nil
}

// tryNamedHashSeek looks up the auto-named hash index for a (label,
// propKey) pair and returns the seek operator + true when present
// and applicable to seekVal.
func tryNamedHashSeek(idxMgr *index.Manager, label, propKey string, seekVal expr.Value) (exec.Operator, bool) {
	if label == "" || propKey == "" {
		return nil, false
	}
	wantName := strings.ToLower(label) + "_" + strings.ToLower(propKey) + "_hash"
	sub, err := idxMgr.GetIndex(wantName)
	if err != nil || sub.Kind() != "hash" {
		return nil, false
	}
	return tryNewHashSeek(sub, seekVal)
}

// tryAnyHashSeek iterates every registered index and returns the
// first hash index that can serve seekVal. It is the fallback when
// the named-index lookup misses.
func tryAnyHashSeek(idxMgr *index.Manager, seekVal expr.Value) (exec.Operator, bool) {
	for _, name := range idxMgr.ListIndexes() {
		sub, err := idxMgr.GetIndex(name)
		if err != nil || sub.Kind() != "hash" {
			continue
		}
		if op, ok := tryNewHashSeek(sub, seekVal); ok {
			return op, true
		}
	}
	return nil, false
}

// extractEqFromAST extracts (propKey, seekVal, true) from a
// BinaryOp{=, Property{nodeVar, key}, literal/param} or its mirror form.
// params may be nil.
func extractEqFromAST(
	predExpr ast.Expression,
	nodeVar string,
	params map[string]expr.Value,
) (propKey string, seekVal expr.Value, ok bool) {
	binOp, ok2 := predExpr.(*ast.BinaryOp)
	if !ok2 || binOp.Operator != "=" {
		return "", nil, false
	}
	// Try left=Property, right=value.
	if prop, isP := binOp.Left.(*ast.Property); isP {
		if varNode, isV := prop.Receiver.(*ast.Variable); isV && varNode.Name == nodeVar {
			if v, err := astLiteralToValue(binOp.Right, params); err == nil {
				return prop.Key, v, true
			}
		}
	}
	// Try right=Property, left=value (mirror form).
	if prop, isP := binOp.Right.(*ast.Property); isP {
		if varNode, isV := prop.Receiver.(*ast.Variable); isV && varNode.Name == nodeVar {
			if v, err := astLiteralToValue(binOp.Left, params); err == nil {
				return prop.Key, v, true
			}
		}
	}
	return "", nil, false
}

// astLiteralToValue converts an AST leaf (string/int/float/bool literal or
// parameter reference) to an expr.Value. Returns a non-nil error for any other
// expression type.
func astLiteralToValue(e ast.Expression, params map[string]expr.Value) (expr.Value, error) {
	switch v := e.(type) {
	case *ast.StringLiteral:
		return expr.StringValue(v.Value), nil
	case *ast.IntLiteral:
		return expr.IntegerValue(v.Value), nil
	case *ast.FloatLiteral:
		return expr.FloatValue(v.Value), nil
	case *ast.BoolLiteral:
		return expr.BoolValue(v.Value), nil
	case *ast.Parameter:
		if params != nil {
			if pv, ok := params[v.Name]; ok {
				return pv, nil
			}
		}
		return expr.Null, nil
	}
	return nil, fmt.Errorf("not a literal: %T", e)
}

// extractEqParamFromPredicate parses the opaque predicate string to extract
// the parameter name and property key from an equality of form
// "(nodeVar.prop = $name)" or "($name = nodeVar.prop)".
// Returns ("", "") when the predicate does not match.
func extractEqParamFromPredicate(pred, nodeVar string) (paramName, propKey string) {
	pred = strings.TrimSpace(pred)
	if strings.HasPrefix(pred, "(") && strings.HasSuffix(pred, ")") {
		pred = pred[1 : len(pred)-1]
	}
	idx := strings.Index(pred, " = ")
	if idx < 0 {
		return "", ""
	}
	left := strings.TrimSpace(pred[:idx])
	right := strings.TrimSpace(pred[idx+3:])

	prefix := nodeVar + "."
	if strings.HasPrefix(left, prefix) && strings.HasPrefix(right, "$") {
		return right[1:], left[len(prefix):]
	}
	if strings.HasPrefix(right, prefix) && strings.HasPrefix(left, "$") {
		return left[1:], right[len(prefix):]
	}
	return "", ""
}

// resolveCountExpr evaluates a SKIP/LIMIT expression at physical-build
// time, applying the openCypher type-and-range rules:
//
//   - The evaluated value must be an integer; a float surfaces a
//     SyntaxError(InvalidArgumentType) typed error.
//   - The value must be non-negative; negative integers surface a
//     SyntaxError(NegativeIntegerArgument).
//
// kind is "SKIP" or "LIMIT" and is used in the error message. The
// returned int64 is safe to pass to exec.NewSkip / exec.NewLimit.
func resolveCountExpr(e ast.Expression, params map[string]expr.Value, reg expr.FunctionRegistry, kind string) (int64, error) {
	v, err := expr.Eval(e, expr.RowContext{}, params, reg)
	if err != nil {
		return 0, fmt.Errorf("cypher: %s evaluation: %w", kind, err)
	}
	switch n := v.(type) {
	case expr.IntegerValue:
		i := int64(n)
		if i < 0 {
			return 0, fmt.Errorf("cypher: SyntaxError.NegativeIntegerArgument: %s requires a non-negative integer, got %d", kind, i)
		}
		return i, nil
	case expr.FloatValue:
		return 0, fmt.Errorf("cypher: SyntaxError.InvalidArgumentType: %s requires an integer, got float", kind)
	}
	return 0, fmt.Errorf("cypher: SyntaxError.InvalidArgumentType: %s requires an integer, got %T", kind, v)
}

// resolveSeekValue resolves p.Value to an expr.Value. If value starts with "$",
// it is a parameter reference looked up in params. Otherwise it is parsed as a
// literal (string in single or double quotes, boolean, integer).
func resolveSeekValue(value string, params map[string]expr.Value) (expr.Value, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "$") {
		name := value[1:]
		if params != nil {
			if v, ok := params[name]; ok {
				return v, nil
			}
		}
		return expr.Null, nil // unbound parameter resolves to NULL
	}
	// Parse literal: delegate to exec's existing literal parser via a minimal
	// inline implementation (string, bool, integer).
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') {
		return expr.StringValue(value[1 : len(value)-1]), nil
	}
	if value == "true" {
		return expr.BoolValue(true), nil
	}
	if value == "false" {
		return expr.BoolValue(false), nil
	}
	// Try integer.
	var n int64
	if _, err := fmt.Sscan(value, &n); err == nil {
		return expr.IntegerValue(n), nil
	}
	return nil, fmt.Errorf("unsupported seek value %q", value)
}

// hashStringLookup is satisfied by hash.Index[string].
type hashStringLookup interface {
	Lookup(value string) *roaring64.Bitmap
}

// hashInt64Lookup is satisfied by hash.Index[int64].
type hashInt64Lookup interface {
	Lookup(value int64) *roaring64.Bitmap
}

// tryNewHashSeek attempts to build a NodeByIndexSeek operator using sub as the
// hash index. It returns (nil, false) when sub is not a supported hash type, or
// when seekVal's kind is incompatible with the index key type.
//
// The kind gate keeps the index a transparent optimisation: a string parameter
// compared against an int64-keyed property is a type-incompatible equality that
// openCypher evaluates to false. Declining the seek here lets the planner fall
// back to the scan+filter, which yields the same zero-row result a non-indexed
// graph would — rather than building a seek that fails at Init with
// [exec.ErrIndexTypeMismatch].
func tryNewHashSeek(sub index.Subscriber, seekVal expr.Value) (*exec.NodeByIndexSeek, bool) {
	if sl, ok := sub.(hashStringLookup); ok {
		if seekVal.Kind() != expr.KindString {
			return nil, false
		}
		return exec.NewNodeByIndexSeek(exec.NewStringHashIndex(sl), seekVal), true
	}
	if il, ok := sub.(hashInt64Lookup); ok {
		if seekVal.Kind() != expr.KindInteger {
			return nil, false
		}
		return exec.NewNodeByIndexSeek(exec.NewInt64HashIndex(il), seekVal), true
	}
	return nil, false
}

// indexedPropKind returns the declared key kind of the hash index that backs
// (label, property), suitable for a [sema.PropTypeResolver]. It mirrors the
// index-discovery order used by tryBuildIndexSeekFromSelection: the auto-named
// "<label>_<property>_hash" index first, then any registered hash index as a
// fallback. ok is false when no hash index is found or its key type is not one
// of the kinds the seek path supports.
//
// Only hash indexes carry a Go-typed key the engine can map to an expr.Kind;
// label and btree indexes return ("", false) and leave the parameter type at
// its conservative default.
func indexedPropKind(idxMgr *index.Manager, label, property string) (expr.Kind, bool) {
	if idxMgr == nil || property == "" {
		return 0, false
	}
	if label != "" {
		wantName := strings.ToLower(label) + "_" + strings.ToLower(property) + "_hash"
		if sub, err := idxMgr.GetIndex(wantName); err == nil && sub.Kind() == "hash" {
			if k, ok := hashIndexKind(sub); ok {
				return k, true
			}
		}
	}
	// Fallback: scan registered indexes, matching tryAnyHashSeek's reach. With
	// no label to disambiguate we accept the first usable hash index, which is
	// the same index that fallback seek would bind.
	for _, name := range idxMgr.ListIndexes() {
		sub, err := idxMgr.GetIndex(name)
		if err != nil || sub.Kind() != "hash" {
			continue
		}
		if k, ok := hashIndexKind(sub); ok {
			return k, true
		}
	}
	return 0, false
}

// hashIndexKind maps a hash index Subscriber to the expr.Kind of its key, by
// the same type assertions tryNewHashSeek uses to bind the seek operator.
func hashIndexKind(sub index.Subscriber) (expr.Kind, bool) {
	if _, ok := sub.(hashStringLookup); ok {
		return expr.KindString, true
	}
	if _, ok := sub.(hashInt64Lookup); ok {
		return expr.KindInteger, true
	}
	return 0, false
}

// lpgPropToExpr converts an lpg.PropertyValue to its expr.Value counterpart.
// Unsupported kinds (PropTime, PropBytes) fall through to expr.Null.
//
// PropString values that begin with a SOH-range byte (0x01..0x06) are decoded
// as temporal values, mirroring the encoding performed by
// [cypher/exec.parseTemporalLiteral]. The tag→kind mapping is:
//
//	0x01 → DateValue
//	0x02 → LocalDateTimeValue
//	0x03 → DateTimeValue
//	0x04 → LocalTimeValue
//	0x05 → TimeValue
//	0x06 → DurationValue
//
// When the body is malformed the value falls back to a plain StringValue
// carrying the raw bytes; this keeps reads forward-safe in the unlikely event
// that a legacy WAL contains a byte-collision payload.
func lpgPropToExpr(pv lpg.PropertyValue) expr.Value {
	switch pv.Kind() {
	case lpg.PropString:
		if s, ok := pv.String(); ok {
			if v, decoded := decodeTemporalString(s); decoded {
				return v
			}
			return expr.StringValue(s)
		}
	case lpg.PropInt64:
		if i, ok := pv.Int64(); ok {
			return expr.IntegerValue(i)
		}
	case lpg.PropFloat64:
		if f, ok := pv.Float64(); ok {
			return expr.FloatValue(f)
		}
	case lpg.PropBool:
		if b, ok := pv.Bool(); ok {
			return expr.BoolValue(b)
		}
	case lpg.PropList:
		if elems, ok := pv.List(); ok {
			lv := make(expr.ListValue, len(elems))
			for i, elem := range elems {
				lv[i] = lpgPropToExpr(elem)
			}
			return lv
		}
	}
	return expr.Null
}

// decodeTemporalString recognises the SOH-range tag introduced by
// cypher/exec/temporal_literal.go and returns the matching temporal Value.
// Returns (nil, false) when s does not start with a recognised tag byte.
func decodeTemporalString(s string) (expr.Value, bool) {
	if len(s) < 2 {
		return nil, false
	}
	body := s[1:]
	switch s[0] {
	case 0x01:
		if v, err := expr.ParseDate(body); err == nil {
			return v, true
		}
	case 0x02:
		if v, err := expr.ParseLocalDateTime(body); err == nil {
			return v, true
		}
	case 0x03:
		if v, err := expr.ParseDateTime(body); err == nil {
			return v, true
		}
	case 0x04:
		if v, err := expr.ParseLocalTime(body); err == nil {
			return v, true
		}
	case 0x05:
		if v, err := expr.ParseTime(body); err == nil {
			return v, true
		}
	case 0x06:
		if v, err := expr.ParseDuration(body); err == nil {
			return v, true
		}
	}
	return nil, false
}

// buildUnwindOperator builds the physical Unwind operator from the IR node.
//
// When the IR carries a parsed AST expression (p.ListExpr != nil), it is
// evaluated via [expr.Eval] against a row context derived from the current
// schema snapshot. When the evaluated value is a [expr.ListValue] the
// elements are expanded; when it is NULL or any non-list type no rows are
// emitted (openCypher semantics). When p.ListExpr is nil (expression could not
// be parsed), the operator always emits an empty expansion.
func buildUnwindOperator(
	p *ir.Unwind,
	child exec.Operator,
	schema map[string]int,
	walker nodeWalkerIface,
	params map[string]expr.Value,
	reg expr.FunctionRegistry,
	bopts *buildOpts,
) (exec.Operator, error) {
	if p.ListExpr == nil {
		// No AST available — emit nothing for every input row.
		return exec.NewUnwind(child, func(_ exec.Row) (expr.ListValue, error) {
			return nil, nil
		})
	}

	var g *lpg.Graph[string, float64]
	if lw, ok := walker.(*lpgNodeWalker); ok {
		g = lw.g
	}

	schemaSnap := copySchema(schema)
	listExpr := p.ListExpr
	capturedParams := params
	capturedReg := reg
	capturedG := g
	capturedBopts := bopts

	return exec.NewUnwind(child, func(row exec.Row) (expr.ListValue, error) {
		rowCtx := buildRowCtx(row, schemaSnap, capturedG, capturedBopts)
		v, err := evalRow(capturedBopts, listExpr, rowCtx, capturedParams, capturedReg)
		if err != nil {
			return nil, err
		}
		if v == expr.Null || v == nil {
			return nil, nil
		}
		lv, ok := v.(expr.ListValue)
		if !ok {
			// Per openCypher: UNWIND on a non-list scalar wraps it in a single-element list.
			return expr.ListValue{v}, nil
		}
		return lv, nil
	})
}

// upgradeNodeIDToValue upgrades a row cell from expr.IntegerValue(NodeID) to a
// full expr.NodeValue carrying labels and properties. The upgrade fires only
// when v is an expr.IntegerValue, g is non-nil, and the mapper resolves the
// integer to a known natural key — i.e. only when the row cell genuinely
// references a graph node. In every other case (nil graph, non-IntegerValue,
// IntegerValue that is not a NodeID such as a literal-integer projection or a
// relationship edge ID) the value is returned unchanged so callers can rely on
// "value-passthrough unless we can prove it's a node".
//
// Relationships are not upgraded here. The engine emits a relationship as three
// separate IntegerValue columns (srcID, edgeID, dstID) and the schema carries
// no per-column kind information, so RelationshipValue construction needs
// schema-level type metadata that this helper deliberately does not touch.
func upgradeNodeIDToValue(v expr.Value, g *lpg.Graph[string, float64]) expr.Value {
	if g == nil {
		return v
	}
	iv, ok := v.(expr.IntegerValue)
	if !ok {
		return v
	}
	id := graph.NodeID(iv)
	// Resolve still gates identity: it distinguishes a genuine NodeID from a
	// plain integer that merely falls in NodeID range. Once identity is
	// confirmed we fetch properties and labels by NodeID, skipping the two
	// internal external-key → NodeID lookups that NodeProperties/NodeLabels
	// would otherwise perform — 3 Mapper ops per node collapse to 1.
	if _, resolved := g.AdjList().Mapper().Resolve(id); !resolved {
		return v
	}
	rawProps := g.NodePropertiesByID(id)
	// Skip the map allocation entirely for propertyless nodes: a nil MapValue
	// reads identically to an empty one (missing-key access yields null,
	// keys()/properties() range as empty, Bolt serialises {}). This removes one
	// allocation per propertyless returned node — common in label-only and
	// relationship-dense graphs.
	var props expr.MapValue
	if len(rawProps) > 0 {
		props = make(expr.MapValue, len(rawProps))
		for k, pv := range rawProps {
			props[k] = lpgPropToExpr(pv)
		}
	}
	labels := g.NodeLabelsByID(id)
	return expr.NodeValue{ID: uint64(id), Labels: labels, Properties: props}
}

// buildNodeValueFromID constructs an expr.NodeValue for a known graph NodeID,
// loading labels and properties from g. If the ID is not found in the mapper,
// an empty NodeValue with only the ID set is returned.
func buildNodeValueFromID(id graph.NodeID, g *lpg.Graph[string, float64]) expr.NodeValue {
	if g == nil {
		return expr.NodeValue{ID: uint64(id)}
	}
	if _, resolved := g.AdjList().Mapper().Resolve(id); !resolved {
		return expr.NodeValue{ID: uint64(id)}
	}
	rawProps := g.NodePropertiesByID(id)
	var props expr.MapValue
	if len(rawProps) > 0 {
		props = make(expr.MapValue, len(rawProps))
		for k, pv := range rawProps {
			props[k] = lpgPropToExpr(pv)
		}
	}
	labels := g.NodeLabelsByID(id)
	return expr.NodeValue{ID: uint64(id), Labels: labels, Properties: props}
}

// buildPathValueFromChainInfo reconstructs an [expr.PathValue] from the
// flat alternating column layout described by cinfo. It mirrors the logic
// inside the named-path fast path in buildIRProjection and is factored out
// so that buildRowCtx can produce a correct PathValue for WHERE-clause
// predicates (e.g. `WHERE length(p) = 1`) that reference a named path
// bound in the preceding MATCH pattern. Returns (zero, false) when the row
// does not contain the expected columns or when the leading column does
// not carry a recognisable node value.
func buildPathValueFromChainInfo(row exec.Row, cinfo pathChainInfo, g *lpg.Graph[string, float64]) (expr.PathValue, bool) {
	if cinfo.leadingCol >= len(row) {
		return expr.PathValue{}, false
	}
	leadVal := row[cinfo.leadingCol]
	if leadVal == nil || expr.IsNull(leadVal) {
		return expr.PathValue{}, false
	}
	var leadNode expr.NodeValue
	switch lv := leadVal.(type) {
	case expr.NodeValue:
		leadNode = lv
	case expr.IntegerValue:
		leadNode = buildNodeValueFromID(graph.NodeID(lv), g)
	default:
		return expr.PathValue{}, false
	}
	nodes := make([]expr.NodeValue, 0, len(cinfo.steps)+1)
	rels := make([]expr.RelationshipValue, 0, len(cinfo.steps))
	nodes = append(nodes, leadNode)
	for _, step := range cinfo.steps {
		if step.edgeCol >= len(row) || step.dstCol >= len(row) {
			return expr.PathValue{}, false
		}
		edgeIDVal, ok1 := row[step.edgeCol].(expr.IntegerValue)
		dstIDVal, ok2 := row[step.dstCol].(expr.IntegerValue)
		if !ok1 || !ok2 {
			return expr.PathValue{}, false
		}
		dstNode := buildNodeValueFromID(graph.NodeID(dstIDVal), g)
		et := step.edgeType
		var edgeProps expr.MapValue
		pathStart := nodes[len(nodes)-1].ID
		pathEnd := dstNode.ID
		storageStart := pathStart
		storageEnd := pathEnd
		if g != nil {
			sKey, sOK := g.AdjList().Mapper().Resolve(graph.NodeID(pathStart))
			dKey, dOK := g.AdjList().Mapper().Resolve(graph.NodeID(pathEnd))
			if sOK && dOK {
				if ets := g.EdgeLabels(sKey, dKey); len(ets) > 0 {
					et = ets[0]
					rawEP := g.EdgeProperties(sKey, dKey)
					edgeProps = make(expr.MapValue, len(rawEP))
					for k, pv := range rawEP {
						edgeProps[k] = lpgPropToExpr(pv)
					}
				} else if ets := g.EdgeLabels(dKey, sKey); len(ets) > 0 {
					et = ets[0]
					rawEP := g.EdgeProperties(dKey, sKey)
					edgeProps = make(expr.MapValue, len(rawEP))
					for k, pv := range rawEP {
						edgeProps[k] = lpgPropToExpr(pv)
					}
					storageStart = pathEnd
					storageEnd = pathStart
				}
			}
		}
		rels = append(rels, expr.RelationshipValue{
			ID:         uint64(edgeIDVal),
			StartID:    storageStart,
			EndID:      storageEnd,
			Type:       et,
			Properties: edgeProps,
		})
		nodes = append(nodes, dstNode)
	}
	return expr.PathValue{Nodes: nodes, Relationships: rels}, true
}

// buildPathValueFromVLEMeta reconstructs an [expr.PathValue] from the flat
// alternating ListValue [srcID, edgePos0, dst0, edgePos1, dst1, …] that the
// VarLengthExpand operator deposits into the named-path column described by
// pmeta. It mirrors the named-path-VLE fast path in buildIRProjection and is
// factored out so that buildRowCtx can produce a real PathValue for
// expression evaluation (e.g. `relationships(p)`, `nodes(p)`,
// `length(p)` over var-length paths). Returns (zero, false) when the
// column is missing, not a ListValue, empty, or carries non-integer entries.
func buildPathValueFromVLEMeta(row exec.Row, pmeta pathVarInfo, g *lpg.Graph[string, float64]) (expr.PathValue, bool) {
	segments := pmeta.segments
	if len(segments) == 0 {
		segments = []pathVarSegment{{listCol: pmeta.listCol, edgeType: pmeta.edgeType}}
	}
	var nodes []expr.NodeValue
	var rels []expr.RelationshipValue
	// Prepend any leading fixed-length Expand hops captured during
	// plan build. Each leading step contributes a (rel, dst) pair;
	// the first step also seeds the path's lead node from its srcCol
	// (Match6 [14] `(:Start)<-[:CONNECTED_TO]-()-[...VLE...]-` puts
	// :Start at position 0 of the path before the VLE list takes over).
	for i, step := range pmeta.leadingSteps {
		if step.edgeCol >= len(row) || step.dstCol >= len(row) || step.srcCol >= len(row) {
			break
		}
		edgeIDVal, eOK := row[step.edgeCol].(expr.IntegerValue)
		dstIDVal, dOK := row[step.dstCol].(expr.IntegerValue)
		srcIDVal, sOK := row[step.srcCol].(expr.IntegerValue)
		if !eOK || !dOK || !sOK {
			break
		}
		if i == 0 {
			nodes = append(nodes, buildNodeValueFromID(graph.NodeID(srcIDVal), g))
		}
		dstNode := buildNodeValueFromID(graph.NodeID(dstIDVal), g)
		prevID := nodes[len(nodes)-1].ID
		et := step.edgeType
		var edgeProps expr.MapValue
		storageStart, storageEnd := prevID, dstNode.ID
		if g != nil {
			sKey, sR := g.AdjList().Mapper().Resolve(graph.NodeID(prevID))
			dKey, dR := g.AdjList().Mapper().Resolve(graph.NodeID(dstNode.ID))
			if sR && dR {
				ets := g.EdgeLabels(sKey, dKey)
				rawEP := g.EdgeProperties(sKey, dKey)
				if len(ets) == 0 && len(rawEP) == 0 {
					ets = g.EdgeLabels(dKey, sKey)
					rawEP = g.EdgeProperties(dKey, sKey)
					if len(ets) > 0 || len(rawEP) > 0 {
						storageStart, storageEnd = dstNode.ID, prevID
					}
				}
				if len(ets) > 0 {
					et = ets[0]
				}
				edgeProps = make(expr.MapValue, len(rawEP))
				for k, pv := range rawEP {
					edgeProps[k] = lpgPropToExpr(pv)
				}
			}
		}
		rels = append(rels, expr.RelationshipValue{
			ID:         uint64(edgeIDVal),
			StartID:    storageStart,
			EndID:      storageEnd,
			Type:       et,
			Properties: edgeProps,
		})
		nodes = append(nodes, dstNode)
	}
	leadingNodeCount := len(nodes)
	for segIdx, seg := range segments {
		if seg.listCol >= len(row) {
			return expr.PathValue{}, false
		}
		lv, ok := row[seg.listCol].(expr.ListValue)
		if !ok {
			return expr.PathValue{}, false
		}
		if len(lv) == 0 {
			continue
		}
		nHops := (len(lv) - 1) / 2
		if segIdx == 0 && leadingNodeCount == 0 {
			leadID, ok2 := lv[0].(expr.IntegerValue)
			if !ok2 {
				return expr.PathValue{}, false
			}
			nodes = append(nodes, buildNodeValueFromID(graph.NodeID(leadID), g))
		}
		for h := 0; h < nHops; h++ {
			edgePos, ok1 := lv[1+2*h].(expr.IntegerValue)
			dstIDVal, ok2 := lv[2+2*h].(expr.IntegerValue)
			if !ok1 || !ok2 {
				return expr.PathValue{}, false
			}
			dstNode := buildNodeValueFromID(graph.NodeID(dstIDVal), g)
			if len(nodes) == 0 {
				if iv, ok3 := lv[0].(expr.IntegerValue); ok3 {
					nodes = append(nodes, buildNodeValueFromID(graph.NodeID(iv), g))
				}
			}
			prev := nodes[len(nodes)-1]
			nodes = append(nodes, dstNode)
			et := seg.edgeType
			var edgeProps expr.MapValue
			storageStart, storageEnd := prev.ID, dstNode.ID
			if g != nil {
				srcKey, sOK := g.AdjList().Mapper().Resolve(graph.NodeID(prev.ID))
				dstKey, dOK := g.AdjList().Mapper().Resolve(graph.NodeID(dstNode.ID))
				if sOK && dOK {
					ets := g.EdgeLabels(srcKey, dstKey)
					rawEP := g.EdgeProperties(srcKey, dstKey)
					if len(ets) == 0 && len(rawEP) == 0 {
						// Reverse pass of an undirected VLE: storage
						// holds the edge as (dstKey -> srcKey). Swap the
						// reported StartID/EndID so PathValue.String
						// renders this hop with the inverted arrow
						// (Match6 [14]).
						ets = g.EdgeLabels(dstKey, srcKey)
						rawEP = g.EdgeProperties(dstKey, srcKey)
						if len(ets) > 0 || len(rawEP) > 0 {
							storageStart, storageEnd = dstNode.ID, prev.ID
						}
					}
					if len(ets) > 0 {
						et = ets[0]
					}
					edgeProps = make(expr.MapValue, len(rawEP))
					for k, pv := range rawEP {
						edgeProps[k] = lpgPropToExpr(pv)
					}
				}
			}
			rels = append(rels, expr.RelationshipValue{
				ID:         uint64(edgePos),
				StartID:    storageStart,
				EndID:      storageEnd,
				Type:       et,
				Properties: edgeProps,
			})
		}
	}
	if len(nodes) == 0 {
		return expr.PathValue{}, false
	}
	return expr.PathValue{Nodes: nodes, Relationships: rels}, true
}

// buildRowCtx converts a row plus a schema snapshot into an expr.RowContext,
// upgrading IntegerValue(nodeID) entries to NodeValue with properties loaded
// from the graph. g may be nil when no graph is available (upgrade is
// skipped). When bopts carries edgeVarMeta entries (T937) the relationship
// variables they describe are reconstructed as full RelationshipValues with
// their typed properties loaded from the graph, so property-access
// expressions such as `r.since` resolve through the bound relationship.
// When bopts carries pathVarChain entries the named path variables are
// reconstructed as PathValues so that WHERE-clause predicates such as
// `length(p) = 1` operate on the documented Path kind rather than on the
// leading node value. When bopts carries vleRelMeta entries the
// variable-length relationship variables are reconstructed as a
// List<RelationshipValue> so expressions such as `last(r)`, `size(r)` and
// `r[0].type` operate on the documented openCypher list-of-relationships
// shape rather than on the raw alternating path encoding emitted by
// VarLengthExpand.
func buildRowCtx(row exec.Row, schema map[string]int, g *lpg.Graph[string, float64], bopts *buildOpts) expr.RowContext {
	ctx := make(expr.RowContext, len(schema))
	for varName, colIdx := range schema {
		if colIdx >= len(row) || row[colIdx] == nil {
			continue
		}
		if bopts != nil && bopts.pathVarChain != nil {
			if cinfo, isChain := bopts.pathVarChain[varName]; isChain {
				if pv, ok := buildPathValueFromChainInfo(row, cinfo, g); ok {
					ctx[varName] = pv
					continue
				}
			}
		}
		if bopts != nil && bopts.pathVarMeta != nil {
			if pmeta, isVLE := bopts.pathVarMeta[varName]; isVLE {
				if pv, ok := buildPathValueFromVLEMeta(row, pmeta, g); ok {
					ctx[varName] = pv
					continue
				}
			}
		}
		if bopts != nil && bopts.vleRelMeta != nil {
			if rmeta, isVLERel := bopts.vleRelMeta[varName]; isVLERel {
				if rl, ok := buildVLERelListFromRow(row, rmeta, g); ok {
					ctx[varName] = rl
					continue
				}
			}
		}
		if bopts != nil && bopts.edgeVarMeta != nil {
			if _, isEdge := bopts.edgeVarMeta[varName]; isEdge {
				// Post-projection forward: if the schema slot for varName
				// already carries a RelationshipValue (an upstream
				// projection emitted it into the column), use that
				// directly. The edgeVarMeta triplet coordinates only
				// apply to the original Expand-emitted shape; after a
				// WITH the column holds a self-describing
				// RelationshipValue, and the triplet slots now belong
				// to other variables (Comparison1 [5] regression: after
				// `WITH a` followed by a plain Apply, edgeVarMeta[a]'s
				// triplet positions point at the Apply-side inner
				// columns).
				if rv, isRel := row[colIdx].(expr.RelationshipValue); isRel {
					ctx[varName] = rv
					continue
				}
				if meta, isEdge2 := bopts.edgeVarMeta[varName]; isEdge2 {
					if rv, ok := buildRelationshipValueFromRow(row, meta, g); ok {
						ctx[varName] = rv
						continue
					}
				}
			}
		}
		// Scalar columns (UNWIND element variables, aggregate outputs,
		// computed projection aliases) pass through unchanged: their
		// integer values are not node ids and must not be upgraded.
		// Without this guard a CREATE/UNWIND that reads an integer
		// through a row variable would silently elevate the integer
		// to a NodeValue when it happened to numerically equal an
		// existing internal node id — breaking downstream property
		// writes, range() arguments, list indexing, and more (Match4
		// [4] / Aggregation6 [5] setup queries / WithSkipLimit3 [3]).
		if bopts != nil && bopts.scalarCols != nil {
			if _, isScalar := bopts.scalarCols[varName]; isScalar {
				ctx[varName] = row[colIdx]
				continue
			}
		}
		if bopts != nil && bopts.projAliasScalarCols != nil {
			if _, isScalar := bopts.projAliasScalarCols[varName]; isScalar {
				ctx[varName] = row[colIdx]
				continue
			}
		}
		ctx[varName] = upgradeNodeIDToValue(row[colIdx], g)
	}
	return ctx
}

// buildVLERelListFromRow reconstructs a List<RelationshipValue> from the
// flat alternating [src, edgePos, dst, edgePos, dst, ...] ListValue emitted
// by VarLengthExpand into the rel-variable column. Returns an empty list
// for a zero-hop result (the variable evaluates to []) and (nil, false)
// when the column is absent or not a ListValue.
func buildVLERelListFromRow(row exec.Row, rmeta vleRelInfo, g *lpg.Graph[string, float64]) (expr.ListValue, bool) {
	if rmeta.listCol >= len(row) {
		return nil, false
	}
	lv, ok := row[rmeta.listCol].(expr.ListValue)
	if !ok {
		return nil, false
	}
	if len(lv) == 0 {
		return expr.ListValue{}, true
	}
	nHops := (len(lv) - 1) / 2
	rels := make(expr.ListValue, 0, nHops)
	srcID := uint64(0)
	if iv, ok2 := lv[0].(expr.IntegerValue); ok2 {
		srcID = uint64(iv)
	}
	for h := 0; h < nHops; h++ {
		edgeID, ok1 := lv[1+2*h].(expr.IntegerValue)
		dstIDVal, ok2 := lv[2+2*h].(expr.IntegerValue)
		if !ok1 || !ok2 {
			continue
		}
		dstID := uint64(dstIDVal)
		et := rmeta.edgeType
		var edgeProps expr.MapValue
		if g != nil {
			srcKey, sOK := g.AdjList().Mapper().Resolve(graph.NodeID(srcID))
			dstKey, dOK := g.AdjList().Mapper().Resolve(graph.NodeID(dstID))
			if sOK && dOK {
				if ets := g.EdgeLabels(srcKey, dstKey); len(ets) > 0 {
					et = ets[0]
				} else if ets := g.EdgeLabels(dstKey, srcKey); len(ets) > 0 {
					et = ets[0]
				}
				rawEP := g.EdgeProperties(srcKey, dstKey)
				if len(rawEP) == 0 {
					rawEP = g.EdgeProperties(dstKey, srcKey)
				}
				edgeProps = make(expr.MapValue, len(rawEP))
				for k, pv := range rawEP {
					edgeProps[k] = lpgPropToExpr(pv)
				}
			}
		}
		rels = append(rels, expr.RelationshipValue{
			ID:         uint64(edgeID),
			StartID:    srcID,
			EndID:      dstID,
			Type:       et,
			Properties: edgeProps,
		})
		srcID = dstID
	}
	return rels, true
}

// buildRelationshipValueFromRow reconstructs a [expr.RelationshipValue] from
// the (srcCol, edgeCol, dstCol) triplet emitted by the [exec.Expand]
// operator. The edge type and Properties are looked up on the live graph
// when both endpoints resolve. Returns (zero, false) when the row does not
// contain the expected columns or when the column types are not
// IntegerValue.
func buildRelationshipValueFromRow(row exec.Row, meta edgeVarInfo, g *lpg.Graph[string, float64]) (expr.RelationshipValue, bool) {
	if meta.edgeCol >= len(row) {
		return expr.RelationshipValue{}, false
	}
	edgeIDVal, ok := row[meta.edgeCol].(expr.IntegerValue)
	if !ok {
		return expr.RelationshipValue{}, false
	}
	var srcID, dstID uint64
	if meta.srcCol < len(row) {
		if iv, ok2 := row[meta.srcCol].(expr.IntegerValue); ok2 {
			srcID = uint64(iv)
		}
	}
	if meta.dstCol < len(row) {
		if iv, ok2 := row[meta.dstCol].(expr.IntegerValue); ok2 {
			dstID = uint64(iv)
		}
	}
	edgeType := meta.edgeType
	var edgeProps expr.MapValue
	storageStart, storageEnd := srcID, dstID
	if g != nil {
		srcKey, srcResolved := g.AdjList().Mapper().Resolve(graph.NodeID(srcID))
		dstKey, dstResolved := g.AdjList().Mapper().Resolve(graph.NodeID(dstID))
		if srcResolved && dstResolved {
			// Forward direction first: covers the common case of a directed
			// expansion or the forward pass of an undirected expansion.
			ets := g.EdgeLabels(srcKey, dstKey)
			rawEP := g.EdgeProperties(srcKey, dstKey)
			if len(ets) == 0 && len(rawEP) == 0 {
				// Reverse-edge pass of an undirected expansion: storage
				// holds the edge as (dstKey -> srcKey); record the
				// inverted storage direction so the PathValue renderer
				// (which compares rel.StartID against the path's
				// preceding node) emits `<-[…]-` for this hop. Without
				// this Match6 [12]/[13] rendered every undirected
				// reverse hop as a forward arrow because StartID still
				// matched the traversal-src.
				ets = g.EdgeLabels(dstKey, srcKey)
				rawEP = g.EdgeProperties(dstKey, srcKey)
				if len(ets) > 0 || len(rawEP) > 0 {
					storageStart, storageEnd = dstID, srcID
				}
			}
			// Per-instance label override: when the CSR slot at
			// edgeIDVal corresponds to a specific parallel CREATE
			// (multigraph mode), narrow the edge's type to that
			// CREATE's label set. The per-pair property union is
			// kept so SET / REMOVE / `r.foo` reflect the live edge
			// state — only the type label benefits from
			// per-instance specialisation (Match2 [6] / Match7 [29]
			// / MatchWhere1 [11]).
			//
			// Stable-handle path first: read the explicit per-edge
			// handle at this forward CSR position and resolve the type
			// by it. This is delete-stable — unlike the positional
			// instance idx (edgeInstanceIdxFor), it does not mis-map
			// after a parallel sibling is deleted and the neighbour
			// slice is compacted. Only applies when the storage
			// direction was NOT inverted above (the handle column is
			// keyed on the forward (srcKey, dstKey) pair). Falls back to
			// the positional idx when no handle column is present or the
			// slot's handle is 0 (a MERGE-created edge).
			handled := false
			if storageStart == srcID && storageEnd == dstID {
				if h := edgeHandleAtFwdPos(g, srcKey, dstKey, uint64(edgeIDVal)); h != 0 {
					if perHandle := g.EdgeLabelsByHandle(srcKey, dstKey, h); len(perHandle) > 0 {
						ets = perHandle
						handled = true
					}
				}
			}
			if !handled {
				instanceIdx, totalCreates, parallelCount := edgeInstanceIdxFor(g, srcKey, dstKey, uint64(edgeIDVal))
				if instanceIdx > 0 && parallelCount >= totalCreates && totalCreates > 0 {
					if perInstance := g.EdgeLabelsAt(srcKey, dstKey, instanceIdx); len(perInstance) > 0 {
						ets = perInstance
					}
				}
			}
			if len(ets) > 0 {
				edgeType = pickEdgeType(ets, meta.acceptedTypes)
			}
			edgeProps = make(expr.MapValue, len(rawEP))
			for k, pv := range rawEP {
				edgeProps[k] = lpgPropToExpr(pv)
			}
		}
	}
	return expr.RelationshipValue{
		ID:         uint64(edgeIDVal),
		StartID:    storageStart,
		EndID:      storageEnd,
		Type:       edgeType,
		Properties: edgeProps,
	}, true
}

// edgeHandleAtFwdPos returns the stable per-edge handle stored at forward
// CSR position edgePos for the (srcKey, dstKey) pair, or 0 when: the graph
// carries no handle column, edgePos is outside srcKey's adjacency range,
// the slot at edgePos does not point at dstKey, or either endpoint is
// unknown to the mapper. A 0 return signals the caller to fall back to the
// positional per-instance idx path.
//
// Unlike edgeInstanceIdxFor this performs no positional counting: it reads
// the handle directly from the CSR slot, which is why it remains correct
// after a parallel sibling is deleted.
func edgeHandleAtFwdPos(g *lpg.Graph[string, float64], srcKey, dstKey string, edgePos uint64) uint64 {
	adj := g.AdjList()
	srcID, ok := adj.Mapper().Lookup(srcKey)
	if !ok {
		return 0
	}
	dstID, ok := adj.Mapper().Lookup(dstKey)
	if !ok {
		return 0
	}
	fwdCSR := csr.BuildFromAdjList(adj)
	handles := fwdCSR.HandlesSlice()
	if handles == nil {
		return 0
	}
	verts := fwdCSR.VerticesSlice()
	edges := fwdCSR.EdgesSlice()
	if uint64(srcID)+1 >= uint64(len(verts)) {
		return 0
	}
	start := verts[uint64(srcID)]
	end := verts[uint64(srcID)+1]
	if edgePos < start || edgePos >= end || edgePos >= uint64(len(edges)) {
		return 0
	}
	// Guard: the slot must actually point at dstKey. Defends against a
	// stale edgePos that survived a concurrent rebuild landing on a
	// different neighbour.
	if edges[edgePos] != dstID {
		return 0
	}
	return handles[edgePos]
}

// edgeInstanceIdxFor returns the 1-based per-CREATE instance index that
// the CSR slot at edgePos corresponds to, together with the total
// CREATE count for (srcKey, dstKey) and the number of parallel CSR
// entries for that pair. Returns 0 / 0 / 0 when the position is out of
// range or either endpoint is unknown to the mapper.
//
// Multigraph storage records one parallel CSR slot per CREATE, so
// counting how many earlier entries (in the src's adjacency range)
// share the same dst — up to and including edgePos — yields the
// CREATE-time instance idx. Simple-graph storage collapses every
// parallel CREATE onto one slot, so parallelCount stays at 1 and
// callers fall back to the per-pair union surfaces.
func edgeInstanceIdxFor(g *lpg.Graph[string, float64], srcKey, dstKey string, edgePos uint64) (instanceIdx, totalCreates, parallelCount int64) {
	totalCreates = g.EdgeCreateCount(srcKey, dstKey)
	if totalCreates == 0 {
		return 0, 0, 0
	}
	adj := g.AdjList()
	srcID, ok := adj.Mapper().Lookup(srcKey)
	if !ok {
		return 0, totalCreates, 0
	}
	dstID, ok := adj.Mapper().Lookup(dstKey)
	if !ok {
		return 0, totalCreates, 0
	}
	fwdCSR := csr.BuildFromAdjList(adj)
	verts := fwdCSR.VerticesSlice()
	edges := fwdCSR.EdgesSlice()
	if uint64(srcID)+1 >= uint64(len(verts)) {
		return 0, totalCreates, 0
	}
	start := verts[uint64(srcID)]
	end := verts[uint64(srcID)+1]
	if edgePos < start || edgePos >= end {
		return 0, totalCreates, 0
	}
	for pos := start; pos <= edgePos; pos++ {
		if pos >= uint64(len(edges)) {
			break
		}
		if edges[pos] == dstID {
			parallelCount++
		}
	}
	// parallelCount now equals the index of edgePos within (srcID, dstID)
	// parallel entries (1-based). Also count remaining occurrences so
	// the caller knows the total parallel storage entries for the pair.
	total := parallelCount
	for pos := edgePos + 1; pos < end; pos++ {
		if pos >= uint64(len(edges)) {
			break
		}
		if edges[pos] == dstID {
			total++
		}
	}
	return parallelCount, totalCreates, total
}

// pickEdgeType chooses the rel-type label to surface for a stored edge.
// LPG merges parallel edges between the same endpoint pair into one entry
// with a label set, so g.EdgeLabels can return more than one label in
// non-deterministic order. When the pattern carries a type filter
// (`r:KNOWS|HATES`), accepted lists the allowed types and pickEdgeType
// returns the first stored label that is also in accepted (deterministic:
// scans stored labels in their EdgeLabels-returned order but prefers any
// accepted match). When accepted is nil or empty, returns the
// alphabetically smallest stored label so the surfaced type is at least
// deterministic across runs. Closes Match2 [6] flake.
func pickEdgeType(stored, accepted []string) string {
	if len(stored) == 0 {
		return ""
	}
	if len(accepted) > 0 {
		acceptSet := make(map[string]struct{}, len(accepted))
		for _, a := range accepted {
			acceptSet[a] = struct{}{}
		}
		for _, s := range stored {
			if _, ok := acceptSet[s]; ok {
				return s
			}
		}
	}
	best := stored[0]
	for _, s := range stored[1:] {
		if s < best {
			best = s
		}
	}
	return best
}

// buildIRProjection converts IR ProjectionItems to a physical Project operator.
// When an item carries a parsed AST expression (item.Expr != nil), the
// executor evaluates it via expr.Eval against a full RowContext — enabling
// property access (n.prop), function calls, and other non-trivial expressions.
// For simple variable references and string-only items the fast schema-lookup
// path is used. The variable fast-path handles plain nodes, relationship
// variables (RelationshipValue reconstruction from edge metadata), and named
// path variables (PathValue reconstruction from flat alternating encoding).
//
// exprContainsAggregate reports whether e or any of its sub-expressions
// invokes one of the openCypher aggregation functions (count/sum/avg/min/
// max/collect/stdev/stdevp/percentileCont/percentileDisc). Used by the
// projection builder to keep the schema-name fast path active for aggregate
// columns even when the projection alias collides with an input variable —
// EagerAggregation upstream has already evaluated the aggregate and stored
// it under the alias name in the schema, so the fast path returns the
// correct value while general eval would re-call the function as a scalar.
func exprContainsAggregate(e ast.Expression) bool {
	if e == nil {
		return false
	}
	switch n := e.(type) {
	case *ast.FunctionInvocation:
		if len(n.Namespace) == 0 {
			switch strings.ToLower(n.Name) {
			case "count", "sum", "avg", "min", "max", "collect",
				"stdev", "stdevp", "percentilecont", "percentiledisc":
				return true
			}
		}
		for _, a := range n.Args {
			if exprContainsAggregate(a) {
				return true
			}
		}
	case *ast.BinaryOp:
		return exprContainsAggregate(n.Left) || exprContainsAggregate(n.Right)
	case *ast.UnaryOp:
		return exprContainsAggregate(n.Operand)
	case *ast.Property:
		return exprContainsAggregate(n.Receiver)
	case *ast.SubscriptExpr:
		return exprContainsAggregate(n.Expr) || exprContainsAggregate(n.Index)
	case *ast.SliceExpr:
		return exprContainsAggregate(n.Expr) || exprContainsAggregate(n.From) || exprContainsAggregate(n.To)
	case *ast.ListLiteral:
		for _, el := range n.Elements {
			if exprContainsAggregate(el) {
				return true
			}
		}
	case *ast.MapLiteral:
		for _, val := range n.Values {
			if exprContainsAggregate(val) {
				return true
			}
		}
	case *ast.CaseExpression:
		if exprContainsAggregate(n.Subject) || exprContainsAggregate(n.ElseExpr) {
			return true
		}
		for _, alt := range n.Alternatives {
			if exprContainsAggregate(alt.Condition) || exprContainsAggregate(alt.Consequent) {
				return true
			}
		}
	}
	return false
}

// exprReferencesVarName reports whether e directly or transitively references
// a *ast.Variable whose Name equals target. Used by the projection builder to
// detect colliding-alias situations where a projection expression references
// the very alias it produces (`RETURN a.id IS NOT NULL AS a`) and the schema
// slot for that alias still holds the upstream variable's value rather than
// the freshly evaluated projection.
func exprReferencesVarName(e ast.Expression, target string) bool {
	if e == nil || target == "" {
		return false
	}
	switch n := e.(type) {
	case *ast.Variable:
		return n.Name == target
	case *ast.Property:
		return exprReferencesVarName(n.Receiver, target)
	case *ast.LabelPredicate:
		return exprReferencesVarName(n.Receiver, target)
	case *ast.BinaryOp:
		return exprReferencesVarName(n.Left, target) || exprReferencesVarName(n.Right, target)
	case *ast.UnaryOp:
		return exprReferencesVarName(n.Operand, target)
	case *ast.FunctionInvocation:
		for _, a := range n.Args {
			if exprReferencesVarName(a, target) {
				return true
			}
		}
	case *ast.SubscriptExpr:
		return exprReferencesVarName(n.Expr, target) || exprReferencesVarName(n.Index, target)
	case *ast.SliceExpr:
		return exprReferencesVarName(n.Expr, target) ||
			exprReferencesVarName(n.From, target) ||
			exprReferencesVarName(n.To, target)
	case *ast.CaseExpression:
		if exprReferencesVarName(n.Subject, target) {
			return true
		}
		for _, alt := range n.Alternatives {
			if exprReferencesVarName(alt.Condition, target) || exprReferencesVarName(alt.Consequent, target) {
				return true
			}
		}
		return exprReferencesVarName(n.ElseExpr, target)
	case *ast.ListLiteral:
		for _, el := range n.Elements {
			if exprReferencesVarName(el, target) {
				return true
			}
		}
	case *ast.MapLiteral:
		for _, val := range n.Values {
			if exprReferencesVarName(val, target) {
				return true
			}
		}
	case *ast.ListComprehension:
		return exprReferencesVarName(n.Source, target) ||
			exprReferencesVarName(n.Predicate, target) ||
			exprReferencesVarName(n.Projection, target)
	case *ast.PatternComprehension:
		return exprReferencesVarName(n.Predicate, target) ||
			exprReferencesVarName(n.Projection, target)
	}
	return false
}

//nolint:gocyclo,cyclop // dispatches over every projection kind and variable type; splitting would obscure the data-flow
func buildIRProjection(
	items []ir.ProjectionItem,
	child exec.Operator,
	schema map[string]int,
	g *lpg.Graph[string, float64],
	params map[string]expr.Value,
	reg expr.FunctionRegistry,
	bopts *buildOpts,
) (*exec.Project, error) {
	projItems := make([]exec.ProjectionItem, len(items))
	// Snapshot the INPUT schema before the loop mutates it. Each projection
	// item's evalFn runs against the INPUT row from the child operator, so
	// fast-path lookups must consult the input schema rather than the
	// progressively-updated live schema. Without this snapshot, an item N
	// taking the fast path on `schema[exprStr]` would resolve to a column
	// index that a PRIOR item set as its OUTPUT position; that index does
	// not address the same value in the INPUT row, so `RETURN a.id AS a,
	// a.id` returned the bound node for the second column instead of the
	// integer property (Return4 [3]).
	inputSchema := copySchema(schema)
	_ = inputSchema // referenced below in fast-path branches
	for i, item := range items {
		name := item.Name
		exprStr := item.Expression

		var evalFn func(exec.Row) (expr.Value, error)
		if item.Expr != nil {
			if v, ok := item.Expr.(*ast.Variable); ok {
				// VLE relationship-list fast path: reconstruct a list of
				// RelationshipValues from the flat alternating ListValue
				// emitted by VarLengthExpand into the rel-variable column.
				if bopts != nil {
					if rmeta, isVLERel := bopts.vleRelMeta[v.Name]; isVLERel {
						capturedMeta := rmeta
						capturedG := g
						evalFn = func(row exec.Row) (expr.Value, error) {
							if capturedMeta.listCol >= len(row) {
								return expr.Null, nil
							}
							lv, ok := row[capturedMeta.listCol].(expr.ListValue)
							if !ok {
								return expr.Null, nil
							}
							if len(lv) == 0 {
								// Empty path (0 hops, possible with *0..0 patterns).
								return expr.ListValue{}, nil
							}
							// Flat format: [srcID, edgePos0, dst0, edgePos1, dst1, …].
							nHops := (len(lv) - 1) / 2
							rels := make(expr.ListValue, 0, nHops)
							srcID := uint64(0)
							if iv, ok2 := lv[0].(expr.IntegerValue); ok2 {
								srcID = uint64(iv)
							}
							for h := 0; h < nHops; h++ {
								edgeID, ok1 := lv[1+2*h].(expr.IntegerValue)
								dstIDVal, ok2 := lv[2+2*h].(expr.IntegerValue)
								if !ok1 || !ok2 {
									continue
								}
								dstID := uint64(dstIDVal)
								et := capturedMeta.edgeType
								var edgeProps expr.MapValue
								if capturedG != nil {
									srcKey, sOK := capturedG.AdjList().Mapper().Resolve(graph.NodeID(srcID))
									dstKey, dOK := capturedG.AdjList().Mapper().Resolve(graph.NodeID(dstID))
									if sOK && dOK {
										if ets := capturedG.EdgeLabels(srcKey, dstKey); len(ets) > 0 {
											et = ets[0]
										} else if ets := capturedG.EdgeLabels(dstKey, srcKey); len(ets) > 0 {
											// Reverse-edge fall-back.
											et = ets[0]
										}
										rawEP := capturedG.EdgeProperties(srcKey, dstKey)
										if len(rawEP) == 0 {
											rawEP = capturedG.EdgeProperties(dstKey, srcKey)
										}
										edgeProps = make(expr.MapValue, len(rawEP))
										for k, pv := range rawEP {
											edgeProps[k] = lpgPropToExpr(pv)
										}
									}
								}
								rels = append(rels, expr.RelationshipValue{
									ID:         uint64(edgeID),
									StartID:    srcID,
									EndID:      dstID,
									Type:       et,
									Properties: edgeProps,
								})
								srcID = dstID
							}
							return rels, nil
						}
					}
				}
				// Path variable fast path: reconstruct PathValue from the flat
				// alternating ListValue(s) emitted by the VarLengthExpand
				// operator(s) — one segment per VLE for a chained pattern
				// (`MATCH p = (a)-[*]->(b)-[*]->(c)`). The segments slice is
				// populated bottom-up so iteration in slice order walks the
				// path left-to-right.
				if bopts != nil && evalFn == nil {
					if pmeta, isPMeta := bopts.pathVarMeta[v.Name]; isPMeta {
						capturedMeta := pmeta
						capturedG := g
						capturedName := v.Name
						capturedSchema := inputSchema
						evalFn = func(row exec.Row) (expr.Value, error) {
							if capturedMeta.listCol >= len(row) {
								return expr.Null, nil
							}
							// Post-aggregation projection: an EagerAggregation
							// that grouped by `p` stored the PathValue directly
							// into the new key column, dropping the flat-list
							// representation pathVarMeta was built against. If
							// the input schema slot for `p` holds a PathValue,
							// forward it unchanged (With6 [4]).
							if capturedSchema != nil {
								if col, ok := capturedSchema[capturedName]; ok && col < len(row) {
									if pv, isPath := row[col].(expr.PathValue); isPath {
										return pv, nil
									}
								}
							}
							segments := capturedMeta.segments
							if len(segments) == 0 {
								// Legacy shape: synthesise a single segment
								// from the top-level listCol/edgeType so the
								// chained reconstruction below covers the
								// uniform code path.
								segments = []pathVarSegment{{
									listCol:  capturedMeta.listCol,
									edgeType: capturedMeta.edgeType,
								}}
							}
							var nodes []expr.NodeValue
							var rels []expr.RelationshipValue
							// Prepend leading fixed-length Expand hops
							// captured during plan build so a path that
							// blends Expand + VLE (Match6 [14]) renders
							// every hop, not just the VLE segment.
							for i, lstep := range capturedMeta.leadingSteps {
								if lstep.edgeCol >= len(row) || lstep.dstCol >= len(row) || lstep.srcCol >= len(row) {
									break
								}
								edgeIDVal, e1 := row[lstep.edgeCol].(expr.IntegerValue)
								dstIDVal, e2 := row[lstep.dstCol].(expr.IntegerValue)
								srcIDVal, e3 := row[lstep.srcCol].(expr.IntegerValue)
								if !e1 || !e2 || !e3 {
									break
								}
								if i == 0 {
									nodes = append(nodes, buildNodeValueFromID(graph.NodeID(srcIDVal), capturedG))
								}
								dstNode := buildNodeValueFromID(graph.NodeID(dstIDVal), capturedG)
								prevID := nodes[len(nodes)-1].ID
								et := lstep.edgeType
								var edgeProps expr.MapValue
								storageStart, storageEnd := prevID, dstNode.ID
								if capturedG != nil {
									sKey, sR := capturedG.AdjList().Mapper().Resolve(graph.NodeID(prevID))
									dKey, dR := capturedG.AdjList().Mapper().Resolve(graph.NodeID(dstNode.ID))
									if sR && dR {
										ets := capturedG.EdgeLabels(sKey, dKey)
										rawEP := capturedG.EdgeProperties(sKey, dKey)
										if len(ets) == 0 && len(rawEP) == 0 {
											ets = capturedG.EdgeLabels(dKey, sKey)
											rawEP = capturedG.EdgeProperties(dKey, sKey)
											if len(ets) > 0 || len(rawEP) > 0 {
												storageStart, storageEnd = dstNode.ID, prevID
											}
										}
										if len(ets) > 0 {
											et = ets[0]
										}
										edgeProps = make(expr.MapValue, len(rawEP))
										for k, pv := range rawEP {
											edgeProps[k] = lpgPropToExpr(pv)
										}
									}
								}
								rels = append(rels, expr.RelationshipValue{
									ID:         uint64(edgeIDVal),
									StartID:    storageStart,
									EndID:      storageEnd,
									Type:       et,
									Properties: edgeProps,
								})
								nodes = append(nodes, dstNode)
							}
							leadingNodeCountInline := len(nodes)
							for segIdx, seg := range segments {
								if seg.listCol >= len(row) {
									return expr.Null, nil
								}
								lv, ok := row[seg.listCol].(expr.ListValue)
								if !ok {
									// PathValue forwarded by an earlier
									// projection; bail with the forwarded
									// value when this is the only segment.
									if pv, isPath := row[seg.listCol].(expr.PathValue); isPath && len(segments) == 1 {
										return pv, nil
									}
									return expr.Null, nil
								}
								if len(lv) == 0 {
									// Empty segment is degenerate: it
									// contributes no hops and no nodes. The
									// chain continues from the previous
									// segment's tail.
									continue
								}
								nHops := (len(lv) - 1) / 2
								if segIdx == 0 && leadingNodeCountInline == 0 {
									if iv, ok2 := lv[0].(expr.IntegerValue); ok2 {
										nodes = append(nodes, buildNodeValueFromID(graph.NodeID(iv), capturedG))
									}
								}
								edgeType := seg.edgeType
								for h := 0; h < nHops; h++ {
									edgePos, ok1 := lv[1+2*h].(expr.IntegerValue)
									dstIDVal, ok2 := lv[2+2*h].(expr.IntegerValue)
									if !ok1 || !ok2 {
										continue
									}
									dstNode := buildNodeValueFromID(graph.NodeID(dstIDVal), capturedG)
									if len(nodes) == 0 {
										// Defensive: a chained segment without
										// a leading-node from segment 0 (e.g.
										// the first segment emitted an empty
										// list and we proceeded). Seed nodes
										// from this segment's src.
										if iv, ok2 := lv[0].(expr.IntegerValue); ok2 {
											nodes = append(nodes, buildNodeValueFromID(graph.NodeID(iv), capturedG))
										}
									}
									prev := nodes[len(nodes)-1]
									nodes = append(nodes, dstNode)
									et := edgeType
									var edgeProps expr.MapValue
									storageStart, storageEnd := prev.ID, dstNode.ID
									if capturedG != nil {
										srcKey, sOK := capturedG.AdjList().Mapper().Resolve(graph.NodeID(prev.ID))
										dstKey, dOK := capturedG.AdjList().Mapper().Resolve(graph.NodeID(dstNode.ID))
										if sOK && dOK {
											ets := capturedG.EdgeLabels(srcKey, dstKey)
											rawEP := capturedG.EdgeProperties(srcKey, dstKey)
											if len(ets) == 0 && len(rawEP) == 0 {
												// Reverse pass of an undirected VLE
												// — Match6 [14]'s `<-[…]-` hops.
												ets = capturedG.EdgeLabels(dstKey, srcKey)
												rawEP = capturedG.EdgeProperties(dstKey, srcKey)
												if len(ets) > 0 || len(rawEP) > 0 {
													storageStart, storageEnd = dstNode.ID, prev.ID
												}
											}
											if len(ets) > 0 {
												et = ets[0]
											}
											edgeProps = make(expr.MapValue, len(rawEP))
											for k, pv := range rawEP {
												edgeProps[k] = lpgPropToExpr(pv)
											}
										}
									}
									rels = append(rels, expr.RelationshipValue{
										ID:         uint64(edgePos),
										StartID:    storageStart,
										EndID:      storageEnd,
										Type:       et,
										Properties: edgeProps,
									})
								}
							}
							if len(nodes) == 0 {
								return expr.Null, nil
							}
							return expr.PathValue{Nodes: nodes, Relationships: rels}, nil
						}
					}
				}
				// Named-path (chain) fast path: reconstruct PathValue from the
				// alternating (srcID, edgeID, dstID) triplets emitted by the
				// fixed-length Expand chain. This covers zero-length
				// (p = (a)), fixed-length (p = (a)-[r]->(b)) and chained
				// (p = (a)-[r1]->(b)-[r2]->(c)) named paths.
				//
				// The fast path is only valid for the FIRST projection above
				// the NamedPath wrapper, while the input row still carries
				// the chain's source columns. Once that projection emits a
				// real PathValue into a new schema slot the entry is removed
				// from pathVarChain so subsequent projections (e.g. RETURN
				// after a WITH) fall through to the regular schema-lookup
				// path.
				if bopts != nil && evalFn == nil {
					if cinfo, isChain := bopts.pathVarChain[v.Name]; isChain {
						capturedInfo := cinfo
						capturedG := g
						capturedName := v.Name
						capturedSchema := inputSchema
						capturedBopts := bopts
						evalFn = func(row exec.Row) (expr.Value, error) {
							// Post-projection forward: if the schema slot for
							// this path variable already carries a PathValue
							// (an earlier projection emitted it into the
							// column), forward it directly. The
							// pathVarChain coordinates only apply to the
							// original chain row layout; after a WITH the
							// column may hold a self-describing PathValue
							// and the chain slots belong to other variables.
							// Without this `WITH … AS p RETURN p` after an
							// aggregating WITH that emitted a ListValue at
							// the p slot would surface NULL (List12 [5]).
							if capturedSchema != nil {
								if col, ok := capturedSchema[capturedName]; ok && col < len(row) {
									switch v := row[col].(type) {
									case expr.PathValue:
										return v, nil
									case expr.ListValue:
										return v, nil
									}
								}
							}
							if capturedInfo.leadingCol >= len(row) {
								return expr.Null, nil
							}
							leadVal := row[capturedInfo.leadingCol]
							if leadVal == nil || expr.IsNull(leadVal) {
								return expr.Null, nil
							}
							// The leading slot may already carry an upgraded
							// NodeValue (when an earlier projection ran) or a
							// raw IntegerValue (from a scan). Both shapes are
							// accepted.
							var leadNode expr.NodeValue
							switch lv := leadVal.(type) {
							case expr.NodeValue:
								leadNode = lv
							case expr.IntegerValue:
								leadNode = buildNodeValueFromID(graph.NodeID(lv), capturedG)
							default:
								return expr.Null, nil
							}
							nodes := make([]expr.NodeValue, 0, len(capturedInfo.steps)+1)
							rels := make([]expr.RelationshipValue, 0, len(capturedInfo.steps))
							nodes = append(nodes, leadNode)
							for _, step := range capturedInfo.steps {
								if step.edgeCol >= len(row) || step.dstCol >= len(row) {
									return expr.Null, nil
								}
								// Accept both IntegerValue (Expand emits raw edge
								// ids) and RelationshipValue (MergeRelationship
								// emits a fully-populated rel into the column).
								var edgeIDVal expr.IntegerValue
								var ok1 bool
								switch ev := row[step.edgeCol].(type) {
								case expr.IntegerValue:
									edgeIDVal, ok1 = ev, true
								case expr.RelationshipValue:
									edgeIDVal, ok1 = expr.IntegerValue(ev.ID), true
								}
								dstIDVal, ok2 := row[step.dstCol].(expr.IntegerValue)
								if !ok1 || !ok2 {
									// OPTIONAL hops or otherwise-missing
									// columns collapse the path to NULL,
									// matching openCypher semantics.
									return expr.Null, nil
								}
								dstNode := buildNodeValueFromID(graph.NodeID(dstIDVal), capturedG)
								et := step.edgeType
								var edgeProps expr.MapValue
								// pathStart/pathEnd track the path's traversal
								// order (preceding node → current dst). The
								// edge's storage direction may run either way;
								// when the graph carries edges in BOTH
								// directions between the endpoint pair (Match6
								// [12]'s `a:A -[:T1]-> b:B` + `b:B -[:T2]->
								// a:A`), probing EdgeLabels(pathStart, pathEnd)
								// alone returns whichever direction happens
								// to be present, even when the row's edge ID
								// references the OPPOSITE direction. Resolve
								// the edge by ID via the bopts resolver to
								// get the true storage endpoints.
								pathStart := nodes[len(nodes)-1].ID
								pathEnd := dstNode.ID
								storageStart := pathStart
								storageEnd := pathEnd
								if resolver := ensureEdgeIDResolver(capturedBopts, capturedG); resolver != nil {
									if rss, rsd, ok := resolver(uint64(edgeIDVal)); ok {
										storageStart, storageEnd = rss, rsd
									}
								}
								if capturedG != nil {
									sKey, sOK := capturedG.AdjList().Mapper().Resolve(graph.NodeID(storageStart))
									dKey, dOK := capturedG.AdjList().Mapper().Resolve(graph.NodeID(storageEnd))
									if sOK && dOK {
										if ets := capturedG.EdgeLabels(sKey, dKey); len(ets) > 0 {
											et = ets[0]
											rawEP := capturedG.EdgeProperties(sKey, dKey)
											edgeProps = make(expr.MapValue, len(rawEP))
											for k, pv := range rawEP {
												edgeProps[k] = lpgPropToExpr(pv)
											}
										}
									}
								}
								rels = append(rels, expr.RelationshipValue{
									ID:         uint64(edgeIDVal),
									StartID:    storageStart,
									EndID:      storageEnd,
									Type:       et,
									Properties: edgeProps,
								})
								nodes = append(nodes, dstNode)
							}
							return expr.PathValue{Nodes: nodes, Relationships: rels}, nil
						}
						// Subsequent projections over the same name read the
						// freshly-projected PathValue column directly via the
						// schema-slot fast-path (round-59 forward) when the
						// row[col] is already a PathValue. The pathVarChain
						// entry is left in place so any pre-projection that
						// runs at runtime BEFORE this projection emits its
						// row (e.g. an aggregation's pre-projection
						// evaluating collect(p) when this RETURN was built
						// in plan-build order) can still reconstruct the
						// PathValue from the chain's original column
						// positions. Deleting at plan-build time was an
						// optimisation that broke nested-aggregate-in-list
						// comprehension cases (List12 [5]).
					}
				}
				// Edge variable fast path: reconstruct RelationshipValue from
				// the three-column triplet (srcID, edgeID, dstID) emitted by
				// the Expand operator.
				if bopts != nil && evalFn == nil {
					if meta, isMeta := bopts.edgeVarMeta[v.Name]; isMeta {
						capturedMeta := meta
						capturedG := g
						capturedName := v.Name
						capturedAlias := name
						capturedSchema := inputSchema
						evalFn = func(row exec.Row) (expr.Value, error) {
							// Post-projection forward: if the input schema
							// slot for this rel variable already carries a
							// RelationshipValue (an earlier projection
							// emitted it into the column), use that
							// directly. The edgeVarMeta srcCol/edgeCol/
							// dstCol coordinates only apply to the Expand-
							// emitted triplet shape; after a WITH the
							// column holds a self-describing
							// RelationshipValue and the triplet slots
							// belong to other variables. Without this
							// short-circuit `MATCH ()-[r]->() WITH r AS
							// r2 MATCH ()-[r2]->() RETURN r2` and the
							// `MATCH … WITH a, r, b … RETURN r` shape
							// surface the wrong edge.
							if capturedSchema != nil {
								if col, ok := capturedSchema[capturedName]; ok && col < len(row) {
									if rv, isRel := row[col].(expr.RelationshipValue); isRel {
										return rv, nil
									}
								}
								// Alias-rename forward: when the projection
								// renames the rel variable (`r1 AS r2`),
								// the input schema carries the renamed
								// column under the ALIAS name. An upstream
								// EagerAggregation that emitted the rel
								// at its grouping-key column also stores
								// the renamed value under the alias. Probe
								// that slot before the triplet
								// reconstruction (whose coordinates are
								// pre-rename and would address other
								// columns in the post-aggregation row).
								if capturedAlias != "" && capturedAlias != capturedName {
									if col, ok := capturedSchema[capturedAlias]; ok && col < len(row) {
										if rv, isRel := row[col].(expr.RelationshipValue); isRel {
											return rv, nil
										}
									}
								}
							}
							if capturedMeta.edgeCol >= len(row) {
								return expr.Null, nil
							}
							edgeIDVal, ok := row[capturedMeta.edgeCol].(expr.IntegerValue)
							if !ok {
								return expr.Null, nil
							}
							edgeID := uint64(edgeIDVal)
							var srcID, dstID uint64
							if capturedMeta.srcCol < len(row) {
								if iv, ok2 := row[capturedMeta.srcCol].(expr.IntegerValue); ok2 {
									srcID = uint64(iv)
								}
							}
							if capturedMeta.dstCol < len(row) {
								if iv, ok2 := row[capturedMeta.dstCol].(expr.IntegerValue); ok2 {
									dstID = uint64(iv)
								}
							}
							// Resolve edge type from the graph if not statically known.
							// For the reverse-edge pass of an undirected expansion
							// the row carries srcID=patternEndpoint and
							// dstID=patternSource; storage holds the edge in the
							// opposite direction. Probe both directions so the
							// reverse pass still carries the relationship's Type
							// and Properties.
							edgeType := capturedMeta.edgeType
							var edgeProps expr.MapValue
							storageStart, storageEnd := srcID, dstID
							// Look up the edge's labels and properties from the
							// live graph. The previous `srcID != 0` guard
							// silently skipped the lookup when the source
							// node's internal ID was 0 — a valid id in
							// sequential allocators (used by the TCK runner),
							// which left the RelationshipValue without its
							// Type. Closes Pattern2 [11] (TCK's first node
							// gets id 0, so the forward path lost `:T`).
							//
							// When the forward direction has no labels but
							// the reverse does, the edge is stored in the
							// opposite direction (undirected MATCH reverse
							// pass). Swap StartID/EndID to reflect the
							// storage direction so the PathValue renderer
							// emits `<-[…]-` for this hop instead of `-[…]->`
							// (Match6 [12]/[13] direction fix).
							if capturedG != nil {
								srcKey, srcResolved := capturedG.AdjList().Mapper().Resolve(graph.NodeID(srcID))
								dstKey, dstResolved := capturedG.AdjList().Mapper().Resolve(graph.NodeID(dstID))
								if srcResolved && dstResolved {
									ets := capturedG.EdgeLabels(srcKey, dstKey)
									rawEP := capturedG.EdgeProperties(srcKey, dstKey)
									if len(ets) == 0 && len(rawEP) == 0 {
										ets = capturedG.EdgeLabels(dstKey, srcKey)
										rawEP = capturedG.EdgeProperties(dstKey, srcKey)
										if len(ets) > 0 || len(rawEP) > 0 {
											storageStart, storageEnd = dstID, srcID
										}
									}
									if len(ets) > 0 {
										edgeType = pickEdgeType(ets, capturedMeta.acceptedTypes)
									}
									edgeProps = make(expr.MapValue, len(rawEP))
									for k, pv := range rawEP {
										edgeProps[k] = lpgPropToExpr(pv)
									}
								}
							}
							return expr.RelationshipValue{
								ID:         edgeID,
								StartID:    storageStart,
								EndID:      storageEnd,
								Type:       edgeType,
								Properties: edgeProps,
							}, nil
						}
					}
				}
				if evalFn == nil {
					// Node variable fast path: direct column lookup, with an in-line
					// IntegerValue(NodeID) → NodeValue upgrade so bare `RETURN u` for
					// a bound node produces the documented shape. Non-node
					// IntegerValues (literals, edge IDs) pass through unchanged.
					//
					// Exception: scalar columns (aggregate outputs such as count(*),
					// sum, avg) must NOT be upgraded — an integer count that
					// numerically equals a real NodeID would be mis-elevated into a
					// full graph node. bopts.scalarCols tracks which variable names
					// were produced by EagerAggregation and must pass through as-is.
					if colIdx, ok2 := schema[v.Name]; ok2 {
						idx := colIdx
						capturedG := g
						varIsScalar := bopts != nil && bopts.scalarCols != nil
						if varIsScalar {
							_, varIsScalar = bopts.scalarCols[v.Name]
						}
						if !varIsScalar && bopts != nil && bopts.projAliasScalarCols != nil {
							if _, ok3 := bopts.projAliasScalarCols[v.Name]; ok3 {
								varIsScalar = true
							}
						}
						// aggKeyScalarCols: a non-Variable EagerAggregation
						// grouping key (e.g. `WITH a.num2 % 3 AS mod`) stores
						// the computed integer at the post-aggregation `mod`
						// column. Reading `mod` via the Variable fast path
						// must NOT upgrade that integer to a NodeValue when
						// it numerically coincides with an interned NodeID.
						// Only the post-aggregation Variable read consults
						// this set; the pre-projection's buildRowCtxFromMutator
						// does not, so the grouping expression itself can
						// still see `a` as a NodeValue (closes WithOrderBy4
						// [12] without regressing Return6 [1]).
						if !varIsScalar && bopts != nil && bopts.aggKeyScalarCols != nil {
							if _, ok3 := bopts.aggKeyScalarCols[v.Name]; ok3 {
								varIsScalar = true
							}
						}
						if varIsScalar {
							// Scalar aggregate output: return the raw value without
							// upgrade. Integer counts/sums can numerically coincide with
							// a real NodeID and must not be elevated to NodeValue.
							evalFn = func(row exec.Row) (expr.Value, error) {
								if idx < len(row) {
									return row[idx], nil
								}
								return expr.Null, nil
							}
						} else {
							evalFn = func(row exec.Row) (expr.Value, error) {
								if idx < len(row) {
									return upgradeNodeIDToValue(row[idx], capturedG), nil
								}
								return expr.Null, nil
							}
						}
					}
				}
			}
			if evalFn == nil {
				// Schema-name fast path: when an upstream operator (e.g.
				// EagerAggregation) has pre-computed and named the output
				// column, prefer a direct index lookup over expression
				// re-evaluation. This avoids calling aggregate functions as
				// scalar functions. The IntegerValue(NodeID) → NodeValue
				// upgrade is deliberately NOT applied here: aggregate
				// results (count(*), sum(...), etc.) are scalar integers
				// that can numerically collide with a real NodeID and would
				// be mis-upgraded into a node row.
				//
				// Soundness guard: the fast path is only safe when the
				// schema slot already carries the projection's output
				// value. That holds in three cases:
				//   (a) name is registered as a scalar aggregate output
				//       column (bopts.scalarCols) — EagerAggregation
				//       pre-computed the column under this name.
				//   (b) the expression string equals the alias text —
				//       no transformation is required.
				//   (c) exprStr is empty — no AST expression was
				//       carried, so direct lookup is the only option.
				// When none of those hold the alias may be shadowing a
				// pre-projection variable whose slot still holds the
				// source value (e.g. RETURN n.name AS n, where schema[n]
				// is the bound node); the fast path would return that
				// stale value instead of the projected one, so the
				// general eval path runs instead.
				//
				// Edge-variable carve-out: when the alias name maps to a
				// bound relationship variable (i.e. the schema slot carries
				// an Expand-emitted edge id rather than the projection's
				// evaluated value), the fast path is unsound. Without this
				// carve-out, `RETURN type(r) AS r` would bypass evaluation
				// of `type(r)` and return the raw IntegerValue edge id
				// instead of the relationship type label.
				aliasIsBoundRel := false
				if bopts != nil && bopts.edgeVarMeta != nil {
					_, aliasIsBoundRel = bopts.edgeVarMeta[name]
				}
				// Narrow soundness guard: when the item is a property
				// access whose alias EXACTLY equals the property's
				// receiver name (e.g. `RETURN a.id AS a`, where
				// schema[a] still holds the bound node), bypass the
				// fast path so general eval computes the property
				// value. Other Property shapes keep the fast path
				// because they reuse the same alias name and the
				// schema slot already carries the projected value.
				//
				// Map-literal extension: a projection item whose
				// expression is a *ast.MapLiteral and whose alias
				// collides with a pre-existing schema entry that
				// holds a bound node (`WITH {first: m.id} AS m`) is
				// the same shape — the schema-name fast path would
				// return the original bound node, not the freshly
				// constructed map.
				skipForCollidingAlias := false
				// Preprojected schema slots already carry the projection-
				// equivalent value (e.g. an EagerAggregation grouping key)
				// — the fast path is sound and skipColliding must not fire.
				isPreprojSlot := false
				if bopts != nil && bopts.preprojectedCols != nil {
					_, isPreprojSlot = bopts.preprojectedCols[name]
				}
				if prop, isProp := item.Expr.(*ast.Property); isProp && exprStr != name && !isPreprojSlot {
					if recv, recvIsVar := prop.Receiver.(*ast.Variable); recvIsVar && recv.Name == name {
						skipForCollidingAlias = true
					}
				} else if _, isMap := item.Expr.(*ast.MapLiteral); isMap && exprStr != name {
					if _, exists := schema[name]; exists {
						skipForCollidingAlias = true
					}
				} else if exprStr != name {
					// Generalised colliding-alias guard: when the projection
					// expression renames a value (`<expr> AS x`) and `x`
					// already exists in the INPUT schema (typically because a
					// prior WITH x... is being shadowed), the schema-name
					// fast path would silently return the upstream value
					// instead of computing the new expression. Route
					// through the general eval path so the new value is
					// projected.
					//
					// Aggregations are exempt: count/sum/avg/etc. are
					// precomputed by EagerAggregation upstream and the
					// schema slot already carries their evaluated value.
					// Falling through to evalRow would re-evaluate them
					// as scalar functions and return the per-row count
					// (always 1) instead of the group's aggregate.
					//
					// Preprojected columns are also exempt: an
					// EagerAggregation grouping key already carries the
					// pre-evaluated grouping expression value in the row
					// slot. The fast path returns that value directly;
					// routing through general eval would re-interpret the
					// variable as its pre-aggregation form.
					//
					// Only the BinaryOp / UnaryOp / arithmetic shapes are
					// flagged here. A bare-Variable expression (`WITH x AS
					// x`) takes the same value either way; a Property/
					// MapLiteral has its own dedicated branch above.
					isPreproj := false
					if bopts != nil && bopts.preprojectedCols != nil {
						_, isPreproj = bopts.preprojectedCols[name]
					}
					isScalar := false
					if bopts != nil && bopts.scalarCols != nil {
						_, isScalar = bopts.scalarCols[name]
					}
					if _, exists := schema[name]; exists && !isPreproj && !isScalar {
						// Case A: the expression references the alias name —
						// the fast path would return the OLD value, but the
						// expression intends to read the OLD value as input
						// and produce a NEW transformed value. Route to
						// general eval (already covered by exprReferencesVarName).
						if exprReferencesVarName(item.Expr, name) && !exprContainsAggregate(item.Expr) {
							skipForCollidingAlias = true
						}
						// Case B: the expression does NOT reference the alias
						// name but still produces a new value (a WITH cascade
						// of two projections that both bind `x`, where the
						// second projection computes a fresh expression that
						// happens to be independent of x). Route to general
						// eval for any computed expression shape; bare
						// Variable / Literal / Parameter projections keep the
						// fast path because their value matches the slot.
						if !skipForCollidingAlias && !exprContainsAggregate(item.Expr) {
							switch item.Expr.(type) {
							case *ast.BinaryOp, *ast.UnaryOp, *ast.FunctionInvocation,
								*ast.SubscriptExpr, *ast.SliceExpr, *ast.CaseExpression,
								*ast.ListComprehension, *ast.PatternComprehension,
								*ast.ListLiteral, *ast.LabelPredicate:
								skipForCollidingAlias = true
							}
						}
					}
				}
				if !aliasIsBoundRel && !skipForCollidingAlias {
					if colIdx, ok2 := schema[name]; ok2 {
						idx := colIdx
						evalFn = func(row exec.Row) (expr.Value, error) {
							if idx < len(row) {
								return row[idx], nil
							}
							return expr.Null, nil
						}
					}
				}
			}
			if evalFn == nil {
				// General path: evaluate full AST expression with loaded RowContext.
				schemaSnap := copySchema(schema)
				capturedExpr := item.Expr
				capturedG := g
				capturedParams := params
				capturedReg := reg
				capturedBopts := bopts
				evalFn = func(row exec.Row) (expr.Value, error) {
					rowCtx := buildRowCtx(row, schemaSnap, capturedG, capturedBopts)
					return evalRow(capturedBopts, capturedExpr, rowCtx, capturedParams, capturedReg)
				}
			}
		} else if colIdx, ok := schema[exprStr]; ok {
			// String-only projection (no AST expression). Aggregate aliases
			// and pre-aggregated columns land here, so we cannot safely
			// upgrade IntegerValue → NodeValue (a scalar count that numerically
			// matches a NodeID would be mis-upgraded).
			idx := colIdx
			evalFn = func(row exec.Row) (expr.Value, error) {
				if idx < len(row) {
					return row[idx], nil
				}
				return expr.Null, nil
			}
		} else if colIdx, ok := schema[name]; ok {
			// Alias fallback. Same caveat as above — no kind information, no
			// upgrade.
			idx := colIdx
			evalFn = func(row exec.Row) (expr.Value, error) {
				if idx < len(row) {
					return row[idx], nil
				}
				return expr.Null, nil
			}
		} else {
			evalFn = func(_ exec.Row) (expr.Value, error) { return expr.Null, nil }
		}

		// The per-item schema update is deferred to the post-projection
		// reset below. Updating schema in-place during the loop would leak
		// item i's OUTPUT column index into item i+1's fast-path lookups,
		// which run against the INPUT row (e.g. `RETURN a.id AS a, a.id`:
		// item 0 sets schema[a.id]=0 as a secondary key for ORDER BY, then
		// item 1 falls through to schema[a.id]=0 and returns INPUT row[0] =
		// bound NodeValue instead of evaluating a.id against it). The
		// inputSchema snapshot taken before the loop still informs the
		// fast-path branches that need to see the input layout.
		projItems[i] = exec.ProjectionItem{Alias: name, Eval: evalFn}
	}
	// Tag every computed (non-Variable) projection alias in scalarCols so
	// downstream operators (Sort, Limit, RETURN) reading the column do not
	// re-upgrade its integer value to a NodeValue when it numerically
	// coincides with an existing node id (WithSkipLimit3 [3]: `WITH
	// a.count AS count` with count=14 was surfacing as `({count: 14})`
	// instead of the integer 14).
	//
	// Skip aliases that shadow an input-schema variable that is NOT
	// already tagged scalar — the upstream still treats that name as a
	// bound entity, and a Selection / pre-projection that reads it via
	// buildRowCtx would otherwise see scalarCols[name] and incorrectly
	// skip the entity upgrade. Closes the round-52 / round-56 collision
	// pattern (TestMerge_OnMatchSet's `count(n) AS n` would have flipped
	// Selection on `n` to a no-upgrade path).
	if bopts != nil {
		for _, item := range items {
			if item.Expr == nil {
				continue
			}
			if _, isVar := item.Expr.(*ast.Variable); isVar {
				continue
			}
			// Skip when the alias name was already in the INPUT schema as
			// a bound entity (where a Selection below this projection
			// might still need to upgrade the variable to a NodeValue /
			// RelationshipValue). Adding such an alias to
			// projAliasScalarCols would taint pre-projection closures
			// that captured bopts and read it at runtime.
			if _, shadowsInput := inputSchema[item.Name]; shadowsInput {
				continue
			}
			if bopts.projAliasScalarCols == nil {
				bopts.projAliasScalarCols = make(map[string]struct{})
			}
			bopts.projAliasScalarCols[item.Name] = struct{}{}
		}
	}
	// Post-projection schema reset: the live row has exactly one column per
	// projection item at indices 0..len(items)-1. Stale entries from the
	// upstream pipeline (e.g. an UNWIND element variable that the projection
	// dropped) MUST be removed from the shared schema map; otherwise
	// schemaWidth(schema) returns a wider value than the actual row, and
	// downstream operators that allocate fresh columns via that helper
	// (Apply, Expand, AllNodesScan, …) mis-offset their bindings.
	//
	// The reset preserves only the alias→index mapping and the secondary
	// expression-string keys registered above; everything else is dropped.
	// Two passes so alias keys are not overwritten by another item's
	// secondary expression key when their names collide. For example
	// `WITH a AS b, b AS tmp` would otherwise let item 1's expression
	// key `b` overwrite item 0's alias key `b` and put the post-
	// projection slot for `b` at item 1's index — which then surfaces
	// item 1's value (`b`'s pre-projection value) instead of item 0's
	// (`a`'s pre-projection value). Closes With7 [1]'s rename-swap
	// chain.
	keep := make(map[string]int, len(items)*2)
	aliasNames := make(map[string]struct{}, len(items))
	for _, item := range items {
		aliasNames[item.Name] = struct{}{}
	}
	for i, item := range items {
		keep[item.Name] = i
	}
	for i, item := range items {
		if item.Expression == "" || item.Expression == item.Name {
			continue
		}
		if _, isAlias := aliasNames[item.Expression]; isAlias {
			continue
		}
		keep[item.Expression] = i
	}
	for k := range schema {
		if _, ok := keep[k]; !ok {
			delete(schema, k)
		}
	}
	for k, v := range keep {
		schema[k] = v
	}
	return exec.NewProject(child, projItems)
}

// execLabelAdapter bridges labelResolverIface to the exec.labelResolver
// interface (which uses *roaring64.Bitmap). Both have identical method
// signatures; this thin wrapper avoids an import cycle.
type execLabelAdapter struct {
	labelSrc labelResolverIface
}

// ResolveLabelBitmap implements exec.labelResolver.
func (a *execLabelAdapter) ResolveLabelBitmap(name string) *roaring64.Bitmap {
	return a.labelSrc.ResolveLabelBitmap(name)
}

// ─────────────────────────────────────────────────────────────────────────────
// RunInTx — write-aware query execution
// ─────────────────────────────────────────────────────────────────────────────

// RunInTx executes a write query against the engine's graph and returns a
// streaming [Result]. Unlike [Run], RunInTx inspects the IR plan for write
// operators; when any write operator is present it builds a mutator adapter so
// that write operators can modify the graph.
//
// For the current in-memory implementation there is no external transaction
// manager (lpg.Graph does not support rollback). "Commit on success, rollback
// on error" means: the pipeline runs to completion with mutations applied
// eagerly; if any operator returns an error the pipeline is drained no further
// (standard Volcano error propagation) and the partial mutations remain in the
// graph. This matches the single-writer, in-memory contract documented in
// CLAUDE.md.
//
// RunInTx is safe for concurrent use (each call creates an independent
// operator tree), subject to the single-writer constraint on write queries.
//
// If ctx is already cancelled or its deadline has elapsed when RunInTx is
// called, it returns promptly — before any parse, plan, or [txn.Store.Begin]
// work — with an error wrapping the context error (matchable via [errors.Is]
// against [context.Canceled] / [context.DeadlineExceeded]).
func (e *Engine) RunInTx(ctx context.Context, query string, params map[string]expr.Value) (res *Result, err error) {
	defer cmetrics.Time("cypher.RunInTx")()
	defer func() {
		if err != nil {
			cmetrics.IncCounter("cypher.RunInTx.errors", 1)
		}
	}()
	// walTx holds the store's single-writer mutex from Begin() (below) until it
	// is rolled back or handed to the Result for Commit/Rollback in
	// Result.Close. It is declared here, before the recover boundary registers,
	// so recoverWriteQueryPanic can roll it back on a panic raised anywhere
	// after Begin — releasing the single-writer mutex that would otherwise
	// deadlock every future write (ACID atomicity + liveness). On the normal
	// build-error path the explicit Rollback below still applies.
	var walTx *txn.Tx[string, float64]
	// Registered last so it runs first on unwind: a recovered panic rolls back
	// walTx and sets err before the cypher.RunInTx.errors counter defer above
	// observes it. RunInTxAny delegates here, so it is covered transitively.
	defer recoverWriteQueryPanic(&err, &walTx, "cypher.RunInTx", "cypher.RunInTx.panics")
	// Honour an already-cancelled/expired context before any synchronous parse,
	// plan, or Begin work. Placed after the metrics/recover defers so a
	// cancellation is still timed and counted (cypher.RunInTx.errors)
	// consistently, but before parseAndAnalyse (and before any txn.Store.Begin)
	// so a caller that has already given up never pays for the parse and never
	// opens a write transaction. O(1) and allocation-free on the happy path.
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	// DDL queries don't require a write transaction.
	if ir.IsDDL(query) {
		return e.runDDL(ctx, query)
	}

	// Freeze a per-query "now" so all temporal constructors (date(), time(),
	// localtime(), datetime(), localdatetime()) observe the same instant within
	// this statement — openCypher requirement (closes Temporal10 [12] flake
	// `RETURN duration.inSeconds(localtime(), localtime())` whose two
	// localtime() calls otherwise advance by one tick).
	//
	// The registry wrapper captures the frozen time per-query and overrides
	// only the zero-argument forms of those five functions, so concurrent
	// Engine.RunInTx calls never race on the process-global statementNow
	// in funcs. (statementNow is still used by the TCK runner and standalone
	// unit tests via funcs.SetStatementNow; see cypher/funcs/now.go.)
	queryReg := newNowAwareRegistry(e.reg, time.Now())

	entry, err := e.parseAndAnalyse(query)
	if err != nil {
		return nil, err
	}
	// Sema fast-path: short-circuit scope violations before opening a tx.
	if entry.semaErr != nil {
		return nil, entry.semaErr
	}
	plan := entry.plan

	if err := e.checkParamTypes(plan, params); err != nil {
		return nil, err
	}

	buf := &exec.IndexBuffer{}

	// undo records the inverse of every in-memory mutation this write statement
	// applies eagerly, so the live graph can be rolled back inside the barrier
	// on a pipeline error or panic (task #1282, Atomicity). It is allocated for
	// every write transaction but its backing slice grows lazily on the first
	// recorded mutation, so a write that mutates nothing (or a read misrouted
	// here) pays nothing.
	undo := &undoLog{}

	// Serialise store-less autocommit writes on the engine writer mutex so they
	// honour the same single-writer contract an explicit transaction relies on:
	// without it, a store-less autocommit write could race an open explicit
	// transaction's writer-mutex hold (#1280 write-write isolation). For a
	// WAL-backed engine lockWriter is a no-op (Begin below takes the store
	// mutex), so the lock is taken at most once on either wiring.
	unlockWriter := e.lockWriter()
	defer unlockWriter()

	// The WAL transaction is opened OUTSIDE the visibility barrier: BeginCtx
	// takes the store's single-writer lock and must not nest under visMu. The
	// acquire is context-aware: under write contention a caller with a
	// deadline gets back the context error the instant ctx is cancelled or
	// expires, instead of blocking on the lock for the holder's full duration
	// (backpressure honoured at the engine↔txn seam, task #1301). The mutator
	// adapter only captures references; no graph reads happen yet.
	var mutator exec.GraphMutator
	if e.store != nil {
		walTx, err = e.store.BeginCtx(ctx)
		if err != nil {
			return nil, err
		}
		mutator = &walMutatorAdapter{g: e.g, tx: walTx, buf: buf, undo: undo}
	} else {
		mutator = &lpgMutatorAdapter{g: e.g, buf: buf, undo: undo}
	}

	r, buildErr := e.execUnderBarrier(ctx, plan, queryReg, params, mutator, buf, undo, walTx, true)
	if buildErr != nil {
		if walTx != nil {
			_ = walTx.Rollback()
		}
		return nil, fmt.Errorf("cypher: build plan: %w", buildErr)
	}
	// An in-barrier WAL fsync failure rolls the write back (in-memory undo
	// replayed, index buffer and WAL transaction rolled back, all under the
	// barrier) and records walErr: report it as a failed statement rather than
	// hand back a Result for a write that is neither visible nor durable
	// (#1281, Durability). The Result is discarded; its ResultSet/finalizer are
	// harmless on the now-empty graph, but close it eagerly to release promptly.
	if r != nil && r.walErr != nil {
		werr := r.walErr
		_ = r.Close()
		return nil, fmt.Errorf("cypher: commit WAL: %w", werr)
	}
	return r, nil
}

// execUnderBarrier builds the physical operator tree for plan and runs the
// whole statement to a materialised [Result] inside one [lpg.Graph.ApplyAtomically]
// (visMu) acquisition — the shared core of both the autocommit [Engine.RunInTx]
// path and the explicit-transaction [ExplicitTx.Exec] path.
//
// Running build + drain under visMu stops a concurrent reader observing a torn
// snapshot and stops a concurrent writer growing the node space mid-build
// (#1077), and makes every eager mutation flip visible to [lpg.Graph.View]
// readers atomically (audit gap F3, docs/isolation-design.md). build runs under
// visMu.Lock, so nothing in it may call g.View / g.ApplyAtomically (visMu is
// non-re-entrant).
//
// commit selects the transaction-finalisation behaviour:
//
//   - commit == true (autocommit RunInTx): the statement is its own transaction.
//     The Result carries buf, walTx, and undo, and commitUnderBarrier runs at the
//     end of the barrier so the WAL is fsynced FIRST and the index buffer
//     committed (durable-then-visible, #1281), or — on a drain error or fsync
//     failure — the undo log is replayed and the index/WAL rolled back, all
//     inside the barrier (#1282). The caller inspects r.walErr.
//
//   - commit == false (explicit-tx Exec): the statement is one of several in a
//     larger transaction owned by an [ExplicitTx]. The eager mutations are
//     applied (and recorded into the SHARED undo, buf, and walTx the handle
//     owns) but NOT committed: the returned Result is a pure read-back of the
//     materialised rows with NO transaction authority (its buf and tx are nil
//     and its undo is unset), so closing it never commits or rolls back. The
//     handle's Commit / Rollback finalises buf, walTx, and the accumulated undo
//     exactly once, later. A per-statement drain error is surfaced via the
//     Result but does NOT trigger a rollback here — the Bolt session decides
//     whether to roll the whole transaction back (#1309).
//
// In BOTH modes a panic raised mid-statement is handled by replayUndoOnPanic
// inside the barrier: it replays the (possibly multi-statement) accumulated
// undo while visMu is still held — so no reader observes the partial
// transaction — and re-raises so the caller's own recover (recoverWriteQueryPanic)
// rolls back walTx, releases the writer serialisation, and converts the panic to
// [ErrInternalPanic]. After such a panic the undo log is emptied (replay is
// idempotent), so a subsequent handle Rollback is a clean no-op against it.
func (e *Engine) execUnderBarrier(
	ctx context.Context,
	plan ir.LogicalPlan,
	queryReg expr.FunctionRegistry,
	params map[string]expr.Value,
	mutator exec.GraphMutator,
	buf *exec.IndexBuffer,
	undo *undoLog,
	walTx *txn.Tx[string, float64],
	commit bool,
) (r *Result, buildErr error) {
	_ = e.g.ApplyAtomically(func() error {
		// Roll the in-memory graph back BEFORE this panic leaves the barrier; see
		// the type-level note above and replayUndoOnPanic for why this must run
		// while visMu is still held.
		defer replayUndoOnPanic(undo)
		walker := &lpgNodeWalker{g: e.g}
		labelSrc := &lpgLabelResolver{g: e.g}
		op, cols, berr := buildPlanWithMutatorFull(plan, walker, labelSrc, queryReg, params, mutator, e.constraintReg, e.g.IndexManager(), e.maxCollectItems)
		if berr != nil {
			buildErr = berr
			return nil
		}
		rs := exec.Run(ctx, op, cols)
		if commit {
			// Autocommit: the Result owns the transaction and finalises it here.
			r = newResultWithLimit(rs, cols, buf, e.g.IndexManager(), walTx, e.maxResultRows, e.maxResultBytes)
			r.undo = undo
			r.materialize()
			r.commitUnderBarrier()
			return nil
		}
		// Explicit-tx statement: build a read-back-only Result (no buf, no tx, no
		// undo) so closing it never commits or rolls back. The mutator still
		// recorded every mutation into the handle's shared buf / walTx / undo, so
		// the handle's later Commit / Rollback is authoritative. Materialise under
		// the barrier so the statement observes a consistent snapshot and its
		// eager writes flip visible atomically with the rest of the open tx.
		r = newResultWithLimit(rs, cols, nil, nil, nil, e.maxResultRows, e.maxResultBytes)
		r.materialize()
		return nil
	})
	return r, buildErr
}

// ─────────────────────────────────────────────────────────────────────────────
// lpgMutatorAdapter — exec.graphMutator backed by *lpg.Graph[string,float64]
// ─────────────────────────────────────────────────────────────────────────────

// lpgMutatorAdapter adapts *lpg.Graph[string, float64] to the
// exec.graphMutator interface used by write operators.
//
// When buf is non-nil every mutation is also enqueued as an index.Change.
// buf is nil for read-only adapter instances.
//
// The embedded mutationUndo records the inverse of each mutation when undo is
// non-nil (write transactions via [Engine.RunInTx]), so a failed statement's
// eager in-memory writes are rolled back inside the visibility barrier (#1282).
// undo is nil for read-only adapter instances, making every record* a no-op.
type lpgMutatorAdapter struct {
	g    *lpg.Graph[string, float64]
	buf  *exec.IndexBuffer // nil for read-only
	undo *undoLog          // nil for read-only / non-transactional
}

// rec returns the inverse-recording helper bound to this adapter's graph and
// undo log.
func (a *lpgMutatorAdapter) rec() mutationUndo { return mutationUndo{g: a.g, undo: a.undo} }

// resolveID translates n to its stable NodeID, returning graph.NodeID(0)
// when the key has not been interned yet.
func (a *lpgMutatorAdapter) resolveID(n string) graph.NodeID {
	id, ok := a.g.AdjList().Mapper().Lookup(n)
	if !ok {
		return graph.NodeID(0)
	}
	return id
}

// AddNode interns n and returns its stable NodeID.
func (a *lpgMutatorAdapter) AddNode(n string) (graph.NodeID, error) {
	idBefore, existed := a.g.AdjList().Mapper().Lookup(n)
	// Capture the tombstone state BEFORE AddNode: AddNode now revives a
	// tombstoned node (clears its tombstone), so checking afterwards would
	// always observe the node live. Re-creating a removed key counts as a
	// fresh creation for side-effect bookkeeping.
	if existed && a.g.IsTombstoned(idBefore) {
		existed = false
	}
	if err := a.g.AddNode(n); err != nil {
		return 0, err
	}
	id, _ := a.g.AdjList().Mapper().Lookup(n)
	if !existed {
		a.g.IncrNodesAdded()
	}
	a.rec().recordAddNode(n, !existed)
	return id, nil
}

// AddEdge inserts a directed edge and returns the endpoint NodeIDs.
func (a *lpgMutatorAdapter) AddEdge(src, dst string, w float64) (graph.NodeID, graph.NodeID, error) {
	_, srcExisted := a.g.AdjList().Mapper().Lookup(src)
	_, dstExisted := a.g.AdjList().Mapper().Lookup(dst)
	if err := a.g.AddEdge(src, dst, w); err != nil {
		return 0, 0, err
	}
	srcID, _ := a.g.AdjList().Mapper().Lookup(src)
	dstID, _ := a.g.AdjList().Mapper().Lookup(dst)
	if !srcExisted {
		a.g.IncrNodesAdded()
	}
	if !dstExisted && src != dst {
		a.g.IncrNodesAdded()
	}
	a.g.IncrEdgesAdded()
	a.rec().recordAddEdge(src, dst, !srcExisted, !dstExisted)
	return srcID, dstID, nil
}

// AddEdgeH mirrors [lpgMutatorAdapter.AddEdge] but allocates and returns a
// stable per-edge handle (see [exec.GraphMutator.AddEdgeH]).
func (a *lpgMutatorAdapter) AddEdgeH(src, dst string, w float64) (graph.NodeID, graph.NodeID, uint64, error) {
	_, srcExisted := a.g.AdjList().Mapper().Lookup(src)
	_, dstExisted := a.g.AdjList().Mapper().Lookup(dst)
	handle, err := a.g.AddEdgeH(src, dst, w)
	if err != nil {
		return 0, 0, 0, err
	}
	srcID, _ := a.g.AdjList().Mapper().Lookup(src)
	dstID, _ := a.g.AdjList().Mapper().Lookup(dst)
	if !srcExisted {
		a.g.IncrNodesAdded()
	}
	if !dstExisted && src != dst {
		a.g.IncrNodesAdded()
	}
	a.g.IncrEdgesAdded()
	a.rec().recordAddEdge(src, dst, !srcExisted, !dstExisted)
	return srcID, dstID, handle, nil
}

// RemoveEdge removes the directed edge (src, dst). The LPG edge removal
// strips the per-pair edge labels/properties once the pair is fully
// disconnected, so re-creating an edge between the same endpoints does not
// resurrect the deleted relationship's type or properties.
func (a *lpgMutatorAdapter) RemoveEdge(src, dst string) {
	present := a.g.AdjList().HasEdge(src, dst)
	r := a.rec()
	var pre removedEdgePreimage
	if r.active() {
		pre = r.captureRemovedEdge(src, dst)
	}
	if present {
		a.g.IncrEdgesRemoved()
	}
	a.g.RemoveEdge(src, dst)
	r.recordRemoveEdge(&pre, present)
}

// SetNodeLabel attaches label to n.
func (a *lpgMutatorAdapter) SetNodeLabel(n, label string) error {
	r := a.rec()
	hadLabel := r.active() && a.g.HasNodeLabel(n, label)
	if err := a.g.SetNodeLabel(n, label); err != nil {
		return err
	}
	r.recordSetNodeLabel(n, label, hadLabel)
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:    index.OpAddNodeLabel,
			Node:  a.resolveID(n),
			Label: uint32(a.g.Registry().Intern(label)),
		})
	}
	return nil
}

// RemoveNodeLabel detaches label from n.
func (a *lpgMutatorAdapter) RemoveNodeLabel(n, label string) {
	r := a.rec()
	hadLabel := r.active() && a.g.HasNodeLabel(n, label)
	a.g.RemoveNodeLabel(n, label)
	r.recordRemoveNodeLabel(n, label, hadLabel)
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:    index.OpRemoveNodeLabel,
			Node:  a.resolveID(n),
			Label: uint32(a.g.Registry().Intern(label)),
		})
	}
}

// RemoveNode tombstones n in the underlying graph.
func (a *lpgMutatorAdapter) RemoveNode(n string) {
	id, ok := a.g.AdjList().Mapper().Lookup(n)
	if !ok {
		return
	}
	wasLive := !a.g.IsTombstoned(id)
	if wasLive {
		a.g.IncrNodesRemoved()
	}
	a.g.RemoveNode(n)
	a.rec().recordRemoveNode(n, wasLive)
}

// IsTombstoned reports whether the NodeID has been tombstoned.
func (a *lpgMutatorAdapter) IsTombstoned(id graph.NodeID) bool {
	return a.g.IsTombstoned(id)
}

// SetNodeProperty sets the named property on n.
func (a *lpgMutatorAdapter) SetNodeProperty(n, key string, value lpg.PropertyValue) error {
	r := a.rec()
	var prev lpg.PropertyValue
	var had bool
	if r.active() {
		prev, had = a.g.GetNodeProperty(n, key)
	}
	if err := a.g.SetNodeProperty(n, key, value); err != nil {
		return err
	}
	r.recordSetNodeProperty(n, key, prev, had)
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:       index.OpSetNodeProperty,
			Node:     a.resolveID(n),
			Property: uint32(a.g.PropertyKeys().Intern(key)),
			NewValue: value,
		})
	}
	return nil
}

// DelNodeProperty removes the named property from n.
func (a *lpgMutatorAdapter) DelNodeProperty(n, key string) {
	r := a.rec()
	var prev lpg.PropertyValue
	var had bool
	if r.active() {
		prev, had = a.g.GetNodeProperty(n, key)
	}
	a.g.DelNodeProperty(n, key)
	r.recordDelNodeProperty(n, key, prev, had)
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:       index.OpDelNodeProperty,
			Node:     a.resolveID(n),
			Property: uint32(a.g.PropertyKeys().Intern(key)),
		})
	}
}

// NodeProperties returns a snapshot of all properties on n.
func (a *lpgMutatorAdapter) NodeProperties(n string) map[string]lpg.PropertyValue {
	return a.g.NodeProperties(n)
}

// NodeLabels returns a snapshot of all labels on n.
func (a *lpgMutatorAdapter) NodeLabels(n string) []string {
	return a.g.NodeLabels(n)
}

// HasEdge reports whether a directed edge from src to dst is present.
func (a *lpgMutatorAdapter) HasEdge(src, dst string) bool {
	return a.g.AdjList().HasEdge(src, dst)
}

// SetEdgeLabel attaches label to the directed edge (src, dst).
func (a *lpgMutatorAdapter) SetEdgeLabel(src, dst, label string) {
	r := a.rec()
	hadLabel := r.active() && a.g.HasEdgeLabel(src, dst, label)
	a.g.SetEdgeLabel(src, dst, label)
	r.recordSetEdgeLabel(src, dst, label, hadLabel)
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:    index.OpAddEdgeLabel,
			Node:  a.resolveID(src),
			Dst:   a.resolveID(dst),
			Label: uint32(a.g.Registry().Intern(label)),
		})
	}
}

// SetEdgeProperty sets the named property on the directed edge (src, dst).
func (a *lpgMutatorAdapter) SetEdgeProperty(src, dst, key string, value lpg.PropertyValue) error {
	r := a.rec()
	var prev lpg.PropertyValue
	var had bool
	if r.active() {
		prev, had = a.g.GetEdgeProperty(src, dst, key)
	}
	if err := a.g.SetEdgeProperty(src, dst, key, value); err != nil {
		return err
	}
	r.recordSetEdgeProperty(src, dst, key, prev, had)
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:       index.OpSetEdgeProperty,
			Node:     a.resolveID(src),
			Dst:      a.resolveID(dst),
			Property: uint32(a.g.PropertyKeys().Intern(key)),
			NewValue: value,
		})
	}
	return nil
}

// DelEdgeProperty removes the named property from the directed edge (src, dst).
func (a *lpgMutatorAdapter) DelEdgeProperty(src, dst, key string) {
	r := a.rec()
	var prev lpg.PropertyValue
	var had bool
	if r.active() {
		prev, had = a.g.GetEdgeProperty(src, dst, key)
	}
	a.g.DelEdgeProperty(src, dst, key)
	r.recordDelEdgeProperty(src, dst, key, prev, had)
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:       index.OpDelEdgeProperty,
			Node:     a.resolveID(src),
			Dst:      a.resolveID(dst),
			Property: uint32(a.g.PropertyKeys().Intern(key)),
		})
	}
}

// EdgeProperties returns a snapshot of every property currently set on the
// directed edge (src, dst).
func (a *lpgMutatorAdapter) EdgeProperties(src, dst string) map[string]lpg.PropertyValue {
	return a.g.EdgeProperties(src, dst)
}

// EdgeLabels returns a snapshot of every label currently attached to the
// directed edge (src, dst).
func (a *lpgMutatorAdapter) EdgeLabels(src, dst string) []string {
	return a.g.EdgeLabels(src, dst)
}

// IncEdgeCreateCount, EdgeCreateCount, DecEdgeCreateCount delegate to
// the underlying [lpg.Graph] CREATE-multiplicity counter.
func (a *lpgMutatorAdapter) IncEdgeCreateCount(src, dst string) int64 {
	n := a.g.IncEdgeCreateCount(src, dst)
	a.rec().recordIncEdgeCreateCount(src, dst)
	return n
}
func (a *lpgMutatorAdapter) EdgeCreateCount(src, dst string) int64 {
	return a.g.EdgeCreateCount(src, dst)
}
func (a *lpgMutatorAdapter) DecEdgeCreateCount(src, dst string) {
	r := a.rec()
	had := r.active() && a.g.EdgeCreateCount(src, dst) > 0
	a.g.DecEdgeCreateCount(src, dst)
	r.recordDecEdgeCreateCount(src, dst, had)
}

// SetEdgeLabelAt / EdgeLabelsAt / SetEdgePropertyAt / EdgePropertiesAt /
// RemoveEdgeInstance delegate to the per-instance metadata stores on
// the underlying [lpg.Graph].
func (a *lpgMutatorAdapter) SetEdgeLabelAt(src, dst string, idx int64, label string) {
	a.g.SetEdgeLabelAt(src, dst, idx, label)
}
func (a *lpgMutatorAdapter) EdgeLabelsAt(src, dst string, idx int64) []string {
	return a.g.EdgeLabelsAt(src, dst, idx)
}
func (a *lpgMutatorAdapter) SetEdgePropertyAt(src, dst string, idx int64, key string, value lpg.PropertyValue) {
	a.g.SetEdgePropertyAt(src, dst, idx, key, value)
}
func (a *lpgMutatorAdapter) EdgePropertiesAt(src, dst string, idx int64) map[string]lpg.PropertyValue {
	return a.g.EdgePropertiesAt(src, dst, idx)
}
func (a *lpgMutatorAdapter) RemoveEdgeInstance(src, dst string, idx int64) {
	a.g.RemoveEdgeInstance(src, dst, idx)
}

// SetEdgeLabelByHandle / EdgeLabelsByHandle / SetEdgePropertyByHandle /
// EdgePropertiesByHandle / RemoveEdgeInstanceByHandle delegate to the
// stable-handle keyed metadata stores on the underlying [lpg.Graph].
func (a *lpgMutatorAdapter) SetEdgeLabelByHandle(src, dst string, handle uint64, label string) {
	a.g.SetEdgeLabelByHandle(src, dst, handle, label)
}
func (a *lpgMutatorAdapter) EdgeLabelsByHandle(src, dst string, handle uint64) []string {
	return a.g.EdgeLabelsByHandle(src, dst, handle)
}
func (a *lpgMutatorAdapter) SetEdgePropertyByHandle(src, dst string, handle uint64, key string, value lpg.PropertyValue) {
	a.g.SetEdgePropertyByHandle(src, dst, handle, key, value)
}
func (a *lpgMutatorAdapter) EdgePropertiesByHandle(src, dst string, handle uint64) map[string]lpg.PropertyValue {
	return a.g.EdgePropertiesByHandle(src, dst, handle)
}
func (a *lpgMutatorAdapter) RemoveEdgeInstanceByHandle(src, dst string, handle uint64) {
	a.g.RemoveEdgeInstanceByHandle(src, dst, handle)
}

// OutNeighbours returns a snapshot of the outgoing neighbour keys of n.
func (a *lpgMutatorAdapter) OutNeighbours(n string) []string {
	var out []string
	for nb := range a.g.AdjList().Neighbours(n) {
		out = append(out, nb)
	}
	return out
}

// InNeighbours returns a snapshot of the incoming neighbour keys of n by
// performing a full graph walk. This is O(V+E) and should only be called for
// DETACH DELETE operations where correctness trumps performance.
func (a *lpgMutatorAdapter) InNeighbours(n string) []string {
	nID, ok := a.g.AdjList().Mapper().Lookup(n)
	if !ok {
		return nil
	}
	var result []string
	a.g.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		if id == nID {
			return true
		}
		nbs, _ := a.g.AdjList().LoadEntry(id)
		for _, nb := range nbs {
			if nb == nID {
				result = append(result, key)
				break
			}
		}
		return true
	})
	return result
}

// OutDegree returns the number of outgoing edges from n.
func (a *lpgMutatorAdapter) OutDegree(n string) int {
	id, ok := a.g.AdjList().Mapper().Lookup(n)
	if !ok {
		return 0
	}
	nbs, _ := a.g.AdjList().LoadEntry(id)
	return len(nbs)
}

// ResolveNodeID translates a node key to its NodeID.
func (a *lpgMutatorAdapter) ResolveNodeID(n string) (graph.NodeID, bool) {
	return a.g.AdjList().Mapper().Lookup(n)
}

// ResolveNodeLabel translates a NodeID back to its node key.
func (a *lpgMutatorAdapter) ResolveNodeLabel(id graph.NodeID) (string, bool) {
	return a.g.AdjList().Mapper().Resolve(id)
}

// WalkNodeIDs calls fn for every interned, non-tombstoned node.
func (a *lpgMutatorAdapter) WalkNodeIDs(fn func(graph.NodeID) bool) {
	a.g.AdjList().Mapper().Walk(func(id graph.NodeID, _ string) bool {
		if a.g.IsTombstoned(id) {
			return true
		}
		return fn(id)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// walMutatorAdapter — exec.GraphMutator backed by txn.Tx + *lpg.Graph
// ─────────────────────────────────────────────────────────────────────────────

// walMutatorAdapter applies every mutation to the in-memory graph eagerly (so
// reads within the same transaction see writes immediately) and also buffers
// the op in the txn.Tx so that [txn.Tx.CommitWALOnly] can fsync it to the WAL
// on [Result.Close].
//
// walMutatorAdapter is NOT safe for concurrent use. The store mutex is held
// from [txn.Store.Begin] (in RunInTx) until [txn.Tx.CommitWALOnly] or
// [txn.Tx.Rollback] (in Result.Close).
//
// The undo log records the inverse of each in-memory mutation so a failed
// statement's eager writes to the live graph are rolled back inside the
// visibility barrier (#1282). It is independent of the WAL transaction and the
// index buffer, which roll back through their own mechanisms; the undo log
// closes only the in-memory-vs-durable divergence.
type walMutatorAdapter struct {
	g    *lpg.Graph[string, float64]
	tx   *txn.Tx[string, float64]
	buf  *exec.IndexBuffer // nil for read-only (never reached via RunInTx)
	undo *undoLog          // nil for read-only (never reached via RunInTx)
}

// rec returns the inverse-recording helper bound to this adapter's graph and
// undo log.
func (a *walMutatorAdapter) rec() mutationUndo { return mutationUndo{g: a.g, undo: a.undo} }

func (a *walMutatorAdapter) resolveID(n string) graph.NodeID {
	id, ok := a.g.AdjList().Mapper().Lookup(n)
	if !ok {
		return graph.NodeID(0)
	}
	return id
}

// AddNode interns n and returns its stable NodeID.
func (a *walMutatorAdapter) AddNode(n string) (graph.NodeID, error) {
	idBefore, existed := a.g.AdjList().Mapper().Lookup(n)
	// Capture the tombstone state BEFORE AddNode: AddNode now revives a
	// tombstoned node, so checking afterwards would always observe it live.
	// Re-creating a removed key counts as a fresh creation for side-effect
	// bookkeeping.
	if existed && a.g.IsTombstoned(idBefore) {
		existed = false
	}
	if err := a.g.AddNode(n); err != nil {
		return 0, err
	}
	_ = a.tx.AddNode(n) //nolint:errcheck // tx is non-nil; only ErrTxFinished possible, which cannot occur here
	id, _ := a.g.AdjList().Mapper().Lookup(n)
	if !existed {
		a.g.IncrNodesAdded()
	}
	a.rec().recordAddNode(n, !existed)
	return id, nil
}

// AddEdge inserts a directed edge and returns the endpoint NodeIDs.
func (a *walMutatorAdapter) AddEdge(src, dst string, w float64) (graph.NodeID, graph.NodeID, error) {
	_, srcExisted := a.g.AdjList().Mapper().Lookup(src)
	_, dstExisted := a.g.AdjList().Mapper().Lookup(dst)
	if err := a.g.AddEdge(src, dst, w); err != nil {
		return 0, 0, err
	}
	_ = a.tx.AddEdge(src, dst, w) //nolint:errcheck // ErrNoWeightCodec cannot occur — store has wcodec via NewEngineWithStore
	srcID, _ := a.g.AdjList().Mapper().Lookup(src)
	dstID, _ := a.g.AdjList().Mapper().Lookup(dst)
	if !srcExisted {
		a.g.IncrNodesAdded()
	}
	if !dstExisted && src != dst {
		a.g.IncrNodesAdded()
	}
	a.g.IncrEdgesAdded()
	a.rec().recordAddEdge(src, dst, !srcExisted, !dstExisted)
	return srcID, dstID, nil
}

// AddEdgeH mirrors [walMutatorAdapter.AddEdge] but allocates a stable
// per-edge handle on the in-memory graph and persists it: the WAL frame is
// the handle-bearing [txn.OpAddEdgeH] carrying the SAME handle stamped onto
// the adjacency slot, so a recovered parallel edge keeps its identity and
// the per-handle type/properties below reattach to it on replay. Replay is
// idempotent against a snapshot that already loaded the handle
// ([lpg.Graph.AddEdgeHIfAbsent]), so snapshot + full-WAL recovery does not
// double the edge. See graph/lpg/edge_handle.go for the durability contract.
func (a *walMutatorAdapter) AddEdgeH(src, dst string, w float64) (graph.NodeID, graph.NodeID, uint64, error) {
	_, srcExisted := a.g.AdjList().Mapper().Lookup(src)
	_, dstExisted := a.g.AdjList().Mapper().Lookup(dst)
	handle, err := a.g.AddEdgeH(src, dst, w)
	if err != nil {
		return 0, 0, 0, err
	}
	_ = a.tx.AddEdgeWithHandle(src, dst, w, handle) //nolint:errcheck // ErrNoWeightCodec cannot occur — store has wcodec via NewEngineWithStore
	srcID, _ := a.g.AdjList().Mapper().Lookup(src)
	dstID, _ := a.g.AdjList().Mapper().Lookup(dst)
	if !srcExisted {
		a.g.IncrNodesAdded()
	}
	if !dstExisted && src != dst {
		a.g.IncrNodesAdded()
	}
	a.g.IncrEdgesAdded()
	a.rec().recordAddEdge(src, dst, !srcExisted, !dstExisted)
	return srcID, dstID, handle, nil
}

// RemoveEdge removes the directed edge (src, dst). The LPG edge removal
// strips the per-pair edge labels/properties once the pair is fully
// disconnected, so re-creating an edge between the same endpoints does not
// resurrect the deleted relationship's type or properties.
func (a *walMutatorAdapter) RemoveEdge(src, dst string) {
	present := a.g.AdjList().HasEdge(src, dst)
	r := a.rec()
	var pre removedEdgePreimage
	if r.active() {
		pre = r.captureRemovedEdge(src, dst)
	}
	if present {
		a.g.IncrEdgesRemoved()
	}
	a.g.RemoveEdge(src, dst)
	_ = a.tx.RemoveEdge(src, dst) //nolint:errcheck // ErrTxFinished impossible here
	r.recordRemoveEdge(&pre, present)
}

// SetNodeLabel attaches label to n.
func (a *walMutatorAdapter) SetNodeLabel(n, label string) error {
	r := a.rec()
	hadLabel := r.active() && a.g.HasNodeLabel(n, label)
	if err := a.g.SetNodeLabel(n, label); err != nil {
		return err
	}
	r.recordSetNodeLabel(n, label, hadLabel)
	_ = a.tx.SetNodeLabel(n, label) //nolint:errcheck // ErrTxFinished impossible here
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:    index.OpAddNodeLabel,
			Node:  a.resolveID(n),
			Label: uint32(a.g.Registry().Intern(label)),
		})
	}
	return nil
}

// RemoveNodeLabel detaches label from n.
func (a *walMutatorAdapter) RemoveNodeLabel(n, label string) {
	r := a.rec()
	hadLabel := r.active() && a.g.HasNodeLabel(n, label)
	a.g.RemoveNodeLabel(n, label)
	r.recordRemoveNodeLabel(n, label, hadLabel)
	_ = a.tx.RemoveNodeLabel(n, label) //nolint:errcheck // ErrTxFinished impossible here
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:    index.OpRemoveNodeLabel,
			Node:  a.resolveID(n),
			Label: uint32(a.g.Registry().Intern(label)),
		})
	}
}

// RemoveNode tombstones n in the underlying graph.
func (a *walMutatorAdapter) RemoveNode(n string) {
	id, ok := a.g.AdjList().Mapper().Lookup(n)
	if !ok {
		return
	}
	wasLive := !a.g.IsTombstoned(id)
	if wasLive {
		a.g.IncrNodesRemoved()
	}
	a.g.RemoveNode(n)
	a.rec().recordRemoveNode(n, wasLive)
}

// IsTombstoned reports whether the NodeID has been tombstoned.
func (a *walMutatorAdapter) IsTombstoned(id graph.NodeID) bool {
	return a.g.IsTombstoned(id)
}

// SetNodeProperty sets the named property on n.
func (a *walMutatorAdapter) SetNodeProperty(n, key string, value lpg.PropertyValue) error {
	r := a.rec()
	var prev lpg.PropertyValue
	var had bool
	if r.active() {
		prev, had = a.g.GetNodeProperty(n, key)
	}
	if err := a.g.SetNodeProperty(n, key, value); err != nil {
		return err
	}
	r.recordSetNodeProperty(n, key, prev, had)
	_ = a.tx.SetNodeProperty(n, key, value) //nolint:errcheck // ErrTxFinished impossible here
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:       index.OpSetNodeProperty,
			Node:     a.resolveID(n),
			Property: uint32(a.g.PropertyKeys().Intern(key)),
			NewValue: value,
		})
	}
	return nil
}

// DelNodeProperty removes the named property from n.
func (a *walMutatorAdapter) DelNodeProperty(n, key string) {
	r := a.rec()
	var prev lpg.PropertyValue
	var had bool
	if r.active() {
		prev, had = a.g.GetNodeProperty(n, key)
	}
	a.g.DelNodeProperty(n, key)
	r.recordDelNodeProperty(n, key, prev, had)
	_ = a.tx.DelNodeProperty(n, key) //nolint:errcheck // ErrTxFinished impossible here
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:       index.OpDelNodeProperty,
			Node:     a.resolveID(n),
			Property: uint32(a.g.PropertyKeys().Intern(key)),
		})
	}
}

// NodeProperties returns a snapshot of all properties on n.
func (a *walMutatorAdapter) NodeProperties(n string) map[string]lpg.PropertyValue {
	return a.g.NodeProperties(n)
}

// NodeLabels returns a snapshot of all labels on n.
func (a *walMutatorAdapter) NodeLabels(n string) []string {
	return a.g.NodeLabels(n)
}

// HasEdge reports whether a directed edge from src to dst is present.
func (a *walMutatorAdapter) HasEdge(src, dst string) bool {
	return a.g.AdjList().HasEdge(src, dst)
}

// SetEdgeLabel attaches label to the directed edge (src, dst).
func (a *walMutatorAdapter) SetEdgeLabel(src, dst, label string) {
	r := a.rec()
	hadLabel := r.active() && a.g.HasEdgeLabel(src, dst, label)
	a.g.SetEdgeLabel(src, dst, label)
	r.recordSetEdgeLabel(src, dst, label, hadLabel)
	_ = a.tx.SetEdgeLabel(src, dst, label) //nolint:errcheck // ErrTxFinished impossible here
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:    index.OpAddEdgeLabel,
			Node:  a.resolveID(src),
			Dst:   a.resolveID(dst),
			Label: uint32(a.g.Registry().Intern(label)),
		})
	}
}

// SetEdgeProperty sets the named property on the directed edge (src, dst).
func (a *walMutatorAdapter) SetEdgeProperty(src, dst, key string, value lpg.PropertyValue) error {
	r := a.rec()
	var prev lpg.PropertyValue
	var had bool
	if r.active() {
		prev, had = a.g.GetEdgeProperty(src, dst, key)
	}
	if err := a.g.SetEdgeProperty(src, dst, key, value); err != nil {
		return err
	}
	r.recordSetEdgeProperty(src, dst, key, prev, had)
	_ = a.tx.SetEdgeProperty(src, dst, key, value) //nolint:errcheck // ErrTxFinished impossible here
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:       index.OpSetEdgeProperty,
			Node:     a.resolveID(src),
			Dst:      a.resolveID(dst),
			Property: uint32(a.g.PropertyKeys().Intern(key)),
			NewValue: value,
		})
	}
	return nil
}

// DelEdgeProperty removes the named property from the directed edge (src, dst).
func (a *walMutatorAdapter) DelEdgeProperty(src, dst, key string) {
	r := a.rec()
	var prev lpg.PropertyValue
	var had bool
	if r.active() {
		prev, had = a.g.GetEdgeProperty(src, dst, key)
	}
	a.g.DelEdgeProperty(src, dst, key)
	r.recordDelEdgeProperty(src, dst, key, prev, had)
	_ = a.tx.DelEdgeProperty(src, dst, key) //nolint:errcheck // ErrTxFinished impossible here
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:       index.OpDelEdgeProperty,
			Node:     a.resolveID(src),
			Dst:      a.resolveID(dst),
			Property: uint32(a.g.PropertyKeys().Intern(key)),
		})
	}
}

// EdgeProperties returns a snapshot of every property currently set on the
// directed edge (src, dst).
func (a *walMutatorAdapter) EdgeProperties(src, dst string) map[string]lpg.PropertyValue {
	return a.g.EdgeProperties(src, dst)
}

// EdgeLabels returns a snapshot of every label currently attached to the
// directed edge (src, dst).
func (a *walMutatorAdapter) EdgeLabels(src, dst string) []string {
	return a.g.EdgeLabels(src, dst)
}

// IncEdgeCreateCount, EdgeCreateCount, DecEdgeCreateCount delegate to
// the underlying [lpg.Graph] CREATE-multiplicity counter.
func (a *walMutatorAdapter) IncEdgeCreateCount(src, dst string) int64 {
	n := a.g.IncEdgeCreateCount(src, dst)
	a.rec().recordIncEdgeCreateCount(src, dst)
	return n
}
func (a *walMutatorAdapter) EdgeCreateCount(src, dst string) int64 {
	return a.g.EdgeCreateCount(src, dst)
}
func (a *walMutatorAdapter) DecEdgeCreateCount(src, dst string) {
	r := a.rec()
	had := r.active() && a.g.EdgeCreateCount(src, dst) > 0
	a.g.DecEdgeCreateCount(src, dst)
	r.recordDecEdgeCreateCount(src, dst, had)
}

// SetEdgeLabelAt / EdgeLabelsAt / SetEdgePropertyAt / EdgePropertiesAt /
// RemoveEdgeInstance delegate to the per-instance metadata stores on
// the underlying [lpg.Graph].
//
// These per-instance / per-handle setters intentionally record NO separate undo
// entry: CreateRelationship is their only caller and always invokes them on a
// handle/instance it allocated via AddEdgeH in the SAME operator, so the matching
// recordAddEdge inverse already removes that edge — and [Graph.RemoveEdge] →
// clearEdgePairState drops the pair's per-handle and per-instance metadata once
// the last edge between the endpoints is gone. The exotic case (a per-handle
// metadata set on an edge that a later failed row removes while a parallel edge
// survives) is handled by the edge-removal undo itself: captureRemovedEdge
// snapshots the removed slot's handle and its per-handle labels/properties, and
// recordRemoveEdge re-adds the instance with that handle and restores them
// (#1327).
func (a *walMutatorAdapter) SetEdgeLabelAt(src, dst string, idx int64, label string) {
	a.g.SetEdgeLabelAt(src, dst, idx, label)
}
func (a *walMutatorAdapter) EdgeLabelsAt(src, dst string, idx int64) []string {
	return a.g.EdgeLabelsAt(src, dst, idx)
}
func (a *walMutatorAdapter) SetEdgePropertyAt(src, dst string, idx int64, key string, value lpg.PropertyValue) {
	a.g.SetEdgePropertyAt(src, dst, idx, key, value)
}
func (a *walMutatorAdapter) EdgePropertiesAt(src, dst string, idx int64) map[string]lpg.PropertyValue {
	return a.g.EdgePropertiesAt(src, dst, idx)
}
func (a *walMutatorAdapter) RemoveEdgeInstance(src, dst string, idx int64) {
	a.g.RemoveEdgeInstance(src, dst, idx)
}

// SetEdgeLabelByHandle / EdgeLabelsByHandle / SetEdgePropertyByHandle /
// EdgePropertiesByHandle / RemoveEdgeInstanceByHandle delegate to the
// stable-handle keyed metadata stores on the underlying [lpg.Graph] AND
// buffer the matching durable WAL op so a recovered parallel edge keeps its
// per-CREATE type and properties (Stage 2). The read-only accessors
// (EdgeLabelsByHandle / EdgePropertiesByHandle) buffer nothing.
func (a *walMutatorAdapter) SetEdgeLabelByHandle(src, dst string, handle uint64, label string) {
	a.g.SetEdgeLabelByHandle(src, dst, handle, label)
	_ = a.tx.SetEdgeLabelByHandle(src, dst, handle, label) //nolint:errcheck // ErrTxFinished impossible here
}
func (a *walMutatorAdapter) EdgeLabelsByHandle(src, dst string, handle uint64) []string {
	return a.g.EdgeLabelsByHandle(src, dst, handle)
}
func (a *walMutatorAdapter) SetEdgePropertyByHandle(src, dst string, handle uint64, key string, value lpg.PropertyValue) {
	a.g.SetEdgePropertyByHandle(src, dst, handle, key, value)
	_ = a.tx.SetEdgePropertyByHandle(src, dst, handle, key, value) //nolint:errcheck // ErrTxFinished impossible here
}
func (a *walMutatorAdapter) EdgePropertiesByHandle(src, dst string, handle uint64) map[string]lpg.PropertyValue {
	return a.g.EdgePropertiesByHandle(src, dst, handle)
}
func (a *walMutatorAdapter) RemoveEdgeInstanceByHandle(src, dst string, handle uint64) {
	a.g.RemoveEdgeInstanceByHandle(src, dst, handle)
	_ = a.tx.RemoveEdgeInstanceByHandle(src, dst, handle) //nolint:errcheck // ErrTxFinished impossible here
}

// OutNeighbours returns a snapshot of the outgoing neighbour keys of n.
func (a *walMutatorAdapter) OutNeighbours(n string) []string {
	var out []string
	for nb := range a.g.AdjList().Neighbours(n) {
		out = append(out, nb)
	}
	return out
}

// InNeighbours returns a snapshot of the incoming neighbour keys of n by
// performing a full graph walk.
func (a *walMutatorAdapter) InNeighbours(n string) []string {
	nID, ok := a.g.AdjList().Mapper().Lookup(n)
	if !ok {
		return nil
	}
	var result []string
	a.g.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		if id == nID {
			return true
		}
		nbs, _ := a.g.AdjList().LoadEntry(id)
		for _, nb := range nbs {
			if nb == nID {
				result = append(result, key)
				break
			}
		}
		return true
	})
	return result
}

// OutDegree returns the number of outgoing edges from n.
func (a *walMutatorAdapter) OutDegree(n string) int {
	id, ok := a.g.AdjList().Mapper().Lookup(n)
	if !ok {
		return 0
	}
	nbs, _ := a.g.AdjList().LoadEntry(id)
	return len(nbs)
}

// ResolveNodeID translates a node key to its NodeID.
func (a *walMutatorAdapter) ResolveNodeID(n string) (graph.NodeID, bool) {
	return a.g.AdjList().Mapper().Lookup(n)
}

// ResolveNodeLabel translates a NodeID back to its node key.
func (a *walMutatorAdapter) ResolveNodeLabel(id graph.NodeID) (string, bool) {
	return a.g.AdjList().Mapper().Resolve(id)
}

// WalkNodeIDs calls fn for every interned, non-tombstoned node.
func (a *walMutatorAdapter) WalkNodeIDs(fn func(graph.NodeID) bool) {
	a.g.AdjList().Mapper().Walk(func(id graph.NodeID, _ string) bool {
		if a.g.IsTombstoned(id) {
			return true
		}
		return fn(id)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// CSR helpers for expand operators
// ─────────────────────────────────────────────────────────────────────────────

// csrPairFromGraph builds a forward and a reverse CSR snapshot from the LPG
// adjacency list.  Both snapshots are constructed in O(V+E) time and are safe
// for lock-free concurrent reads after construction.
func csrPairFromGraph(g *lpg.Graph[string, float64]) (fwd, rev *csr.CSR[float64]) {
	adj := g.AdjList()
	fwd = csr.BuildFromAdjList(adj)
	rev = fwd.BuildReverse()
	return
}

// ensureEdgeIDResolver makes sure bopts.edgeIDResolver is populated and
// returns it. The resolver maps a forward-CSR edge position (the
// IntegerValue Expand emits) to the edge's storage endpoints
// (storage_src, storage_dst). Path-reconstruction fast paths use it to
// determine the relationship's storage direction when the row's
// traversal columns disagree (undirected MATCH reverse-pass rows
// carry traversal_src ≠ storage_src).
//
// The resolver is built lazily on first use because most queries never
// reconstruct paths. CSR construction is O(V+E) but happens at most
// once per query.
func ensureEdgeIDResolver(bopts *buildOpts, g *lpg.Graph[string, float64]) func(uint64) (uint64, uint64, bool) {
	if bopts == nil || g == nil {
		return nil
	}
	if bopts.edgeIDResolver != nil {
		return bopts.edgeIDResolver
	}
	fwd, _ := csrPairFromGraph(g)
	verts := fwd.VerticesSlice()
	edges := fwd.EdgesSlice()
	nEdges := uint64(len(edges))
	bopts.edgeIDResolver = func(edgeID uint64) (uint64, uint64, bool) {
		if edgeID >= nEdges {
			return 0, 0, false
		}
		storageDst := uint64(edges[edgeID])
		// Binary search for the largest src such that verts[src] <= edgeID.
		lo, hi := 0, len(verts)-1
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if verts[mid] <= edgeID {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		return uint64(lo), storageDst, true
	}
	return bopts.edgeIDResolver
}

// nodeIDOrNodeValue extracts a NodeID from a row column. The slot may hold a
// raw IntegerValue (the in-pipeline encoding emitted by Expand) or a
// NodeValue (the canonical projected form), so both shapes are accepted.
// Returns (0, false) for null or any other value type.
func nodeIDOrNodeValue(v expr.Value) (uint64, bool) {
	switch x := v.(type) {
	case expr.IntegerValue:
		return uint64(int64(x)), true
	case expr.NodeValue:
		return x.ID, true
	}
	return 0, false
}

// buildEdgeTypeFilter constructs an edge-type filter map for the forward CSR
// of g.  The map key is the edge's absolute position in the CSR's EdgesSlice;
// the value is a label attached to that edge in the LPG.
//
// When relTypes is non-empty an edge passes the filter if ANY of the labels
// attached to that edge matches one of the listed types; the stored value
// is the matching label (used by reverseEdgePassesFilter and downstream
// per-edge bookkeeping). An empty relTypes slice means "accept all edge
// types" — the returned map lists every labelled edge with its first label.
//
// The any-label semantics let `MATCH (a)-[:T]->(b)` match an edge that
// carries multiple labels (e.g. PLAYS_FOR + SUPPORTS on the same (src,dst)
// pair) when one of them equals T. Closes Match7 [29] and unblocks the
// general "multiple labels per edge" scenario.
//
// O(V+E) time; allocates one map entry per labelled edge.
func buildEdgeTypeFilter(g *lpg.Graph[string, float64], relTypes []string) map[uint64]string {
	adj := g.AdjList()
	fwdCSR := csr.BuildFromAdjList(adj)
	verts := fwdCSR.VerticesSlice()
	edges := fwdCSR.EdgesSlice()
	// handles aligns slot-for-slot with edges when the graph carries
	// stable per-edge handles (multigraph CREATEs). It is nil for a graph
	// that never stamped a handle (simple-graph / MERGE-only), in which
	// case every slot takes the positional fallback below.
	handles := fwdCSR.HandlesSlice()
	mapper := adj.Mapper()

	// Pre-build a set of accepted types for O(1) lookup.
	acceptAll := len(relTypes) == 0
	accept := make(map[string]struct{}, len(relTypes))
	for _, t := range relTypes {
		accept[t] = struct{}{}
	}

	filter := make(map[uint64]string)
	// Bound the loop on the SNAPSHOT CSR, not the live graph. fwdCSR was
	// built from a point-in-time copy of adj above; verts has a fixed length
	// of fwdCSR.MaxNodeID()+1, so verts[srcID+1] is in-range by construction
	// for srcID < fwdCSR.MaxNodeID(). Re-reading adj.MaxNodeID() here would
	// tear the read if a concurrent writer grew the node space between the
	// CSR build and this loop (panic: index out of range on verts[srcID+1]).
	maxID := uint64(fwdCSR.MaxNodeID())
	for srcID := uint64(0); srcID < maxID; srcID++ {
		start := verts[srcID]
		end := verts[srcID+1]
		srcStr, ok := mapper.Resolve(graph.NodeID(srcID))
		if !ok {
			continue
		}
		// dstSeen drives only the positional fallback (handle-less /
		// MERGE slots): it counts parallel CSR occurrences per dst so a
		// fallback slot maps to its CREATE-instance idx. The
		// handle-driven path below ignores it entirely. dstParallelTotal
		// lets the fallback tell multigraph (N_csr == N_create) from
		// simple-graph (N_csr < N_create) storage for each pair.
		dstParallelTotal := make(map[graph.NodeID]int64, end-start)
		for pos := start; pos < end; pos++ {
			dstParallelTotal[edges[pos]]++
		}
		dstSeen := make(map[graph.NodeID]int64, len(dstParallelTotal))
		for pos := start; pos < end; pos++ {
			dst := edges[pos]
			dstStr, ok := mapper.Resolve(dst)
			if !ok {
				continue
			}
			dstSeen[dst]++
			var labels []string
			if pos < uint64(len(handles)) && handles[pos] != 0 {
				// Stable-handle path: resolve this slot's type by the
				// explicit per-edge handle read directly from the CSR
				// position. This is delete-stable — removing a parallel
				// sibling compacts the neighbour slice but the surviving
				// slot keeps its original handle, so the type no longer
				// mis-maps the way the positional idx did (Match2 [6] /
				// Match7 [29]).
				labels = g.EdgeLabelsByHandle(srcStr, dstStr, handles[pos])
			} else {
				// Fallback (handle column absent, or a MERGE-created slot
				// with handle 0): keep the prior positional inference.
				totalCreates := g.EdgeCreateCount(srcStr, dstStr)
				parallel := dstParallelTotal[dst]
				if parallel >= totalCreates && totalCreates > 0 {
					// Multigraph: one CSR slot per CREATE. Use the
					// per-instance label set for this specific slot.
					labels = g.EdgeLabelsAt(srcStr, dstStr, dstSeen[dst])
				} else {
					// Simple-graph (or no per-instance store): merge every
					// instance's labels with the per-pair union so a
					// filter targeting any CREATE's label still matches.
					labels = collectAllInstanceLabels(g, srcStr, dstStr, totalCreates)
				}
			}
			if len(labels) == 0 {
				labels = g.EdgeLabels(srcStr, dstStr)
			}
			if len(labels) == 0 {
				continue
			}
			if acceptAll {
				filter[pos] = labels[0]
				continue
			}
			// any-label match: include the edge when at least one label
			// is in the accept set; record the matching label so
			// reverseEdgePassesFilter can route the lookup.
			for _, lbl := range labels {
				if _, ok := accept[lbl]; ok {
					filter[pos] = lbl
					break
				}
			}
		}
	}
	return filter
}

// collectAllInstanceLabels returns the union of every per-CREATE label
// recorded for (srcStr, dstStr) over instance indices 1..totalCreates.
// Used by simple-graph filter construction, where one CSR slot must
// service every collapsed CREATE.
func collectAllInstanceLabels(g *lpg.Graph[string, float64], srcStr, dstStr string, totalCreates int64) []string {
	if totalCreates <= 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for i := int64(1); i <= totalCreates; i++ {
		for _, l := range g.EdgeLabelsAt(srcStr, dstStr, i) {
			if _, ok := seen[l]; !ok {
				seen[l] = struct{}{}
				out = append(out, l)
			}
		}
	}
	return out
}

// irDirToExec converts an IR Direction to the corresponding exec Direction.
func irDirToExec(d ir.Direction) exec.Direction {
	switch d {
	case ir.DirectionIncoming:
		return exec.DirIn
	case ir.DirectionBoth:
		return exec.DirBoth
	default:
		return exec.DirOut
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Compile-time assertions
// ─────────────────────────────────────────────────────────────────────────────

var _ nodeWalkerIface = (*lpgNodeWalker)(nil)
var _ labelResolverIface = (*lpgLabelResolver)(nil)
var _ exec.GraphMutator = (*walMutatorAdapter)(nil)
