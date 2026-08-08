package isolationtest

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/goldens"
)

// Check runs the spec and compares its transcript against
// testdata/<spec name>.golden, failing the test with a permutation-anchored diff
// on any difference.
//
// The golden file IS the assertion. It records, for every interleaving, exactly
// what each step returned — so a change in isolation behaviour shows up as a
// diff naming the permutation and the step. That is the property a randomised
// search cannot give (see the package doc, and rmp #2333 / #2336).
//
// Comparison and updating go through [goldens.Assert], the project's existing
// golden-file helper, so `-update` and `GOGRAPH_UPDATE_GOLDENS=1` behave here
// exactly as they do everywhere else in the module. READ THE DIFF BEFORE
// UPDATING: these transcripts exist to make an isolation change impossible to
// merge unnoticed, and blanket-updating them is the one action that defeats
// them.
//
// The pre-diff below runs only when the transcripts differ and the run is NOT
// an update; it costs nothing on the passing path and turns goldens.Assert's
// line diff into something that names a re-runnable permutation.
func Check(t *testing.T, s *Spec, r *Runner) {
	t.Helper()
	var buf bytes.Buffer
	if err := r.Run(context.Background(), s, &buf); err != nil {
		t.Fatalf("run spec %s: %v", s.Name, err)
	}
	path := filepath.Join("testdata", s.Name+".golden")
	if !goldens.UpdateRequested() {
		if want, err := readGolden(path); err == nil && !bytes.Equal(want, buf.Bytes()) {
			t.Errorf("spec %s: transcript differs from %s\n%s",
				s.Name, path, diffLines(string(want), buf.String()))
		}
	}
	goldens.Assert(t, path, buf.Bytes())
}

// diffLines renders the first divergences between two transcripts, each tagged
// with the permutation that was in force, so a failure names something the
// reader can replay on its own via [Runner.Only].
func diffLines(want, got string) string {
	wl := strings.Split(want, "\n")
	gl := strings.Split(got, "\n")
	var b strings.Builder
	perm := "<before any permutation>"
	shown := 0
	n := max(len(wl), len(gl))
	for i := range n {
		var wline, gline string
		if i < len(wl) {
			wline = wl[i]
		}
		if i < len(gl) {
			gline = gl[i]
		}
		if p, ok := strings.CutPrefix(wline, "starting permutation: "); ok {
			perm = p
		}
		if wline == gline {
			continue
		}
		if shown == 0 {
			b.WriteString("first divergence:\n")
		}
		if shown >= 8 {
			b.WriteString("  … (further differences suppressed)\n")
			break
		}
		b.WriteString("  permutation: " + perm + "\n")
		b.WriteString("  line " + strconv.Itoa(i+1) + "\n")
		b.WriteString("    want: " + wline + "\n")
		b.WriteString("    got:  " + gline + "\n")
		b.WriteString("  replay it alone with Runner{Only: \"" + perm + "\"}\n")
		shown++
	}
	if shown == 0 {
		b.WriteString("(transcripts differ only in trailing content)\n")
	}
	return b.String()
}

// readGolden reads a golden transcript, relative to the calling test's package
// directory exactly as [goldens.Assert] resolves it.
func readGolden(path string) ([]byte, error) {
	return os.ReadFile(filepath.Clean(path))
}
