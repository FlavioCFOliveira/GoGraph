// Package tmphygiene holds the temp-area hygiene gates for the GoGraph
// repository: the suite must not strand directories in the system temporary
// area, and the volume that area lives on must have room for the gate to run.
//
// The package is test-only by design; it has no production surface. It follows
// the precedent of internal/docscheck and internal/scriptgate, which hold the
// documentation and shell-gate regressions on the same terms.
//
// # Why this exists (rmp #2527)
//
// A `make ci` run once failed with seventeen packages red — the openCypher TCK,
// cypher/exec and eight examples among them — every one reporting
//
//	wal: durability failed; the un-synced suffix was discarded and this writer
//	is poisoned: ... no space left on device
//
// The volume was at 100% with 936 MiB free of 460 GiB. That message is not a
// misreport: with the volume full the WAL genuinely cannot fsync, and it
// correctly refuses to acknowledge writes it cannot make durable. The danger is
// that a poisoned writer discarding an un-synced suffix is also exactly what a
// GENUINE durability defect looks like, so a full disk manufactures the most
// alarming possible evidence. The incident's real cost was misattribution, not
// the megabytes.
//
// Two gates follow from that, and they are deliberately different in kind:
//
//   - TestTempArea_OwnedDirectoriesDoNotAccumulate watches the directories this
//     module creates and can therefore fix.
//   - TestTempArea_VolumeHasRoomForTheGate watches free space, whoever consumed
//     it. This is the one that would have saved the triage, and measurement says
//     it has to be scoped this way: on the machine where #2527 was diagnosed
//     this module's own stranded directories held 533 MB, while `go build`'s
//     abandoned work directories held 10.0 GB. A guard that counted only
//     "gograph-*" would have exonerated the temp area on the very run that the
//     temp area broke.
package tmphygiene

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// ─── what the module puts in the system temp area ────────────────────────────

// ownedTempPrefixes are the os.MkdirTemp patterns this module has passed with an
// empty base directory, i.e. the names it creates directly in the system temp
// area. A trailing "*" is stripped: os.MkdirTemp substitutes the random suffix
// there, so what remains is the literal leading text of every directory the
// pattern can produce.
//
// The list is a SUPERSET of the module's current call sites, on purpose, and
// TestOwnedTempPrefixes_CoverEveryTempRootedCallSite enforces exactly that
// direction. A prefix that has been RETIRED — a site moved to a caller-owned
// directory, say — stays listed, so the directories it stranded before the move
// remain visible until someone prunes them. A prefix that is ADDED without being
// listed fails that gate, so the guard cannot silently go blind on a new site.
var ownedTempPrefixes = []string{
	// internal/crashinject — the helper binary cache (the #2527 leak itself).
	"gograph-crashinject-",
	// cmd/crashinject-helper — only reachable when the harness passes no
	// directory, which crashinject.Run never does; listed because the binary is
	// runnable by hand.
	"crashinject-",
	// internal/sim — cross-release worktrees and image directories.
	"gograph-xrelease-image-",
	"gograph-xrelease-diff-",
	"gograph-xrelease-",
	"sim-bulkimport-img-",
	"sim-bulkimport-",
	"sim-bulk-",
	"sim-extern-",
	// internal/sim — the cert-rotation scenario's projection directory (rmp #2481).
	// CertReloader reads through os.Stat and tls.LoadX509KeyPair, so the images the
	// SimDisk holds must be projected onto real files for it to read at all.
	"sim-cert-rotation-",
	// examples/ — each example's throwaway store.
	"gograph-ex04-",
	"gograph-ex05-",
	"gograph-ex17-crash-",
	"gograph-ex17-",
	"gograph-ex18-",
	"gograph-ex21-",
	"gograph-ex35-",
	// examples/27_concurrent_txn passes its prefix through openEngine, so the
	// literal lives at the CALL sites; see indirectTempSites.
	"gograph-ex27-sweep-",
	"gograph-ex27-",
	"ex37-store-",
	"plandiff-shared-",
	// store/... — godoc Example stores and cross-process test fixtures.
	"bulk-example",
	"checkpoint-example",
	"csrfile-example",
	"extern-example-",
	"recovery-example",
	"snap-child-",
	"snapshot-csr-example",
	"snapshot-example",
	"store-db-example",
	"txn-recover-example",
	"txn-example",
	"wal-example",
	// RETIRED (rmp #2527): store/snapshot's hostile-snapshot child now lays its
	// snapshot out inside the parent-owned t.TempDir() it is given as a working
	// directory. Kept listed so the directories it stranded on every previous
	// soak run stay counted.
	"sec-store-oom-",
}

// indirectTempSite records what is known about a file that calls os.MkdirTemp
// with an empty base and a NON-LITERAL pattern. The pattern is a parameter, so
// the scanner in mkdirtemp_scan_test.go cannot read the prefix off the call; the
// prefixes have to be declared here, having been read off the call sites.
//
// sites is the number of such calls expected in that file. Declaring the count
// keeps the escape hatch narrow: a SECOND indirect call added to the same file
// still fails the gate, so the hatch excuses the sites that were inspected and
// nothing else.
type indirectTempSite struct {
	sites    int
	prefixes []string
}

// indirectTempSites is that declaration, keyed by module-relative file path.
// Every prefix listed must also appear in ownedTempPrefixes, which
// TestOwnedTempPrefixes_CoverEveryTempRootedCallSite checks, so an indirect site
// cannot be used to smuggle an unwatched prefix past the guard.
var indirectTempSites = map[string]indirectTempSite{
	// openEngine(ctx, prefix) takes the prefix as a parameter and is called
	// twice, at main.go:303 with "gograph-ex27-" and at main.go:1423 with
	// "gograph-ex27-sweep-". Both paths remove the directory through the
	// cleanup func they return.
	"examples/27_concurrent_txn/main.go": {sites: 1, prefixes: []string{"gograph-ex27-", "gograph-ex27-sweep-"}},
}

// ─── thresholds ──────────────────────────────────────────────────────────────

const (
	// ownedSoftLimit is where the report turns into an explicit warning. It is a
	// warning and not a failure because the temp area legitimately holds live
	// directories from a concurrent run of this same suite, and a gate that goes
	// red because a colleague is running `make ci` in another terminal is worse
	// than no gate: it trains people to ignore it.
	ownedSoftLimit = 16

	// ownedHardLimit is where the report becomes a failure. It is set from
	// measurement, not taste:
	//
	//   - Steady state after #2527 is ZERO. Measured: `go test ./internal/
	//     crashinject/` moved the gograph-crashinject-* count 35→36→37 before
	//     the fix and held it at 37 across four runs after.
	//   - Live directories during a run are a handful. Each is held only while
	//     the creating process or example function runs, so even four concurrent
	//     suites stay far under twenty.
	//   - The #2527 incident had 697.
	//
	// 128 therefore sits more than thirty full-gate runs above anything
	// legitimate concurrency can produce, and more than five times below the
	// accumulation that filled the volume. It bites on real drift and cannot be
	// reached by a colleague's parallel run.
	ownedHardLimit = 128

	// ownedStaleAfter is the age past which an owned directory can only be a leak.
	//
	// This is the dimension the count thresholds CANNOT supply, and the reason it
	// exists is the shape of the #2527 incident itself: the leak was one or two
	// directories per run, so the population crossed no interesting ceiling for
	// weeks. A ceiling of 128 would have fired only on the 65th gate run. Age
	// fires on the NEXT day, because it does not ask how many there are — it asks
	// whether any of them outlived the process that owned it.
	//
	// After #2527 no owned directory survives its creating process at all. The
	// longest a legitimate one can exist is therefore one process lifetime, and
	// the longest sanctioned process lifetime in this repository is the
	// nightly layer's per-package budget (Makefile: NIGHTLY_TIMEOUT ?= 12h).
	// 24 h is twice that, so a directory this old cannot belong to any run that
	// is still permitted to be alive — not a concurrent `make ci`, not a soak
	// run (SOAK_TIMEOUT ?= 4h), not a nightly one.
	ownedStaleAfter = 24 * time.Hour

	// gateFreeBytesFloor is the free space below which `make ci` cannot be
	// trusted, derived from what the gate itself writes. The coverage step
	// produces cover.out (measured 636,852,854 B) and cover.lib.out (measured
	// 499,337,233 B); scripts/cover_gate.sh materialises each as a ".tmp.$$",
	// copies it to a ".pub.$$" and renames that over the previously published
	// file, so up to three copies of each coexist: 3 x (607 + 476) MiB ~= 3.2
	// GiB, before the Go build cache, the test binaries and the WAL fixtures.
	//
	// 4 GiB is the smallest round floor above that. Below it the run is not
	// merely tight — it is the regime in which the WAL starts reporting ENOSPC
	// as a poisoned writer, which is the misreading this gate exists to prevent.
	gateFreeBytesFloor = 4 << 30
)

// ─── the scan ────────────────────────────────────────────────────────────────

// tempFamily is one class of entry in the temp area. owned marks the classes
// this module creates, which are the only ones it can be held responsible for.
type tempFamily struct {
	name  string
	owned bool
	match func(name string) (prefix string, ok bool)
}

// tempFamilies classifies temp-area entries. Ordering matters: the first
// matching family wins, and the module's own prefixes are tested first so a
// directory this module created is never miscredited to another family.
func tempFamilies() []tempFamily {
	return []tempFamily{
		{
			name:  "module temp dirs",
			owned: true,
			match: matchOwnedPrefix,
		},
		{
			// t.TempDir() names its directory after the test. These belong to
			// whatever Go test binary created them — this module's or any other
			// project's on the same machine — so they are reported, never
			// judged. Measured on the #2527 machine: 83 directories, 10.2 MB
			// total, spanning three weeks. Real, but not the disk-filling agent.
			name:  "t.TempDir() leftovers (any Go project)",
			owned: false,
			match: func(n string) (string, bool) {
				for _, p := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
					if strings.HasPrefix(n, p) {
						return p + "*", true
					}
				}
				return "", false
			},
		},
		{
			// The go tool's per-invocation work directory, removed when the tool
			// exits normally and stranded when it is killed. Measured on the
			// #2527 machine: 108 directories, 10.0 GB — 19x this module's own
			// footprint, and the overwhelming majority of the 8.0 GiB the
			// incident's clean-up recovered. Reported, not judged: no change to
			// this module can prevent it.
			name:  "go build work dirs (the go tool)",
			owned: false,
			match: func(n string) (string, bool) {
				if strings.HasPrefix(n, "go-build") {
					return "go-build*", true
				}
				return "", false
			},
		},
	}
}

// matchOwnedPrefix returns the LONGEST prefix in ownedTempPrefixes that name
// starts with. Longest-match matters because the list contains nested prefixes
// ("gograph-xrelease-" and "gograph-xrelease-image-"); without it the per-prefix
// table would credit an image directory to the worktree site.
func matchOwnedPrefix(name string) (string, bool) {
	best := ""
	for _, p := range ownedTempPrefixes {
		if strings.HasPrefix(name, p) && len(p) > len(best) {
			best = p
		}
	}
	return best, best != ""
}

// staleDir is one owned directory that has outlived any process entitled to hold
// it. See ownedStaleAfter.
type staleDir struct {
	name string
	age  time.Duration
}

// familyCount is one family's contribution to a scan.
type familyCount struct {
	name     string
	owned    bool
	dirs     int
	bytes    int64
	byPrefix map[string]int
}

// tempScan is what one pass over a temp root observed.
type tempScan struct {
	root       string
	entries    int
	families   []familyCount
	ownedDirs  int
	ownedBytes int64
	// ownedStale lists the owned directories older than ownedStaleAfter, newest
	// first, and is the verdict input for TestTempArea_NoStaleOwnedDirectories.
	ownedStale []staleDir
	// truncated records that the file budget ran out, so bytes is a lower bound.
	truncated bool
}

// sizeBudget caps how many files a scan will stat. A leaked directory can hold a
// large store; the byte figure is diagnostic context, never a verdict, so it is
// not worth an unbounded walk inside every `go test ./...`.
const sizeBudget = 50_000

// scanTempArea classifies the immediate children of root. It never recurses for
// classification — only for the diagnostic byte total of entries that already
// matched a family — so an unrelated deep tree in the temp area costs nothing.
func scanTempArea(root string) (tempScan, error) {
	return scanTempAreaAt(root, time.Now())
}

// scanTempAreaAt is scanTempArea with the reference instant injected, so the
// staleness verdict can be exercised against directories planted with a known
// mtime instead of waiting a day for one to age.
func scanTempAreaAt(root string, now time.Time) (tempScan, error) {
	dirEntries, err := os.ReadDir(root)
	if err != nil {
		return tempScan{}, fmt.Errorf("read temp root %q: %w", root, err)
	}

	fams := tempFamilies()
	counts := make([]familyCount, len(fams))
	for i, f := range fams {
		counts[i] = familyCount{name: f.name, owned: f.owned, byPrefix: map[string]int{}}
	}

	scan := tempScan{root: root, entries: len(dirEntries)}
	budget := sizeBudget
	for _, e := range dirEntries {
		if !e.IsDir() {
			continue
		}
		for i, f := range fams {
			prefix, ok := f.match(e.Name())
			if !ok {
				continue
			}
			counts[i].dirs++
			counts[i].byPrefix[prefix]++
			n, spent, full := dirBytes(filepath.Join(root, e.Name()), budget)
			counts[i].bytes += n
			budget -= spent
			if full {
				scan.truncated = true
			}
			if f.owned {
				// A vanished entry (a concurrent run finishing mid-scan) is not
				// stale and not an error; it is exactly the outcome wanted.
				if info, ierr := e.Info(); ierr == nil {
					if age := now.Sub(info.ModTime()); age > ownedStaleAfter {
						scan.ownedStale = append(scan.ownedStale, staleDir{name: e.Name(), age: age})
					}
				}
			}
			break
		}
	}

	for _, c := range counts {
		scan.families = append(scan.families, c)
		if c.owned {
			scan.ownedDirs += c.dirs
			scan.ownedBytes += c.bytes
		}
	}
	slices.SortFunc(scan.ownedStale, func(a, b staleDir) int {
		if a.age != b.age {
			return int(a.age - b.age) // ascending age == newest first
		}
		return strings.Compare(a.name, b.name)
	})
	return scan, nil
}

// dirBytes sums the apparent size of the files under dir, stopping once budget
// files have been examined. It returns the bytes counted, the number of files
// examined, and whether the budget was exhausted. Errors are swallowed by
// design: a temp entry can vanish under a concurrent run mid-walk, and a
// diagnostic byte total must never turn that race into a failure.
func dirBytes(dir string, budget int) (bytes int64, examined int, exhausted bool) {
	if budget <= 0 {
		return 0, 0, true
	}
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // a vanishing temp entry is expected, not a failure
		}
		if d.IsDir() {
			return nil
		}
		examined++
		if examined > budget {
			exhausted = true
			return filepath.SkipAll
		}
		if info, ierr := d.Info(); ierr == nil {
			bytes += info.Size()
		}
		return nil
	})
	_ = err // see the doc comment: walk errors are diagnostic noise here.
	return bytes, examined, exhausted
}

// report renders a scan as a stable, human-readable table for t.Logf. It is
// emitted on every run, pass or fail: a guard whose output is only visible when
// it fails cannot be sanity-checked when it passes.
func (s *tempScan) report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "temp area %s — %d entries\n", s.root, s.entries)
	for _, f := range s.families {
		tag := "reported only"
		if f.owned {
			tag = "OWNED by this module"
		}
		fmt.Fprintf(&b, "  %-40s %4d dirs %9.1f MB  (%s)\n", f.name, f.dirs, float64(f.bytes)/1e6, tag)
		prefixes := make([]string, 0, len(f.byPrefix))
		for p := range f.byPrefix {
			prefixes = append(prefixes, p)
		}
		sort.Slice(prefixes, func(i, j int) bool {
			if f.byPrefix[prefixes[i]] != f.byPrefix[prefixes[j]] {
				return f.byPrefix[prefixes[i]] > f.byPrefix[prefixes[j]]
			}
			return prefixes[i] < prefixes[j]
		})
		for _, p := range prefixes {
			fmt.Fprintf(&b, "      %4d  %s\n", f.byPrefix[p], p)
		}
	}
	if len(s.ownedStale) > 0 {
		fmt.Fprintf(&b, "  STALE owned dirs (older than %s — cannot belong to a live run):\n", ownedStaleAfter)
		for _, d := range s.ownedStale {
			fmt.Fprintf(&b, "      %6.1f h  %s\n", d.age.Hours(), d.name)
		}
	}
	if s.truncated {
		fmt.Fprintf(&b, "  (byte totals are a LOWER BOUND: the %d-file sizing budget was exhausted)\n", sizeBudget)
	}
	return b.String()
}

// TestTempArea_NoStaleOwnedDirectories is the gate that sees a SLOW leak.
//
// # What this protects against, and what it does not
//
// TestTempArea_OwnedDirectoriesDoNotAccumulate answers "are there suddenly a lot
// of them?", which catches a burst — a single run stranding dozens. It is
// structurally blind to the failure that actually happened in #2527: one or two
// directories per run, which crosses no ceiling for weeks. At the measured
// pre-fix rate of two per `make ci`, a ceiling of 128 fires on the 65th run.
//
// This gate asks a different question — "did any of them outlive the process that
// owned it?" — and answers it on the next day rather than the 65th run. One
// stranded directory is enough to fail it, so a steady one-per-run leak cannot
// hide under a threshold.
//
// It still does NOT do two things, and should not be read as if it did. It cannot
// attribute a stranded directory to the package that created it: the temp area is
// flat and carries no provenance beyond the prefix. And it cannot see a leak that
// something else prunes within ownedStaleAfter — a developer clearing the temp
// area, or an OS reaper on a host that has one. Attribution stays with the
// per-site reasoning in the code, and the free-space gate remains the backstop
// for the case where the volume fills whatever the cause.
func TestTempArea_NoStaleOwnedDirectories(t *testing.T) {
	t.Parallel()

	scan, err := scanTempArea(os.TempDir())
	if err != nil {
		t.Skipf("cannot read the system temp area: %v", err)
	}

	if len(scan.ownedStale) == 0 {
		t.Logf("no owned directory in %s is older than %s (%d owned present in total)",
			scan.root, ownedStaleAfter, scan.ownedDirs)
		return
	}

	names := make([]string, 0, len(scan.ownedStale))
	for _, d := range scan.ownedStale {
		names = append(names, fmt.Sprintf("%s (%.1f h old)", d.name, d.age.Hours()))
	}
	t.Errorf("%d directory/directories created by this module have outlived every process "+
		"entitled to hold them (older than %s; the longest sanctioned process lifetime in this "+
		"repository is the nightly layer's %s per package):\n    %s\n\n"+
		"Each one is a site that created a directory and did not remove it. Deferred cleanup "+
		"cannot survive a killed process, so a run interrupted with Ctrl-C or SIGKILL strands "+
		"one legitimately — if that is what happened, prune it and move on:\n    %s\n\n"+
		"Otherwise find the site: this is the shape of rmp #2527, where one or two per run "+
		"reached 697 without ever crossing a count threshold.",
		len(scan.ownedStale), ownedStaleAfter, "12h", strings.Join(names, "\n    "), pruneHint(scan.root))
}

// ─── gate 1: the module's own directories ────────────────────────────────────

// TestTempArea_OwnedDirectoriesDoNotAccumulate fails when the directories this
// module creates in the system temp area have accumulated past the point where
// only a leak explains them. See ownedHardLimit for the derivation of the
// threshold and for why the softer band warns rather than fails.
func TestTempArea_OwnedDirectoriesDoNotAccumulate(t *testing.T) {
	t.Parallel()

	scan, err := scanTempArea(os.TempDir())
	if err != nil {
		// An unreadable temp root is an environment precondition, not a defect
		// in this module.
		t.Skipf("cannot read the system temp area: %v", err)
	}

	// The witness, always. This is the line that makes the guard auditable on a
	// green run, and the line a triage would read next to a WAL ENOSPC error.
	t.Logf("%s", scan.report())

	switch {
	case scan.ownedDirs > ownedHardLimit:
		t.Errorf("%d directories created by this module are stranded in %s (%.1f MB); "+
			"the limit is %d.\n\n"+
			"This is how rmp #2527 happened: stranded temp directories filled the volume, "+
			"and a full volume makes every WAL append fail with ENOSPC, which the WAL "+
			"reports as a poisoned writer that discarded its un-synced suffix — "+
			"indistinguishable from a genuine durability defect.\n\n"+
			"Find the site that is not cleaning up, then prune the backlog with:\n"+
			"    %s",
			scan.ownedDirs, scan.root, float64(scan.ownedBytes)/1e6, ownedHardLimit, pruneHint(scan.root))
	case scan.ownedDirs > ownedSoftLimit:
		t.Logf("WARNING: %d module-owned directories are present in %s (%.1f MB), above the "+
			"soft limit of %d. Live directories from a concurrent run of this suite are "+
			"expected and harmless; a count that keeps climbing between runs is a leak. "+
			"The hard limit is %d.",
			scan.ownedDirs, scan.root, float64(scan.ownedBytes)/1e6, ownedSoftLimit, ownedHardLimit)
	default:
		t.Logf("%d module-owned directories present (soft limit %d, hard limit %d)",
			scan.ownedDirs, ownedSoftLimit, ownedHardLimit)
	}
}

// pruneHint renders a ready-to-paste removal command for the module's own
// prefixes. It is printed instead of shipping a Makefile target, so the fix is
// visible at the point of failure and needs no second file to discover.
func pruneHint(root string) string {
	quoted := make([]string, 0, len(ownedTempPrefixes))
	for _, p := range ownedTempPrefixes {
		quoted = append(quoted, "'"+p+"'*")
	}
	return "rm -rf " + filepath.Join(root, "{"+strings.Join(quoted, ",")+"}")
}

// TestScanTempArea_DetectsPlantedAccumulation is the sensitivity proof for gate
// 1. The gate is worth nothing unless it fails on a real accumulation, so this
// plants directories on a real filesystem — under t.TempDir(), never the shared
// temp root — and asserts the scan both counts them and crosses each threshold.
func TestScanTempArea_DetectsPlantedAccumulation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// plant maps a directory name to a file it should contain.
		plant     []string
		wantOwned int
		wantBand  string // "clean" | "warn" | "fail"
	}{
		{
			name:      "empty temp root is clean",
			plant:     nil,
			wantOwned: 0,
			wantBand:  "clean",
		},
		{
			name:      "unrelated directories are not credited to this module",
			plant:     []string{"someone-elses-cache", "go-build999", "TestOther123"},
			wantOwned: 0,
			wantBand:  "clean",
		},
		{
			name:      "a handful of module dirs stays clean",
			plant:     plantNames("gograph-crashinject-", 3),
			wantOwned: 3,
			wantBand:  "clean",
		},
		{
			name:      "past the soft limit warns",
			plant:     plantNames("gograph-crashinject-", ownedSoftLimit+1),
			wantOwned: ownedSoftLimit + 1,
			wantBand:  "warn",
		},
		{
			name:      "past the hard limit FAILS",
			plant:     plantNames("gograph-crashinject-", ownedHardLimit+1),
			wantOwned: ownedHardLimit + 1,
			wantBand:  "fail",
		},
		{
			name: "the accumulation is detected across MIXED prefixes, not just one",
			plant: slices.Concat(
				plantNames("gograph-crashinject-", 40),
				plantNames("sec-store-oom-", 40),
				plantNames("gograph-ex17-crash-", 40),
				plantNames("wal-example", 9),
			),
			wantOwned: 129,
			wantBand:  "fail",
		},
		{
			name:      "a RETIRED prefix is still counted",
			plant:     plantNames("sec-store-oom-", ownedHardLimit+1),
			wantOwned: ownedHardLimit + 1,
			wantBand:  "fail",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			for _, name := range tc.plant {
				dir := filepath.Join(root, name)
				if err := os.Mkdir(dir, 0o750); err != nil {
					t.Fatalf("plant %q: %v", name, err)
				}
				// A payload, so the byte column is exercised too.
				if err := os.WriteFile(filepath.Join(dir, "payload"), []byte("0123456789"), 0o600); err != nil {
					t.Fatalf("plant payload in %q: %v", name, err)
				}
			}

			scan, err := scanTempArea(root)
			if err != nil {
				t.Fatalf("scanTempArea: %v", err)
			}
			if scan.ownedDirs != tc.wantOwned {
				t.Errorf("ownedDirs = %d, want %d\n%s", scan.ownedDirs, tc.wantOwned, scan.report())
			}
			if got := band(scan.ownedDirs); got != tc.wantBand {
				t.Errorf("band = %q, want %q (ownedDirs=%d)", got, tc.wantBand, scan.ownedDirs)
			}
			if tc.wantOwned > 0 && scan.ownedBytes != int64(tc.wantOwned)*10 {
				t.Errorf("ownedBytes = %d, want %d — the sizing walk is not reaching the planted payloads",
					scan.ownedBytes, tc.wantOwned*10)
			}
		})
	}
}

// TestScanTempArea_DetectsStaleOwnedDirectory is the sensitivity proof for
// TestTempArea_NoStaleOwnedDirectories. The gate's whole value is that ONE
// stranded directory fails it, which is unfalsifiable unless a single planted
// stale directory is shown to be detected while a young one beside it is not.
//
// The clock is injected rather than waited on, and the mtimes are planted with
// os.Chtimes, so the proof exercises the real scan on the real filesystem.
func TestScanTempArea_DetectsStaleOwnedDirectory(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		// plant maps directory name to its age at `now`.
		plant     map[string]time.Duration
		wantStale []string
	}{
		{
			name:      "a single stale owned dir is DETECTED",
			plant:     map[string]time.Duration{"gograph-crashinject-1": 48 * time.Hour},
			wantStale: []string{"gograph-crashinject-1"},
		},
		{
			name: "a young owned dir beside it is NOT flagged",
			plant: map[string]time.Duration{
				"gograph-crashinject-1": 48 * time.Hour,
				"gograph-crashinject-2": 5 * time.Minute,
			},
			wantStale: []string{"gograph-crashinject-1"},
		},
		{
			name: "a long soak or nightly run's live dir is NOT flagged",
			plant: map[string]time.Duration{
				// The nightly layer's per-package budget is 12h; a directory held
				// for its whole duration must stay acceptable.
				"gograph-crashinject-nightly": 12 * time.Hour,
				"gograph-ex17-soak":           4 * time.Hour,
			},
			wantStale: nil,
		},
		{
			name:      "exactly at the boundary is NOT stale (strictly greater)",
			plant:     map[string]time.Duration{"gograph-ex04-boundary": ownedStaleAfter},
			wantStale: nil,
		},
		{
			name:      "one second past the boundary IS stale",
			plant:     map[string]time.Duration{"gograph-ex04-boundary": ownedStaleAfter + time.Second},
			wantStale: []string{"gograph-ex04-boundary"},
		},
		{
			name: "an ancient dir that is NOT ours is ignored",
			plant: map[string]time.Duration{
				"go-build999":  90 * 24 * time.Hour,
				"TestOther123": 90 * 24 * time.Hour,
			},
			wantStale: nil,
		},
		{
			name: "the one-per-run leak shape: several stale, reported newest first",
			plant: map[string]time.Duration{
				"gograph-crashinject-oldest": 96 * time.Hour,
				"gograph-crashinject-middle": 72 * time.Hour,
				"gograph-crashinject-newest": 48 * time.Hour,
			},
			wantStale: []string{
				"gograph-crashinject-newest",
				"gograph-crashinject-middle",
				"gograph-crashinject-oldest",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			for name, age := range tc.plant {
				dir := filepath.Join(root, name)
				if err := os.Mkdir(dir, 0o750); err != nil {
					t.Fatalf("plant %q: %v", name, err)
				}
				stamp := now.Add(-age)
				if err := os.Chtimes(dir, stamp, stamp); err != nil {
					t.Fatalf("chtimes %q: %v", name, err)
				}
			}

			scan, err := scanTempAreaAt(root, now)
			if err != nil {
				t.Fatalf("scanTempAreaAt: %v", err)
			}
			got := make([]string, 0, len(scan.ownedStale))
			for _, d := range scan.ownedStale {
				got = append(got, d.name)
			}
			if !slices.Equal(got, tc.wantStale) {
				t.Errorf("stale = %v, want %v\n%s", got, tc.wantStale, scan.report())
			}
		})
	}
}

// band names the verdict TestTempArea_OwnedDirectoriesDoNotAccumulate would
// reach for a given count. Both the gate and its sensitivity proof read the same
// thresholds through it, so the proof cannot drift from the gate it certifies.
func band(ownedDirs int) string {
	switch {
	case ownedDirs > ownedHardLimit:
		return "fail"
	case ownedDirs > ownedSoftLimit:
		return "warn"
	default:
		return "clean"
	}
}

func plantNames(prefix string, n int) []string {
	names := make([]string, 0, n)
	for i := range n {
		names = append(names, fmt.Sprintf("%s%09d", prefix, i))
	}
	return names
}

// ─── gate 2: free space ──────────────────────────────────────────────────────

// TestTempArea_VolumeHasRoomForTheGate is the gate that answers the misreading
// #2527 caused. When the temp volume is nearly full, the WAL's ENOSPC report
// reads as a durability defect; this states plainly that it is not.
//
// It is a FAILURE and not a warning for two reasons. Below the floor `make ci`
// cannot pass anyway — the coverage step alone needs ~3.2 GiB — so the check
// cannot be a nuisance on a volume that could have run the gate. And a warning
// buried in a twenty-minute log beside seventeen red packages is precisely the
// signal that went unread.
func TestTempArea_VolumeHasRoomForTheGate(t *testing.T) {
	t.Parallel()

	root := os.TempDir()
	free, err := availableBytes(root)
	if err != nil {
		// Degrade ONE capability rather than deleting the whole gate: on a
		// platform without the statfs binding the free-space figure is
		// unavailable, and saying so is more honest than a silent pass.
		t.Skipf("free space on %s is not observable on this platform: %v", root, err)
	}

	t.Logf("temp volume %s: %.2f GiB free (floor %.2f GiB)",
		root, float64(free)/(1<<30), float64(gateFreeBytesFloor)/(1<<30))

	if free < gateFreeBytesFloor {
		t.Errorf("only %.2f GiB free on the volume holding %s; the gate needs at least %.2f GiB.\n\n"+
			"READ ANY WAL DURABILITY FAILURE IN THIS RUN AS ENVIRONMENTAL. On a full volume "+
			"fsync fails with ENOSPC and store/wal correctly refuses to acknowledge the write, "+
			"reporting a poisoned writer that discarded its un-synced suffix. That message is "+
			"identical to the one a genuine durability defect produces, and in rmp #2527 it "+
			"turned seventeen red packages into a suspected data-loss bug. Free space first, "+
			"then re-read the failures.\n\n"+
			"The floor is what this gate itself writes: scripts/cover_gate.sh keeps up to three "+
			"copies each of cover.out (~607 MiB) and cover.lib.out (~476 MiB) while publishing "+
			"them, ~3.2 GiB, before the Go build cache and the test binaries.",
			float64(free)/(1<<30), root, float64(gateFreeBytesFloor)/(1<<30))
	}
}

// TestAvailableBytes_ReportsAPlausibleFigure is the sensitivity proof for gate
// 2: it certifies the instrument rather than the machine. A statfs binding that
// silently returned zero would make the gate fire on every host; one that
// returned an error for a valid path would make it skip on every host, which is
// the same as deleting it.
func TestAvailableBytes_ReportsAPlausibleFigure(t *testing.T) {
	t.Parallel()

	free, err := availableBytes(os.TempDir())
	if err != nil {
		t.Skipf("free space is not observable on this platform: %v", err)
	}
	if free == 0 {
		t.Fatal("availableBytes reported 0 for the system temp area; the binding is broken, " +
			"which would make the free-space gate fire unconditionally")
	}

	// A path that does not exist must be an ERROR, not a zero. Without this the
	// gate could not tell "no room" from "could not look".
	missing := filepath.Join(t.TempDir(), "no-such-directory")
	if _, err := availableBytes(missing); err == nil {
		t.Errorf("availableBytes(%q) returned no error for a non-existent path; "+
			"an unobservable volume would be indistinguishable from a full one", missing)
	}
	t.Logf("availableBytes(%s) = %.2f GiB, and a missing path errors — the instrument discriminates",
		os.TempDir(), float64(free)/(1<<30))
}

// ─── gate 3: the prefix list cannot go stale ─────────────────────────────────

// TestOwnedTempPrefixes_CoverEveryTempRootedCallSite reads the module's own
// source and asserts that every os.MkdirTemp call with an EMPTY base directory —
// i.e. every site that writes straight into the system temp area — uses a
// pattern the guard above recognises.
//
// Without it the guard rots: a new site with a new prefix would be invisible,
// and the first gate would keep reporting a reassuring zero while that site
// stranded directories on every run.
func TestOwnedTempPrefixes_CoverEveryTempRootedCallSite(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	sites, err := tempRootedMkdirTempSites(root)
	if err != nil {
		t.Fatalf("scan module source: %v", err)
	}

	// NON-VACUITY, separate from the verdict. Zero sites would mean the scanner
	// is broken, and the loop below would then pass without examining anything.
	if len(sites) == 0 {
		t.Fatal("found no os.MkdirTemp call with an empty base directory; the source scan " +
			"is broken, so the coverage check below is vacuous")
	}
	t.Logf("%d temp-rooted os.MkdirTemp site(s) found", len(sites))

	indirectSeen := map[string]int{}
	for _, s := range sites {
		file, _, _ := strings.Cut(s.where, ":")

		if s.pattern == "" {
			// A parameterised pattern: the prefix is not readable here, so the
			// file must declare it.
			decl, declared := indirectTempSites[file]
			if !declared {
				t.Errorf("%s: os.MkdirTemp with an empty base and a NON-LITERAL pattern. "+
					"The guard in this package matches on literal prefixes and cannot see the "+
					"directories this creates. Either give the site a literal prefix, point it "+
					"at a caller-owned directory (t.TempDir(), or the working directory that "+
					"subproc.RunCtx already sets to the parent's t.TempDir()), or — if the "+
					"prefix must stay a parameter — declare the file and its call sites' "+
					"prefixes in indirectTempSites.", s.where)
				continue
			}
			indirectSeen[file]++
			for _, prefix := range decl.prefixes {
				if _, ok := matchOwnedPrefix(prefix); !ok {
					t.Errorf("indirectTempSites[%q] declares prefix %q, which ownedTempPrefixes "+
						"does not list; the guard would still be blind to it", file, prefix)
				}
			}
			continue
		}

		if _, ok := matchOwnedPrefix(strings.TrimSuffix(s.pattern, "*")); !ok {
			t.Errorf("%s: os.MkdirTemp(\"\", %q) creates directories in the system temp area "+
				"under a prefix that ownedTempPrefixes does not list, so the accumulation "+
				"guard is blind to it. Add %q to ownedTempPrefixes, or hand the site a "+
				"caller-owned directory (t.TempDir(), or the working directory that "+
				"subproc.RunCtx already sets to the parent's t.TempDir()).",
				s.where, s.pattern, strings.TrimSuffix(s.pattern, "*"))
		}
	}

	// The escape hatch must stay exactly as wide as it was inspected to be: no
	// extra indirect call in a declared file, and no declaration left behind for
	// a file that no longer has one.
	for file, decl := range indirectTempSites {
		if got := indirectSeen[file]; got != decl.sites {
			t.Errorf("indirectTempSites[%q] excuses %d non-literal os.MkdirTemp call(s), but the "+
				"scan found %d. Re-read the file: an added call needs its prefixes checked, and "+
				"a removed one means the declaration should go.", file, decl.sites, got)
		}
	}
}
