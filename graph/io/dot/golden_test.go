package dot_test

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/io/dot"
	"gograph/internal/goldens"
)

// TestDOTWrite_Golden produces a deterministic DOT output for a 4-node directed
// cycle and compares it byte-for-byte against a stored golden file.
//
// To regenerate the golden file run:
//
//	GOGRAPH_UPDATE_GOLDENS=1 go test -run TestDOTWrite_Golden ./graph/io/dot/...
func TestDOTWrite_Golden(t *testing.T) {
	t.Parallel()

	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	for i, w := range []int64{10, 20, 30, 40} {
		src := strconv.Itoa(i)
		dst := strconv.Itoa((i + 1) % 4)
		if err := a.AddEdge(src, dst, w); err != nil {
			t.Fatalf("AddEdge %d->%d: %v", i, (i+1)%4, err)
		}
	}

	var buf bytes.Buffer
	if err := dot.Write(&buf, a); err != nil {
		t.Fatalf("Write: %v", err)
	}

	goldens.Assert(t, "testdata/golden_cycle4.dot", buf.Bytes())

	t.Run("graphviz_valid", func(t *testing.T) {
		t.Parallel()
		// Skip if the Graphviz dot binary is not installed.
		if _, err := exec.LookPath("dot"); err != nil {
			t.Skip("graphviz dot not installed")
		}

		cmd := exec.Command("dot", "-Tsvg", "-")
		cmd.Stdin = strings.NewReader(buf.String())
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("dot -Tsvg: %v", err)
		}
		if !strings.Contains(string(out), "<svg") {
			t.Errorf("dot -Tsvg output does not contain <svg:\n%s", out)
		}
	})
}
