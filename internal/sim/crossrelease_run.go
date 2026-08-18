package sim

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
)

// CrossReleaseUpgradeResult summarises a cross-release UPGRADE run: a PRIOR
// release wrote a durable store image, then BOTH the prior release (via its own
// recovery) and the CURRENT code reopened it. The cross-version
// data-compatibility contract is that the current code recovers the prior image
// IDENTICALLY to the prior release's own recovery of it — so a prior-release WAL
// that does not round-trip in its own release (a pre-existing prior defect) is
// surfaced as [PriorWALFidelityGap] rather than blamed on the current code.
type CrossReleaseUpgradeResult struct {
	// DataCompatError is set when the current code FAILED-STOP opening the prior
	// image (refused to recover it). This is the explicit, non-silent
	// data-compatibility signal: a clear error rather than a silent mis-recovery.
	DataCompatError error
	// Tag is the prior release that wrote the image.
	Tag string
	// CountMismatch is set when the current code OPENED the image but recovered
	// different node/edge counts than the prior release's own recovery — a genuine
	// current-code data-compatibility regression.
	CountMismatch string
	// PriorLiveNodes / PriorLiveEdges are the prior release's LIVE engine counts
	// after the write phase (before any reopen).
	PriorLiveNodes int64
	PriorLiveEdges int64
	// PriorSelfNodes / PriorSelfEdges are the counts the prior release's OWN
	// recovery rebuilds from the image — the durable truth the current code must
	// reproduce.
	PriorSelfNodes int64
	PriorSelfEdges int64
	// RecoveredNodes / RecoveredEdges are the CURRENT code's counts after reopen.
	RecoveredNodes int64
	RecoveredEdges int64
	// ReplayedWALOps is how many WAL ops the current recovery replayed.
	ReplayedWALOps int
	// PriorWALFidelityGap is true when the prior release's OWN recovery already
	// diverges from its live counts (its WAL does not round-trip in its own
	// release). This is a PRIOR-release defect, recorded but NOT charged to the
	// current code; the current/prior-self contract can still hold.
	PriorWALFidelityGap bool

	// ── Snapshot provenance (rmp #2477) ──────────────────────────────────────
	//
	// Everything below describes the half of the image the harness could not see
	// before this task: the SNAPSHOT DIRECTORY the prior release published, and
	// whether the current code genuinely opened it.

	// HelperBuildFallbackErr is the error that forced the helper build to drop
	// its checkpoint half at this tag, or nil when the checkpoint-bearing build
	// succeeded. See [BuildPriorReleaseHelper].
	HelperBuildFallbackErr error
	// CheckpointErr is the prior release's own checkpoint failure, empty when the
	// checkpoint succeeded or was never attempted.
	CheckpointErr string
	// SnapshotProvenanceGap is set when the image carries a snapshot directory on
	// disk that the CURRENT recovery did not load, or loaded but could not parse.
	// It is the fault this task exists to be able to detect at all.
	SnapshotProvenanceGap string
	// PriorSnapshot is what the current manifest reader saw in the prior
	// release's snapshot directory, read straight off disk.
	PriorSnapshot PriorSnapshotFacts
	// SnapshotLabels / SnapshotProperties are how many label and property records
	// the prior release's snapshot components fed back into the recovered graph.
	SnapshotLabels     int
	SnapshotProperties int
	// HelperCheckpointBuilt reports that the helper for this tag was built WITH
	// its checkpoint half; false means the tag's checkpoint API did not match and
	// the image is WAL-only.
	HelperCheckpointBuilt bool
	// CheckpointPublished reports that the prior release actually published the
	// checkpoint (it built AND ran).
	CheckpointPublished bool
	// SnapshotOpened reports that the CURRENT recovery loaded a snapshot from the
	// image. Compared against [PriorSnapshotFacts.Present], which is read from
	// the filesystem, so "there was a snapshot and the reader ignored it" is a
	// distinguishable state rather than an unfalsifiable false.
	SnapshotOpened bool
	// SnapshotOnlyRecovery reports the strongest provenance outcome: a non-empty
	// graph recovered with a snapshot loaded and ZERO WAL ops replayed, so every
	// recovered node came through the prior release's snapshot bytes and the WAL
	// cannot account for any of it.
	SnapshotOnlyRecovery bool
}

// Parity reports whether the current code reopened the prior image faithfully:
// no fail-stop, the current recovery matched the prior release's own recovery,
// and — when the image carries one — the prior release's snapshot directory was
// actually opened rather than skipped. A prior-release WAL fidelity gap does
// not, by itself, fail parity.
func (r *CrossReleaseUpgradeResult) Parity() bool {
	return r.DataCompatError == nil && r.CountMismatch == "" && r.SnapshotProvenanceGap == ""
}

// String renders the result for a test failure message.
func (r CrossReleaseUpgradeResult) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "cross-release upgrade %s -> current: priorLive(n=%d e=%d) priorSelf(n=%d e=%d) currentRecovered(n=%d e=%d) walOps=%d",
		r.Tag, r.PriorLiveNodes, r.PriorLiveEdges, r.PriorSelfNodes, r.PriorSelfEdges, r.RecoveredNodes, r.RecoveredEdges, r.ReplayedWALOps)
	fmt.Fprintf(&b, "\n  image: checkpointBuilt=%v published=%v snapshotOnDisk=%v snapshotOpened=%v snapshotOnly=%v manifestV%d integrity=%q verified=%v files=%v labels=%d props=%d",
		r.HelperCheckpointBuilt, r.CheckpointPublished, r.PriorSnapshot.Present, r.SnapshotOpened, r.SnapshotOnlyRecovery,
		r.PriorSnapshot.ManifestVersion, r.PriorSnapshot.Integrity, r.PriorSnapshot.IntegrityVerified,
		r.PriorSnapshot.Files, r.SnapshotLabels, r.SnapshotProperties)
	if r.HelperBuildFallbackErr != nil {
		fmt.Fprintf(&b, "\n  NOTE: helper at %s built WITHOUT its checkpoint half (image is WAL-only): %v", r.Tag, r.HelperBuildFallbackErr)
	}
	if r.CheckpointErr != "" {
		fmt.Fprintf(&b, "\n  NOTE: prior release %s checkpoint failed: %s", r.Tag, r.CheckpointErr)
	}
	if r.PriorSnapshot.ManifestErr != nil {
		fmt.Fprintf(&b, "\n  MANIFEST READ FAULT: %v", r.PriorSnapshot.ManifestErr)
	}
	if r.SnapshotProvenanceGap != "" {
		fmt.Fprintf(&b, "\n  SNAPSHOT PROVENANCE GAP: %s", r.SnapshotProvenanceGap)
	}
	if r.PriorWALFidelityGap {
		fmt.Fprintf(&b, "\n  NOTE: prior release %s WAL does not round-trip in its OWN recovery (live!=self) — a PRIOR-release defect, not charged to current code", r.Tag)
	}
	if r.DataCompatError != nil {
		fmt.Fprintf(&b, "\n  DATA-COMPAT FAIL-STOP: %v", r.DataCompatError)
	}
	if r.CountMismatch != "" {
		fmt.Fprintf(&b, "\n  COUNT MISMATCH (current vs prior-self): %s", r.CountMismatch)
	}
	return b.String()
}

// RunCrossReleaseUpgrade performs a true CROSS-VERSION upgrade test: it builds a
// prior-release helper from tag, has the prior release WRITE a durable store
// image (running a deterministic op stream derived from seed), then reopens that
// SAME image with BOTH the prior release's own recovery and the CURRENT recovery
// code, and asserts the current code recovers the image IDENTICALLY to the prior
// release.
//
// This is the genuine guard for the data-compatibility regression class the
// project hit (v0.2.0 -> v0.3.x adjlist recovery panic): if the current code
// cannot faithfully rebuild a prior release's image it must fail-stop with a
// clear error (carried in [CrossReleaseUpgradeResult.DataCompatError]) or recover
// a different graph than the prior release did (CountMismatch) — never silently
// lose or fabricate data. A prior-release WAL that does not even round-trip in
// its own release is flagged (PriorWALFidelityGap) but not charged to the current
// code, because the current code's job is to read the prior image faithfully, not
// to retroactively fix a prior release's persistence bug.
//
// repoRoot is the GoGraph working-tree root. A build/worktree failure is
// returned as an error for the caller to treat as a clean environment-precondition
// skip; an honest data-compatibility fault is carried in the result, not the
// error.
func RunCrossReleaseUpgrade(ctx context.Context, repoRoot, tag string, seed uint64, ops int) (CrossReleaseUpgradeResult, error) {
	helper, err := BuildPriorReleaseHelper(ctx, repoRoot, tag)
	if err != nil {
		return CrossReleaseUpgradeResult{}, err
	}
	defer func() { _ = helper.Close() }()

	opStream, err := GenerateCrossReleaseOps(seed, ops)
	if err != nil {
		return CrossReleaseUpgradeResult{}, err
	}

	imageDir, err := os.MkdirTemp("", "gograph-xrelease-image-")
	if err != nil {
		return CrossReleaseUpgradeResult{}, fmt.Errorf("sim: cross-release: image dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(imageDir) }()

	prior, err := helper.WriteImage(ctx, imageDir, opStream)
	if err != nil {
		return CrossReleaseUpgradeResult{}, fmt.Errorf("sim: cross-release: prior %s write image: %w", tag, err)
	}

	out := CrossReleaseUpgradeResult{
		Tag:                    tag,
		PriorLiveNodes:         prior.Nodes,
		PriorLiveEdges:         prior.Edges,
		HelperCheckpointBuilt:  helper.CheckpointSupported,
		HelperBuildFallbackErr: helper.BuildFallbackErr,
		CheckpointPublished:    prior.Checkpoint,
		CheckpointErr:          prior.CheckpointErr,
		// Read the published directory off the filesystem BEFORE any reopen, with
		// the current manifest reader. This is the independent half of the
		// provenance check: recovery's own SnapshotHit cannot be its own witness.
		PriorSnapshot: InspectPriorSnapshotDir(imageDir),
	}

	// Durable truth: how the PRIOR release reads its OWN image back.
	selfN, selfE, err := helper.SelfRecoverCounts(ctx, imageDir)
	if err != nil {
		return CrossReleaseUpgradeResult{}, fmt.Errorf("sim: cross-release: prior %s self-recovery: %w", tag, err)
	}
	out.PriorSelfNodes = selfN
	out.PriorSelfEdges = selfE
	out.PriorWALFidelityGap = selfN != prior.Nodes || selfE != prior.Edges

	// Upgrade boundary: reopen the prior image with the CURRENT recovery code.
	rec, openErr := recoverImageGraph(ctx, imageDir)
	if openErr != nil {
		// The current code refused the prior image: explicit, non-silent fail-stop.
		out.DataCompatError = openErr
		return out, nil
	}
	out.ReplayedWALOps = rec.walOps
	out.SnapshotOpened = rec.snapshotHit
	out.SnapshotLabels = rec.snapshotLabels
	out.SnapshotProperties = rec.snapshotProperties
	rn, _ := rec.engine.NodeCount()
	re, _ := rec.engine.EdgeCount()
	out.RecoveredNodes = rn
	out.RecoveredEdges = re
	out.SnapshotOnlyRecovery = rec.snapshotHit && rec.walOps == 0 && rn > 0
	out.SnapshotProvenanceGap = classifySnapshotProvenance(&out)

	// The cross-version contract: current recovery reproduces the prior release's
	// own recovery node-for-node. Edges are compared under the harness's matched
	// simple-graph config (recoverImageGraph), which neutralises the no-persisted-
	// config multigraph default that pre-config WALs would otherwise trip; the
	// node count is config-independent and is the load-bearing comparison.
	if rn != selfN {
		out.CountMismatch = fmt.Sprintf("nodes: current=%d prior-self=%d (prior-live=%d)", rn, selfN, prior.Nodes)
	}
	return out, nil
}

// classifySnapshotProvenance adjudicates the snapshot half of an upgrade run and
// returns the fault description, or "" when the image's snapshot state and the
// current reader's behaviour agree (rmp #2477).
//
// The rule is a comparison between two INDEPENDENT observations of the same
// image: what the filesystem holds ([PriorSnapshotFacts], read by the current
// manifest reader) and what recovery says it loaded (SnapshotOpened). A claim
// resting on either alone is unfalsifiable — a reader that ignores a snapshot
// directory reports exactly the false a WAL-only image reports — so only the
// pair can distinguish "there was nothing to open" from "there was something and
// it was skipped".
//
// It deliberately does NOT fault a WAL-only image. A tag whose checkpoint half
// would not build produces one legitimately, and the harness records that
// separately (HelperBuildFallbackErr); turning it into a failure here would
// convert a known coverage degradation into noise on an unrelated contract.
func classifySnapshotProvenance(r *CrossReleaseUpgradeResult) string {
	switch {
	case r.PriorSnapshot.Present && r.PriorSnapshot.ManifestErr != nil:
		// The prior release wrote a manifest the CURRENT reader cannot parse: a
		// genuine cross-version snapshot-format fault, and precisely the class this
		// task made reachable.
		return fmt.Sprintf("prior release %s published a manifest the current reader rejects: %v",
			r.Tag, r.PriorSnapshot.ManifestErr)
	case r.PriorSnapshot.Present && !r.SnapshotOpened:
		return fmt.Sprintf("prior release %s published a snapshot directory (manifest v%d, files %v) "+
			"but the current recovery did not load it (SnapshotHit=false)",
			r.Tag, r.PriorSnapshot.ManifestVersion, r.PriorSnapshot.Files)
	case r.SnapshotOpened && !r.PriorSnapshot.Present:
		// Recovery claims a snapshot the filesystem does not show. Either the
		// directory moved under the harness or SnapshotHit is not what it says.
		return fmt.Sprintf("current recovery reports a snapshot hit on the %s image, "+
			"but no manifest.json is present on disk", r.Tag)
	}
	return ""
}

// CrossReleaseDivergence classifies one op's prior-vs-current observable
// difference. Benign divergences (a query whose result is legitimately
// plan-dependent, or a deliberately-fixed-bug behaviour) are recorded as
// classified, NOT flagged as failures; an unexpected difference is a regression.
type CrossReleaseDivergence struct {
	// Op is the diverging op.
	Op Op
	// PriorRows / CurrentRows are the two canonical row signatures.
	PriorRows   string
	CurrentRows string
	// Reason explains the classification.
	Reason string
	// Index is the op index that diverged.
	Index int
	// Benign reports whether the divergence is an expected/benign class.
	Benign bool
}

// CrossReleaseDiffResult is the outcome of a cross-release DIFFERENTIAL run:
// whether the prior and current releases agreed on every op (modulo benign,
// classified divergences) and the full list of classified divergences.
type CrossReleaseDiffResult struct {
	// Tag is the prior release compared against.
	Tag string
	// Divergences lists every op-level difference, each classified. A run can
	// agree overall while still carrying benign divergences here.
	Divergences []CrossReleaseDivergence
	// PriorNodes/PriorEdges/CurrentNodes/CurrentEdges are the end-state counts.
	PriorNodes   int64
	PriorEdges   int64
	CurrentNodes int64
	CurrentEdges int64
	// Agreed reports whether no UNEXPECTED (non-benign) divergence occurred.
	Agreed bool
	// FinalCountsMatch reports whether the prior and current end-state counts
	// matched.
	FinalCountsMatch bool
}

// String renders the differential result.
func (r CrossReleaseDiffResult) String() string {
	var b strings.Builder
	verdict := "AGREED"
	if !r.Agreed {
		verdict = "DIVERGED"
	}
	fmt.Fprintf(&b, "cross-release differential %s vs current: %s (%d classified divergences); end-state prior(n=%d e=%d) current(n=%d e=%d) match=%v",
		r.Tag, verdict, len(r.Divergences), r.PriorNodes, r.PriorEdges, r.CurrentNodes, r.CurrentEdges, r.FinalCountsMatch)
	for _, d := range r.Divergences {
		fmt.Fprintf(&b, "\n  op %d %s %q: prior=%s current=%s benign=%v (%s)",
			d.Index, d.Op.Kind, d.Op.Cypher, d.PriorRows, d.CurrentRows, d.Benign, d.Reason)
	}
	return b.String()
}

// RunCrossReleaseDifferential replays the SAME deterministic op stream against a
// prior release (over its store, via the helper) and against the CURRENT
// in-process engine, then diffs the observable per-op results and the end-state.
// Each per-op difference is CLASSIFIED: a legitimately plan-dependent result
// (e.g. an unordered LIMIT) is benign; any other difference is an unexpected
// divergence that fails the comparison.
//
// repoRoot is the GoGraph working-tree root. A build/worktree failure is
// returned as an error (clean environment-precondition skip); a behavioural
// divergence is carried in the result.
func RunCrossReleaseDifferential(ctx context.Context, repoRoot, tag string, seed uint64, ops int) (CrossReleaseDiffResult, error) {
	helper, err := BuildPriorReleaseHelper(ctx, repoRoot, tag)
	if err != nil {
		return CrossReleaseDiffResult{}, err
	}
	defer func() { _ = helper.Close() }()

	opStream, err := GenerateCrossReleaseOps(seed, ops)
	if err != nil {
		return CrossReleaseDiffResult{}, err
	}

	// Prior side: drive the op stream through the prior release's store. The
	// helper writes to a throwaway image dir (we only need its per-op results).
	imageDir, err := os.MkdirTemp("", "gograph-xrelease-diff-")
	if err != nil {
		return CrossReleaseDiffResult{}, fmt.Errorf("sim: cross-release: diff image dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(imageDir) }()
	prior, err := helper.WriteImage(ctx, imageDir, opStream)
	if err != nil {
		return CrossReleaseDiffResult{}, fmt.Errorf("sim: cross-release: prior %s differential: %w", tag, err)
	}

	// Current side: replay the SAME op stream in-process and capture each op's
	// signature with the same canonical encoding the helper uses.
	cur, err := replayCurrentSignatures(ctx, opStream)
	if err != nil {
		return CrossReleaseDiffResult{}, err
	}

	result := CrossReleaseDiffResult{
		Tag:          tag,
		Agreed:       true,
		PriorNodes:   prior.Nodes,
		PriorEdges:   prior.Edges,
		CurrentNodes: cur.nodes,
		CurrentEdges: cur.edges,
	}
	result.FinalCountsMatch = prior.Nodes == cur.nodes && prior.Edges == cur.edges

	for i, op := range opStream {
		pr := prior.Ops[i].Rows
		cr := cur.rows[i]
		if canonicalHelperRowsMatch(pr, cr) {
			continue
		}
		div := classifyDivergence(i, op, pr, cr)
		result.Divergences = append(result.Divergences, div)
		if !div.Benign {
			result.Agreed = false
		}
	}

	// An end-state count mismatch that is not explained by a benign per-op
	// divergence is itself an unexpected divergence.
	if !result.FinalCountsMatch && allBenign(result.Divergences) {
		result.Agreed = false
		result.Divergences = append(result.Divergences, CrossReleaseDivergence{
			Index:       len(opStream),
			PriorRows:   fmt.Sprintf("nodes=%d edges=%d", prior.Nodes, prior.Edges),
			CurrentRows: fmt.Sprintf("nodes=%d edges=%d", cur.nodes, cur.edges),
			Benign:      false,
			Reason:      "end-state node/edge counts differ with no benign per-op explanation",
		})
	}

	return result, nil
}

// currentSignatures holds the current in-process engine's per-op signatures and
// final counts for a differential run.
type currentSignatures struct {
	rows  []string
	nodes int64
	edges int64
}

// replayCurrentSignatures replays opStream against a fresh current in-process
// engine (the same directed-simple-graph shape the prior helper uses) and
// records each op's canonical row signature, matching the helper's encoding so
// the two are directly comparable.
func replayCurrentSignatures(ctx context.Context, opStream []Op) (currentSignatures, error) {
	v := EngineVariant{Name: "current"}
	eng := v.buildEngine()
	out := currentSignatures{rows: make([]string, 0, len(opStream))}
	for i, op := range opStream {
		if err := ctx.Err(); err != nil {
			return currentSignatures{}, err
		}
		sig, err := currentOpSignature(ctx, eng, op)
		if err != nil {
			return currentSignatures{}, fmt.Errorf("sim: cross-release: current op %d: %w", i, err)
		}
		out.rows = append(out.rows, sig)
	}
	n, _ := eng.NodeCount()
	e, _ := eng.EdgeCount()
	out.nodes = n
	out.edges = e
	return out, nil
}

// currentOpSignature runs one op against the current engine and returns the
// canonical row signature, encoded identically to the prior helper's
// canonicalRows so prior and current signatures compare byte-for-byte.
func currentOpSignature(ctx context.Context, eng *EngineAdapter, op Op) (string, error) {
	var (
		res Result
		err error
	)
	if op.Kind.IsWrite() {
		res, err = eng.RunWrite(ctx, op.Cypher, op.Params)
	} else {
		res, err = eng.Run(ctx, op.Cypher, op.Params)
	}
	if err != nil {
		return "error", nil
	}
	// Render via the SAME Record-map + %v encoding the prior-release helper uses
	// (cmd/sim-xrelease-helper canonicalRows), so prior and current signatures
	// compare byte-for-byte. The differential's own canonicalRows is for the
	// in-process variant pair and is not reused here.
	sig := canonicalRecordSignature(res)
	_ = res.Close()
	return sig, nil
}

// canonicalRecordSignature drains a current-engine result and renders an
// order-independent signature byte-identical to the prior-release helper's
// canonicalRows: each row's columns read from Record (a column-name -> value
// map) and rendered with %v, sorted, and joined. Reading through the adapter's
// underlying *cypher.Result keeps the encoding in lockstep with the helper.
func canonicalRecordSignature(res Result) string {
	ra, ok := res.(*resultAdapter)
	if !ok {
		return "<non-adapter-result>"
	}
	cols := ra.res.Columns()
	var out []string
	for ra.Next() {
		parts := make([]string, 0, len(cols))
		for _, c := range cols {
			parts = append(parts, fmt.Sprintf("%v", ra.res.Record()[c]))
		}
		out = append(out, strings.Join(parts, ","))
	}
	sort.Strings(out)
	return "[" + strings.Join(out, "|") + "]"
}

// classifyDivergence labels a prior-vs-current per-op difference. A query whose
// row SELECTION is legitimately plan-dependent (an unordered LIMIT) is benign:
// two releases may each return a different valid subset, so a row-content
// difference there is expected, not a regression. Every other difference is an
// unexpected divergence the caller treats as a failure to investigate.
func classifyDivergence(index int, op Op, priorRows, currentRows string) CrossReleaseDivergence {
	d := CrossReleaseDivergence{Index: index, Op: op, PriorRows: priorRows, CurrentRows: currentRows}
	if plannerNondeterministicRows(op.Cypher) {
		d.Benign = true
		d.Reason = "unordered LIMIT: row selection is legitimately plan/version-dependent"
		return d
	}
	d.Benign = false
	d.Reason = "observable result differs between prior release and current code"
	return d
}

// allBenign reports whether every divergence in ds is benign (or ds is empty).
func allBenign(ds []CrossReleaseDivergence) bool {
	for _, d := range ds {
		if !d.Benign {
			return false
		}
	}
	return true
}
