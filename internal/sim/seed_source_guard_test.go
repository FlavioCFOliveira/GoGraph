package sim

import (
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

// seedSourceOwner is the ONLY file in this package allowed to import a
// randomness package. It builds [Seed] as an explicitly-seeded PCG generator;
// every other file must draw from that Seed.
const seedSourceOwner = "seed.go"

// randomnessImportHits returns, for every .go file in dir, each import of a
// randomness package that the file is not entitled to, as "file: path" strings
// sorted for a stable report. It also returns the number of .go files actually
// parsed, so a caller can prove the scan was not vacuous.
//
// The rule it encodes, stated in this package's doc comment, is that the whole
// simulation must be a pure function of one caller-supplied seed:
//
//   - math/rand/v2 is permitted in seedSourceOwner alone, where it is used only
//     as rand.New(rand.NewPCG(val, ...)) — an explicitly-seeded generator.
//   - math/rand (v1) is permitted nowhere. Its top-level functions are
//     auto-seeded, and a rand.Rand built from it adds nothing PCG does not.
//   - crypto/rand is permitted nowhere. It cannot be seeded at all, so any
//     value drawn from it is unreproducible by construction.
func randomnessImportHits(dir string) (hits []string, parsed int, err error) {
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
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return nil, parsed, fmt.Errorf("parse %s: %w", path, parseErr)
		}
		parsed++
		for _, imp := range f.Imports {
			p, uErr := strconv.Unquote(imp.Path.Value)
			if uErr != nil {
				return nil, parsed, fmt.Errorf("unquote import in %s: %w", path, uErr)
			}
			switch p {
			case "math/rand/v2":
				if e.Name() == seedSourceOwner {
					continue
				}
			case "math/rand", "crypto/rand":
				// Never permitted, seedSourceOwner included.
			default:
				continue
			}
			hits = append(hits, fmt.Sprintf("%s: %s", e.Name(), p))
		}
	}
	sort.Strings(hits)
	return hits, parsed, nil
}

// TestSim_SeedIsTheOnlyRandomnessSource enforces the invariant this package's
// doc comment already states: every probabilistic decision anywhere in the
// simulator draws from the single caller-supplied [Seed].
//
// It is the internal half of the rmp #2663 guard. The CLI half
// (cmd/sim.TestSim_NoNonDeterministicRandomSource) stops a run from starting
// without a seed; this one stops a seeded run from consulting anything the seed
// does not control, which would break replay just as completely — and far more
// quietly, since the run would still print a seed that no longer reproduces it.
//
// [TestSim_RandomnessImportScannerCanFire] proves the scanner fires, so a pass
// here means "no unseeded randomness", never "the scanner stopped looking".
func TestSim_SeedIsTheOnlyRandomnessSource(t *testing.T) {
	t.Parallel()
	// The test binary's working directory is the package's source directory.
	hits, parsed, err := randomnessImportHits(".")
	if err != nil {
		t.Fatalf("scan internal/sim for a randomness source: %v", err)
	}
	if parsed == 0 {
		t.Fatal("the scan parsed 0 .go files: it proves nothing about internal/sim")
	}
	if len(hits) != 0 {
		t.Fatalf("%d unpermitted randomness import(s) in internal/sim:\n  %s\n"+
			"The simulation must be a pure function of its seed: a value drawn from anywhere else makes the run\n"+
			"unreproducible while it still reports a seed, so the reported seed becomes a lie. Draw from the Seed\n"+
			"threaded through the call instead — do NOT add a package-level generator, and do NOT seed one from the\n"+
			"clock. %s is the only file entitled to import a randomness package, and only to build the PCG behind Seed.",
			len(hits), strings.Join(hits, "\n  "), seedSourceOwner)
	}
}

// TestSim_RandomnessImportScannerCanFire is the non-vacuity gate for
// [TestSim_SeedIsTheOnlyRandomnessSource].
//
// The real package satisfies the rule today, so the scanner runs against a clean
// tree and would report zero whether it worked or not. These cases build the
// forbidden shapes in a temporary directory and require each to be caught, and
// build the permitted and near-miss shapes and require each to be let through.
func TestSim_RandomnessImportScannerCanFire(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		file string
		src  string
		want string // "" means: expect no finding
	}{
		{
			name: "math/rand/v2 outside the seed owner",
			file: "workload.go",
			src:  "package sim\n\nimport \"math/rand/v2\"\n\nfunc pick() int { return rand.IntN(3) }\n",
			want: "math/rand/v2",
		},
		{
			name: "math/rand/v2 aliased outside the seed owner",
			file: "actor_test.go",
			src:  "package sim\n\nimport mrand \"math/rand/v2\"\n\nfunc pick() uint64 { return mrand.Uint64() }\n",
			want: "math/rand/v2",
		},
		{
			name: "math/rand v1, even in the seed owner",
			file: seedSourceOwner,
			src:  "package sim\n\nimport \"math/rand\"\n\nfunc pick() int64 { return rand.Int63() }\n",
			want: "math/rand",
		},
		{
			name: "crypto/rand, even in the seed owner",
			file: seedSourceOwner,
			src:  "package sim\n\nimport \"crypto/rand\"\n\nvar _ = rand.Reader\n",
			want: "crypto/rand",
		},
		{
			name: "the real exemption: math/rand/v2 in the seed owner",
			file: seedSourceOwner,
			src:  "package sim\n\nimport \"math/rand/v2\"\n\nvar _ = rand.New(rand.NewPCG(1, 2))\n",
		},
		{
			name: "time is not a randomness import and is used for deadlines",
			file: "simserver.go",
			src:  "package sim\n\nimport \"time\"\n\nvar _ = time.Now\n",
		},
		{
			name: "no imports at all",
			file: "oracle.go",
			src:  "package sim\n\nfunc pick() int { return 7 }\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(tc.src), 0o600); err != nil {
				t.Fatalf("write probe: %v", err)
			}
			hits, parsed, err := randomnessImportHits(dir)
			if err != nil {
				t.Fatalf("scan the probe: %v", err)
			}
			if parsed != 1 {
				t.Fatalf("parsed %d files, want exactly the 1 probe", parsed)
			}
			if tc.want == "" {
				if len(hits) != 0 {
					t.Fatalf("scanner fired on a permitted probe: %v", hits)
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
