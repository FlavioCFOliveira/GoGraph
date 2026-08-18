package sim

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// xreleaseHelperPkg is the import path of the prior-release subprocess driver
// (cmd/sim-xrelease-helper). The harness copies its single source file into a
// worktree of the target tag and builds it there, so the resulting binary
// embeds that tag's store/cypher code. See [BuildPriorReleaseHelper].
const xreleaseHelperPkg = "cmd/sim-xrelease-helper"

// xreleaseHelperMainRel is the path, relative to the repository root, of the
// helper's ALWAYS-staged source file. It is copied verbatim into the prior-tag
// worktree (which ships no such file) so it compiles against that tag's
// packages.
const xreleaseHelperMainRel = "cmd/sim-xrelease-helper/main.go"

// xreleaseHelperCheckpointRel is the helper's OPTIONALLY-staged source file: the
// half that publishes a checkpoint (rmp #2477). It is staged first and dropped
// only if the tag will not build with it, so a tag whose checkpoint API differs
// still yields a WAL-only image instead of disappearing from coverage as an
// "unbuildable tag" skip. See [BuildPriorReleaseHelper] and the file's own
// package comment.
const xreleaseHelperCheckpointRel = "cmd/sim-xrelease-helper/checkpoint.go"

// PriorReleaseHelper is a built prior-release helper binary plus the worktree it
// was compiled in. Close removes both, deterministically. It is the
// cross-release equivalent of [subproc]: instead of re-execing the current test
// binary, it spawns a binary built from a PRIOR git tag's source so the harness
// can observe genuine cross-version behaviour.
//
// # Concurrency contract
//
// A PriorReleaseHelper is not safe for concurrent use across its own methods,
// but [PriorReleaseHelper.WriteImage] is a pure spawn-and-wait and may be called
// from one goroutine at a time. Close is idempotent.
type PriorReleaseHelper struct {
	// Tag is the git tag the helper was built from (e.g. "v0.3.0").
	Tag string
	// BinPath is the absolute path of the built helper binary.
	BinPath string
	// BuildFallbackErr is the error the FIRST (checkpoint-bearing) build
	// returned, when the harness had to fall back to the WAL-only helper. It is
	// nil when the checkpoint-bearing build succeeded. Callers report it so a
	// silent loss of checkpoint coverage at some tag is visible in the test log
	// rather than inferred from CheckpointSupported alone.
	BuildFallbackErr error
	// CheckpointSupported reports that the tag built WITH
	// [xreleaseHelperCheckpointRel] staged, so [PriorReleaseHelper.WriteImage]
	// will publish a snapshot directory alongside the WAL. False means the tag's
	// checkpoint API is not the one the staged file uses and the helper was
	// rebuilt without it.
	CheckpointSupported bool

	worktree string
	tmpRoot  string
}

// BuildPriorReleaseHelper checks out tag into a temporary git worktree, copies
// the current helper sources into it, and builds the helper binary against that
// tag's packages. The returned helper's Close removes the worktree and the
// temporary build root.
//
// repoRoot must be the absolute path of the GoGraph repository working tree
// (the directory holding .git). The build runs `go build` inside the worktree so
// the binary links the tag's store/txn/wal/cypher code.
//
// # The build is two-stage (rmp #2477)
//
// Both [xreleaseHelperMainRel] and [xreleaseHelperCheckpointRel] are staged and
// the build is attempted. If THAT build fails, the checkpoint file alone is
// removed and the build is retried with the WAL-only helper. The distinction is
// recorded in [PriorReleaseHelper.CheckpointSupported] and, for the failing
// case, [PriorReleaseHelper.BuildFallbackErr].
//
// The fallback exists because the two files carry different compatibility
// risk. main.go is pinned to the API stable across v0.2.0..HEAD; checkpoint.go
// reaches the younger store/checkpoint API. Putting both in one file would make
// any checkpoint-API drift at some tag fail the WHOLE build, which the caller
// reports as an environment-precondition SKIP — silently deleting that release
// from cross-release coverage entirely. Two stages degrade one capability
// instead of losing the tag.
//
// An error from this function is an ENVIRONMENT-PRECONDITION failure (the tag is
// not present, git worktree is unavailable, or the tag's tree does not build
// with the current toolchain EVEN WITHOUT the checkpoint file): callers gate on
// it as a clean skip, exactly like an optional external tool being absent, NOT
// as a test failure.
func BuildPriorReleaseHelper(ctx context.Context, repoRoot, tag string) (*PriorReleaseHelper, error) {
	if !commitishExists(ctx, repoRoot, tag) {
		return nil, fmt.Errorf("sim: cross-release: ref %q not present in repo", tag)
	}

	tmpRoot, err := os.MkdirTemp("", "gograph-xrelease-")
	if err != nil {
		return nil, fmt.Errorf("sim: cross-release: temp root: %w", err)
	}
	worktree := filepath.Join(tmpRoot, "wt-"+sanitiseTag(tag))

	// Detached worktree at the tag: never touches the live working tree or
	// branch state, and removing it later is a clean `git worktree remove`.
	if out, err := runGit(ctx, repoRoot, "worktree", "add", "--detach", "--quiet", worktree, tag); err != nil {
		_ = os.RemoveAll(tmpRoot)
		return nil, fmt.Errorf("sim: cross-release: worktree add %q: %w (%s)", tag, err, strings.TrimSpace(out))
	}

	cleanup := func() {
		_, _ = runGit(context.Background(), repoRoot, "worktree", "remove", "--force", worktree)
		_ = os.RemoveAll(tmpRoot)
	}

	// Copy the current helper sources into the worktree, overwriting any tag copy
	// (there is none, but be idempotent). The destination directory may not exist
	// at the tag, so create it.
	for _, rel := range []string{xreleaseHelperMainRel, xreleaseHelperCheckpointRel} {
		if err := copyFileInto(filepath.Join(repoRoot, rel), filepath.Join(worktree, rel)); err != nil {
			cleanup()
			return nil, fmt.Errorf("sim: cross-release: stage helper source %q: %w", rel, err)
		}
	}

	binPath := filepath.Join(tmpRoot, "helper")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}

	// Stage 1: build WITH the checkpoint file.
	withCheckpointErr := buildXreleaseHelper(ctx, worktree, binPath)
	if withCheckpointErr == nil {
		return &PriorReleaseHelper{
			Tag: tag, BinPath: binPath, worktree: worktree, tmpRoot: tmpRoot,
			CheckpointSupported: true,
		}, nil
	}

	// Stage 2: drop the checkpoint file and retry. Only a failure HERE is an
	// unbuildable tag.
	if err := os.Remove(filepath.Join(worktree, xreleaseHelperCheckpointRel)); err != nil {
		cleanup()
		return nil, fmt.Errorf("sim: cross-release: drop checkpoint source at %q: %w (first build: %w)", tag, err, withCheckpointErr)
	}
	if err := buildXreleaseHelper(ctx, worktree, binPath); err != nil {
		cleanup()
		return nil, fmt.Errorf("sim: cross-release: build helper at %q: %w", tag, err)
	}
	return &PriorReleaseHelper{
		Tag: tag, BinPath: binPath, worktree: worktree, tmpRoot: tmpRoot,
		CheckpointSupported: false, BuildFallbackErr: withCheckpointErr,
	}, nil
}

// buildXreleaseHelper runs `go build` for the helper package inside worktree,
// writing the binary to binPath. It is the single build seam both stages of
// [BuildPriorReleaseHelper] use, so the two attempts differ ONLY in which source
// files are staged — never in build flags or environment.
func buildXreleaseHelper(ctx context.Context, worktree, binPath string) error {
	//nolint:gosec // G204: fixed `go build` of a constant, harness-internal package path.
	build := exec.CommandContext(ctx, "go", "build", "-o", binPath, "./"+xreleaseHelperPkg)
	build.Dir = worktree
	build.Env = os.Environ()
	var buildErr bytes.Buffer
	build.Stderr = &buildErr
	if err := build.Run(); err != nil {
		return fmt.Errorf("%w (%s)", err, strings.TrimSpace(buildErr.String()))
	}
	return nil
}

// Close removes the worktree and temporary build artefacts. It is idempotent.
func (h *PriorReleaseHelper) Close() error {
	if h == nil || h.tmpRoot == "" {
		return nil
	}
	_, _ = runGit(context.Background(), "", "worktree", "prune")
	if h.worktree != "" {
		_, _ = runGit(context.Background(), filepath.Dir(h.tmpRoot), "worktree", "remove", "--force", h.worktree)
	}
	err := os.RemoveAll(h.tmpRoot)
	h.tmpRoot = ""
	return err
}

// HelperOpResult is the prior release's observable outcome for one op: whether
// it committed and a canonical, order-independent signature of its result rows.
type HelperOpResult struct {
	Rows      string
	Committed bool
}

// HelperRunResult is the full outcome of driving an op stream through the prior
// release: the per-op results in order, the prior engine's final counts, and
// whether the prior release published a checkpoint over the image.
type HelperRunResult struct {
	// CheckpointErr is the prior release's own checkpoint failure message, empty
	// when it succeeded or was never attempted.
	CheckpointErr string
	Ops           []HelperOpResult
	Nodes         int64
	Edges         int64
	// Checkpoint reports that the image at dir carries a SNAPSHOT DIRECTORY the
	// prior release published, not merely a WAL (rmp #2477).
	Checkpoint bool
}

// WriteImage drives ops through the prior-release helper, which opens a
// WAL-backed store under dir, runs each op, publishes a checkpoint when the tag
// supports it, and closes (flush+fsync) so dir holds a durable store image
// written ENTIRELY by the prior release. It returns the prior release's per-op
// results, final counts, and whether the image carries a snapshot directory.
//
// dir must be an existing, empty directory the current process owns; after this
// returns, the current code can reopen dir via [recovery.Open] to perform the
// cross-version upgrade check. When [HelperRunResult.Checkpoint] is true that
// reopen parses the PRIOR RELEASE'S snapshot bytes — manifest.json, csr.bin and
// the component files — which is the surface the WAL-only image never reached.
func (h *PriorReleaseHelper) WriteImage(ctx context.Context, dir string, ops []Op) (HelperRunResult, error) {
	var stdin bytes.Buffer
	enc := json.NewEncoder(&stdin)
	for i, op := range ops {
		if err := enc.Encode(wireOp{Kind: string(op.Kind), Cypher: op.Cypher, Params: op.Params}); err != nil {
			return HelperRunResult{}, fmt.Errorf("sim: cross-release: encode op %d: %w", i, err)
		}
	}

	//nolint:gosec // G204: h.BinPath is a harness-built binary, dir is a harness-owned temp dir.
	cmd := exec.CommandContext(ctx, h.BinPath, "write", dir)
	cmd.Stdin = &stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return HelperRunResult{}, fmt.Errorf("sim: cross-release: helper %q write: %w (%s)", h.Tag, err, strings.TrimSpace(stderr.String()))
	}

	return parseHelperOutput(stdout.Bytes(), len(ops))
}

// SelfRecoverCounts reopens a dir this helper previously wrote using the PRIOR
// release's OWN recovery and returns the node/edge counts it recovers. It is the
// durable truth of what the prior release wrote, as the prior release itself
// reads it back — the reference the current code's recovery must reproduce. It
// discriminates a prior-release WAL that does not round-trip in its own release
// (a prior defect) from one the current code mis-reads (a current regression):
// the cross-version contract is current-recovery == prior-self-recovery.
func (h *PriorReleaseHelper) SelfRecoverCounts(ctx context.Context, dir string) (nodes, edges int64, err error) {
	//nolint:gosec // G204: h.BinPath is a harness-built binary, dir is a harness-owned temp dir.
	cmd := exec.CommandContext(ctx, h.BinPath, "selfcheck", dir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, 0, fmt.Errorf("sim: cross-release: helper %q selfcheck: %w (%s)", h.Tag, err, strings.TrimSpace(stderr.String()))
	}
	var line wireResultLine
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &line); err != nil {
		return 0, 0, fmt.Errorf("sim: cross-release: decode selfcheck %q: %w", strings.TrimSpace(stdout.String()), err)
	}
	if !line.Done {
		return 0, 0, fmt.Errorf("sim: cross-release: selfcheck produced no done marker")
	}
	return line.Nodes, line.Edges, nil
}

// wireOp / the result shapes mirror cmd/sim-xrelease-helper's protocol. They are
// restated here (rather than imported) because the helper lives under cmd/ and
// is built from a different source tree; the JSON contract is the only coupling.
type wireOp struct {
	Params map[string]any `json:"params"`
	Kind   string         `json:"kind"`
	Cypher string         `json:"cypher"`
}

type wireResultLine struct {
	Rows          string `json:"rows"`
	CheckpointErr string `json:"checkpoint_err"`
	Index         int    `json:"i"`
	Nodes         int64  `json:"nodes"`
	Edges         int64  `json:"edges"`
	Committed     bool   `json:"committed"`
	Done          bool   `json:"done"`
	Checkpoint    bool   `json:"checkpoint"`
}

// parseHelperOutput decodes the helper's line protocol: nOps result lines
// followed by one done line. It validates the count and ordering so a truncated
// or scrambled stream is a hard error, never a silently short comparison.
func parseHelperOutput(stdout []byte, nOps int) (HelperRunResult, error) {
	res := HelperRunResult{Ops: make([]HelperOpResult, 0, nOps)}
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	sawDone := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var l wireResultLine
		if err := json.Unmarshal(line, &l); err != nil {
			return HelperRunResult{}, fmt.Errorf("sim: cross-release: decode helper line %q: %w", string(line), err)
		}
		if l.Done {
			res.Nodes = l.Nodes
			res.Edges = l.Edges
			res.Checkpoint = l.Checkpoint
			res.CheckpointErr = l.CheckpointErr
			sawDone = true
			continue
		}
		if l.Index != len(res.Ops) {
			return HelperRunResult{}, fmt.Errorf("sim: cross-release: helper op out of order: got index %d, want %d", l.Index, len(res.Ops))
		}
		res.Ops = append(res.Ops, HelperOpResult{Committed: l.Committed, Rows: l.Rows})
	}
	if err := sc.Err(); err != nil {
		return HelperRunResult{}, fmt.Errorf("sim: cross-release: read helper output: %w", err)
	}
	if !sawDone {
		return HelperRunResult{}, fmt.Errorf("sim: cross-release: helper output missing done marker (got %d/%d ops)", len(res.Ops), nOps)
	}
	if len(res.Ops) != nOps {
		return HelperRunResult{}, fmt.Errorf("sim: cross-release: helper produced %d op results, want %d", len(res.Ops), nOps)
	}
	return res, nil
}

// GenerateCrossReleaseOps produces a deterministic write-biased op stream from
// seed for the cross-release harness. It is the SAME workload the in-process
// upgrade harness drives (so the two are directly comparable), captured as a
// flat slice the harness can serialise to the prior-release helper AND replay
// in-process. Params are normalised through a JSON round-trip so the prior
// helper (which receives them as JSON) and the current side bind byte-identical
// parameter values.
func GenerateCrossReleaseOps(seed uint64, n int) ([]Op, error) {
	if n <= 0 {
		n = 400
	}
	s := NewSeed(seed)
	oracle := NewGraphOracle()
	wl := WriteHeavyWorkload(s)
	ops := make([]Op, 0, n)
	for i := 0; i < n; i++ {
		actor := wl.SelectActor(s)
		op := actor.NextOp(s, oracle)
		// Advance the generation oracle exactly as a real run would, so the op
		// mix (which depends on current modelled contents) matches what the
		// in-process harness produces. Reads/writes both inform later choices.
		applyOpToOracle(oracle, op, true)
		norm, err := normaliseOpThroughJSON(op)
		if err != nil {
			return nil, fmt.Errorf("sim: cross-release: normalise op %d: %w", i, err)
		}
		ops = append(ops, norm)
	}
	return ops, nil
}

// normaliseOpThroughJSON round-trips an op's params through JSON so the value
// kinds match exactly what the prior-release helper receives (JSON numbers
// decode to float64). The current side then binds the identical normalised
// params, eliminating any int64-vs-float64 binding skew between the two
// releases as a source of false divergence.
func normaliseOpThroughJSON(op Op) (Op, error) {
	if len(op.Params) == 0 {
		return op, nil
	}
	raw, err := json.Marshal(op.Params)
	if err != nil {
		return Op{}, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Op{}, err
	}
	return Op{Kind: op.Kind, Cypher: op.Cypher, Params: decoded}, nil
}

// recoveredImage is the outcome of reopening a prior-release image with the
// CURRENT full-stack recovery: an engine over the rebuilt graph plus the
// provenance the upgrade check inspects — how much of the graph came from the
// prior release's SNAPSHOT and how much from its WAL.
type recoveredImage struct {
	engine  *EngineAdapter
	tailErr error
	// walOps is how many WAL ops the current recovery replayed. After a prior
	// release published a checkpoint this is the POST-checkpoint tail only, which
	// is what makes it evidence: a full graph recovered from few WAL ops can only
	// have come through the snapshot.
	walOps int
	// snapshotHit reports that the current recovery found and loaded a snapshot
	// directory under <dir>/snapshot — one the PRIOR RELEASE wrote.
	snapshotHit bool
	// snapshotVersion is the on-disk manifest version of the snapshot the current
	// reader accepted, or 0 when there was none.
	snapshotVersion int
	// snapshotLabels / snapshotProperties are how many label and property records
	// the prior release's snapshot components contributed back into the graph.
	snapshotLabels     int
	snapshotProperties int
	clean              bool
}

// recoverImageGraph reopens a prior-release-written store image at dir with the
// CURRENT recovery code and returns an engine over the recovered graph plus the
// replay provenance. A non-nil error is a fail-stop signal: the current code
// REFUSED to open the prior image (genuine corruption or an unsupported format),
// which the upgrade check treats as a data-compatibility fault to surface, never
// to swallow.
//
// # Why the FULL-STACK path, not the WAL-only core (rmp #2477)
//
// It routes through [recovery.OpenCtx] — snapshot load THEN WAL replay — rather
// than calling [recovery.ReplayWAL] directly. Two reasons, and the second is the
// whole point of the change:
//
//  1. The prior release now publishes a checkpoint, so its WAL holds only the
//     post-checkpoint tail. A WAL-only replay of that image would recover an
//     almost-empty graph and compare it against a full one.
//  2. A WAL-only replay never opens <dir>/snapshot at all. The prior release's
//     manifest.json, csr.bin, labels.bin, properties.bin and mapper.bin — an
//     entire on-disk format family — were therefore never parsed by current code
//     in any cross-release test. Now they are.
//
// The graph shape is whatever the image declares. [recovery.OpenCtx] honours the
// snapshot manifest's persisted graph_config when it carries one and falls back
// to its documented no-config default (Multigraph: true) when it does not, so a
// pre-config prior image can rebuild as a multigraph where the writer had a
// simple graph. That is a real property of the image, not a harness artefact, and
// it is why the upgrade contract compares NODE counts — which no adjacency
// configuration can change — and reports edge counts rather than asserting them.
// The comparison is also like-for-like: the prior release's own self-recovery
// (cmd/sim-xrelease-helper "selfcheck") goes through ITS recovery.Open, the same
// full-stack path, so both sides resolve the config by the same rule.
func recoverImageGraph(ctx context.Context, dir string) (recoveredImage, error) {
	res, err := recovery.OpenCtx[string, float64](ctx, dir, recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		return recoveredImage{}, fmt.Errorf("full-stack reopen of prior image: %w", err)
	}
	if !res.IsClean() {
		return recoveredImage{}, fmt.Errorf("current recovery found corruption in prior image: %w", res.TailErr)
	}
	return recoveredImage{
		engine:             NewEngineAdapter(cypher.NewEngine(res.Graph)),
		walOps:             res.WALOps,
		snapshotHit:        res.SnapshotHit,
		snapshotVersion:    res.SnapshotSchemaVersion,
		snapshotLabels:     res.SnapshotLabels,
		snapshotProperties: res.SnapshotProperties,
		clean:              res.IsClean(),
		tailErr:            res.TailErr,
	}, nil
}

// PriorSnapshotFacts is what the snapshot directory a PRIOR RELEASE published
// looks like from the CURRENT reader's point of view, read straight off disk and
// independently of what recovery reports (rmp #2477).
//
// It is read independently on purpose. "The current code opened the prior
// release's snapshot" is otherwise a single-source claim — recovery's own
// SnapshotHit — and a reader that silently ignored the directory would report
// exactly the same false as one that was never given a directory at all. Reading
// the filesystem separately makes those two states distinguishable.
type PriorSnapshotFacts struct {
	// ManifestErr is the error the CURRENT manifest reader returned for the
	// prior release's manifest.json, or nil when it loaded. A non-nil value with
	// Present true is a cross-version manifest-format fault.
	ManifestErr error
	// Integrity is the manifest's declared integrity scheme, empty for a manifest
	// written before the CRC32C trailer existed (rmp #2520).
	Integrity string
	// Files lists the component file names the manifest declares, in manifest
	// order.
	Files []string
	// ManifestVersion is the on-disk schema version the prior release stamped.
	ManifestVersion int
	// Present reports that <dir>/snapshot/manifest.json exists on disk.
	Present bool
	// IntegrityVerified reports that the current reader VERIFIED a checksum
	// trailer over these bytes. False for every manifest written before rmp
	// #2520 — which is the expected, documented outcome for a prior release and
	// is asserted as such, not tolerated.
	IntegrityVerified bool
}

// InspectPriorSnapshotDir reads <dir>/snapshot/manifest.json with the CURRENT
// manifest reader and reports what it found. A missing directory is not an
// error: it yields Present false, which is the WAL-only image shape.
func InspectPriorSnapshotDir(dir string) PriorSnapshotFacts {
	path := filepath.Join(dir, "snapshot", "manifest.json")
	if _, err := os.Stat(path); err != nil {
		return PriorSnapshotFacts{}
	}
	out := PriorSnapshotFacts{Present: true}
	m, err := snapshot.ReadManifestFile(path)
	if err != nil {
		out.ManifestErr = err
		return out
	}
	out.ManifestVersion = m.Version
	out.Integrity = m.Integrity
	out.IntegrityVerified = m.IntegrityVerified
	out.Files = make([]string, 0, len(m.Files))
	for _, f := range m.Files {
		out.Files = append(out.Files, f.Name)
	}
	return out
}

// runGit runs a git subcommand in dir (cwd when dir is empty) and returns its
// combined output. It is the harness's only git seam, kept narrow so worktree
// lifecycle stays auditable.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	//nolint:gosec // G204: fixed `git` binary with harness-constructed argv (no user input).
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// commitishExists reports whether ref resolves to a commit in repoRoot. It
// accepts any committish (a tag like "v0.3.0" or a symbolic ref like "HEAD"), so
// the harness can be smoke-tested against HEAD-as-prior without a release tag.
func commitishExists(ctx context.Context, repoRoot, ref string) bool {
	_, err := runGit(ctx, repoRoot, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

// sanitiseTag makes a tag safe as a directory-name fragment.
func sanitiseTag(tag string) string {
	return strings.NewReplacer("/", "_", ".", "_").Replace(tag)
}

// copyFileInto copies src to dst, creating dst's parent directory tree.
func copyFileInto(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	data, err := os.ReadFile(src) //nolint:gosec // src is a repo-internal, harness-controlled path
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644) //nolint:gosec // staged helper source, not a secret
}

// canonicalHelperRowsMatch reports whether two canonical row signatures are
// observably equal. It is a named seam so the cross-release differential's
// equality rule has a single home; benign-class relaxation is handled by
// [classifyDivergence], keeping the raw comparison here exact.
func canonicalHelperRowsMatch(a, b string) bool { return a == b }
