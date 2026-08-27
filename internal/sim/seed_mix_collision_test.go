package sim

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// seedConst is one parsed constant: a decorrelating mix, or a scenario's
// catalogue default seed.
type seedConst struct {
	name  string
	value uint64
	site  string
	isMix bool
}

// scanSeedConstants parses every non-test .go file in this package and returns
// the seed-mix and default-seed constants it declares.
//
// It reads the SOURCE rather than the values, which is the whole point.
// [boltSeedMixPairs] records in its own comment that "the mixes are unexported
// constants private to each scenario's file. A table is the closest thing to
// iteration that the types allow" — true of the type system, not of a test. A
// hand-maintained table guards the scenarios someone remembered to add to it;
// this guards the ones nobody will.
func scanSeedConstants(t *testing.T) []seedConst {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var out []seedConst
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, id := range vs.Names {
					// Case-insensitive on purpose. The bare `seedMix` in seed.go is
					// the PCG stream-word constant XORed inside NewSeed itself, so
					// it applies to EVERY default seed, not to one scenario — the
					// widest exposure of the three, and the one a case-sensitive
					// suffix silently skipped.
					lower := strings.ToLower(id.Name)
					mix := strings.HasSuffix(lower, "seedmix") || strings.HasSuffix(lower, "seemix")
					def := strings.HasSuffix(lower, "defaultseed")
					if !mix && !def {
						continue
					}
					if i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.INT {
						continue
					}
					// ParseUint handles Go's cosmetic digit separators, which is
					// exactly what made the original collision invisible on review:
					// 0x2485_B0_0C and 0x2485_B00C are the same number.
					v, err := strconv.ParseUint(strings.ReplaceAll(lit.Value, "_", ""), 0, 64)
					if err != nil {
						continue
					}
					pos := fset.Position(id.Pos())
					out = append(out, seedConst{
						name:  id.Name,
						value: v,
						site:  fmt.Sprintf("%s:%d", filepath.Base(pos.Filename), pos.Line),
						isMix: mix,
					})
				}
			}
		}
	}
	return out
}

// Floors for what the scan must find. They are the counts measured on
// 2026-08-25 (44 mixes, 18 default seeds) minus a small margin, so a rename or a
// parser change that silently stops matching FAILS instead of passing with an
// empty set. A gate that scans nothing agrees with everything.
const (
	minSeedMixConstants     = 41
	minDefaultSeedConstants = 16
)

// TestSeedMixNeverCancelsADefaultSeed guards rmp #2565 for every scenario in the
// package, including ones that do not exist yet.
//
// XOR is self-annihilating. A scenario derives a sub-stream as
// NewSeed(seed ^ someSeedMix) so the sub-stream is decorrelated from the arm
// seed; when the mix equals the catalogue DEFAULT seed, the XOR is zero exactly
// at the default, so the sub-stream runs on NewSeed(0) and the mix achieves
// nothing on the one run every report starts from. The stated intent is silently
// defeated, and the instance that prompted this was invisible on review because
// Go's digit separators are cosmetic: 0x2485_B0_0C and 0x2485_B00C are one
// number.
//
// The rule asserted is stronger than "not equal to its OWN default": no mix may
// equal ANY default seed in the package. These are hand-picked 64-bit values, so
// an accidental equality across two scenarios is not a coincidence — it is a
// constant copied from the wrong place — and the strict form survives a mix
// being repointed at a different scenario, which the pairing form does not.
//
// Sweep result, 2026-08-25: 44 mixes against 18 default seeds, zero collisions.
// The 44th is the bare `seedMix` in seed.go, which a case-sensitive suffix match
// missed — found by running two independent scans and reconciling their counts.
func TestSeedMixNeverCancelsADefaultSeed(t *testing.T) {
	t.Parallel()
	consts := scanSeedConstants(t)

	var mixes, defaults []seedConst
	for _, c := range consts {
		if c.isMix {
			mixes = append(mixes, c)
		} else {
			defaults = append(defaults, c)
		}
	}

	// ASSERT SOMETHING WAS SEEN. Without this the whole test passes when the
	// naming convention changes and the scan quietly matches nothing.
	if len(mixes) < minSeedMixConstants {
		t.Fatalf("scanned only %d seed-mix constant(s), want at least %d: the scan has stopped "+
			"matching and this gate is checking nothing", len(mixes), minSeedMixConstants)
	}
	if len(defaults) < minDefaultSeedConstants {
		t.Fatalf("scanned only %d default-seed constant(s), want at least %d: the scan has stopped "+
			"matching and this gate is checking nothing", len(defaults), minDefaultSeedConstants)
	}

	for _, m := range mixes {
		for _, d := range defaults {
			if m.value != d.value {
				continue
			}
			t.Errorf("%s (%s) = %#x, the same value as %s (%s), so a run at that default seed "+
				"XORs to zero and draws from NewSeed(0): the mix decorrelates nothing on the one "+
				"run every report starts from (rmp #2565). Pick a mix that differs from every "+
				"catalogue default seed.", m.name, m.site, m.value, d.name, d.site)
		}
	}
	t.Logf("swept %d seed-mix constant(s) against %d default seed(s)", len(mixes), len(defaults))
}
