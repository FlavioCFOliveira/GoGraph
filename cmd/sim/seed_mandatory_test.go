package main

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestRun_RequiresSeed is the regression guard for rmp #2663.
//
// Every mode that actually runs a simulation must refuse to start when no
// positional seed is given. On the pre-fix tree each of these invocations
// silently drew a seed from the auto-seeded top-level math/rand/v2 generator and
// exited 0, so two identical commands ran two different experiments; on the
// fixed tree each exits 2 and says why.
//
// The two non-simulating modes are covered by
// [TestRun_SeedFreeModesNeedNoSeed], which fails if this refusal is ever widened
// onto them.
func TestRun_RequiresSeed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
	}{
		{"engine", []string{"--ticks=20"}},
		{"engine with no flags at all", nil},
		{"scenario", []string{"--scenario=read-heavy"}},
		{"replay", []string{"--replay"}},
		{"replay of a scenario", []string{"--replay", "--scenario=read-heavy"}},
		{"swarm", []string{"--swarm", "--runs=2", "--workers=1", "--scenario=read-heavy"}},
		{"swarm with coverage report", []string{"--swarm", "--runs=2", "--workers=1", "--coverage-report"}},
		{"wire mode", []string{"--mode=wire"}},
		{"concurrent mode", []string{"--mode=concurrent", "--conns=2", "--ops-per-conn=2"}},
		{"liveness mode", []string{"--mode=liveness", "--conns=2", "--ops-per-conn=2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			code := run(tc.args, &out, &errBuf)
			if code != 2 {
				t.Fatalf("exit code = %d, want 2 (a seedless DST run must be refused, never invented);\nstdout=%q\nstderr=%q",
					code, out.String(), errBuf.String())
			}
			if !strings.Contains(errBuf.String(), "a seed is REQUIRED") {
				t.Fatalf("stderr does not say the seed is required, got %q", errBuf.String())
			}
			// The refusal must precede any simulation work: nothing may be
			// reported on stdout, least of all a "Simulation passed" line.
			if out.Len() != 0 {
				t.Fatalf("a refused run wrote to stdout: %q", out.String())
			}
		})
	}
}

// TestRun_SeedFreeModesNeedNoSeed pins the exact complement of
// [TestRun_RequiresSeed]: the two modes that run no simulation must keep working
// with no seed. Without this, "make the seed mandatory" could be over-applied to
// the whole CLI and nobody would notice that the catalogue became unreadable.
func TestRun_SeedFreeModesNeedNoSeed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"list-scenarios", []string{"--list-scenarios"}, "Scenario catalogue"},
		{"coverage-report without swarm", []string{"--coverage-report"}, "Coverage:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			if code := run(tc.args, &out, &errBuf); code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr=%q", code, errBuf.String())
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("missing %q in output, got %q", tc.want, out.String())
			}
		})
	}
}

// TestRun_SeededModesStillStart proves the refusal did not break the seeded
// path: the same invocations that [TestRun_RequiresSeed] rejects are accepted
// once a seed is supplied, and the seed reaches the run.
func TestRun_SeededModesStillStart(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	if code := run([]string{"42", "--ticks=20"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "Seed: 42") {
		t.Fatalf("the supplied seed did not reach the run, got %q", out.String())
	}
}

// nonDeterministicRandomImports returns, for every .go file in dir, the
// math/rand import paths it declares, as "file: path" strings sorted for a
// stable report. It also returns the number of .go files it actually parsed, so
// a caller can prove the scan was not vacuous.
//
// math/rand and math/rand/v2 both expose an AUTO-SEEDED top-level generator
// (rand.Uint64, rand.Intn, ...) whose value nothing in this command may depend
// on. cmd/sim needs no randomness whatsoever: every value it feeds the
// simulation comes from the caller's seed. Banning the import outright is
// therefore exact, and far more robust than trying to tell an auto-seeded call
// apart from an explicitly-seeded one.
func nonDeterministicRandomImports(dir string) (hits []string, parsed int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return nil, parsed, fmt.Errorf("parse %s: %w", path, err)
		}
		parsed++
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return nil, parsed, fmt.Errorf("unquote import in %s: %w", path, err)
			}
			if p == "math/rand" || p == "math/rand/v2" {
				hits = append(hits, fmt.Sprintf("%s: %s", e.Name(), p))
			}
		}
	}
	sort.Strings(hits)
	return hits, parsed, nil
}

// TestSim_NoNonDeterministicRandomSource is the structural half of the rmp #2663
// guard: it makes the mandatory seed impossible to undo quietly.
//
// [TestRun_RequiresSeed] pins the behaviour, but a future change could restore
// the old convenience by drawing a seed somewhere else in the package. This test
// fails if any file in cmd/sim imports math/rand or math/rand/v2 at all. On the
// pre-fix tree it reports main.go's `mrand "math/rand/v2"` and fails.
//
// [TestSim_RandomSourceScannerCanFire] proves the scanner still fires now that
// the real offender is gone, so a pass here means "no randomness source",
// never "the scanner stopped looking".
func TestSim_NoNonDeterministicRandomSource(t *testing.T) {
	t.Parallel()
	// The test binary's working directory is the package's source directory.
	hits, parsed, err := nonDeterministicRandomImports(".")
	if err != nil {
		t.Fatalf("scan cmd/sim for a randomness source: %v", err)
	}
	if parsed == 0 {
		t.Fatal("the scan parsed 0 .go files: it proves nothing about cmd/sim")
	}
	if len(hits) != 0 {
		t.Fatalf("cmd/sim imports a randomness source in %d place(s):\n  %s\n"+
			"The DST seed is MANDATORY: this command must never be able to invent one, because a run's seed is its\n"+
			"identity and an invented seed makes the run neither reproducible nor comparable. Take the value from the\n"+
			"caller's positional seed instead — do NOT re-add a default, a fallback, or an -auto-seed flag.",
			len(hits), strings.Join(hits, "\n  "))
	}
}

// TestSim_RandomSourceScannerCanFire is the non-vacuity gate for
// [TestSim_NoNonDeterministicRandomSource].
//
// The scanner's subject was deleted by the fix it guards, so from now on it runs
// against a clean package for ever and would report zero whether it worked or
// not. These cases reconstruct the deleted import in a temporary directory —
// under each of its two spellings and under an alias, which is how the offender
// was actually written — and require every one to be caught, plus the imports
// that must NOT be caught.
func TestSim_RandomSourceScannerCanFire(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  string
		want string // "" means: expect no finding
	}{
		{
			// The deleted offender, verbatim in shape.
			name: "aliased math/rand/v2, as the offender was written",
			src:  "package main\n\nimport mrand \"math/rand/v2\"\n\nfunc pick() uint64 { return mrand.Uint64() }\n",
			want: "math/rand/v2",
		},
		{
			name: "plain math/rand/v2",
			src:  "package main\n\nimport \"math/rand/v2\"\n\nfunc pick() uint64 { return rand.Uint64() }\n",
			want: "math/rand/v2",
		},
		{
			name: "the v1 package",
			src:  "package main\n\nimport \"math/rand\"\n\nfunc pick() int64 { return rand.Int63() }\n",
			want: "math/rand",
		},
		{
			name: "inside a grouped import block",
			src:  "package main\n\nimport (\n\t\"fmt\"\n\t\"math/rand\"\n)\n\nfunc pick() string { return fmt.Sprint(rand.Int()) }\n",
			want: "math/rand",
		},
		{
			name: "crypto/rand is a different package and is not the subject",
			src:  "package main\n\nimport \"crypto/rand\"\n\nvar _ = rand.Reader\n",
		},
		{
			name: "a package whose name merely contains rand",
			src:  "package main\n\nimport \"example.com/randomiser\"\n\nvar _ = randomiser.X\n",
		},
		{
			name: "no imports at all",
			src:  "package main\n\nfunc pick() int { return 7 }\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "probe.go"), []byte(tc.src), 0o600); err != nil {
				t.Fatalf("write probe: %v", err)
			}
			hits, parsed, err := nonDeterministicRandomImports(dir)
			if err != nil {
				t.Fatalf("scan the probe: %v", err)
			}
			if parsed != 1 {
				t.Fatalf("parsed %d files, want exactly the 1 probe", parsed)
			}
			if tc.want == "" {
				if len(hits) != 0 {
					t.Fatalf("scanner fired on a clean probe: %v", hits)
				}
				return
			}
			if len(hits) != 1 {
				t.Fatalf("scanner reported %v, want exactly one hit on %s", hits, tc.want)
			}
			if !strings.HasSuffix(hits[0], ": "+tc.want) {
				t.Fatalf("scanner reported %q, want a hit naming %s", hits[0], tc.want)
			}
		})
	}
}
