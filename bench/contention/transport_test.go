package contention_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bench/contention"
	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
)

// The transport A/B campaign (rmp #2711).
//
// Two entry points, both opt-in via their own output directory and both subject
// to [requireFreshRun]:
//
//   - [TestTransportNoiseFloor] measures the SAME arm against itself, cell by
//     cell, by the same machinery that will produce the experiment's numbers. It
//     runs FIRST because a delta smaller than the floor is not a finding, and an
//     unmeasured floor makes every delta unfalsifiable.
//   - [TestTransportAB] measures the pipe against a loopback socket with the
//     transport as the only variable.
//
// Both interleave their arms and alternate the order between replicas. Running
// all of one arm and then all of the other lets thermal drift, frequency scaling
// and background activity load onto the arm difference; alternating puts that
// drift into BOTH arms instead.
//
// Each replica of each cell is a FRESH CHILD PROCESS, for the reason the package
// documentation gives: heap warmth, GC goal, mapped pages and populated pools
// are all cumulative within a process, and a second measurement in the same
// process inherits every one of them. Here that would be worse than usual — the
// two arms allocate differently (a pipe buffer against the netpoller), so the
// inheritance would not even be symmetric.

// transportChildMode runs ONE unprofiled window of ONE transport arm.
//
// argv: <kind> <query> <level> <outDir>
const transportChildMode = "transport-observe"

// envTransportFloorDir and envTransportABDir name absolute directories to write
// each campaign's artefacts into. When unset the campaign is skipped: these are
// measurement campaigns, not unit tests, and must never run as a side effect of
// `go test ./...`.
const (
	envTransportFloorDir = "GOGRAPH_TRANSPORT_FLOOR_DIR"
	envTransportABDir    = "GOGRAPH_TRANSPORT_AB_DIR"
)

// envTransportReplicas overrides the replica count per arm per cell.
const envTransportReplicas = "GOGRAPH_TRANSPORT_REPLICAS"

// envTransportLevels overrides the goroutine ladder, as a comma list.
const envTransportLevels = "GOGRAPH_TRANSPORT_LEVELS"

// envTransportQueries restricts the campaign to a comma list of query keys.
const envTransportQueries = "GOGRAPH_TRANSPORT_QUERIES"

// envTransportArms overrides the arms [TestTransportAB] compares, as a comma
// list of names from [contention.TransportArmNames].
//
// It exists for the ATTRIBUTION pass. The default two arms answer "does the
// transport move the curve"; naming the deadline probes instead answers "which
// part of the pipe's transport cost does". Every arm still runs interleaved
// with every other, and the ratio column is still taken against the FIRST arm
// named, which is therefore the baseline the probes are priced against.
const envTransportArms = "GOGRAPH_TRANSPORT_ARMS"

// defaultTransportReplicas is how many times each arm of each cell is measured.
//
// Nine, not one. The floor for a Bolt cell in this repository has been shown to
// be BIMODAL elsewhere in this sprint — 43 of 46 interleaved runs within +/-4% of
// the median while three fell 24-32% below it — so a single run can carry +/-32%
// while a median of ten carries +/-4%. An odd count is chosen so the median is an
// observed value rather than an average of two.
const defaultTransportReplicas = 9

func init() {
	subproc.Register(transportChildMode, func(args []string) int {
		if len(args) != 4 {
			fmt.Fprintf(os.Stderr, "usage: %s <kind> <query> <level> <outDir>\n", transportChildMode)
			return 2
		}
		kind, query, levelArg, outDir := args[0], args[1], args[2], args[3]
		level, err := strconv.Atoi(levelArg)
		if err != nil || level < 1 {
			fmt.Fprintf(os.Stderr, "bad level %q\n", levelArg)
			return 2
		}
		w, err := contention.TransportWorkload(kind, query, level)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 2
		}
		m, err := contention.Observe(w, level, contention.WindowEffect, outDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "observe: %v\n", err)
			return 1
		}
		if err := contention.WriteMetrics(outDir, &m); err != nil {
			fmt.Fprintf(os.Stderr, "write metrics: %v\n", err)
			return 1
		}
		fmt.Printf("OK workload=%s level=%d ops=%d ops_per_sec=%.1f p50_ns=%d p99_ns=%d errors=%d\n",
			m.Workload, m.Level, m.Ops, m.OpsPerSec, m.P50Nanos, m.P99Nanos, m.Errors)
		return 0
	})
}

// transportCell is one measured configuration.
type transportCell struct {
	kind  string
	query string
	level int
}

func (c transportCell) String() string {
	return fmt.Sprintf("%s/%s@%d", c.kind, c.query, c.level)
}

// transportRun is one replica's result.
type transportRun struct {
	cell    transportCell
	label   string // the arm label under which this replica was taken
	replica int
	opsPerS float64
	p50Ns   int64
	p99Ns   int64
	errors  int64
	wall    time.Duration
}

// TestTransportNoiseFloor measures each cell AGAINST ITSELF.
//
// Both labels drive byte-identical children; the only thing that distinguishes
// A1 from A2 is the order in which they were run. Every ratio it reports should
// therefore be 1.000, and how far it is not is this host's floor for that cell,
// under this machinery, on this tree.
//
// It also prints every raw observation, because a floor quoted as a single
// number hides the shape of the distribution it came from. A cell whose nine
// values fall into two clusters has a bimodal floor, and a median-of-nine is the
// right summary of it only if the clusters are read and reported.
func TestTransportNoiseFloor(t *testing.T) {
	root := os.Getenv(envTransportFloorDir)
	if root == "" {
		t.Skipf("set %s=<abs dir> to run the transport noise floor, and pass -v or its output is discarded", envTransportFloorDir)
	}
	requireFreshRun(t)
	runTransportCampaign(t, root, "floor", []string{"A1", "A2"})
}

// TestTransportAB measures the in-memory pipe against a loopback socket.
//
// The two arms ARE the two transports, so unlike the floor the ratio here is
// expected to move. Whether it moves by more than the floor is the verdict.
func TestTransportAB(t *testing.T) {
	root := os.Getenv(envTransportABDir)
	if root == "" {
		t.Skipf("set %s=<abs dir> to run the transport A/B, and pass -v or its output is discarded", envTransportABDir)
	}
	requireFreshRun(t)
	arms := []string{contention.TransportSim, contention.TransportTCP}
	if s := os.Getenv(envTransportArms); s != "" {
		parts := strings.Split(s, ",")
		arms = make([]string, 0, len(parts))
		known := map[string]bool{}
		for _, k := range contention.TransportArmNames() {
			known[k] = true
		}
		for _, part := range parts {
			p := strings.TrimSpace(part)
			if !known[p] {
				t.Fatalf("unknown arm %q in %s; known: %s", p, envTransportArms,
					strings.Join(contention.TransportArmNames(), ", "))
			}
			arms = append(arms, p)
		}
		if len(arms) < 2 {
			t.Fatalf("%s needs at least two arms to compare, got %d", envTransportArms, len(arms))
		}
	}
	runTransportCampaign(t, root, "ab", arms)
}

// runTransportCampaign drives every (cell, label, replica) task in interleaved
// order and writes the raw observations plus a summary.
//
// mode is "floor" (both labels are the SAME transport, so the ratio measures
// noise) or "ab" (the labels ARE the transports).
func runTransportCampaign(t *testing.T, root, mode string, labels []string) {
	t.Helper()
	if !filepath.IsAbs(root) {
		t.Fatalf("output directory must be absolute (the child runs in its own temp cwd), got %q", root)
	}
	replicas := transportReplicas(t)
	levels := transportLevels(t)
	queries := transportQueryKeys(t)

	if err := os.MkdirAll(root, 0o750); err != nil { //nolint:gosec // G703: root is the operator-supplied artefact directory this suite asserts is absolute; the directory name carries no caller-controlled segment.
		t.Fatalf("mkdir %s: %v", root, err)
	}
	before := loadavg(t)
	fmt.Printf("transport %s campaign: %s\n", mode, root)
	fmt.Printf("loadavg BEFORE: %s\n", before)

	// Build the interleaved task list. Within one replica every cell is
	// visited once per label, and the label order FLIPS between replicas so
	// that neither label is systematically first.
	type task struct {
		cell    transportCell
		label   string
		replica int
	}
	var tasks []task
	for r := range replicas {
		for _, q := range queries {
			for _, lvl := range levels {
				order := make([]string, len(labels))
				copy(order, labels)
				if r%2 == 1 {
					for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
						order[i], order[j] = order[j], order[i]
					}
				}
				for _, lab := range order {
					kind := lab
					if mode == "floor" {
						// Both labels drive the SAME transport. Which one is
						// derived from the cell, so the floor is measured for
						// every transport the experiment will use.
						kind = ""
					}
					tasks = append(tasks, task{cell: transportCell{kind: kind, query: q, level: lvl}, label: lab, replica: r})
				}
			}
		}
	}
	// In floor mode each (query, level) is measured for both transports, so
	// expand the empty kind into the real ones.
	if mode == "floor" {
		expanded := make([]task, 0, len(tasks)*len(contention.TransportKinds()))
		for _, tk := range tasks {
			for _, kind := range contention.TransportKinds() {
				c := tk.cell
				c.kind = kind
				expanded = append(expanded, task{cell: c, label: tk.label, replica: tk.replica})
			}
		}
		tasks = expanded
	}

	runs := make([]transportRun, 0, len(tasks))
	start := time.Now()
	for i, tk := range tasks {
		dir := filepath.Join(root, fmt.Sprintf("%s-%s-%s-%d-r%d", tk.cell.kind, tk.cell.query, tk.label, tk.cell.level, tk.replica))
		t0 := time.Now()
		stdout, stderr, err := subproc.RunWithTimeout(t, 20*time.Minute,
			transportChildMode, tk.cell.kind, tk.cell.query, strconv.Itoa(tk.cell.level), dir)
		if err != nil {
			t.Fatalf("%s label=%s r%d: child failed: %v\nstdout: %s\nstderr: %s",
				tk.cell, tk.label, tk.replica, err, stdout, stderr)
		}
		m, err := contention.ReadMetrics(dir)
		if err != nil {
			t.Fatalf("%s label=%s r%d: read metrics: %v", tk.cell, tk.label, tk.replica, err)
		}
		if m.Errors != 0 {
			t.Fatalf("%s label=%s r%d: %d operation errors; a cell with errors is not a measurement",
				tk.cell, tk.label, tk.replica, m.Errors)
		}
		runs = append(runs, transportRun{
			cell: tk.cell, label: tk.label, replica: tk.replica,
			opsPerS: m.OpsPerSec, p50Ns: m.P50Nanos, p99Ns: m.P99Nanos,
			errors: m.Errors, wall: time.Duration(m.WallNanos),
		})
		if (i+1)%10 == 0 || i == len(tasks)-1 {
			fmt.Printf("  %d/%d tasks, %s elapsed (last %s took %s)\n",
				i+1, len(tasks), time.Since(start).Round(time.Second),
				tk.cell, time.Since(t0).Round(time.Millisecond))
		}
	}

	after := loadavg(t)
	fmt.Printf("loadavg AFTER: %s\n", after)

	report := renderTransportReport(mode, runs, labels, before, after, replicas)
	fmt.Print(report)
	if err := os.WriteFile(filepath.Join(root, "report.txt"), []byte(report), 0o600); err != nil { //nolint:gosec // G703: the directory component is a path this test created and the leaf name is a literal, so no traversal segment can enter.
		t.Fatalf("write report: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "raw.tsv"), []byte(renderTransportRaw(runs)), 0o600); err != nil { //nolint:gosec // G703: the directory component is a path this test created and the leaf name is a literal, so no traversal segment can enter.
		t.Fatalf("write raw: %v", err)
	}
	fmt.Printf("transport %s campaign complete: %s\n", mode, filepath.Join(root, "report.txt"))
}

// renderTransportRaw writes every observation, so nothing in the summary rests
// on a number a reader cannot see.
func renderTransportRaw(runs []transportRun) string {
	var b strings.Builder
	fmt.Fprintf(&b, "kind\tquery\tlevel\tlabel\treplica\tops_per_sec\tp50_us\tp99_us\twall_ms\terrors\n")
	for _, r := range runs {
		fmt.Fprintf(&b, "%s\t%s\t%d\t%s\t%d\t%.1f\t%.2f\t%.2f\t%.1f\t%d\n",
			r.cell.kind, r.cell.query, r.cell.level, r.label, r.replica,
			r.opsPerS, float64(r.p50Ns)/1000, float64(r.p99Ns)/1000,
			float64(r.wall.Nanoseconds())/1e6, r.errors)
	}
	return b.String()
}

// reportCell is the key a cell is SUMMARISED under, which is not always the key
// it was RUN under.
//
// In "ab" mode the label IS the transport, so the two arms of one comparison are
// the sim and tcp runs of the same (query, level): the kind must leave the key,
// or each arm becomes its own single-label cell and no ratio is ever formed.
// That is not hypothetical — the first pilot of this harness printed exactly
// that, four cells each with one label and an empty ratio column.
//
// In "floor" mode the labels are A1/A2 and the kind stays in the key, because
// the floor of the pipe and the floor of the socket are different questions.
func reportCell(mode string, c transportCell) transportCell {
	if mode == "ab" {
		c.kind = ""
	}
	return c
}

// renderTransportReport summarises the campaign.
func renderTransportReport(mode string, runs []transportRun, labels []string, before, after string, replicas int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n=== transport %s campaign ===\n", mode)
	fmt.Fprintf(&b, "replicas per arm per cell: %d\n", replicas)
	fmt.Fprintf(&b, "loadavg before: %s\nloadavg after:  %s\n\n", before, after)

	// group[cell][label] = the ops/s observations, in the order taken.
	group := map[transportCell]map[string][]float64{}
	p50 := map[transportCell]map[string][]float64{}
	p99 := map[transportCell]map[string][]float64{}
	var cells []transportCell
	for _, r := range runs {
		c := reportCell(mode, r.cell)
		if _, ok := group[c]; !ok {
			group[c] = map[string][]float64{}
			p50[c] = map[string][]float64{}
			p99[c] = map[string][]float64{}
			cells = append(cells, c)
		}
		group[c][r.label] = append(group[c][r.label], r.opsPerS)
		p50[c][r.label] = append(p50[c][r.label], float64(r.p50Ns)/1000)
		p99[c][r.label] = append(p99[c][r.label], float64(r.p99Ns)/1000)
	}
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].query != cells[j].query {
			return cells[i].query < cells[j].query
		}
		if cells[i].kind != cells[j].kind {
			return cells[i].kind < cells[j].kind
		}
		return cells[i].level < cells[j].level
	})

	fmt.Fprintf(&b, "%-22s %-6s %10s %10s %10s %8s %9s %9s %8s\n",
		"cell", "label", "median/s", "min/s", "max/s", "spread", "p50 us", "p99 us", "ratio")
	for _, c := range cells {
		var first float64
		for i, lab := range labels {
			vs := group[c][lab]
			if len(vs) == 0 {
				continue
			}
			med, lo, hi := medianMinMax(vs)
			spread := 0.0
			if med > 0 {
				spread = (hi - lo) / med * 100
			}
			ratio := ""
			if i == 0 {
				first = med
			} else if first > 0 {
				ratio = fmt.Sprintf("%.4f", med/first)
			}
			name := ""
			if i == 0 {
				name = c.String()
			}
			m50, _, _ := medianMinMax(p50[c][lab])
			m99, _, _ := medianMinMax(p99[c][lab])
			fmt.Fprintf(&b, "%-22s %-6s %10.1f %10.1f %10.1f %7.2f%% %9.2f %9.2f %8s\n",
				name, lab, med, lo, hi, spread, m50, m99, ratio)
		}
		// Every raw observation, in the order taken, so the summary above
		// rests on nothing a reader cannot see -- and so a bimodal cell is
		// visible as two clusters rather than hidden inside a median.
		for _, lab := range labels {
			vs := group[c][lab]
			if len(vs) == 0 {
				continue
			}
			parts := make([]string, len(vs))
			for i, v := range vs {
				parts[i] = fmt.Sprintf("%.0f", v)
			}
			fmt.Fprintf(&b, "%-22s %-6s raw: %s\n", "", lab, strings.Join(parts, " "))
		}
	}

	// Scaling, normalised against the SAME arm's own level-1 median.
	fmt.Fprintf(&b, "\nscaling vs the arm's OWN level 1 (median of %d)\n", replicas)
	fmt.Fprintf(&b, "%-10s %-6s %-6s %10s %10s\n", "query", "kind", "label", "level", "scaling")
	type key struct {
		kind, query, label string
	}
	base := map[key]float64{}
	for _, c := range cells {
		if c.level != 1 {
			continue
		}
		for _, lab := range labels {
			if vs := group[c][lab]; len(vs) > 0 {
				med, _, _ := medianMinMax(vs)
				base[key{c.kind, c.query, lab}] = med
			}
		}
	}
	type scaled struct {
		query, kind, label string
		level              int
		scaling            float64
	}
	var rowsOut []scaled
	for _, c := range cells {
		for _, lab := range labels {
			vs := group[c][lab]
			if len(vs) == 0 {
				continue
			}
			med, _, _ := medianMinMax(vs)
			b1 := base[key{c.kind, c.query, lab}]
			if b1 <= 0 {
				continue
			}
			kind := c.kind
			if kind == "" {
				kind = lab
			}
			rowsOut = append(rowsOut, scaled{c.query, kind, lab, c.level, med / b1})
			fmt.Fprintf(&b, "%-10s %-6s %-6s %10d %10.3f\n", c.query, kind, lab, c.level, med/b1)
		}
	}

	// The scaling RATIO between the two labels is the number the verdict turns
	// on, so it is computed here rather than left to a reader with a
	// calculator. Each arm is normalised against its OWN level-1 cell first
	// (rmp #2712: an un-normalised ceiling ratio was published twice in this
	// sprint and had to be corrected both times).
	fmt.Fprintf(&b, "\nscaling ratio %s / %s, per query and level\n", labels[len(labels)-1], labels[0])
	fmt.Fprintf(&b, "%-10s %10s %12s %12s %10s\n", "query", "level", labels[0], labels[len(labels)-1], "ratio")
	byKey := map[scaled]float64{}
	for _, r := range rowsOut {
		byKey[scaled{query: r.query, label: r.label, level: r.level}] = r.scaling
	}
	seen := map[string]bool{}
	for _, r := range rowsOut {
		k := fmt.Sprintf("%s@%d", r.query, r.level)
		if seen[k] {
			continue
		}
		seen[k] = true
		a := byKey[scaled{query: r.query, label: labels[0], level: r.level}]
		z := byKey[scaled{query: r.query, label: labels[len(labels)-1], level: r.level}]
		ratio := ""
		if a > 0 {
			ratio = fmt.Sprintf("%.4f", z/a)
		}
		fmt.Fprintf(&b, "%-10s %10d %12.3f %12.3f %10s\n", r.query, r.level, a, z, ratio)
	}
	return b.String()
}

// medianMinMax returns the median, minimum and maximum of vs. vs is copied
// before sorting, so the caller's observation order is preserved.
func medianMinMax(vs []float64) (med, lo, hi float64) {
	s := make([]float64, len(vs))
	copy(s, vs)
	sort.Float64s(s)
	n := len(s)
	if n == 0 {
		return 0, 0, 0
	}
	if n%2 == 1 {
		med = s[n/2]
	} else {
		med = (s[n/2-1] + s[n/2]) / 2
	}
	return med, s[0], s[n-1]
}

// loadavg reads the host's load averages. It is called immediately before and
// immediately after the measured window, because a number taken on a busy host
// is contaminated and a run cannot be called idle without having looked.
func loadavg(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("/usr/sbin/sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		// Not fatal: the campaign is still valid, but the bracket is missing
		// and must be reported as missing rather than silently omitted.
		return fmt.Sprintf("UNAVAILABLE (%v)", err)
	}
	return strings.TrimSpace(string(out))
}

func transportReplicas(t *testing.T) int {
	t.Helper()
	if s := os.Getenv(envTransportReplicas); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			t.Fatalf("bad %s=%q", envTransportReplicas, s)
		}
		return n
	}
	return defaultTransportReplicas
}

func transportLevels(t *testing.T) []int {
	t.Helper()
	if s := os.Getenv(envTransportLevels); s != "" {
		parts := strings.Split(s, ",")
		out := make([]int, 0, len(parts))
		for _, part := range parts {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || n < 1 {
				t.Fatalf("bad level %q in %s", part, envTransportLevels)
			}
			out = append(out, n)
		}
		return out
	}
	return []int{1, 8, 64}
}

func transportQueryKeys(t *testing.T) []string {
	t.Helper()
	if s := os.Getenv(envTransportQueries); s != "" {
		parts := strings.Split(s, ",")
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			p := strings.TrimSpace(part)
			if _, err := contention.TransportWorkload(contention.TransportSim, p, 1); err != nil {
				t.Fatalf("bad query %q in %s: %v", p, envTransportQueries, err)
			}
			out = append(out, p)
		}
		return out
	}
	return contention.TransportQueryNames()
}
