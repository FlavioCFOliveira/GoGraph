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
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// xreleaseHelperPkg is the import path of the prior-release subprocess driver
// (cmd/sim-xrelease-helper). The harness copies its single source file into a
// worktree of the target tag and builds it there, so the resulting binary
// embeds that tag's store/cypher code. See [BuildPriorReleaseHelper].
const xreleaseHelperPkg = "cmd/sim-xrelease-helper"

// xreleaseHelperMainRel is the path, relative to the repository root, of the
// helper's single source file. It is copied verbatim into the prior-tag
// worktree (which ships no such file) so it compiles against that tag's
// packages.
const xreleaseHelperMainRel = "cmd/sim-xrelease-helper/main.go"

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

	worktree string
	tmpRoot  string
}

// BuildPriorReleaseHelper checks out tag into a temporary git worktree, copies
// the current helper source ([xreleaseHelperMainRel]) into it, and builds the
// helper binary against that tag's packages. The returned helper's Close removes
// the worktree and the temporary build root.
//
// repoRoot must be the absolute path of the GoGraph repository working tree
// (the directory holding .git). The build runs `go build` inside the worktree so
// the binary links the tag's store/txn/wal/cypher code.
//
// An error from this function is an ENVIRONMENT-PRECONDITION failure (the tag is
// not present, git worktree is unavailable, or the tag's tree does not build
// with the current toolchain): callers gate on it as a clean skip, exactly like
// an optional external tool being absent, NOT as a test failure.
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

	// Copy the current helper source into the worktree, overwriting any tag copy
	// (there is none, but be idempotent). The destination directory may not exist
	// at the tag, so create it.
	srcMain := filepath.Join(repoRoot, xreleaseHelperMainRel)
	dstMain := filepath.Join(worktree, xreleaseHelperMainRel)
	if err := copyFileInto(srcMain, dstMain); err != nil {
		cleanup()
		return nil, fmt.Errorf("sim: cross-release: stage helper source: %w", err)
	}

	binPath := filepath.Join(tmpRoot, "helper")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	//nolint:gosec // G204: fixed `go build` of a constant, harness-internal package path.
	build := exec.CommandContext(ctx, "go", "build", "-o", binPath, "./"+xreleaseHelperPkg)
	build.Dir = worktree
	build.Env = os.Environ()
	var buildErr bytes.Buffer
	build.Stderr = &buildErr
	if err := build.Run(); err != nil {
		cleanup()
		return nil, fmt.Errorf("sim: cross-release: build helper at %q: %w (%s)", tag, err, strings.TrimSpace(buildErr.String()))
	}

	return &PriorReleaseHelper{Tag: tag, BinPath: binPath, worktree: worktree, tmpRoot: tmpRoot}, nil
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
	Committed bool
	Rows      string
}

// HelperRunResult is the full outcome of driving an op stream through the prior
// release: the per-op results in order, plus the prior engine's final counts.
type HelperRunResult struct {
	Ops   []HelperOpResult
	Nodes int64
	Edges int64
}

// WriteImage drives ops through the prior-release helper, which opens a
// WAL-backed store under dir, runs each op, and closes (flush+fsync) so dir
// holds a durable store image written ENTIRELY by the prior release. It returns
// the prior release's per-op results and final counts.
//
// dir must be an existing, empty directory the current process owns; after this
// returns, the current code can reopen dir via [recovery.Open] to perform the
// cross-version upgrade check.
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
	Kind   string         `json:"kind"`
	Cypher string         `json:"cypher"`
	Params map[string]any `json:"params"`
}

type wireResultLine struct {
	Index     int    `json:"i"`
	Committed bool   `json:"committed"`
	Rows      string `json:"rows"`
	Done      bool   `json:"done"`
	Nodes     int64  `json:"nodes"`
	Edges     int64  `json:"edges"`
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

// recoveredImage is the outcome of reopening a prior-release WAL image with the
// CURRENT recovery core: an engine over the rebuilt graph plus the replay
// metadata the upgrade check inspects.
type recoveredImage struct {
	engine  *EngineAdapter
	walOps  int
	clean   bool
	tailErr error
}

// recoverImageGraph reopens a prior-release-written store image at dir with the
// CURRENT recovery code and returns an engine over the recovered graph plus the
// replay metadata. A non-nil error is a fail-stop signal: the current code
// REFUSED to open the prior image (genuine corruption or an unsupported format),
// which the upgrade check treats as a data-compatibility fault to surface, never
// to swallow.
//
// It replays the WAL via the SAME [recovery.ReplayWAL] core that the in-process
// upgrade harness ([OpenSimStore]) uses, into a graph constructed with the
// helper's exact configuration (a directed SIMPLE graph — see
// cmd/sim-xrelease-helper). This matches the writer's shape: a prior-release WAL
// predates the persisted graph_config, so [recovery.Open]'s no-config default
// (Multigraph: true) would rebuild a simple-graph image as a multigraph and
// inflate parallel-edge and node counts — a harness artefact, not a
// data-compatibility fault. Replaying into the matching config keeps the oracle
// (which models a simple graph) a faithful reference.
func recoverImageGraph(ctx context.Context, dir string) (recoveredImage, error) {
	walPath := filepath.Join(dir, "wal")
	rh, err := os.Open(walPath) //nolint:gosec // walPath is a harness-owned temp dir join
	if err != nil {
		return recoveredImage{}, fmt.Errorf("open prior image WAL: %w", err)
	}
	defer func() { _ = rh.Close() }()

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: false})
	reader := wal.NewReader(rh, rh)
	replay, err := recovery.ReplayWAL[string, float64](
		ctx, reader, g, txn.NewStringCodec(), txn.NewFloat64WeightCodec(), txn.DefaultMaxTxnOps,
	)
	if err != nil {
		return recoveredImage{}, fmt.Errorf("replay prior image WAL: %w", err)
	}
	if !replay.IsClean() {
		return recoveredImage{}, fmt.Errorf("current recovery found corruption in prior image: %w", replay.TailErr)
	}
	return recoveredImage{
		engine:  NewEngineAdapter(cypher.NewEngine(g)),
		walOps:  replay.WALOps,
		clean:   replay.IsClean(),
		tailErr: replay.TailErr,
	}, nil
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
