package exec

// columnar_limit.go — chunk-transparent LIMIT (rmp #2186).
//
// The chunk chain rule (docs/columnar-deepening-design.md §0) is that a columnar
// chain reaches the sink only if every operator between the leaf and the sink is a
// [ChunkProducer]. Plain [Limit] is not, so a LIMIT at the plan root forced the
// ENTIRE suffix below it into row mode — even a semantically inert one. The round-3
// audit measured `WHERE n.v > 10 RETURN n.v LIMIT 100000` over a 98 900-row result at
// 4.3x the time and 2414x the allocations of the identical LIMIT-free query
// (docs/audit-2026-07-26-streams/s05-runtime.md F1).
//
// ColumnarLimit is the fix, and it needs no new logic: LIMIT is a prefix of its
// child's stream, so filling a chunk under a limit is the child's own FillChunk with
// maxRows clamped to the rows still allowed. There is no per-row work, no predicate
// and no re-boxing.
//
// # Concurrency
//
// ColumnarLimit is NOT safe for concurrent use, matching [Limit].

// ColumnarLimit is a [Limit] that additionally implements [ChunkProducer], keeping
// the chunk chain unbroken through a LIMIT.
//
// The [Limit] is embedded BY VALUE, so a ColumnarLimit is one heap allocation exactly
// like the plain Limit it replaces, and a ColumnarLimit whose parent turns out to
// consume it row-at-a-time costs nothing extra: the promoted [Limit.Next] runs
// unchanged, and `emitted` is the SAME field either way, so the two paths cannot
// disagree about how many rows have been emitted.
//
// ColumnarLimit is NOT safe for concurrent use.
type ColumnarLimit struct {
	Limit                    // boxed fallback: promoted Next/Close/Init, n, emitted, ctx
	chunkChild ChunkProducer // the columnar child (the SAME object as Limit.child)
}

// NewColumnarLimit returns a [ColumnarLimit] over lim when lim's child is a
// [ChunkProducer], and (nil, false) otherwise — in which case the caller keeps the
// plain [Limit], whose behaviour is identical.
//
// lim must not have been initialised yet: the returned operator takes over its state.
func NewColumnarLimit(lim *Limit) (*ColumnarLimit, bool) {
	if lim == nil {
		return nil, false
	}
	cp, ok := lim.child.(ChunkProducer)
	if !ok {
		return nil, false
	}
	return &ColumnarLimit{Limit: *lim, chunkChild: cp}, true
}

// NewOutputChunk returns a [Chunk] shaped like the child's output: LIMIT truncates
// the row stream and never changes the column layout, so its output schema equals its
// child's. It implements [ChunkProducer].
func (op *ColumnarLimit) NewOutputChunk(capacity int) *Chunk {
	return op.chunkChild.NewOutputChunk(capacity)
}

// FillChunk appends up to maxRows rows into dst (column-major) and returns the number
// appended, 0 once the limit is reached or the child is exhausted. It implements
// [ChunkProducer].
//
// The clamp is the whole operator: at most `n - emitted` further rows may ever be
// emitted, so the child is asked for no more than that. Because a consumer treats a
// short fill (n < maxRows) as end-of-stream, clamping below the requested maxRows also
// signals exhaustion at exactly the right moment — the limit is reached — and a
// subsequent call returns 0 regardless.
//
// Rows appended here count towards the SAME `emitted` counter [Limit.Next] uses, so
// a plan that drains partly column-major and partly row-at-a-time still emits exactly
// n rows in total.
func (op *ColumnarLimit) FillChunk(dst *Chunk, maxRows int) (int, error) {
	if err := op.ctx.Err(); err != nil {
		return 0, err
	}
	remaining := op.n - op.emitted
	if remaining <= 0 {
		return 0, nil
	}
	if int64(maxRows) > remaining {
		maxRows = int(remaining)
	}
	if maxRows <= 0 {
		return 0, nil
	}
	n, err := op.chunkChild.FillChunk(dst, maxRows)
	op.emitted += int64(n)
	return n, err
}
