package exec

// hash_join_columnar.go — ColumnarHashJoin, the [ChunkProducer] counterpart of
// [HashJoin] for the disconnected equi-join pattern (#2105, design docs/
// columnar-deepening-design.md §5).
//
// ColumnarHashJoin is a drop-in, result-identical replacement for [HashJoin]
// that the planner substitutes ONLY when BOTH children are [ChunkProducer]s (a
// bare scan on each arm — the common `MATCH (a:A),(b:B) WHERE a.x = b.y` shape).
// It preserves the exact multiset [HashJoin] produces; the win is ALLOCATION,
// not scan throughput (joins are the least common analytic-Cypher shape, so this
// is the lowest-leverage columnar operator — design §1, §5).
//
// # What it removes
//
// [HashJoin] snapshots every surviving build row with a per-row make(Row)+copy
// held for the whole probe phase, and boxes every build/probe cell through its
// child's row-at-a-time Next. ColumnarHashJoin instead:
//
//   - drains the build child COLUMN-MAJOR via [ChunkProducer.FillChunk] into an
//     owned buffer chunk that grows across batches (no per-row Row box on the
//     build side);
//   - retains only ROW-IDS in the hash buckets (hash → []int32), indexing into
//     that column-major buffer, so a matched build row is late-materialised from
//     its columns at emit time rather than snapshotted up front (DuckDB
//     PhysicalHashJoin / Velox HashBuild);
//   - streams the probe child column-major, likewise;
//   - copies matched build + probe columns UNBOXED into its output chunk
//     ([Chunk.CopyCellTo]) on the [ChunkProducer] output path, boxing only at the
//     sink.
//
// # Key semantics — identical to HashJoin (never reimplemented)
//
// The join key is an arbitrary expression evaluated per row by [KeyFn]. Because
// FillChunk's boxed output equals the child's Next output cell-for-cell (the
// ChunkProducer contract every scan/filter/expand upholds), evaluating the KeyFn
// against [Chunk.BoxRow] of a buffered row yields the identical key the row path
// would. Keys are hashed with [canonicalKeyHash] ([expr.EquivalentHash], the
// float64-domain fold so integer 1 and float 1.0 collide) and every bucket hit
// is re-verified with [expr.Value.Equal] (exact cross-type numeric comparison,
// never Go ==): a ≥2^53 cross-type collision (int 2^53+1 vs float 2^53.0) shares
// a bucket but is correctly separated. NULL and NaN keys are excluded exactly
// (via [isUnjoinableKey]) — they satisfy no equi-join under openCypher 9 §3.2/
// §3.4.
//
// # Memory bound (#1841)
//
// The retained build buffer is bounded by the same estimated-byte budget as
// [HashJoin] ([ColumnarHashJoin.WithByteBudget]): buffering stops with
// [ErrHashJoinMemoryExceeded] once the retained estimate crosses the ceiling —
// byte-for-byte the same threshold and sentinel the row-mode hash join enforces,
// so a build side over budget fails-stop identically rather than growing an
// unbounded columnar buffer.
//
// # Concurrency
//
// ColumnarHashJoin is NOT safe for concurrent use. Each pipeline segment owns its
// own operator tree. It spawns no goroutines.
//
// # Cancellation
//
// ctx.Err() is checked at the top of every Next/FillChunk call and every 4096
// iterations of the build drain and the output loop.

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ColumnarHashJoin is the [ChunkProducer] equi-join operator described in the
// file comment. It implements both the row-mode [Operator] contract (its Next is
// the byte-identical fallback for a non-columnar parent) and the column-major
// [ChunkProducer] contract (FillChunk, preferred by a columnar-aware sink).
//
// ColumnarHashJoin is NOT safe for concurrent use.
type ColumnarHashJoin struct {
	build ChunkProducer
	probe ChunkProducer
	ctx   context.Context //nolint:containedctx // stored for per-Next ctx check

	buildFn KeyFn
	probeFn KeyFn

	// build table (column-major). buildBuf retains only joinable build rows;
	// table maps a canonical key hash to their row-ids; keyCol holds each
	// buffered row's evaluated key for the exact per-bucket Equal re-verify.
	buildBuf     *Chunk
	buildScratch *Chunk
	table        map[uint64][]int32
	keyCol       []expr.Value

	// probe streaming state.
	probeScratch *Chunk
	probeKey     expr.Value
	bucket       []int32

	// keyScratch is the single reused Row that BoxRow fills for a KeyFn call, so
	// no per-row Row is allocated to evaluate a key. outBuf is the reused row-mode
	// output buffer (Next path only).
	keyScratch Row
	outBuf     Row

	// outKinds is the output chunk's per-column logical kind (build||probe or
	// probe||build per buildOnLeft), precomputed from the children's output
	// schema so NewOutputChunk builds a typed, pre-sized chunk instead of a
	// dynamic one that would reallocate its backing as it grows (#2105 byte gate).
	outKinds []expr.Kind

	budget byteBudget

	buildCols int
	probeCols int

	probeLen    int // rows in the current probe batch
	probeRowCur int // next unread row index within probeScratch
	probeCurRow int // active probe row (its bucket is being scanned)
	bucketIdx   int // next index into bucket to test

	buildOnLeft  bool
	built        bool
	activeProbe  bool // a probe row's bucket is loaded and being scanned
	probeScanEOS bool // the probe child has been fully drained
}

// NewColumnarHashJoin returns a [ColumnarHashJoin] when BOTH build and probe are
// [NodeIDColumnProducer]s, or (nil, false) otherwise — in which case the caller
// keeps the row-mode [HashJoin] (design §6.2: the columnar operator is wired only
// when every child qualifies, so existing plans are unchanged).
//
// The [NodeIDColumnProducer] requirement (stronger than plain [ChunkProducer]) is
// load-bearing for the allocation win: because every hash-join arm is a bare node
// scan, both children carry each bound node's raw int64 NodeID unboxed, so the
// join's output columns are all raw NodeIDs too and ColumnarHashJoin can itself be
// a [NodeIDColumnProducer]. That lets a [ColumnarProject] above the join read a
// node property (a.k / b.k) unboxed straight from the output chunk, keeping the
// chunk chain unbroken to the sink (design §0). Gating on plain [ChunkProducer]
// instead would break that chain for node-property projection: the projection
// falls back to row-input Next and re-boxes every output node cell under fan-out —
// a measured allocation REGRESSION (#2065 pattern). A child that is a
// [ChunkProducer] but not a [NodeIDColumnProducer] therefore keeps the row-mode
// [HashJoin].
//
//   - build is drained fully, column-major, into the hash table.
//   - probe is streamed column-major against the table.
//   - buildFn / probeFn extract the join key from a build / probe row.
//   - buildOnLeft selects the output column order (build||probe vs probe||build),
//     matching the Apply the planner replaces exactly as [HashJoin] does.
//
// ColumnarHashJoin takes ownership of both plans; callers must not use them
// afterwards.
func NewColumnarHashJoin(build, probe Operator, buildFn, probeFn KeyFn, buildOnLeft bool) (*ColumnarHashJoin, bool) {
	bcp, ok := build.(NodeIDColumnProducer)
	if !ok {
		return nil, false
	}
	pcp, ok := probe.(NodeIDColumnProducer)
	if !ok {
		return nil, false
	}
	// Templates for the children's output schema (Init-independent for every
	// ChunkProducer; capacity 1 → minimal backing). Column counts and the output
	// column kinds are fixed by this schema and needed by NewOutputChunk before Init.
	bt := bcp.NewOutputChunk(1)
	pt := pcp.NewOutputChunk(1)
	op := &ColumnarHashJoin{
		build:       bcp,
		probe:       pcp,
		buildFn:     buildFn,
		probeFn:     probeFn,
		buildOnLeft: buildOnLeft,
		buildCols:   bt.NumCols(),
		probeCols:   pt.NumCols(),
	}
	// Output column order mirrors the join's row schema (build||probe or
	// probe||build per buildOnLeft), so a ColumnarProject above the join reads a
	// node column from the position the schema records.
	if buildOnLeft {
		op.outKinds = appendColKinds(appendColKinds(nil, bt), pt)
	} else {
		op.outKinds = appendColKinds(appendColKinds(nil, pt), bt)
	}
	return op, true
}

// appendColKinds appends every column's logical [expr.Kind] of c to dst.
func appendColKinds(dst []expr.Kind, c *Chunk) []expr.Kind {
	for j := 0; j < c.NumCols(); j++ {
		dst = append(dst, c.ColKind(j))
	}
	return dst
}

// nodeIDColumnProducer marks ColumnarHashJoin as a [NodeIDColumnProducer]: every
// output column mirrors a bound node variable and carries that node's raw int64
// NodeID, copied unboxed from the children (themselves NodeIDColumnProducers, a
// constructor precondition). The output column order is probe||build or
// build||probe per buildOnLeft, which mirrors the join's row schema
// (schema[nodeVar] == output column), so a [ColumnarProject] above the join reads
// a node property from the right column unboxed. Sound only because
// [NewColumnarHashJoin] refuses to build unless both children are
// NodeIDColumnProducers.
func (op *ColumnarHashJoin) nodeIDColumnProducer() {}

// buildBufCap returns the capacity to pre-size the retained build buffer to: the
// build child's exact row-count hint when it exposes one (a scan reports its
// candidate-set size after Init, an upper bound on the joinable rows retained),
// else [DefaultChunkCapacity]. Pre-sizing to the hint holds the whole build side
// without the geometric-growth reallocations a default-capacity buffer pays on a
// large build, and — the point of #2105's no-regression gate — avoids the fixed
// 4096-row over-allocation on a small build.
func buildBufCap(cp ChunkProducer) int {
	if h, ok := cp.(rowCountHinter); ok {
		if n, hasHint := h.rowCountHint(); hasHint && n > 0 {
			return n
		}
	}
	return DefaultChunkCapacity
}

// streamBatchCap bounds a streaming fill buffer (the per-batch build/probe
// scratch): the child's row-count hint clamped to [DefaultChunkCapacity], so a
// small input is sized exactly (no 4096-row over-allocation) while a large input
// still streams in bounded batches rather than materialising whole.
func streamBatchCap(cp ChunkProducer) int {
	n := buildBufCap(cp)
	if n > DefaultChunkCapacity {
		return DefaultChunkCapacity
	}
	return n
}

// WithByteBudget bounds the estimated retained size of the build buffer by
// maxBytes, returning [ErrHashJoinMemoryExceeded] when exceeded (#1841). It
// mirrors [HashJoin.WithByteBudget] exactly (same estimator, same sentinel), so
// the columnar and row-mode joins trip at the identical threshold. A non-positive
// maxBytes or nil estimateRow leaves it unbounded. Returns op for chaining and
// must be called before Init.
func (op *ColumnarHashJoin) WithByteBudget(maxBytes int64, estimateRow func(Row) int64) *ColumnarHashJoin {
	op.budget.set(maxBytes, estimateRow)
	return op
}

// Init initialises both child plans and resets join state.
func (op *ColumnarHashJoin) Init(ctx context.Context) error {
	op.ctx = ctx
	op.table = nil
	op.buildBuf = nil
	op.buildScratch = nil
	op.probeScratch = nil
	op.keyCol = op.keyCol[:0]
	op.bucket = nil
	op.probeKey = nil
	op.built = false
	op.activeProbe = false
	op.probeScanEOS = false
	op.probeLen = 0
	op.probeRowCur = 0
	op.probeCurRow = 0
	op.bucketIdx = 0
	op.outBuf = op.outBuf[:0]
	op.budget.reset()
	if err := op.build.Init(ctx); err != nil {
		return err
	}
	return op.probe.Init(ctx)
}

// buildTable drains the build child column-major and constructs the hash table.
// Only joinable rows (non-NULL, non-NaN key) are retained in buildBuf and
// bucketed; NULL/NaN keys are excluded exactly as [HashJoin] does, so neither the
// buffer nor the byte budget accounts for a row that can never match.
func (op *ColumnarHashJoin) buildTable() error {
	// Pre-size both buffers from the build child's row-count hint (valid now: the
	// child was Init'd in op.Init, and a scan reports its candidate-set size after
	// Init). buildBuf holds every joinable build row (sized to the hint, an upper
	// bound); buildScratch is the bounded per-batch fill buffer. Both mirror the
	// child's output schema (via NewOutputChunk). A retained row is appended into
	// buildBuf column-by-column via CopyCellTo, which handles a static or dynamic
	// destination column alike (committing/promoting as needed) and boxes back
	// byte-identically at the sink.
	bufCap := buildBufCap(op.build)
	op.buildBuf = op.build.NewOutputChunk(bufCap)
	op.buildScratch = op.build.NewOutputChunk(streamBatchCap(op.build))
	// Pre-size the parallel key column to the same bound so it does not reallocate
	// geometrically as joinable rows accrue (keeps B/op flat vs the row-mode join,
	// whose keys live in the bucket slices — #2105 byte gate).
	if cap(op.keyCol) < bufCap {
		op.keyCol = make([]expr.Value, 0, bufCap)
	}
	op.table = make(map[uint64][]int32)
	var iter int
	for {
		op.buildScratch.Reset()
		n, err := op.build.FillChunk(op.buildScratch, op.buildScratch.Cap())
		if err != nil {
			return err
		}
		if n == 0 {
			break // build child exhausted
		}
		for row := 0; row < n; row++ {
			iter++
			if iter&4095 == 0 {
				if err := op.ctx.Err(); err != nil {
					return err
				}
			}
			if err := op.retainBuildRow(row); err != nil {
				return err
			}
		}
	}
	op.built = true
	return nil
}

// retainBuildRow evaluates the build key for row of the current build batch and,
// when the key is joinable, appends the row column-major into buildBuf and
// buckets its row-id. The byte budget is charged on the boxed row exactly as
// [HashJoin] charges it, so the retained-memory threshold is identical.
func (op *ColumnarHashJoin) retainBuildRow(row int) error {
	op.keyScratch = op.buildScratch.BoxRow(row, op.keyScratch)
	key, err := op.buildFn(op.keyScratch)
	if err != nil {
		return err
	}
	if isUnjoinableKey(key) {
		return nil
	}
	if op.budget.charge(op.keyScratch) {
		return ErrHashJoinMemoryExceeded
	}
	for j := 0; j < op.buildCols; j++ {
		op.buildScratch.CopyCellTo(j, row, op.buildBuf, j)
	}
	id := int32(op.buildBuf.Len() - 1)
	op.keyCol = append(op.keyCol, key)
	h := canonicalKeyHash(key)
	op.table[h] = append(op.table[h], id)
	return nil
}

// Next advances the join in row mode, boxing the matched (build||probe or
// probe||build) columns into a reused output buffer. It is the byte-identical
// fallback [HashJoin.Next] provides for a non-columnar parent.
func (op *ColumnarHashJoin) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if !op.built {
		if err := op.buildTable(); err != nil {
			return false, err
		}
	}
	probeRow, buildID, ok, err := op.nextMatch()
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	op.emitRow(out, probeRow, buildID)
	return true, nil
}

// NewOutputChunk returns a [Chunk] sized for the join output: one column per
// output column, typed by the children's output schema (build||probe or
// probe||build per buildOnLeft) and pre-sized to capacity. Because both children
// are NodeIDColumnProducers, every output column has a known scalar kind, so a
// typed chunk — rather than a dynamic one that reallocates its backing as it fills
// — copies each cell unboxed via [Chunk.CopyCellTo] and boxes byte-identically at
// the sink. It implements [ChunkProducer].
func (op *ColumnarHashJoin) NewOutputChunk(capacity int) *Chunk {
	return NewChunk(capacity, op.outKinds...)
}

// FillChunk appends up to maxRows matched output rows into dst column-major and
// returns the number appended (0 at end-of-stream). Matched build columns are
// copied from the retained build buffer and probe columns from the current probe
// batch, both unboxed via [Chunk.CopyCellTo]. It implements [ChunkProducer].
func (op *ColumnarHashJoin) FillChunk(dst *Chunk, maxRows int) (int, error) {
	if err := op.ctx.Err(); err != nil {
		return 0, err
	}
	if !op.built {
		if err := op.buildTable(); err != nil {
			return 0, err
		}
	}
	n := 0
	for n < maxRows {
		if n&4095 == 0 && n > 0 {
			if err := op.ctx.Err(); err != nil {
				return n, err
			}
		}
		probeRow, buildID, ok, err := op.nextMatch()
		if err != nil {
			return n, err
		}
		if !ok {
			return n, nil
		}
		op.emitChunk(dst, probeRow, buildID)
		n++
	}
	return n, nil
}

// nextMatch advances the probe/bucket cursors to the next matching (probe row,
// build row-id) pair, pulling probe batches as needed, and returns ok=false at
// end-of-stream. It is shared by the row-mode Next and the columnar FillChunk so
// both emit exactly the same sequence of matches; only the emit step differs.
func (op *ColumnarHashJoin) nextMatch() (probeRow int, buildID int32, ok bool, err error) {
	for {
		// Drain the current bucket against the active probe row, verifying each hit
		// with the exact openCypher comparator (never Go ==).
		if op.activeProbe {
			for op.bucketIdx < len(op.bucket) {
				id := op.bucket[op.bucketIdx]
				op.bucketIdx++
				if expr.IsTruthy(op.keyCol[id].Equal(op.probeKey)) {
					return op.probeCurRow, id, true, nil
				}
			}
			op.activeProbe = false
			op.bucket = nil
			op.bucketIdx = 0
		}
		r, has, perr := op.pullProbeRow()
		if perr != nil {
			return 0, 0, false, perr
		}
		if !has {
			return 0, 0, false, nil
		}
		op.keyScratch = op.probeScratch.BoxRow(r, op.keyScratch)
		key, kerr := op.probeFn(op.keyScratch)
		if kerr != nil {
			return 0, 0, false, kerr
		}
		if isUnjoinableKey(key) {
			continue // NULL/NaN probe key matches nothing — skip without a lookup
		}
		op.probeKey = key
		op.probeCurRow = r
		op.bucket = op.table[canonicalKeyHash(key)]
		op.bucketIdx = 0
		op.activeProbe = true
	}
}

// pullProbeRow returns the next unread probe row index within the current probe
// batch, pulling a fresh batch via [ChunkProducer.FillChunk] when the current one
// is drained. It returns has=false at end-of-stream. The returned index is valid
// only until the next pull that refills probeScratch — the caller must consume it
// (evaluate the key, emit every match) before advancing.
func (op *ColumnarHashJoin) pullProbeRow() (row int, has bool, err error) {
	if op.probeScratch == nil {
		// Bounded per-batch buffer sized from the probe hint (exact for a small
		// probe, clamped so a large probe still streams in bounded batches).
		op.probeScratch = op.probe.NewOutputChunk(streamBatchCap(op.probe))
	}
	for {
		if op.probeRowCur < op.probeLen {
			r := op.probeRowCur
			op.probeRowCur++
			return r, true, nil
		}
		if op.probeScanEOS {
			return 0, false, nil
		}
		op.probeScratch.Reset()
		n, ferr := op.probe.FillChunk(op.probeScratch, op.probeScratch.Cap())
		if ferr != nil {
			return 0, false, ferr
		}
		op.probeLen = n
		op.probeRowCur = 0
		if n == 0 {
			op.probeScanEOS = true
			return 0, false, nil
		}
	}
}

// emitChunk copies the matched build and probe columns into dst honouring
// buildOnLeft, unboxed.
func (op *ColumnarHashJoin) emitChunk(dst *Chunk, probeRow int, buildID int32) {
	if op.buildOnLeft {
		op.copyBuildCols(dst, buildID, 0)
		op.copyProbeCols(dst, probeRow, op.buildCols)
		return
	}
	op.copyProbeCols(dst, probeRow, 0)
	op.copyBuildCols(dst, buildID, op.probeCols)
}

// copyBuildCols copies every build column of build row buildID into dst starting
// at column offset.
func (op *ColumnarHashJoin) copyBuildCols(dst *Chunk, buildID int32, offset int) {
	for j := 0; j < op.buildCols; j++ {
		op.buildBuf.CopyCellTo(j, int(buildID), dst, offset+j)
	}
}

// copyProbeCols copies every probe column of the current probe row into dst
// starting at column offset.
func (op *ColumnarHashJoin) copyProbeCols(dst *Chunk, probeRow, offset int) {
	for j := 0; j < op.probeCols; j++ {
		op.probeScratch.CopyCellTo(j, probeRow, dst, offset+j)
	}
}

// emitRow boxes the matched build and probe columns into the reused output buffer
// honouring buildOnLeft. The boxed cells are value copies (immutable scalars /
// string headers), so the row stays valid across a subsequent probe-batch refill,
// exactly as [HashJoin.emit]'s reused outBuf does.
func (op *ColumnarHashJoin) emitRow(out *Row, probeRow int, buildID int32) {
	need := op.buildCols + op.probeCols
	if cap(op.outBuf) < need {
		op.outBuf = make(Row, need)
	}
	op.outBuf = op.outBuf[:need]
	if op.buildOnLeft {
		op.boxBuildCols(buildID, 0)
		op.boxProbeCols(probeRow, op.buildCols)
	} else {
		op.boxProbeCols(probeRow, 0)
		op.boxBuildCols(buildID, op.probeCols)
	}
	*out = op.outBuf
}

// boxBuildCols boxes every build column of build row buildID into op.outBuf
// starting at column offset.
func (op *ColumnarHashJoin) boxBuildCols(buildID int32, offset int) {
	for j := 0; j < op.buildCols; j++ {
		op.outBuf[offset+j] = op.buildBuf.BoxCell(j, int(buildID))
	}
}

// boxProbeCols boxes every probe column of the current probe row into op.outBuf
// starting at column offset.
func (op *ColumnarHashJoin) boxProbeCols(probeRow, offset int) {
	for j := 0; j < op.probeCols; j++ {
		op.outBuf[offset+j] = op.probeScratch.BoxCell(j, probeRow)
	}
}

// Close releases the hash table and closes both child plans.
func (op *ColumnarHashJoin) Close() error {
	buildErr := op.build.Close()
	probeErr := op.probe.Close()
	op.table = nil
	op.buildBuf = nil
	op.buildScratch = nil
	op.probeScratch = nil
	op.keyCol = nil
	op.bucket = nil
	op.probeKey = nil
	op.outBuf = nil
	if buildErr != nil {
		return buildErr
	}
	return probeErr
}
