package sim

import (
	"context"
	"fmt"
)

// defaultMaxShrinkIterations bounds the number of scripted-replay attempts the
// shrinker performs, so ddmin always terminates even on a pathological trace.
// ddmin is already O(n²) in the worst case; this is a hard ceiling on top of
// that, after which the best reduction found so far is returned.
const defaultMaxShrinkIterations = 10_000

// ShrinkConfig parameterises trace shrinking. The zero value is valid and uses
// the defaults.
type ShrinkConfig struct {
	// MaxIterations caps the scripted-replay attempts. A non-positive value uses
	// [defaultMaxShrinkIterations].
	MaxIterations int
}

// ShrinkResult is the outcome of shrinking: the minimal trace found, the
// violation it still reproduces, and the work the shrinker did.
type ShrinkResult struct {
	// Minimal is the reduced trace that still reproduces the target violation.
	Minimal Trace
	// OriginalLen / MinimalLen are the op counts before and after shrinking.
	OriginalLen int
	MinimalLen  int
	// Iterations is the number of scripted-replay attempts performed.
	Iterations int
	// Violation is a representative violation the minimal trace reproduces (the
	// first one found on the final replay), for the report.
	Violation Violation
}

// Ratio returns the reduction factor (original / minimal); 1 means no reduction.
// It is reported so a caller can assert an orders-of-magnitude shrink.
func (r ShrinkResult) Ratio() float64 {
	if r.MinimalLen == 0 {
		return float64(r.OriginalLen)
	}
	return float64(r.OriginalLen) / float64(r.MinimalLen)
}

// violationSignature is the equivalence class the shrinker preserves: a
// reduction is kept only when its replay reproduces a violation with the SAME
// signature as the original failure. Keying on (kind, op-label) is strong enough
// to avoid the shrinker latching onto a DIFFERENT bug than the one it started
// from, without being so strict (e.g. exact message text with embedded ids)
// that a legitimate smaller reproducer is rejected.
type violationSignature struct {
	kind ViolationKind
	op   string
}

// signatureOf returns the signature of the first violation in a report, or the
// zero signature when the report is nil (no violation).
func signatureOf(report *SimReport) (violationSignature, Violation, bool) {
	if report == nil || len(report.Violations) == 0 {
		return violationSignature{}, Violation{}, false
	}
	v := report.Violations[0]
	return violationSignature{kind: v.Kind, op: v.Op}, v, true
}

// ShrinkTrace reduces a failing trace to a (near-)minimal subsequence that still
// reproduces the SAME violation, via delta-debugging (ddmin). It first confirms
// the full trace fails under scripted replay and captures the target violation
// signature, then repeatedly partitions the op sequence into n chunks and tries
// (a) removing each chunk and (b) keeping each chunk's complement, accepting the
// smallest candidate whose replay still reproduces the target signature and
// increasing the granularity otherwise — the classic Zeller–Hildebrandt ddmin.
//
// Determinism: every step is a scripted [ReplayTrace] (no seed draws, no
// goroutines, no wall clock), so shrinking the same failing trace always yields
// the same minimal trace. Boundedness: the search is capped at
// cfg.MaxIterations replay attempts; the best reduction found by then is
// returned.
//
// Cross-op dependencies are preserved implicitly: a candidate that drops an op
// the violation depends on (e.g. the lost-write CREATE itself, or a node a
// later edge needs) replays to a DIFFERENT signature (or to no violation), so
// ddmin rejects it and keeps the op. No explicit reference repair is required
// because the "still reproduces the same violation" oracle is exact.
//
// It returns an error only when the input trace does NOT reproduce a violation
// under scripted replay (there is nothing to shrink), or when ctx is cancelled.
func ShrinkTrace(ctx context.Context, trace Trace, cfg ShrinkConfig) (ShrinkResult, error) {
	maxIter := cfg.MaxIterations
	if maxIter <= 0 {
		maxIter = defaultMaxShrinkIterations
	}

	full, err := ReplayTrace(ctx, trace)
	if err != nil {
		return ShrinkResult{}, fmt.Errorf("sim: shrink: replay full trace: %w", err)
	}
	target, targetViol, ok := signatureOf(full.Report)
	if !ok {
		return ShrinkResult{}, fmt.Errorf("sim: shrink: input trace does not reproduce a violation; nothing to shrink")
	}

	sh := &shrinker{ctx: ctx, target: target, maxIter: maxIter, lastViol: targetViol}
	minimal, err := sh.run(trace)
	if err != nil {
		return ShrinkResult{}, err
	}
	return ShrinkResult{
		Minimal:     minimal,
		OriginalLen: trace.Len(),
		MinimalLen:  minimal.Len(),
		Iterations:  sh.iterations,
		Violation:   sh.lastViol,
	}, nil
}

// shrinker carries the ddmin search state.
type shrinker struct {
	ctx        context.Context
	target     violationSignature
	maxIter    int
	iterations int
	lastViol   Violation
}

// reproduces reports whether the candidate op slice still reproduces the target
// violation signature under scripted replay. It charges one iteration and
// records the matching violation so the result can report it.
func (s *shrinker) reproduces(ops []TracedOp, base Trace) bool {
	if s.iterations >= s.maxIter {
		return false
	}
	s.iterations++
	res, err := ReplayTrace(s.ctx, base.withOps(ops))
	if err != nil {
		return false
	}
	sig, viol, ok := signatureOf(res.Report)
	if ok && sig == s.target {
		s.lastViol = viol
		return true
	}
	return false
}

// run performs the ddmin loop over the trace's ops and returns the minimal trace.
func (s *shrinker) run(trace Trace) (Trace, error) {
	ops := trace.Ops
	n := 2
	for len(ops) >= 2 {
		if err := s.ctx.Err(); err != nil {
			return Trace{}, err
		}
		if s.iterations >= s.maxIter {
			break
		}
		chunkSize := len(ops) / n
		reduced, advanced := s.reduceAtGranularity(ops, n, chunkSize, trace)
		if advanced {
			ops = reduced
			n = max(n-1, 2) // a successful reduction restarts at coarser granularity
			continue
		}
		if n >= len(ops) {
			break // granularity exhausted: cannot subdivide further
		}
		n = min(n*2, len(ops)) // increase granularity
	}
	return trace.withOps(ops), nil
}

// reduceAtGranularity tries to remove a single chunk (and then to keep a single
// chunk's complement) at the current granularity n. It returns the reduced op
// slice and whether any reduction was accepted.
func (s *shrinker) reduceAtGranularity(ops []TracedOp, n, chunkSize int, trace Trace) (reduced []TracedOp, advanced bool) {
	// Phase 1: try removing each chunk (ddmin "reduce to complement" is phase 2;
	// phase 1 here is "remove subset" — both are part of the standard algorithm).
	for i := 0; i < n; i++ {
		lo := i * chunkSize
		hi := lo + chunkSize
		if i == n-1 {
			hi = len(ops) // last chunk absorbs the remainder
		}
		candidate := removeRange(ops, lo, hi)
		if len(candidate) > 0 && s.reproduces(candidate, trace) {
			return candidate, true
		}
		if s.iterations >= s.maxIter {
			return ops, false
		}
	}
	// Phase 2: try reducing to each chunk's complement (keep only one chunk).
	for i := 0; i < n; i++ {
		lo := i * chunkSize
		hi := lo + chunkSize
		if i == n-1 {
			hi = len(ops)
		}
		candidate := keepRange(ops, lo, hi)
		if len(candidate) > 0 && len(candidate) < len(ops) && s.reproduces(candidate, trace) {
			return candidate, true
		}
		if s.iterations >= s.maxIter {
			return ops, false
		}
	}
	return ops, false
}

// removeRange returns a fresh slice with ops[lo:hi] removed.
func removeRange(ops []TracedOp, lo, hi int) []TracedOp {
	out := make([]TracedOp, 0, len(ops)-(hi-lo))
	out = append(out, ops[:lo]...)
	out = append(out, ops[hi:]...)
	return out
}

// keepRange returns a fresh slice holding only ops[lo:hi].
func keepRange(ops []TracedOp, lo, hi int) []TracedOp {
	out := make([]TracedOp, hi-lo)
	copy(out, ops[lo:hi])
	return out
}
