// Command kgverify is the knowledge graph's fidelity gate.
//
// The project mandates that the knowledge graph be kept current at every
// commit, and that it be the authority a reader trusts INSTEAD of reading
// files. Nothing checked either claim, and by the close of sprint 352 the graph
// had gone further than merely stale: it asserted facts that were never true.
// Two Test nodes, TestSchemaWalkHoisted and TestEvalWithReentrancy, existed in
// the graph and nowhere in the repository; a previous "fidelity repair" had
// itself invented two symbols the code deleted two days later. A gap is
// recoverable, a fabrication is not — it is a wrong answer delivered with the
// authority of a verified one.
//
// This command makes fidelity enforceable rather than aspirational. It exits
// non-zero when the graph's claims about the tree, about rmp, or about its own
// documented schema stop holding.
//
// # The fabrication route, and how this closes it
//
// A fabricated node is created when a symbol's NAME is typed from a task
// description or a report instead of being read out of the tree. The oracle
// here is go/parser: the inventory of what exists is built by PARSING every .go
// file, never by scanning text. That distinction is the whole mechanism, and it
// is measurable rather than rhetorical — edgeTypeFilterFor, deleted from the
// code in sprint 352, still appears in four Go comments and one Markdown
// document, so a text scan reports it present while the declaration inventory
// correctly reports it absent.
//
// Detection is the enforceable half: symbol-absent has a baseline of zero, so
// any node naming a non-declaration fails the gate. Generation is the other
// half, and it is an affordance rather than a hard block, because this command
// cannot intercept `rmp graph create`: `-emit symbols` prints the inventory and
// `-emit missing` prints the declarations that have no node yet, so the
// supported way to write a symbol node is to copy a name out of the tree's own
// output. A hand-typed name therefore survives at most until the next run.
//
// # Baselines
//
// Four counted backlogs predate this gate and cannot be repaired by a checker.
// Each is recorded in baseline.json with the count measured when it was
// written; the gate fails when a count EXCEEDS its baseline, and reports every
// count on every run so the remaining figure is never a qualitative claim. The
// zero-baseline checks are the ones that must never regress at all:
// fixture-fabrication-present, symbol-absent, task-id-not-int,
// task-legacy-identity, task-status-invalid and task-absent-in-rmp. This is the
// same ratchet the openCypher TCK gate uses, for the same reason.
//
// # Usage
//
//	go run ./cmd/kgverify              # verify; exit 1 on any regression
//	go run ./cmd/kgverify -v           # list every violation, not just a sample
//	go run ./cmd/kgverify -json        # machine-readable results
//	go run ./cmd/kgverify -emit symbols   # every declaration go/parser found
//	go run ./cmd/kgverify -emit missing   # declarations with no node: the sync worklist
//	go run ./cmd/kgverify -emit packages  # per-package declarations vs nodes
//	go run ./cmd/kgverify -emit absent    # nodes naming a symbol the tree does not have
//	go run ./cmd/kgverify -emit cypher    # one statement per line that closes the gap
//
// Exit codes are deliberately distinct, so a broken harness can never be read
// as either a pass or a fidelity defect:
//
//	0  every check at or below its baseline
//	1  at least one check exceeds its baseline
//	2  usage error
//	3  the harness could not conclude (rmp or git unavailable, a .go file that
//	   will not parse, an implausibly small inventory, a stale fixture)
package main

import (
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed baseline.json
var baselineFS embed.FS

const (
	exitOK      = 0
	exitFail    = 1
	exitUsage   = 2
	exitHarness = 3
)

// baseline is the recorded, reviewable state of each counted backlog.
type baseline struct {
	Measured  string            `json:"measured"`
	Commit    string            `json:"commit"`
	Fixture   []string          `json:"fabricationFixture"`
	Sanity    sanityFloors      `json:"sanityFloors"`
	Checks    map[string]int    `json:"checks"`
	Rationale map[string]string `json:"rationale,omitempty"`
}

// sanityFloors are the floors that stop this gate from ever passing vacuously.
// A checker that reads nothing finds nothing wrong, and would report a clean
// graph while proving only that it failed to look.
type sanityFloors struct {
	Declarations   int `json:"declarations"`
	GoFiles        int `json:"goFiles"`
	SymbolNodes    int `json:"symbolNodes"`
	DocumentedTags int `json:"documentedLabels"`
	DocumentedEdge int `json:"documentedEdgeForms"`
	TasksResolved  int `json:"tasksResolved"`
}

func (b *baseline) of(id string) int { return b.Checks[id] }

func main() {
	code, err := run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "kgverify: %v\n", err)
	}
	os.Exit(code)
}

type options struct {
	repo    string
	roadmap string
	since   string
	emit    string
	verbose bool
	asJSON  bool
	detail  int
	// exclude names checks whose count must be REPORTED but must not fail the run.
	// It exists for one reason (rmp #2677): task-status-disagrees-with-rmp is
	// TIME-VARYING — rmp is the authority, so a task closed without a graph sync
	// raises it with no code change involved. Inside `ci` that would fail a push
	// for a reason unrelated to the change under test. An excluded check still
	// prints its count, so the number never goes unseen.
	exclude map[string]struct{}
}

func parseFlags() (options, error) {
	var o options
	fs := flag.NewFlagSet("kgverify", flag.ContinueOnError)
	fs.StringVar(&o.repo, "repo", "", "repository root (default: git rev-parse --show-toplevel)")
	fs.StringVar(&o.roadmap, "roadmap", "gograph", "rmp roadmap holding the knowledge graph")
	fs.StringVar(&o.since, "since", "", "revision the audited range starts at (default: merge-base with develop)")
	fs.StringVar(&o.emit, "emit", "", "instead of verifying, print the tree's own facts: symbols | missing | packages | absent")
	fs.BoolVar(&o.verbose, "v", false, "list every violation instead of a sample")
	fs.BoolVar(&o.asJSON, "json", false, "emit results as JSON")
	fs.IntVar(&o.detail, "detail", 8, "violations to show per failing check unless -v")
	var excl string
	fs.StringVar(&excl, "exclude", "", "comma-separated check ids to report but not fail on (see options.exclude)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return o, err
	}
	o.exclude = map[string]struct{}{}
	for _, id := range strings.Split(excl, ",") {
		if id = strings.TrimSpace(id); id != "" {
			o.exclude[id] = struct{}{}
		}
	}
	switch o.emit {
	case "", "symbols", "missing", "packages", "absent", "cypher":
	default:
		return o, fmt.Errorf("-emit must be symbols, missing, packages, absent or cypher, got %q", o.emit)
	}
	return o, nil
}

func loadBaseline() (*baseline, error) {
	data, err := baselineFS.ReadFile("baseline.json")
	if err != nil {
		return nil, err
	}
	var b baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("decode baseline.json: %w", err)
	}
	if b.Checks == nil {
		return nil, errors.New("baseline.json has no checks map")
	}
	return &b, nil
}

func run() (int, error) {
	opts, err := parseFlags()
	if err != nil {
		return exitUsage, err
	}
	repo := opts.repo
	if repo == "" {
		cwd, cwdErr := os.Getwd()
		if cwdErr != nil {
			return exitHarness, cwdErr
		}
		if repo, err = gitTopLevel(cwd); err != nil {
			return exitHarness, fmt.Errorf("locating repo root: %w", err)
		}
	}

	base, err := loadBaseline()
	if err != nil {
		return exitHarness, err
	}
	modulePath, err := modulePathOf(repo)
	if err != nil {
		return exitHarness, fmt.Errorf("reading module path: %w", err)
	}
	inv, err := buildInventory(repo, modulePath)
	if err != nil {
		return exitHarness, fmt.Errorf("building tree inventory: %w", err)
	}
	mdl, err := parseModel(filepath.Join(repo, "knowledge-model.md"))
	if err != nil {
		return exitHarness, fmt.Errorf("parsing knowledge-model.md: %w", err)
	}

	symbols, err := readSymbolNodes(repo, opts.roadmap)
	if err != nil {
		return exitHarness, fmt.Errorf("reading symbol nodes: %w", err)
	}
	cov, unresolved := buildPackageCoverage(inv, symbols)
	if opts.emit != "" {
		head, date := gitHeadStamp(repo)
		return emit(opts.emit, inv, symbols, cov, unresolved, head, date)
	}
	if code, err := checkFloors(base.Sanity, inv, len(symbols), mdl); err != nil {
		return code, err
	}

	a := &auditor{inv: inv, mdl: mdl}
	if err := runChecks(a, repo, &opts, base, symbols); err != nil {
		return exitHarness, err
	}
	a.checkPackages(cov, base)
	return report(a, base, repo, &opts, inv, symbols, cov, unresolved)
}

func checkFloors(f sanityFloors, inv *inventory, symbolNodes int, mdl *model) (int, error) {
	type floor struct {
		what string
		got  int
		want int
	}
	for _, c := range []floor{
		{"declarations parsed from the tree", len(inv.decls), f.Declarations},
		{".go files parsed", inv.goFiles, f.GoFiles},
		{"symbol nodes read from the graph", symbolNodes, f.SymbolNodes},
		{"node labels documented in knowledge-model.md", len(mdl.labels), f.DocumentedTags},
		{"edge forms documented in knowledge-model.md", len(mdl.edgeForms), f.DocumentedEdge},
	} {
		if c.got < c.want {
			return exitHarness, fmt.Errorf("harness cannot conclude: only %d %s, floor is %d; a gate that reads nothing finds nothing wrong", c.got, c.what, c.want)
		}
	}
	return exitOK, nil
}

func runChecks(a *auditor, repo string, opts *options, base *baseline, symbols []symNode) error {
	byName := make(map[string][]symNode)
	for _, n := range symbols {
		byName[n.Name] = append(byName[n.Name], n)
	}
	allNames, err := readNamedNodes(repo, opts.roadmap, base.Fixture)
	if err != nil {
		return fmt.Errorf("reading fixture nodes: %w", err)
	}
	if err := a.checkFixture(base.Fixture, byName, allNames, base.of(checkFixtureFabrication)); err != nil {
		return err
	}
	a.checkSymbols(symbols, base)

	tasks, err := readTaskNodes(repo, opts.roadmap)
	if err != nil {
		return fmt.Errorf("reading Task nodes: %w", err)
	}
	ids := uniqueIntIDs(tasks)
	live, missing, err := fetchTasks(repo, opts.roadmap, ids)
	if err != nil {
		return fmt.Errorf("resolving tasks against rmp: %w", err)
	}
	if len(live) < base.Sanity.TasksResolved {
		return fmt.Errorf("harness cannot conclude: rmp resolved only %d tasks, floor is %d", len(live), base.Sanity.TasksResolved)
	}
	a.checkTasks(tasks, live, missing, base)

	since := opts.since
	if since == "" {
		since = gitDefaultSince(repo)
	}
	touched, err := gitTouchedFiles(repo, since, a.inv)
	if err != nil {
		return fmt.Errorf("listing touched files: %w", err)
	}
	a.checkProvenance(touched, symbols, base)

	shapes, err := readEdgeShapes(repo, opts.roadmap)
	if err != nil {
		return fmt.Errorf("reading edge shapes: %w", err)
	}
	a.checkEdges(shapes, base)

	labels, err := readLabelCounts(repo, opts.roadmap)
	if err != nil {
		return fmt.Errorf("reading label counts: %w", err)
	}
	a.checkLabels(labels, base)

	comps, err := readComponentNodes(repo, opts.roadmap)
	if err != nil {
		return fmt.Errorf("reading Component nodes: %w", err)
	}
	a.checkComponents(comps, base)
	return nil
}

func uniqueIntIDs(tasks []taskNode) []int {
	seen := map[int]struct{}{}
	var ids []int
	for _, t := range tasks {
		if id, ok := jsonInt(t.RawID); ok {
			if _, dup := seen[id]; !dup {
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		}
	}
	sort.Ints(ids)
	return ids
}

// emit prints the tree's own facts, which is the other half of closing the
// fabrication route: -emit symbols is the inventory, and -emit missing is the
// sync worklist. A symbol node written by copying a name out of this output
// cannot be a fabrication, because the name came from go/parser rather than
// from prose.
func emit(mode string, inv *inventory, symbols []symNode, cov []pkgCoverage, unresolved map[string]int, syncCommit, syncDate string) (int, error) {
	switch mode {
	case "packages":
		return emitTo(os.Stdout, emitPackages(cov, unresolved),
			fmt.Sprintf("kgverify: %d packages accounted, %d unresolved package keys (packages)\n", len(cov), len(unresolved)))
	case "cypher":
		repair, create := buildSync(inv, symbols, syncCommit, syncDate)
		var b strings.Builder
		for _, st := range repair {
			b.WriteString(st)
			b.WriteByte('\n')
		}
		for _, st := range create {
			b.WriteString(st)
			b.WriteByte('\n')
		}
		return emitTo(os.Stdout, b.String(), fmt.Sprintf(
			"kgverify: emitted %d repair and %d create statements (cypher); run each line through `rmp graph client -r <roadmap> --query`\n",
			len(repair), len(create)))
	case "absent":
		body, n := emitAbsent(inv, symbols)
		return emitTo(os.Stdout, body,
			fmt.Sprintf("kgverify: emitted %d of %d symbol nodes naming a declaration absent from the tree (absent)\n", n, len(symbols)))
	}
	const header = "# kind\tname\timportPath\tfile\trecv\n"
	var buf strings.Builder
	buf.WriteString(header)
	skip := map[string]struct{}{}
	if mode == "missing" {
		for i := range symbols {
			skip[symbols[i].key()] = struct{}{}
		}
	}
	shown := 0
	seen := map[string]struct{}{}
	for _, d := range inv.decls {
		if _, ok := skip[d.key()]; ok {
			continue
		}
		// One line per declaration IDENTITY. Emitting a duplicate key twice
		// would make a MERGE-based sync write the second over the first.
		if _, dup := seen[d.key()]; dup {
			continue
		}
		seen[d.key()] = struct{}{}
		shown++
		fmt.Fprintf(&buf, "%s\t%s\t%s\t%s\t%s\n", d.Kind, d.Name, d.ImportPath, d.File, d.Recv)
	}
	return emitTo(os.Stdout, buf.String(),
		fmt.Sprintf("kgverify: emitted %d of %d declarations (%s)\n", shown, len(inv.decls), mode))
}

// emitTo writes an emit mode's body to out and its provenance line to stderr,
// so the body can be piped into a sync script while the count stays visible to
// the operator. A count that goes unseen is a count nobody ratchets.
func emitTo(out *os.File, body, note string) (int, error) {
	if _, err := out.WriteString(body); err != nil {
		return exitHarness, err
	}
	if _, err := os.Stderr.WriteString(note); err != nil {
		return exitHarness, err
	}
	return exitOK, nil
}

func report(a *auditor, base *baseline, repo string, opts *options, inv *inventory, symbols []symNode, cov []pkgCoverage, unresolved map[string]int) (int, error) {
	failed := 0
	for _, r := range a.results {
		if _, skip := opts.exclude[r.ID]; skip {
			continue
		}
		if r.Exceeded() {
			failed++
		}
	}
	if opts.asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{
			"repo": repo, "baselineMeasured": base.Measured, "results": a.results,
			"failedChecks": failed, "packages": cov, "unresolvedPackageKeys": unresolved,
		}); err != nil {
			return exitHarness, err
		}
		return exitCodeFor(failed), nil
	}

	hash, branch := gitDescribeHead(repo)
	fmt.Printf("kgverify — knowledge-graph fidelity gate\n")
	fmt.Printf("  repo             %s\n", repo)
	fmt.Printf("  head             %s (%s)\n", hash, branch)
	fmt.Printf("  baseline         measured %s at %s\n\n", base.Measured, base.Commit)
	fmt.Printf("harness (floors proved, so a pass cannot be vacuous)\n")
	fmt.Printf("  declarations parsed        %6d\n", len(inv.decls))
	fmt.Printf("  .go files parsed           %6d\n", inv.goFiles)
	fmt.Printf("  symbol nodes in graph      %6d\n", len(symbols))
	fmt.Printf("  labels documented          %6d\n", len(a.mdl.labels))
	fmt.Printf("  edge forms documented      %6d\n", len(a.mdl.edgeForms))
	fmt.Printf("  symbol coverage of tree    %5.1f%%  (%d declarations matched by file+name)\n\n",
		100*float64(coverage(inv, symbols))/float64(max(len(inv.decls), 1)), coverage(inv, symbols))

	modelled, atParity, worst := 0, 0, make([]pkgCoverage, 0, 10)
	for _, c := range cov {
		if !c.Modelled() {
			continue
		}
		modelled++
		if c.Gap == 0 {
			atParity++
		} else if len(worst) < cap(worst) {
			worst = append(worst, c)
		}
	}
	fmt.Printf("per-package parity (acceptance criterion 1 of rmp #2719)\n")
	fmt.Printf("  packages in the tree       %6d\n", len(cov))
	fmt.Printf("  modelled by the graph      %6d  (at least one symbol node)\n", modelled)
	fmt.Printf("  at parity                  %6d  (every declaration has a node)\n", atParity)
	fmt.Printf("  unresolved package keys    %6d  (node keys naming no package in the tree)\n", len(unresolved))
	if len(worst) > 0 {
		fmt.Printf("  widest gaps:\n")
		for _, c := range worst {
			fmt.Printf("    %-58s decls %5d  nodes %5d  gap %5d\n", c.ImportPath, c.Decls, c.Nodes, c.Gap)
		}
	}
	fmt.Printf("  full table: go run ./cmd/kgverify -emit packages\n\n")

	fmt.Printf("%-34s %8s %9s  %s\n", "CHECK", "ACTUAL", "BASELINE", "STATUS")
	for _, r := range a.results {
		status := "ok"
		switch {
		case r.Exceeded():
			status = "FAIL"
		case r.Actual < r.Baseline:
			status = "improved — ratchet the baseline"
		}
		fmt.Printf("%-34s %8d %9d  %s\n", r.ID, r.Actual, r.Baseline, status)
	}

	for _, r := range a.results {
		if !r.Exceeded() && !opts.verbose {
			continue
		}
		if len(r.Violations) == 0 {
			continue
		}
		fmt.Printf("\n%s (%d, baseline %d) — %s\n", r.ID, r.Actual, r.Baseline, r.Note)
		limit := len(r.Violations)
		if !opts.verbose && limit > opts.detail {
			limit = opts.detail
		}
		for _, v := range r.Violations[:limit] {
			fmt.Printf("  - %s\n", v)
		}
		if limit < len(r.Violations) {
			fmt.Printf("  … %d more (use -v)\n", len(r.Violations)-limit)
		}
	}

	fmt.Println()
	if failed == 0 {
		fmt.Printf("PASS: %d checks, every count at or below its baseline\n", len(a.results))
		return exitOK, nil
	}
	var names []string
	for _, r := range a.results {
		if _, skip := opts.exclude[r.ID]; skip {
			continue
		}
		if r.Exceeded() {
			names = append(names, r.ID)
		}
	}
	fmt.Printf("FAIL: %d of %d checks exceed their baseline: %s\n", failed, len(a.results), strings.Join(names, ", "))
	return exitFail, nil
}

// coverage counts the tree's declarations that have a graph node keyed on the
// same file and name. It is context, not a check: the graph was bootstrapped at
// an older commit and has never claimed to cover every declaration. Printing it
// beside the provenance counts is what stops a global gap being misread as a
// sprint-local sync failure.
func coverage(inv *inventory, symbols []symNode) int {
	have := make(map[string]struct{}, len(symbols))
	for i := range symbols {
		have[symbols[i].key()] = struct{}{}
	}
	n := 0
	for i := range inv.decls {
		if _, ok := have[inv.decls[i].key()]; ok {
			n++
		}
	}
	return n
}

func exitCodeFor(failed int) int {
	if failed > 0 {
		return exitFail
	}
	return exitOK
}
