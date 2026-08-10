package exec

import (
	"fmt"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// Chunk is a column-major (struct-of-arrays) execution batch, the foundation
// for late materialization in the Cypher executor (rmp #1704, DuckDB DataChunk
// style). The row-at-a-time model ([Row] = []expr.Value) boxes every scalar into
// the [expr.Value] interface once per cell; a Chunk instead keeps scalar columns
// in contiguous, unboxed, typed backing slices ([]int64/[]float64/[]string/
// []bool) so that later phases can scan them without interface dispatch and box
// only at the sink ([Chunk.BoxCell]/[Chunk.BoxRow]).
//
// This type is purely additive: as of its introduction NO operator is wired to
// it. It lands the layout, its API, and its allocation discipline behind the
// existing [Operator] interface so later phases can migrate operators onto it
// without changing observable behaviour.
//
// # Design (validated with the columnar-db-expert against DuckDB / Apache Arrow)
//
//   - Struct-of-arrays: a single logical row count (length) and capacity live at
//     the Chunk level, never per column, so columns cannot silently drift.
//   - Discriminated-fields column (a tagged union): each [column] holds every
//     possible typed backing as a separate field and a storage tag selecting the
//     live one. Only the live backing is allocated; the rest stay nil (GC
//     leaves). This avoids both interface{} at the Chunk boundary (which a
//     generic Column[T] behind a non-generic holder would reintroduce) and
//     unsafe reinterpretation. The scalar type set is closed by the Cypher
//     language, so a tagged union is the idiomatic fit.
//   - Validity: a packed bitmap ([]uint64, 1 bit per row, bit set = valid /
//     non-null, LSB-first) per column, matching the Arrow columnar spec. A
//     sentinel value is impossible (0 is a valid int, NaN a valid float, "" a
//     valid string); a []bool mask wastes 8x the memory. An allValid fast path
//     (an explicit flag, not an overloaded nil) skips the bitmap entirely while a
//     column has seen no NULL; the bitmap is materialized lazily on the first
//     NULL and, under pooling, its backing is retained across [Chunk.Reset].
//   - String columns use []string for this phase. An offsets+bytes arena
//     (Arrow-style) is the eventual memory win but is deferred: the strings the
//     engine produces already exist as Go strings, so []string costs only a
//     16-byte header copy and no new byte allocation, which is enough to remove
//     the per-cell interface box that #1704 targets. The cell accessor
//     [Chunk.String] returns a string (a view), so a later arena swap behind it
//     is non-breaking; [Chunk.StringColumn] returning []string is Phase-1 only.
//   - Box-at-sink: the validity bitmap is authoritative for every column kind
//     (including boxed); a NULL cell boxes to [expr.Null] regardless of the
//     backing slot's value.
//
// # Deferred, deliberately (foundational decisions recorded for later phases)
//
//   - Selection vectors: accessors index rows PHYSICALLY (logical index ==
//     physical index). Late-materialization filtering via a selection vector
//     (DuckDB SelectionVector) is a named later phase; callers must not assume
//     the physical-index contract is permanent.
//   - Dynamic type promotion: a column's storage is fixed at construction from
//     its declared [expr.Kind]. Scalar kinds map to typed backings; every other
//     kind (List/Map/Node/Relationship/Path/temporals) maps to a boxed
//     []expr.Value backing, which also serves genuinely heterogeneous columns.
//     A typed column rejects a value of a different kind (programmer error);
//     promote-a-typed-column-to-boxed-on-conflict is deferred. Declare
//     heterogeneous columns with a non-scalar kind so they are boxed from the
//     start.
//
// # Concurrency
//
// A Chunk is NOT safe for concurrent use. Each pipeline stage owns its own
// instance, typically obtained from a [ChunkPool].
type Chunk struct {
	cols     []column
	capacity int
}

// storageTag selects which typed backing of a [column] is live.
type storageTag uint8

const (
	stBoxed   storageTag = iota // []expr.Value backing (non-scalar or heterogeneous)
	stI64                       // []int64 backing
	stF64                       // []float64 backing
	stStr                       // []string backing
	stBool                      // []bool backing
	stDynamic                   // undecided: no backing until the first Put commits one
)

// column is a single column of a [Chunk]: a discriminated union of typed
// backings plus a validity bitmap. Exactly one backing (selected by store) is
// allocated and live; the others are nil.
type column struct {
	i64   []int64      // live iff store == stI64
	f64   []float64    // live iff store == stF64
	str   []string     // live iff store == stStr
	b     []bool       // live iff store == stBool
	boxed []expr.Value // live iff store == stBoxed

	// valid is the packed validity bitmap (bit set = non-null, LSB-first). It is
	// nil while allValid is true; it is materialized lazily on the first NULL and,
	// once allocated, its backing is retained across Reset for pool reuse.
	valid []uint64

	n int // number of rows filled in this column

	kind     expr.Kind  // logical Cypher type of this column
	store    storageTag // which backing below is live
	dynamic  bool       // true iff constructed as a dynamic (Put-decided) column
	allValid bool       // true while no NULL has been recorded; valid may be unallocated
}

// DefaultChunkCapacity is the default per-column row capacity of a [Chunk]. It
// matches [DefaultSlabCapacity] so a Chunk aligns with the pipeline's row-batch
// boundary; the capacity is a pre-sizing hint (appends beyond it grow the
// backing), not a hard bound.
const DefaultChunkCapacity = 4096

// kindToStorage maps a logical [expr.Kind] to the backing that stores it
// unboxed. Only the four scalar kinds are unboxed; every other kind is boxed.
func kindToStorage(k expr.Kind) storageTag {
	switch k {
	case expr.KindInteger:
		return stI64
	case expr.KindFloat:
		return stF64
	case expr.KindString:
		return stStr
	case expr.KindBool:
		return stBool
	default:
		return stBoxed
	}
}

// NewChunk creates an empty Chunk with one column per kind in kinds, each
// pre-sized to capacity rows. A capacity < 1 defaults to [DefaultChunkCapacity].
// Scalar kinds (Integer/Float/String/Bool) get a typed backing; every other kind
// gets a boxed []expr.Value backing. The returned Chunk has length 0.
func NewChunk(capacity int, kinds ...expr.Kind) *Chunk {
	if capacity < 1 {
		capacity = DefaultChunkCapacity
	}
	c := &Chunk{
		capacity: capacity,
		cols:     make([]column, len(kinds)),
	}
	for j, k := range kinds {
		col := &c.cols[j]
		col.kind = k
		col.store = kindToStorage(k)
		col.allValid = true
		switch col.store {
		case stI64:
			col.i64 = make([]int64, 0, capacity)
		case stF64:
			col.f64 = make([]float64, 0, capacity)
		case stStr:
			col.str = make([]string, 0, capacity)
		case stBool:
			col.b = make([]bool, 0, capacity)
		case stBoxed:
			col.boxed = make([]expr.Value, 0, capacity)
		}
	}
	return c
}

// NewDynamicChunk creates an empty Chunk with ncols dynamic columns and a row
// capacity of capacity. A capacity < 1 defaults to [DefaultChunkCapacity].
//
// Unlike [NewChunk] the constructor allocates no backing, because it cannot:
// a dynamic column's type is not known until its first value arrives. The
// backing is instead allocated — once, pre-sized to capacity — by the Put that
// commits the column to a type (see [Chunk.commitDynamic]), so a dynamic column
// pays the same single sized allocation a statically declared one does.
//
// A dynamic column has no declared kind and no backing at construction: the
// first [Chunk.PutInt64]/[Chunk.PutFloat64]/[Chunk.PutString]/[Chunk.PutBool]
// COMMITS it to the matching typed backing, and a later value of a conflicting
// scalar kind — or any non-scalar value via [Chunk.PutValue] — PROMOTES it to a
// boxed []expr.Value backing (re-boxing the values already stored). This is the
// dynamic type promotion the [Chunk] type documentation defers for statically
// constructed columns; it is what the Cypher late-materialisation projection
// needs, because a property column's kind is not known until the values are read
// (openCypher permits a property to carry different types across nodes). A column
// whose first appended value is NULL commits to boxed (a null-first column stays
// boxed — a correct, minor missed optimisation). Box-at-sink ([Chunk.BoxCell])
// is unaffected: a typed cell boxes to the identical [expr.Value] a boxed cell
// would, and a NULL boxes to [expr.Null].
func NewDynamicChunk(capacity, ncols int) *Chunk {
	if capacity < 1 {
		capacity = DefaultChunkCapacity
	}
	c := &Chunk{
		capacity: capacity,
		cols:     make([]column, ncols),
	}
	for j := range c.cols {
		col := &c.cols[j]
		col.store = stDynamic
		col.dynamic = true
		col.allValid = true
	}
	return c
}

// ─────────────────────────────────────────────────────────────────────────────
// Introspection
// ─────────────────────────────────────────────────────────────────────────────

// NumCols returns the number of columns in the Chunk.
func (c *Chunk) NumCols() int { return len(c.cols) }

// Cap returns the per-column row capacity hint the Chunk was constructed with.
func (c *Chunk) Cap() int { return c.capacity }

// Len returns the logical row count. For a well-formed (rectangular) Chunk every
// column shares this length; it is the length of the first column, or 0 when the
// Chunk has no columns.
func (c *Chunk) Len() int {
	if len(c.cols) == 0 {
		return 0
	}
	return c.cols[0].n
}

// ColKind returns the declared logical [expr.Kind] of column j.
func (c *Chunk) ColKind(j int) expr.Kind { return c.cols[j].kind }

// ─────────────────────────────────────────────────────────────────────────────
// Builders — append (primary) and positional set
// ─────────────────────────────────────────────────────────────────────────────

// AppendInt64 appends v to integer column j, marking it non-null. It panics if
// column j is not an integer column.
func (c *Chunk) AppendInt64(j int, v int64) { c.pushI64(c.typedCol(j, stI64), v) }

// AppendFloat64 appends v to float column j, marking it non-null. It panics if
// column j is not a float column.
func (c *Chunk) AppendFloat64(j int, v float64) { c.pushF64(c.typedCol(j, stF64), v) }

// AppendString appends v to string column j, marking it non-null. It panics if
// column j is not a string column.
func (c *Chunk) AppendString(j int, v string) { c.pushStr(c.typedCol(j, stStr), v) }

// AppendBool appends v to bool column j, marking it non-null. It panics if column
// j is not a bool column.
func (c *Chunk) AppendBool(j int, v bool) { c.pushBool(c.typedCol(j, stBool), v) }

// pushI64/pushF64/pushStr/pushBool/pushBoxed append one already-typed value to
// col's live backing, advance the row count, and mark the cell non-null. They
// are the shared primitives behind the strict Append* API (statically typed
// columns) and the dynamic Put* API (Put-decided columns); the caller has
// already selected/committed col.store to the matching backing.
//
// Each escalates the backing to the Chunk's capacity through [growTo] before
// appending. That call is inert for a statically sized column and for every push
// but the one that exhausts [dynamicCommitFloor]; it is what keeps a dynamic
// column that fills to capacity at two allocations rather than a doubling series.
func (c *Chunk) pushI64(col *column, v int64) {
	row := col.n
	col.i64 = append(growTo(col.i64, c.capacity), v)
	col.n++
	c.recordValid(col, row)
}

func (c *Chunk) pushF64(col *column, v float64) {
	row := col.n
	col.f64 = append(growTo(col.f64, c.capacity), v)
	col.n++
	c.recordValid(col, row)
}

func (c *Chunk) pushStr(col *column, v string) {
	row := col.n
	col.str = append(growTo(col.str, c.capacity), v)
	col.n++
	c.recordValid(col, row)
}

func (c *Chunk) pushBool(col *column, v bool) {
	row := col.n
	col.b = append(growTo(col.b, c.capacity), v)
	col.n++
	c.recordValid(col, row)
}

func (c *Chunk) pushBoxed(col *column, v expr.Value) {
	row := col.n
	col.boxed = append(growTo(col.boxed, c.capacity), v)
	col.n++
	c.recordValid(col, row)
}

// AppendNull appends a NULL to column j. A placeholder zero value is written to
// the live backing to keep row indices aligned; the validity bitmap records the
// NULL, and it is the bitmap — never the placeholder — that box-at-sink honours.
//
// It escalates through [growTo] for the same reason the push helpers do: a
// column fed mostly NULLs — an OPTIONAL MATCH that misses, a projection over a
// sparse property — reaches the batch through this path and nothing else, and
// without the escalation it would walk the doubling series up from
// [dynamicCommitFloor] that growTo exists to avoid.
func (c *Chunk) AppendNull(j int) {
	col := &c.cols[j]
	switch col.store {
	case stI64:
		col.i64 = append(growTo(col.i64, c.capacity), 0)
	case stF64:
		col.f64 = append(growTo(col.f64, c.capacity), 0)
	case stStr:
		col.str = append(growTo(col.str, c.capacity), "")
	case stBool:
		col.b = append(growTo(col.b, c.capacity), false)
	case stBoxed:
		col.boxed = append(growTo(col.boxed, c.capacity), nil)
	}
	row := col.n
	col.n++
	c.recordNull(col, row)
}

// AppendValue appends a boxed [expr.Value] to column j at the sink boundary,
// routing it to the column's typed backing. A nil interface or [expr.Null]
// appends a NULL. For a typed (scalar) column the value's concrete type must
// match the column's storage, otherwise AppendValue panics (dynamic promotion is
// deferred; see the type doc). A boxed column accepts any value.
func (c *Chunk) AppendValue(j int, v expr.Value) {
	if v == nil || expr.IsNull(v) {
		c.AppendNull(j)
		return
	}
	col := &c.cols[j]
	switch col.store {
	case stI64:
		iv, ok := v.(expr.IntegerValue)
		if !ok {
			panic(fmt.Sprintf("exec: Chunk.AppendValue: column %d is Integer, got %s", j, v.Kind()))
		}
		c.AppendInt64(j, int64(iv))
	case stF64:
		fv, ok := v.(expr.FloatValue)
		if !ok {
			panic(fmt.Sprintf("exec: Chunk.AppendValue: column %d is Float, got %s", j, v.Kind()))
		}
		c.AppendFloat64(j, float64(fv))
	case stStr:
		sv, ok := v.(expr.StringValue)
		if !ok {
			panic(fmt.Sprintf("exec: Chunk.AppendValue: column %d is String, got %s", j, v.Kind()))
		}
		c.AppendString(j, string(sv))
	case stBool:
		bv, ok := v.(expr.BoolValue)
		if !ok {
			panic(fmt.Sprintf("exec: Chunk.AppendValue: column %d is Bool, got %s", j, v.Kind()))
		}
		c.AppendBool(j, bool(bv))
	case stBoxed:
		c.pushBoxed(col, v)
	}
}

// AppendRowFrom appends the whole logical row srcRow of src to c, one cell per
// column, WITHOUT boxing a scalar: each source cell is copied into c's matching
// column through the typed append primitives, and a NULL source cell appends a
// NULL. It is the row-compaction primitive [ColumnarFilter] uses to copy a
// passing row from its source batch into its output batch. c and src MUST have
// the same column count and matching per-column storage (the filter's output
// chunk is built from the source producer's schema), otherwise the typed append
// panics on a kind mismatch. It panics on an out-of-range srcRow.
func (c *Chunk) AppendRowFrom(src *Chunk, srcRow int) {
	if len(c.cols) != len(src.cols) {
		panic(fmt.Sprintf("exec: Chunk.AppendRowFrom: column count mismatch: dst %d, src %d",
			len(c.cols), len(src.cols)))
	}
	for j := range src.cols {
		sc := &src.cols[j]
		src.checkRow(sc, srcRow)
		if !isValid(sc, srcRow) {
			c.AppendNull(j)
			continue
		}
		switch sc.store {
		case stI64:
			c.AppendInt64(j, sc.i64[srcRow])
		case stF64:
			c.AppendFloat64(j, sc.f64[srcRow])
		case stStr:
			c.AppendString(j, sc.str[srcRow])
		case stBool:
			c.AppendBool(j, sc.b[srcRow])
		case stBoxed:
			c.AppendValue(j, sc.boxed[srcRow])
		case stDynamic:
			// A valid cell always has a committed backing; an uncommitted dynamic
			// column holds no rows, so this arm is unreachable for a valid row.
		}
	}
}

// IsInt64Column reports whether column j's live backing is an unboxed int64
// slice — the storage the scan emits for a raw NodeID column. The columnar
// projection's chunk-input fast path uses it to confirm a source column really
// holds raw NodeIDs before reading them via [Chunk.Int64], falling back to the
// boxed row path otherwise so a non-int64 column never triggers a kind-mismatch
// panic on the read path.
func (c *Chunk) IsInt64Column(j int) bool { return c.cols[j].store == stI64 }

// IsFloat64Column reports whether column j's live backing is an unboxed
// []float64. Together with [Chunk.IsInt64Column]/[Chunk.IsStringColumn]/
// [Chunk.IsBoolColumn] it lets a consumer (e.g. the columnar EagerAggregation
// grouping-key path, #2049) read a scalar cell unboxed via the typed accessor
// for that kind and fall back to [Chunk.BoxCell] for a boxed or promoted column.
func (c *Chunk) IsFloat64Column(j int) bool { return c.cols[j].store == stF64 }

// IsStringColumn reports whether column j's live backing is an unboxed []string.
// See [Chunk.IsFloat64Column].
func (c *Chunk) IsStringColumn(j int) bool { return c.cols[j].store == stStr }

// IsBoolColumn reports whether column j's live backing is an unboxed []bool. See
// [Chunk.IsFloat64Column].
func (c *Chunk) IsBoolColumn(j int) bool { return c.cols[j].store == stBool }

// CopyCellTo copies cell (srcCol, srcRow) of c into column dstCol of dst WITHOUT
// boxing a plain scalar: a typed cell is appended through dst's matching dynamic
// [Chunk.PutInt64]/[Chunk.PutFloat64]/[Chunk.PutString]/[Chunk.PutBool], a boxed
// cell through [Chunk.PutValue] (which re-routes a plain scalar back to a typed
// backing and keeps every other kind boxed), and a NULL cell through
// [Chunk.PutNull]. It reads nothing but the already-materialised value the source
// chunk holds — no graph access — so it is the scalar-column passthrough primitive
// (rmp #2045): a projection over an already-columnar child copies a materialised
// value from the child's chunk instead of re-boxing it row-at-a-time or re-reading
// the graph by NodeID (which the box-at-sink isolation contract forbids). dst
// columns must be dynamic or match the source kind — the [Chunk.PutValue]/Put*
// family promotes a dynamic column on a kind conflict rather than panicking, so a
// heterogeneous passthrough column stays byte-identical to the row-at-a-time path.
// It panics on an out-of-range srcRow.
func (c *Chunk) CopyCellTo(srcCol, srcRow int, dst *Chunk, dstCol int) {
	col := &c.cols[srcCol]
	c.checkRow(col, srcRow)
	if !isValid(col, srcRow) {
		dst.PutNull(dstCol)
		return
	}
	switch col.store {
	case stI64:
		dst.PutInt64(dstCol, col.i64[srcRow])
	case stF64:
		dst.PutFloat64(dstCol, col.f64[srcRow])
	case stStr:
		dst.PutString(dstCol, col.str[srcRow])
	case stBool:
		dst.PutBool(dstCol, col.b[srcRow])
	case stBoxed:
		dst.PutValue(dstCol, col.boxed[srcRow])
	case stDynamic:
		// An uncommitted dynamic column holds no rows; a valid cell always has a
		// committed backing. Reached only defensively.
		dst.PutNull(dstCol)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Builders — dynamic Put API (type-decided, promoting), for NewDynamicChunk
// ─────────────────────────────────────────────────────────────────────────────
//
// These target a column constructed by [NewDynamicChunk] whose kind is not known
// until the values arrive. The first Put of a given scalar kind COMMITS the
// column to the matching typed backing; a later value of a different scalar kind,
// or any non-scalar via [Chunk.PutValue], PROMOTES the column to a boxed backing
// (re-boxing the values already stored). Once boxed a column stays boxed. Unlike
// the strict Append* API they never panic on a kind change — promotion is the
// designed response to a heterogeneous column. They also work on a statically
// typed (non-dynamic) column, where a matching-kind Put is an ordinary append and
// a mismatching-kind Put promotes to boxed rather than panicking.

// dynamicCommitFloor is the number of rows a committing Put reserves for an
// undecided column, instead of the Chunk's full capacity.
//
// It exists because a Chunk's capacity is a HINT, not a prediction: when the
// plan exposes no sound [ResultSet.RowCountHint] — which an indexed point lookup
// correctly does not, since every operator that can drop rows deliberately
// withholds one — [NewDynamicChunk] falls back to [DefaultChunkCapacity]. A
// reservation made against that fallback is a reservation against a number
// nobody claimed, and rmp #2389 measured what that costs: a query returning ONE
// row reserved 32 KB to hold 8 bytes, 3.2 million times over, for -56% throughput
// on examples/35_mvcc_mixed_workload.
//
// 16 rows is chosen so that the overwhelmingly common small result — a point
// lookup, a LIMIT, an existence check — is served by a single 128-byte
// allocation for an int64 column, while a column that goes on to fill escalates
// in one step (see [growTo]) rather than walking append's doubling series. The
// full-batch case therefore still costs two allocations, not sixteen.
const dynamicCommitFloor = 16

// commitDynamic commits an undecided column to storage st and reserves a small
// floor of backing for it when it has none yet.
//
// It exists because [NewDynamicChunk] cannot size a backing whose type it does
// not know, and a column left at nil walks append's whole growth series up from
// one element. Reserving [dynamicCommitFloor] rows removes that series for small
// results outright and shortens it to a single escalation for large ones: the
// first push that finds the floor full jumps straight to the Chunk's capacity in
// [growTo]. Measured on one int64 column filled to the default 4096-row capacity,
// the fill costs 2 allocations against the 16 a nil backing walked.
//
// The cap == 0 guard is what keeps a reused chunk free: [Chunk.Reset] restores
// the undecided tag but RETAINS every backing, so a pooled chunk re-committing
// to the same kind finds its array already there and allocates nothing. The
// make is therefore paid at most once per backing per chunk lifetime.
//
// The trade this accepts, stated plainly: a column that fills to capacity pays
// one extra allocation and one copy of at most [dynamicCommitFloor] elements,
// against a column that stops short no longer holding a capacity-sized array it
// never used. rmp #2389 measured both halves on the two examples that sit at
// opposite ends of that trade and neither regressed.
func (c *Chunk) commitDynamic(col *column, st storageTag) {
	col.store = st
	n := min(c.capacity, dynamicCommitFloor)
	switch st {
	case stI64:
		if cap(col.i64) == 0 {
			col.i64 = make([]int64, 0, n)
		}
	case stF64:
		if cap(col.f64) == 0 {
			col.f64 = make([]float64, 0, n)
		}
	case stStr:
		if cap(col.str) == 0 {
			col.str = make([]string, 0, n)
		}
	case stBool:
		if cap(col.b) == 0 {
			col.b = make([]bool, 0, n)
		}
	case stBoxed:
		if cap(col.boxed) == 0 {
			col.boxed = make([]expr.Value, 0, n)
		}
	}
}

// growTo raises s's capacity to target in ONE step when s is full and still
// short of target, and returns it unchanged otherwise.
//
// It is the second half of the [dynamicCommitFloor] trade: a column that starts
// at the floor and goes on to fill would otherwise walk append's doubling series
// from 16 to the capacity, copying more in total than the single reservation it
// replaced. Jumping straight to the capacity the first time the floor is
// exhausted keeps the filling column at two allocations.
//
// It is inert for a statically typed column: [NewChunk] already sizes those to
// the capacity, so cap(s) >= target holds from the start and the first branch
// returns immediately. Past the capacity it is inert too, and append's own
// amortised doubling takes over — the capacity is a hint, never a cap on rows.
func growTo[T any](s []T, target int) []T {
	if len(s) < cap(s) || cap(s) >= target {
		return s
	}
	grown := make([]T, len(s), target)
	copy(grown, s)
	return grown
}

// PutInt64 appends v as an integer to column j, committing or promoting its
// backing as described in [NewDynamicChunk].
func (c *Chunk) PutInt64(j int, v int64) {
	col := &c.cols[j]
	switch col.store {
	case stI64:
		c.pushI64(col, v)
	case stDynamic:
		c.commitDynamic(col, stI64)
		c.pushI64(col, v)
	case stBoxed:
		c.pushBoxed(col, expr.IntegerValue(v))
	default: // stF64 / stStr / stBool — scalar-kind conflict
		c.promoteToBoxed(col)
		c.pushBoxed(col, expr.IntegerValue(v))
	}
}

// PutFloat64 appends v as a float to column j, committing or promoting its
// backing as described in [NewDynamicChunk].
func (c *Chunk) PutFloat64(j int, v float64) {
	col := &c.cols[j]
	switch col.store {
	case stF64:
		c.pushF64(col, v)
	case stDynamic:
		c.commitDynamic(col, stF64)
		c.pushF64(col, v)
	case stBoxed:
		c.pushBoxed(col, expr.FloatValue(v))
	default:
		c.promoteToBoxed(col)
		c.pushBoxed(col, expr.FloatValue(v))
	}
}

// PutString appends v as a string to column j, committing or promoting its
// backing as described in [NewDynamicChunk].
func (c *Chunk) PutString(j int, v string) {
	col := &c.cols[j]
	switch col.store {
	case stStr:
		c.pushStr(col, v)
	case stDynamic:
		c.commitDynamic(col, stStr)
		c.pushStr(col, v)
	case stBoxed:
		c.pushBoxed(col, expr.StringValue(v))
	default:
		c.promoteToBoxed(col)
		c.pushBoxed(col, expr.StringValue(v))
	}
}

// PutBool appends v as a bool to column j, committing or promoting its backing as
// described in [NewDynamicChunk].
func (c *Chunk) PutBool(j int, v bool) {
	col := &c.cols[j]
	switch col.store {
	case stBool:
		c.pushBool(col, v)
	case stDynamic:
		c.commitDynamic(col, stBool)
		c.pushBool(col, v)
	case stBoxed:
		c.pushBoxed(col, expr.BoolValue(v))
	default:
		c.promoteToBoxed(col)
		c.pushBoxed(col, expr.BoolValue(v))
	}
}

// PutNull appends a NULL to column j. On a dynamic column the first value being
// NULL commits the column to a boxed backing (a null-first column stays boxed);
// on an already-committed column it records a NULL in the live backing exactly
// like [Chunk.AppendNull]. The validity bitmap — never the placeholder — is what
// box-at-sink honours.
func (c *Chunk) PutNull(j int) {
	col := &c.cols[j]
	if col.store == stDynamic {
		c.commitDynamic(col, stBoxed)
	}
	c.AppendNull(j)
}

// PutValue appends a boxed [expr.Value] to column j, routing a plain scalar
// (Integer/Float/String/Bool) to the typed fast paths — so a fallback that
// produces an already-boxed scalar keeps the column typed — and boxing every
// other kind (temporal / point / list / map / node / …) into the boxed backing,
// promoting the column if necessary. A nil interface or [expr.Null] appends a
// NULL. This is the sink-boundary entry point the columnar projection uses for
// values it must keep boxed for byte-identity with the row-at-a-time path.
func (c *Chunk) PutValue(j int, v expr.Value) {
	switch cv := v.(type) {
	case nil:
		c.PutNull(j)
	case expr.IntegerValue:
		c.PutInt64(j, int64(cv))
	case expr.FloatValue:
		c.PutFloat64(j, float64(cv))
	case expr.StringValue:
		c.PutString(j, string(cv))
	case expr.BoolValue:
		c.PutBool(j, bool(cv))
	default:
		if expr.IsNull(v) {
			c.PutNull(j)
			return
		}
		col := &c.cols[j]
		switch col.store {
		case stBoxed:
			// already boxed
		case stDynamic:
			c.commitDynamic(col, stBoxed)
		default:
			c.promoteToBoxed(col)
		}
		c.pushBoxed(&c.cols[j], v)
	}
}

// promoteToBoxed converts col's committed typed backing to a boxed
// []expr.Value backing, re-boxing every value already stored (a NULL cell boxes
// to nil so box-at-sink yields [expr.Null]). It is the one-time O(n) cost paid
// when a dynamic column first sees a value of a conflicting kind; heterogeneous
// columns are rare, so this is amortised negligible. col.store must be a typed
// scalar backing on entry; it is stBoxed on return.
func (c *Chunk) promoteToBoxed(col *column) {
	// Sized past the rows already stored, but only to the same floor
	// [Chunk.commitDynamic] reserves rather than to the Chunk's full capacity:
	// the promotion happens mid-fill, so a slice of exactly col.n would be
	// regrown by the very next push, while a capacity-sized one reserves against
	// a hint nobody claimed (rmp #2389). [growTo] escalates it in one step if the
	// rest of the batch does arrive.
	boxed := make([]expr.Value, col.n, max(col.n, min(dynamicCommitFloor, c.capacity)))
	for row := 0; row < col.n; row++ {
		if !isValid(col, row) {
			continue // leave nil → boxes to expr.Null
		}
		switch col.store {
		case stI64:
			boxed[row] = expr.IntegerValue(col.i64[row])
		case stF64:
			boxed[row] = expr.FloatValue(col.f64[row])
		case stStr:
			boxed[row] = expr.StringValue(col.str[row])
		case stBool:
			boxed[row] = expr.BoolValue(col.b[row])
		}
	}
	col.boxed = boxed
	col.i64, col.f64, col.str, col.b = nil, nil, nil, nil
	col.store = stBoxed
}

// SetInt64 overwrites row of integer column j with v, marking it non-null. row
// must be an already-appended row (0 <= row < column length). It panics on a
// kind mismatch or an out-of-range row.
func (c *Chunk) SetInt64(j, row int, v int64) {
	col := c.typedCol(j, stI64)
	c.checkRow(col, row)
	col.i64[row] = v
	c.recordValid(col, row)
}

// SetFloat64 overwrites row of float column j with v, marking it non-null. See
// [Chunk.SetInt64] for the row and panic contract.
func (c *Chunk) SetFloat64(j, row int, v float64) {
	col := c.typedCol(j, stF64)
	c.checkRow(col, row)
	col.f64[row] = v
	c.recordValid(col, row)
}

// SetString overwrites row of string column j with v, marking it non-null. See
// [Chunk.SetInt64] for the row and panic contract.
func (c *Chunk) SetString(j, row int, v string) {
	col := c.typedCol(j, stStr)
	c.checkRow(col, row)
	col.str[row] = v
	c.recordValid(col, row)
}

// SetBool overwrites row of bool column j with v, marking it non-null. See
// [Chunk.SetInt64] for the row and panic contract.
func (c *Chunk) SetBool(j, row int, v bool) {
	col := c.typedCol(j, stBool)
	c.checkRow(col, row)
	col.b[row] = v
	c.recordValid(col, row)
}

// SetNull marks row of column j as NULL, releasing any reference the slot held
// (string header or boxed value) so the batch does not pin it. See
// [Chunk.SetInt64] for the row and panic contract.
func (c *Chunk) SetNull(j, row int) {
	col := &c.cols[j]
	c.checkRow(col, row)
	switch col.store {
	case stStr:
		col.str[row] = ""
	case stBoxed:
		col.boxed[row] = nil
	case stI64, stF64, stBool:
		// no reference to release
	}
	c.recordNull(col, row)
}

// ─────────────────────────────────────────────────────────────────────────────
// Cell accessors (secondary) — typed, no interface dispatch
// ─────────────────────────────────────────────────────────────────────────────

// Int64 returns the value and validity of row in integer column j. When the cell
// is NULL the returned value is the (meaningless) placeholder and valid is false;
// callers must check valid. It panics on a kind mismatch or an out-of-range row.
func (c *Chunk) Int64(j, row int) (value int64, valid bool) {
	col := c.typedCol(j, stI64)
	c.checkRow(col, row)
	return col.i64[row], isValid(col, row)
}

// Float64 returns the value and validity of row in float column j. See
// [Chunk.Int64] for the NULL and panic contract.
func (c *Chunk) Float64(j, row int) (value float64, valid bool) {
	col := c.typedCol(j, stF64)
	c.checkRow(col, row)
	return col.f64[row], isValid(col, row)
}

// String returns the value and validity of row in string column j. The returned
// string is a view; the arena swap planned for a later phase keeps this
// signature. See [Chunk.Int64] for the NULL and panic contract.
func (c *Chunk) String(j, row int) (value string, valid bool) {
	col := c.typedCol(j, stStr)
	c.checkRow(col, row)
	return col.str[row], isValid(col, row)
}

// Bool returns the value and validity of row in bool column j. See [Chunk.Int64]
// for the NULL and panic contract.
func (c *Chunk) Bool(j, row int) (value, valid bool) {
	col := c.typedCol(j, stBool)
	c.checkRow(col, row)
	return col.b[row], isValid(col, row)
}

// IsValid reports whether row of column j is non-null. It panics on an
// out-of-range row.
func (c *Chunk) IsValid(j, row int) bool {
	col := &c.cols[j]
	c.checkRow(col, row)
	return isValid(col, row)
}

// IsNull reports whether row of column j is NULL. It panics on an out-of-range
// row.
func (c *Chunk) IsNull(j, row int) bool { return !c.IsValid(j, row) }

// ─────────────────────────────────────────────────────────────────────────────
// Vectorized column accessors (primary hot-path API)
// ─────────────────────────────────────────────────────────────────────────────
//
// These hand out the typed backing slice for a whole column so a kernel can scan
// data[:len] in a tight loop. This is the primary, cache-friendly access shape;
// the per-cell accessors above are a convenience/fallback. The returned data is
// sliced to the column's logical length. valid is the raw packed bitmap and is
// meaningful only when allValid is false; when allValid is true every row is
// non-null and valid may be nil. Use [BitSet] to test a bit of valid.

// Int64Column returns the backing of integer column j. It panics on a kind
// mismatch.
func (c *Chunk) Int64Column(j int) (data []int64, valid []uint64, allValid bool) {
	col := c.typedCol(j, stI64)
	return col.i64[:col.n], col.valid, col.allValid
}

// Float64Column returns the backing of float column j. It panics on a kind
// mismatch.
func (c *Chunk) Float64Column(j int) (data []float64, valid []uint64, allValid bool) {
	col := c.typedCol(j, stF64)
	return col.f64[:col.n], col.valid, col.allValid
}

// StringColumn returns the backing of string column j. It panics on a kind
// mismatch.
//
// This []string shape is Phase-1 only: once the string bytes arena lands it will
// no longer exist. Consumers that must survive that change should use the cell
// accessor [Chunk.String] (which keeps its signature).
func (c *Chunk) StringColumn(j int) (data []string, valid []uint64, allValid bool) {
	col := c.typedCol(j, stStr)
	return col.str[:col.n], col.valid, col.allValid
}

// BoolColumn returns the backing of bool column j. It panics on a kind mismatch.
func (c *Chunk) BoolColumn(j int) (data []bool, valid []uint64, allValid bool) {
	col := c.typedCol(j, stBool)
	return col.b[:col.n], col.valid, col.allValid
}

// ─────────────────────────────────────────────────────────────────────────────
// Box-at-sink — convert unboxed columns back to expr.Value at the boundary
// ─────────────────────────────────────────────────────────────────────────────

// BoxCell boxes cell (j, row) back to an [expr.Value]. A NULL cell (validity 0)
// boxes to [expr.Null] regardless of the backing slot's value. This, and
// [Chunk.BoxRow], are the only places boxing happens once operators are wired.
// It panics on an out-of-range row.
func (c *Chunk) BoxCell(j, row int) expr.Value {
	col := &c.cols[j]
	c.checkRow(col, row)
	if !isValid(col, row) {
		return expr.Null
	}
	switch col.store {
	case stI64:
		return expr.IntegerValue(col.i64[row])
	case stF64:
		return expr.FloatValue(col.f64[row])
	case stStr:
		return expr.StringValue(col.str[row])
	case stBool:
		return expr.BoolValue(col.b[row])
	case stBoxed:
		if col.boxed[row] == nil {
			return expr.Null
		}
		return col.boxed[row]
	case stDynamic:
		// An uncommitted dynamic column holds no rows; a boxable cell always has a
		// committed backing. Reached only defensively.
		return expr.Null
	}
	return expr.Null
}

// BoxRow boxes the whole logical row at index row into dst, one [expr.Value] per
// column, and returns dst (reused when it has capacity, else freshly allocated).
// It is the row-materialization primitive later phases use to emit a [Row] at the
// sink. It panics if the Chunk is ragged (columns of unequal length) or row is
// out of range.
func (c *Chunk) BoxRow(row int, dst Row) Row {
	n := len(c.cols)
	if cap(dst) < n {
		dst = make(Row, n)
	} else {
		dst = dst[:n]
	}
	if row < 0 || row >= c.rows() {
		panic(fmt.Sprintf("exec: Chunk.BoxRow: row %d out of range [0,%d)", row, c.rows()))
	}
	for j := range c.cols {
		dst[j] = c.BoxCell(j, row)
	}
	return dst
}

// RowByteEstimate returns a coarse, allocation-free byte estimate for the whole
// logical row at index row, for a columnar sink's byte-budget accounting: per
// column it charges overhead for a NULL cell or a fixed-width scalar
// (int64/float64/bool), overhead plus the byte length for a string cell, and
// estimateBoxed applied to the stored [expr.Value] for a boxed cell. It boxes
// nothing. Summed this way it equals what a per-value estimator (overhead-based,
// string-length-aware) summed over the row's boxed values would yield, so a
// columnar drain's byte budget trips at the same point the row-oriented drain
// does. It panics on an out-of-range row.
func (c *Chunk) RowByteEstimate(row int, overhead int64, estimateBoxed func(expr.Value) int64) int64 {
	var total int64
	for j := range c.cols {
		col := &c.cols[j]
		c.checkRow(col, row)
		if !isValid(col, row) {
			total += overhead
			continue
		}
		switch col.store {
		case stStr:
			total += overhead + int64(len(col.str[row]))
		case stBoxed:
			total += estimateBoxed(col.boxed[row])
		default: // stI64 / stF64 / stBool
			total += overhead
		}
	}
	return total
}

// ─────────────────────────────────────────────────────────────────────────────
// Reset
// ─────────────────────────────────────────────────────────────────────────────

// Reset clears every column for reuse while retaining the backing allocations.
// It resets the length to 0 and, per column: leaves fixed-width backings
// (int64/float64/bool) untouched (they hold no pointers, so stale values are
// never traced by the GC and are invisible past length 0); nils the used slots of
// string and boxed backings (their headers hold pointers and would otherwise pin
// memory); zeroes the validity bitmap words while keeping the bitmap backing; and
// restores the allValid fast path.
func (c *Chunk) Reset() {
	for j := range c.cols {
		col := &c.cols[j]
		// Reset every backing's length, retaining its array for pool reuse. A
		// static column has exactly one live backing; a dynamic column may have a
		// committed typed backing (and, after a promotion, also a boxed one). The
		// pointer-bearing backings (string headers, boxed values) are nilled so the
		// batch does not pin memory past its logical end; the fixed-width backings
		// hold no pointers and stale values are invisible past length 0.
		col.i64 = col.i64[:0]
		col.f64 = col.f64[:0]
		if len(col.str) > 0 {
			for i := range col.str {
				col.str[i] = ""
			}
			col.str = col.str[:0]
		}
		col.b = col.b[:0]
		if len(col.boxed) > 0 {
			for i := range col.boxed {
				col.boxed[i] = nil
			}
			col.boxed = col.boxed[:0]
		}
		if col.dynamic {
			// Restore the undecided state so a pooled dynamic chunk re-discovers
			// each column's kind on its next fill; the retained backings above are
			// reused when the next commit picks the same kind.
			col.store = stDynamic
		}
		if len(col.valid) > 0 {
			clear(col.valid)
		}
		col.allValid = true
		col.n = 0
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// typedCol returns column j, panicking if its live backing is not want.
func (c *Chunk) typedCol(j int, want storageTag) *column {
	col := &c.cols[j]
	if col.store != want {
		panic(fmt.Sprintf("exec: Chunk column %d has kind %s (storage %d), wanted storage %d",
			j, col.kind, col.store, want))
	}
	return col
}

// checkRow panics if row is not an already-filled row of col.
func (c *Chunk) checkRow(col *column, row int) {
	if row < 0 || row >= col.n {
		panic(fmt.Sprintf("exec: Chunk row %d out of range [0,%d)", row, col.n))
	}
}

// rows returns the logical row count, panicking if the Chunk is ragged (columns
// of unequal length). Used by row-spanning operations that require a rectangular
// Chunk.
func (c *Chunk) rows() int {
	if len(c.cols) == 0 {
		return 0
	}
	n := c.cols[0].n
	for j := 1; j < len(c.cols); j++ {
		if c.cols[j].n != n {
			panic(fmt.Sprintf("exec: Chunk is ragged: column 0 has %d rows, column %d has %d",
				n, j, c.cols[j].n))
		}
	}
	return n
}

// recordValid records that (col, row) is non-null. Under the allValid fast path
// there is nothing to do; once the bitmap has been materialized the bit must be
// set explicitly.
func (c *Chunk) recordValid(col *column, row int) {
	if col.allValid {
		return
	}
	ensureValidLen(col, row+1)
	col.valid[row>>6] |= uint64(1) << (uint(row) & 63)
}

// recordNull records that (col, row) is NULL. The first NULL materializes an
// all-ones bitmap for the previously-appended rows [0,row), clears the allValid
// flag, then clears the bit for row.
func (c *Chunk) recordNull(col *column, row int) {
	if col.allValid {
		ensureValidLen(col, row+1)
		setValidUpTo(col.valid, row) // rows [0,row) were valid
		col.allValid = false
	} else {
		ensureValidLen(col, row+1)
	}
	col.valid[row>>6] &^= uint64(1) << (uint(row) & 63)
}

// ensureValidLen grows col.valid so it can address at least nbits bits. New words
// are zero, which reads as NULL under the bitmap convention; callers set the bits
// they need.
func ensureValidLen(col *column, nbits int) {
	need := (nbits + 63) >> 6
	if len(col.valid) >= need {
		return
	}
	if cap(col.valid) >= need {
		col.valid = col.valid[:need]
		return
	}
	grown := make([]uint64, need)
	copy(grown, col.valid)
	col.valid = grown
}

// setValidUpTo sets bits [0,count) of bitmap to 1 (valid).
func setValidUpTo(bitmap []uint64, count int) {
	fullWords := count >> 6
	for i := 0; i < fullWords; i++ {
		bitmap[i] = ^uint64(0)
	}
	if rem := count & 63; rem != 0 {
		bitmap[fullWords] |= (uint64(1) << uint(rem)) - 1
	}
}

// isValid reports whether (col, row) is non-null, honouring the allValid fast
// path and the packed bitmap.
func isValid(col *column, row int) bool {
	if col.allValid {
		return true
	}
	w := row >> 6
	if w >= len(col.valid) {
		return false
	}
	return col.valid[w]&(uint64(1)<<(uint(row)&63)) != 0
}

// BitSet reports whether bit i of a packed validity bitmap is set (LSB-first,
// the Arrow convention). It is the canonical test for a raw bitmap returned by
// the vectorized column accessors when allValid is false.
func BitSet(bitmap []uint64, i int) bool {
	w := i >> 6
	if w < 0 || w >= len(bitmap) {
		return false
	}
	return bitmap[w]&(uint64(1)<<(uint(i)&63)) != 0
}

// ─────────────────────────────────────────────────────────────────────────────
// ChunkPool
// ─────────────────────────────────────────────────────────────────────────────

// ChunkPool is a [sync.Pool]-backed pool of [Chunk] instances with a fixed
// schema (column kinds) and capacity. Operators that process a high volume of
// batches should obtain chunks from a shared pool to reduce GC pressure.
//
// ChunkPool is safe for concurrent use; the [Chunk] instances it vends are not.
type ChunkPool struct {
	p sync.Pool
}

// NewChunkPool creates a ChunkPool that vends Chunks with the given capacity and
// column kinds. The kinds slice is copied, so the caller may reuse it.
func NewChunkPool(capacity int, kinds ...expr.Kind) *ChunkPool {
	kindsCopy := append([]expr.Kind(nil), kinds...)
	cp := &ChunkPool{}
	cp.p = sync.Pool{
		New: func() any {
			return NewChunk(capacity, kindsCopy...)
		},
	}
	return cp
}

// Get retrieves a Chunk from the pool, or allocates a new one. A pooled Chunk was
// [Chunk.Reset] before being returned, so it is empty.
func (cp *ChunkPool) Get() *Chunk {
	metrics.IncCounter("cypher.pool.chunk.get", 1)
	return cp.p.Get().(*Chunk) //nolint:forcetypeassert // pool invariant: New always returns *Chunk
}

// Put resets c and returns it to the pool.
func (cp *ChunkPool) Put(c *Chunk) {
	metrics.IncCounter("cypher.pool.chunk.put", 1)
	c.Reset()
	cp.p.Put(c)
}
